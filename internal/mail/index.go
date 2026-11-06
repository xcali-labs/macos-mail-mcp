package mail

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
)

// LocalAccount is the account name used for "On My Mac" mailboxes, which
// belong to no mail account.
const LocalAccount = "local"

const (
	indexEnvVar        = "APPLE_MAIL_MCP_INDEX"
	coreDataEpochShift = 978307200 // seconds from 1970-01-01 to 2001-01-01
)

// ErrBadSearch marks a search the caller got wrong (an unknown account). It is
// reported to the model, never answered by falling back to the slow engine.
var ErrBadSearch = errors.New("bad search")

var registerFoldOnce sync.Once

func registerFold() {
	registerFoldOnce.Do(func() {
		sqlite.MustRegisterDeterministicScalarFunction("fold", 1, func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			s, _ := args[0].(string)
			return foldText(s), nil
		})
	})
}

// foldReplacer maps the accented letters of Slovak, French, Portuguese and
// German to plain ones, so "faktura" finds "Faktúra".
var foldReplacer = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ä", "a", "ã", "a", "å", "a",
	"č", "c", "ç", "c", "ď", "d",
	"é", "e", "è", "e", "ê", "e", "ë", "e", "ě", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i",
	"ĺ", "l", "ľ", "l", "ň", "n", "ñ", "n",
	"ó", "o", "ò", "o", "ô", "o", "ö", "o", "õ", "o",
	"ŕ", "r", "ř", "r", "š", "s", "ß", "ss", "ť", "t",
	"ú", "u", "ù", "u", "û", "u", "ü", "u", "ů", "u",
	"ý", "y", "ÿ", "y", "ž", "z", "œ", "oe", "æ", "ae",
)

func foldText(s string) string {
	return foldReplacer.Replace(strings.ToLower(s))
}

// IndexEngine searches Mail's own Envelope Index database, read-only. It sees
// every mailbox at once in milliseconds, but needs Full Disk Access.
type IndexEngine struct {
	path string
	// accounts returns mailbox-URL host (the account id) -> account name.
	accounts func(context.Context) (map[string]string, error)
}

// FindIndexPath returns the newest Envelope Index under home, or an error.
func FindIndexPath(home string) (string, error) {
	if p := strings.TrimSpace(os.Getenv(indexEnvVar)); p != "" {
		return p, nil
	}
	matches, err := filepath.Glob(filepath.Join(home, "Library", "Mail", "V*", "MailData", "Envelope Index"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", errors.New("no Envelope Index found (or no permission to read ~/Library/Mail)")
	}
	version := regexp.MustCompile(`/V(\d+)/MailData/`)
	sort.Slice(matches, func(i, j int) bool {
		vi, vj := 0, 0
		if m := version.FindStringSubmatch(matches[i]); m != nil {
			vi, _ = strconv.Atoi(m[1])
		}
		if m := version.FindStringSubmatch(matches[j]); m != nil {
			vj, _ = strconv.Atoi(m[1])
		}
		return vi > vj
	})
	return matches[0], nil
}

// NewIndexEngine builds an engine for the database at path.
func NewIndexEngine(path string, accounts func(context.Context) (map[string]string, error)) *IndexEngine {
	registerFold()
	return &IndexEngine{path: path, accounts: accounts}
}

func (e *IndexEngine) open() (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: e.path}
	dsn := u.String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(3000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

type indexSchema struct {
	hasRead, hasDeleted, hasGlobalID, hasComment, hasSenderTable, hasPrefix bool
}

func columnsOf(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		cols[n] = true
	}
	return cols, rows.Err()
}

func readSchema(ctx context.Context, db *sql.DB) (indexSchema, error) {
	msgs, err := columnsOf(ctx, db, "messages")
	if err != nil {
		return indexSchema{}, err
	}
	for _, need := range []string{"subject", "sender", "mailbox", "date_received"} {
		if !msgs[need] {
			return indexSchema{}, fmt.Errorf("messages table has no %q column: unknown index layout", need)
		}
	}
	addrs, err := columnsOf(ctx, db, "addresses")
	if err != nil {
		return indexSchema{}, err
	}
	if !addrs["address"] {
		return indexSchema{}, errors.New("addresses table has no address column: unknown index layout")
	}
	senders, err := columnsOf(ctx, db, "sender_addresses")
	if err != nil {
		return indexSchema{}, err
	}
	return indexSchema{
		hasRead:        msgs["read"],
		hasDeleted:     msgs["deleted"],
		hasGlobalID:    msgs["global_message_id"],
		hasComment:     addrs["comment"],
		hasSenderTable: senders["sender"] && senders["address"],
		hasPrefix:      msgs["subject_prefix"],
	}, nil
}

type indexMailbox struct {
	id      int64
	account string
	name    string // leaf name, as Mail's scripting shows it
	path    string
}

// parseMailboxURL splits imap://ACCOUNT-ID/%5BGmail%5D/Sent%20Mail into the
// account id and the decoded mailbox path.
func parseMailboxURL(raw string) (scheme, host, path string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return "", "", "", false
	}
	return u.Scheme, u.Host, strings.Trim(u.Path, "/"), true
}

