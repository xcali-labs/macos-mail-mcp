package mail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// buildIndex makes a synthetic Envelope Index. All content is invented.
func buildIndex(t *testing.T, senderTable bool) string {
	return buildIndexLayout(t, senderTable, senderTable)
}

// buildIndexLayout: senderTable creates sender_addresses; viaTable makes
// messages.sender point at it. Current macOS has the table but points direct.
func buildIndexLayout(t *testing.T, senderTable, viaTable bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Envelope Index")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE subjects (ROWID INTEGER PRIMARY KEY, subject TEXT)`,
		`CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT)`,
		`CREATE TABLE mailboxes (ROWID INTEGER PRIMARY KEY, url TEXT)`,
		`CREATE TABLE messages (ROWID INTEGER PRIMARY KEY, subject INTEGER, sender INTEGER, mailbox INTEGER, date_received INTEGER, read INTEGER, deleted INTEGER, global_message_id INTEGER)`,
		`INSERT INTO subjects VALUES (1,'Échéance d''octobre'),(2,'Lunch'),(3,'Gone')`,
		`INSERT INTO addresses VALUES (1,'info@banque.example','Banque Générale'),(2,'ivan@example.com','Ivan')`,
		`INSERT INTO mailboxes VALUES (1,'imap://ACC-1/INBOX'),(2,'imap://ACC-1/%5BGmail%5D/All%20Mail'),(3,'imap://ACC-1/Trash'),(4,'imap://ACC-2/INBOX'),(5,'local:///Old%20letters'),(6,'imap://GONE/INBOX')`,
	}
	if senderTable {
		stmts = append(stmts,
			`CREATE TABLE sender_addresses (sender INTEGER PRIMARY KEY, address INTEGER)`,
			`INSERT INTO sender_addresses VALUES (10,1),(20,2)`)
	}
	bank, ivan := 1, 2
	if viaTable {
		bank, ivan = 10, 20
	}
	base := int64(1793000000)
	rows := []struct {
		id, subj, sender, box int
		date                  int64
		read, del, global     int
	}{
		{1, 1, bank, 1, base, 0, 0, 100},       // bank mail in INBOX
		{2, 1, bank, 2, base, 0, 0, 100},       // same message in All Mail
		{3, 2, ivan, 4, base - 100, 1, 0, 101}, // second account
		{4, 3, ivan, 1, base - 200, 1, 1, 102}, // deleted: never shown
		{5, 2, ivan, 3, base - 300, 1, 0, 103}, // in Trash
		{6, 2, ivan, 5, base - 400, 1, 0, 104}, // On My Mac
		{7, 2, ivan, 6, base - 500, 1, 0, 105}, // removed account
	}
	for _, r := range rows {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO messages VALUES (%d,%d,%d,%d,%d,%d,%d,%d)`, r.id, r.subj, r.sender, r.box, r.date, r.read, r.del, r.global))
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return path
}

func testAccounts(context.Context) (map[string]string, error) {
	return map[string]string{"ACC-1": "Google", "ACC-2": "Work"}, nil
}

func TestIndexSearchBothLayouts(t *testing.T) {
	for _, senderTable := range []bool{false, true} {
		t.Run(fmt.Sprintf("senderTable=%v", senderTable), func(t *testing.T) {
			e := NewIndexEngine(buildIndex(t, senderTable), testAccounts)
			ctx := context.Background()

			res, err := e.Search(ctx, SearchQuery{Limit: 20})
			if err != nil {
				t.Fatal(err)
			}
			if res.Engine != "index" {
				t.Fatalf("engine = %q", res.Engine)
			}
			// bank (deduped to INBOX), Work, On My Mac. Not deleted, Trash or removed account.
			if len(res.Messages) != 3 {
				t.Fatalf("want 3 messages, got %+v", res.Messages)
			}
			first := res.Messages[0]
			if first.Mailbox != "INBOX" || first.Account != "Google" || first.Sender != "Banque Générale <info@banque.example>" {
				t.Fatalf("unexpected first row: %+v", first)
			}
			if first.ID != encodeMessageRef("Google", "INBOX", "1") {
				t.Fatalf("id: %s", first.ID)
			}
			if first.Date.Unix() != 1793000000 {
				t.Fatalf("date: %v", first.Date)
			}
			local := res.Messages[2]
			if local.Account != LocalAccount || local.Mailbox != "Old letters" {
				t.Fatalf("local row: %+v", local)
			}
		})
	}
}

func TestIndexSendersOnCurrentLayout(t *testing.T) {
	e := NewIndexEngine(buildIndexLayout(t, true, false), testAccounts)
	res, err := e.Search(context.Background(), SearchQuery{Limit: 20, SenderContains: "generale"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].Sender != "Banque Générale <info@banque.example>" {
		t.Fatalf("sender not resolved: %+v", res.Messages)
	}
}

func TestIndexSearchFilters(t *testing.T) {
	e := NewIndexEngine(buildIndex(t, true), testAccounts)
	ctx := context.Background()
	from := time.Unix(1793000000-50, 0)
	cases := []struct {
		name string
		q    SearchQuery
		want int
	}{
		{"accent-free subject finds accented", SearchQuery{SubjectContains: "ECHEANCE"}, 1},
		{"accented subject finds accented", SearchQuery{SubjectContains: "échéance"}, 1},
		{"sender by name without accent", SearchQuery{SenderContains: "generale"}, 1},
		{"sender by address", SearchQuery{SenderContains: "banque.example"}, 1},
		{"unread only", SearchQuery{UnreadOnly: true}, 1},
		{"account", SearchQuery{Account: "work"}, 1},
		{"date from", SearchQuery{DateFrom: &from}, 1},
		{"trash when named", SearchQuery{Mailbox: "Trash"}, 1},
		{"percent is literal", SearchQuery{SubjectContains: "%"}, 0},
		{"no match", SearchQuery{SubjectContains: "zzz"}, 0},
	}
	for _, c := range cases {
		c.q.Limit = 20
		res, err := e.Search(ctx, c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(res.Messages) != c.want {
			t.Fatalf("%s: want %d, got %+v", c.name, c.want, res.Messages)
		}
	}
	if _, err := e.Search(ctx, SearchQuery{Account: "Nope"}); !errors.Is(err, ErrBadSearch) {
		t.Fatalf("unknown account should be ErrBadSearch, got %v", err)
	}
}

func TestIndexSearchLimitReportsMore(t *testing.T) {
	e := NewIndexEngine(buildIndex(t, true), testAccounts)
	res, err := e.Search(context.Background(), SearchQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || !res.More {
		t.Fatalf("expected 1 message and More: %+v", res)
	}
}

func TestIndexIsNeverWritten(t *testing.T) {
	path := buildIndex(t, true)
	e := NewIndexEngine(path, testAccounts)
	db, err := e.open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM messages`); err == nil {
		t.Fatal("write through the index connection must fail")
	}
}