func loadMailboxes(ctx context.Context, db *sql.DB, accounts map[string]string) ([]indexMailbox, error) {
	rows, err := db.QueryContext(ctx, "SELECT ROWID, url FROM mailboxes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []indexMailbox
	for rows.Next() {
		var id int64
		var raw sql.NullString
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		scheme, host, path, ok := parseMailboxURL(raw.String)
		if !ok || path == "" {
			continue
		}
		name := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			name = path[i+1:]
		}
		account := LocalAccount
		if scheme != "local" {
			known, found := accounts[host]
			if !found {
				continue // a removed account: its mail is not in Mail any more
			}
			account = known
		}
		out = append(out, indexMailbox{id: id, account: account, name: name, path: path})
	}
	return out, rows.Err()
}

func selectMailboxes(all []indexMailbox, q SearchQuery) ([]indexMailbox, error) {
	wantAccount := foldText(strings.TrimSpace(q.Account))
	wantMailbox := foldText(strings.TrimSpace(q.Mailbox))
	accountSeen := false
	var picked []indexMailbox
	for _, mb := range all {
		if wantAccount != "" && foldText(mb.account) != wantAccount {
			continue
		}
		accountSeen = true
		if wantMailbox == "" {
			if isProtectedMailbox(mb.name) {
				continue // trash and junk only when asked for by name
			}
			picked = append(picked, mb)
			continue
		}
		if foldText(mb.name) == wantMailbox || strings.Contains(foldText(mb.path), wantMailbox) {
			picked = append(picked, mb)
		}
	}
	if wantAccount != "" && !accountSeen {
		return nil, fmt.Errorf("%w: account %q not found; call list_accounts for the names", ErrBadSearch, q.Account)
	}
	return picked, nil
}

func likeArg(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(foldText(s)) + "%"
}