type failingIndex struct{ err error }

func (f failingIndex) Search(context.Context, SearchQuery) (SearchResult, error) {
	return SearchResult{}, f.err
}

func TestClientFallsBackToMailWhenIndexFails(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	row := "META\x1f60\x1f60\x1ffalse\x1e1\x1fHello\x1fa@example.com\x1f2026-11-3-0\x1ffalse\x1fINBOX"
	c := NewClient(&fakeRunner{runFunc: func(ctx context.Context, s string) (string, error) { return row, nil }}, logger)
	c.UseIndex(failingIndex{errors.New("no permission")}, false)
	res, err := c.SearchMessages(context.Background(), SearchQuery{Account: "Google"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Engine != "mail" || !res.Fallback || len(res.Messages) != 1 {
		t.Fatalf("unexpected: %+v", res)
	}

	c.UseIndex(failingIndex{fmt.Errorf("%w: account", ErrBadSearch)}, false)
	if _, err := c.SearchMessages(context.Background(), SearchQuery{Account: "Nope"}); !errors.Is(err, ErrBadSearch) {
		t.Fatalf("a bad search must not fall back: %v", err)
	}

	c.UseIndex(failingIndex{errors.New("no permission")}, true)
	if _, err := c.SearchMessages(context.Background(), SearchQuery{Account: "Google"}); err == nil {
		t.Fatal("index-only mode must return the error")
	}
}

func TestKnownRecipientCountsOnlySentMail(t *testing.T) {
	path := buildIndex(t, false)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE recipients (message INTEGER, address INTEGER, type INTEGER)`,
		`INSERT INTO addresses VALUES (3,'friend@example.com','Friend'),(4,'thief@example.net','Thief')`,
		`INSERT INTO mailboxes VALUES (7,'imap://ACC-1/Odoslan%C3%A9')`,
		`INSERT INTO messages VALUES (20,2,1,7,1793000000,1,0,200)`, // she sent this
		`INSERT INTO recipients VALUES (20,3,0)`,                    // to friend
		`INSERT INTO recipients VALUES (1,4,0)`,                     // thief only appears on incoming mail
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	db.Close()

	e := NewIndexEngine(path, testAccounts)
	ctx := context.Background()
	for addr, want := range map[string]bool{
		"friend@example.com": true,
		"FRIEND@example.com": true,
		"thief@example.net":  false, // in the address table, never written to
		"info@banque.example": false, // a sender she never wrote to
		"nobody@example.com": false,
	} {
		got, err := e.KnownRecipient(ctx, addr)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: got %v want %v", addr, got, want)
		}
	}
}

func TestSentMessagesSpreadsOverTime(t *testing.T) {
	path := buildIndex(t, false)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE recipients (ROWID INTEGER PRIMARY KEY, message INTEGER, address INTEGER, type INTEGER, position INTEGER)`,
		`INSERT INTO mailboxes VALUES (7,'imap://ACC-1/Sent%20Messages')`,
	}
	for i := 0; i < 30; i++ {
		stmts = append(stmts,
			fmt.Sprintf(`INSERT INTO messages VALUES (%d,2,1,7,%d,1,0,%d)`, 100+i, 1700000000+i*86400, 300+i),
			fmt.Sprintf(`INSERT INTO recipients (message,address,type,position) VALUES (%d,2,0,0)`, 100+i))
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	db.Close()
	e := NewIndexEngine(path, testAccounts)
	a, err := e.SentMessages(context.Background(), 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.SentMessages(context.Background(), 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 5 || len(b) != 5 || a[0].ID == b[0].ID {
		t.Fatalf("pages: %+v %+v", a, b)
	}
	if a[0].Date.Sub(a[len(a)-1].Date) < 20*24*time.Hour {
		t.Fatalf("sample must spread over time: %v to %v", a[0].Date, a[len(a)-1].Date)
	}
	if a[0].To != "ivan@example.com" {
		t.Fatalf("recipient: %+v", a[0])
	}
}