// Search runs the query. Without an account or mailbox it covers everything.
func (e *IndexEngine) Search(ctx context.Context, q SearchQuery) (SearchResult, error) {
	accounts, err := e.accounts(ctx)
	if err != nil {
		return SearchResult{}, fmt.Errorf("read account names: %w", err)
	}
	db, err := e.open()
	if err != nil {
		return SearchResult{}, err
	}
	defer db.Close()

	schema, err := readSchema(ctx, db)
	if err != nil {
		return SearchResult{}, err
	}
	mailboxes, err := loadMailboxes(ctx, db, accounts)
	if err != nil {
		return SearchResult{}, err
	}
	picked, err := selectMailboxes(mailboxes, q)
	if err != nil {
		return SearchResult{}, err
	}
	if len(picked) == 0 {
		return SearchResult{Engine: "index"}, nil
	}
	byID := make(map[int64]indexMailbox, len(picked))
	ids := make([]string, 0, len(picked))
	for _, mb := range picked {
		byID[mb.id] = mb
		ids = append(ids, strconv.FormatInt(mb.id, 10))
	}

	var shift int64
	var newest sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT MAX(date_received) FROM messages").Scan(&newest); err != nil {
		return SearchResult{}, err
	}
	if newest.Valid && newest.Int64 < 1_000_000_000 {
		shift = coreDataEpochShift
	}

	// messages.sender points straight at addresses on current macOS. Older
	// layouts went through sender_addresses, so that stays as a second try.
	senderJoin := "LEFT JOIN addresses a1 ON m.sender = a1.ROWID"
	senderAddr := "COALESCE(a1.address, '')"
	senderName := "''"
	if schema.hasComment {
		senderName = "COALESCE(a1.comment, '')"
	}
	if schema.hasSenderTable {
		senderJoin += " LEFT JOIN sender_addresses sa ON a1.ROWID IS NULL AND m.sender = sa.sender LEFT JOIN addresses a2 ON sa.address = a2.ROWID"
		senderAddr = "COALESCE(a1.address, a2.address, '')"
		if schema.hasComment {
			senderName = "COALESCE(a1.comment, a2.comment, '')"
		}
	}
	subjectCol := "COALESCE(s.subject, '')"
	if schema.hasPrefix {
		subjectCol = "COALESCE(m.subject_prefix, '') || COALESCE(s.subject, '')"
	}
	readCol, globalCol := "1", "0"
	if schema.hasRead {
		readCol = "COALESCE(m.read, 0)"
	}
	if schema.hasGlobalID {
		globalCol = "COALESCE(m.global_message_id, 0)"
	}

	where := []string{"m.mailbox IN (" + strings.Join(ids, ",") + ")"}
	args := []any{}
	if schema.hasDeleted {
		where = append(where, "COALESCE(m.deleted, 0) = 0")
	}
	if q.UnreadOnly {
		if !schema.hasRead {
			return SearchResult{}, errors.New("index has no read column")
		}
		where = append(where, "COALESCE(m.read, 0) = 0")
	}
	if q.SubjectContains != "" {
		where = append(where, `fold(COALESCE(s.subject, '')) LIKE ? ESCAPE '\'`)
		args = append(args, likeArg(q.SubjectContains))
	}
	if q.SenderContains != "" {
		cond := "fold(" + senderAddr + `) LIKE ? ESCAPE '\'`
		args = append(args, likeArg(q.SenderContains))
		if schema.hasComment {
			cond = "(" + cond + " OR fold(" + senderName + `) LIKE ? ESCAPE '\')`
			args = append(args, likeArg(q.SenderContains))
		}
		where = append(where, cond)
	}
	if q.DateFrom != nil {
		where = append(where, "m.date_received >= ?")
		args = append(args, q.DateFrom.Unix()-shift)
	}
	if q.DateTo != nil {
		where = append(where, "m.date_received <= ?")
		args = append(args, q.DateTo.Unix()-shift)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	query := fmt.Sprintf(`SELECT m.ROWID, %s, %s, %s, m.mailbox, COALESCE(m.date_received, 0), %s, %s
FROM messages m
LEFT JOIN subjects s ON m.subject = s.ROWID
%s
WHERE %s
ORDER BY m.date_received DESC
LIMIT ?`, subjectCol, senderAddr, senderName, readCol, globalCol, senderJoin, strings.Join(where, " AND "))
	// Gmail lists one message in several mailboxes, so fetch extra and dedupe.
	args = append(args, limit*4+1)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return SearchResult{}, err
	}
	defer rows.Close()

	var msgs []MessageSummary
	seen := map[int64]int{}
	more := false
	for rows.Next() {
		var id, mailboxID, date, global int64
		var read int
		var subject, address, name string
		if err := rows.Scan(&id, &subject, &address, &name, &mailboxID, &date, &read, &global); err != nil {
			return SearchResult{}, err
		}
		mb := byID[mailboxID]
		sender := address
		if name != "" {
			sender = name + " <" + address + ">"
		}
		msg := MessageSummary{
			ID:      strconv.FormatInt(id, 10),
			Subject: subject,
			Sender:  sender,
			Date:    time.Unix(date+shift, 0).UTC(),
			Read:    read != 0,
			Mailbox: mb.name,
			Account: mb.account,
		}
		if global != 0 {
			if at, dup := seen[global]; dup {
				if isAllMail(msgs[at].Mailbox) && !isAllMail(mb.name) {
					msgs[at] = msg // prefer the real folder over Gmail's All Mail
				}
				continue
			}
			seen[global] = len(msgs)
		}
		if len(msgs) >= limit {
			more = true
			break
		}
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return SearchResult{}, err
	}
	for i := range msgs {
		msgs[i].ID = encodeMessageRef(msgs[i].Account, msgs[i].Mailbox, msgs[i].ID)
	}
	return SearchResult{Messages: msgs, Engine: "index", More: more}, nil
}

// sentMailboxNames are lower-case, accent-free names of sent-mail folders.
var sentMailboxNames = map[string]bool{
	"sent": true, "sent mail": true, "sent messages": true, "sent items": true,
	"envoyes": true, "messages envoyes": true, "elements envoyes": true,
	"odoslane": true, "odoslana posta": true, "odoslane spravy": true, "odoslane polozky": true,
	"gesendet": true, "gesendete objekte": true, "gesendete nachrichten": true,
	"enviados": true, "enviadas": true, "itens enviados": true, "mensagens enviadas": true,
}

// KnownRecipient reports whether address received mail from a sent-mail folder.
// Only mail the user sent counts: an incoming email cannot put anyone on this list.
func (e *IndexEngine) KnownRecipient(ctx context.Context, address string) (bool, error) {
	db, err := e.open()
	if err != nil {
		return false, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT ROWID, url FROM mailboxes")
	if err != nil {
		return false, err
	}
	var ids []string
	for rows.Next() {
		var id int64
		var raw sql.NullString
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return false, err
		}
		_, _, path, ok := parseMailboxURL(raw.String)
		if !ok {
			continue
		}
		name := path
		if j := strings.LastIndex(path, "/"); j >= 0 {
			name = path[j+1:]
		}
		if sentMailboxNames[foldText(name)] {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return false, nil
	}
	var found int
	err = db.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM recipients r
JOIN addresses a ON r.address = a.ROWID
JOIN messages m ON r.message = m.ROWID
WHERE a.address = ? COLLATE NOCASE AND m.mailbox IN (`+strings.Join(ids, ",")+`))`, address).Scan(&found)
	return found == 1, err
}

// SentMessages lists messages from sent folders, spread evenly from newest to
// oldest so a sample covers years, not one busy week. Page 0, 1, 2 give
// different messages.
func (e *IndexEngine) SentMessages(ctx context.Context, size, page int) ([]SentRef, error) {
	accounts, err := e.accounts(ctx)
	if err != nil {
		return nil, err
	}
	db, err := e.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	mailboxes, err := loadMailboxes(ctx, db, accounts)
	if err != nil {
		return nil, err
	}
	byID := map[int64]indexMailbox{}
	var ids []string
	for _, mb := range mailboxes {
		if sentMailboxNames[foldText(mb.name)] {
			byID[mb.id] = mb
			ids = append(ids, strconv.FormatInt(mb.id, 10))
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("no sent folder found")
	}
	in := "m.mailbox IN (" + strings.Join(ids, ",") + ")"
	var total int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages m WHERE "+in+" AND COALESCE(m.deleted, 0) = 0").Scan(&total); err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, nil
	}
	stride := total / size
	if stride < 1 {
		stride = 1
	}
	var shift int64
	var newest sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT MAX(date_received) FROM messages").Scan(&newest); err != nil {
		return nil, err
	}
	if newest.Valid && newest.Int64 < 1_000_000_000 {
		shift = coreDataEpochShift
	}
	rows, err := db.QueryContext(ctx, `SELECT id, box, date, COALESCE((SELECT a.address FROM recipients r JOIN addresses a ON r.address = a.ROWID WHERE r.message = id ORDER BY r.position LIMIT 1), '')
FROM (SELECT m.ROWID AS id, m.mailbox AS box, COALESCE(m.date_received, 0) AS date, ROW_NUMBER() OVER (ORDER BY m.date_received DESC) AS n
      FROM messages m WHERE `+in+` AND COALESCE(m.deleted, 0) = 0)
WHERE (n + ?) % ? = 0 LIMIT ?`, page, stride, size)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SentRef
	for rows.Next() {
		var id, box, date int64
		var to string
		if err := rows.Scan(&id, &box, &date, &to); err != nil {
			return nil, err
		}
		mb := byID[box]
		out = append(out, SentRef{ID: encodeMessageRef(mb.account, mb.name, strconv.FormatInt(id, 10)), To: to, Date: time.Unix(date+shift, 0).UTC()})
	}
	return out, rows.Err()
}

func isAllMail(name string) bool {
	n := foldText(name)
	return n == "all mail" || n == "tous les messages" || n == "vsetky spravy"
}

// AccountHosts lists the account ids in the mailbox URLs, each mapped to itself.
// index-check uses it to time a search without needing Mail's account names.
func (e *IndexEngine) AccountHosts(ctx context.Context) (map[string]string, error) {
	db, err := e.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT url FROM mailboxes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if _, host, _, ok := parseMailboxURL(raw.String); ok && host != "" {
			out[host] = host
		}
	}
	return out, rows.Err()
}

// IndexReport is what index-check prints: structure and timings, never content.
type IndexReport struct {
	Path           string
	Messages       int64
	Mailboxes      int
	MessageColumns []string
	AddressColumns []string
	SenderTable    bool
	OpenAndCount   time.Duration
	RecipientCols  []string
	SentFolders    int
}

// Check opens the index and reports its shape. It reads no message content.
func (e *IndexEngine) Check(ctx context.Context) (IndexReport, error) {
	start := time.Now()
	db, err := e.open()
	if err != nil {
		return IndexReport{}, err
	}
	defer db.Close()
	r := IndexReport{Path: e.path}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages").Scan(&r.Messages); err != nil {
		return r, err
	}
	r.OpenAndCount = time.Since(start)
	var boxes int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailboxes").Scan(&boxes); err != nil {
		return r, err
	}
	r.Mailboxes = boxes
	msgCols, err := columnsOf(ctx, db, "messages")
	if err != nil {
		return r, err
	}
	addrCols, err := columnsOf(ctx, db, "addresses")
	if err != nil {
		return r, err
	}
	senderCols, err := columnsOf(ctx, db, "sender_addresses")
	if err != nil {
		return r, err
	}
	for c := range msgCols {
		r.MessageColumns = append(r.MessageColumns, c)
	}
	for c := range addrCols {
		r.AddressColumns = append(r.AddressColumns, c)
	}
	sort.Strings(r.MessageColumns)
	sort.Strings(r.AddressColumns)
	r.SenderTable = len(senderCols) > 0
	recipCols, err := columnsOf(ctx, db, "recipients")
	if err != nil {
		return r, err
	}
	for c := range recipCols {
		r.RecipientCols = append(r.RecipientCols, c)
	}
	sort.Strings(r.RecipientCols)
	rows, err := db.QueryContext(ctx, "SELECT url FROM mailboxes")
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return r, err
		}
		if _, _, path, ok := parseMailboxURL(raw.String); ok {
			name := path
			if j := strings.LastIndex(path, "/"); j >= 0 {
				name = path[j+1:]
			}
			if sentMailboxNames[foldText(name)] {
				r.SentFolders++
			}
		}
	}
	return r, rows.Err()
}
