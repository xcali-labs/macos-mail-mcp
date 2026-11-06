package mail

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestQuoteAS(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "hello", want: `"hello"`},
		{name: "quote and slash", in: "a\"b\\c", want: `"a\"b\\c"`},
		{name: "newline and tab", in: "a\n\tb", want: `"a\n\tb"`},
		{name: "control chars removed", in: "a\x01b\x7fc", want: `"abc"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quoteAS(tt.in); got != tt.want {
				t.Fatalf("quoteAS() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQuoteASMultiline(t *testing.T) {
	got := quoteASMultiline("line1\nline2\r\nline3")
	want := `"line1" & linefeed & "line2" & linefeed & "line3"`
	if got != want {
		t.Fatalf("quoteASMultiline() = %q, want %q", got, want)
	}
}

func TestBuildSearchMessagesScriptIncludesFilters(t *testing.T) {
	from := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	script := buildSearchMessagesScript(SearchQuery{
		Account:         "Personal",
		Mailbox:         "INBOX",
		SenderContains:  "alice",
		SubjectContains: "invoice",
		UnreadOnly:      true,
		DateFrom:        &from,
		DateTo:          &to,
		Limit:           10,
	})

	checks := []string{
		`set accountName to "Personal"`,
		`set mailboxName to "INBOX"`,
		`set maxResults to 10`,
		`set senderNeedle to "alice"`,
		`set subjectNeedle to "invoice"`,
		`set unreadOnly to true`,
		`set scanLimit to 5000`,
		`set dateFrom to my mkDate(`,
		`set dateTo to my mkDate(`,
		`if subjectNeedle is not "" then set subjList to subject of messages pageStart thru pageEnd of targetMailbox`,
		`set timeBudget to 45`,
		`set outText to "META" & fieldSep`,
		`set pageSize to 250`,
		`my stamp(msgDate)`,
		`set targetMailbox to missing value`,
		`repeat with mb in every mailbox of acc`,
		`(mbName contains mailboxName) or (mailboxName contains mbName)`,
		`& fieldSep & mailboxName`,
		`set collectedCount to collectedCount + 1`,
		`return outText`,
	}
	for _, c := range checks {
		if !strings.Contains(script, c) {
			t.Fatalf("script missing expected fragment %q", c)
		}
	}
}

func TestBuildListAccountsScriptUsesDelimitedJoin(t *testing.T) {
	script := buildListAccountsScript()
	if !strings.Contains(script, `repeat with i from 1 to (count of namesList)`) {
		t.Fatalf("expected record separator join, got: %s", script)
	}
}

func TestBuildDraftScriptSavesAndUsesLinefeedConcatenation(t *testing.T) {
	script := buildDraftScript(ComposeInput{
		To:      []string{"alice@example.com"},
		Subject: "subject",
		Body:    "line1\nline2",
	})

	if !strings.Contains(script, `"line1" & linefeed & "line2"`) {
		t.Fatalf("script body does not use linefeed concatenation: %s", script)
	}
	if !strings.Contains(script, "save newMessage") {
		t.Fatalf("draft script should save the message")
	}
}

func TestBuildReplyDraftScriptQuotesSavesAndReportsRecipients(t *testing.T) {
	script := buildReplyDraftScript("42", "iCloud", "INBOX", "Thanks \"Ivan\"\nsee you")
	for _, want := range []string{
		`set messageIDText to "42"`,
		`set replyText to "Thanks \"Ivan\"" & linefeed & "see you"`,
		`reply msg opening window false reply to all false`,
		`"> " & (p as text)`,
		`save replyMsg`,
		`set toList to address of every to recipient of replyMsg`,
		`error "message_not_found:" & messageIDText`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("reply script missing %q:\n%s", want, script)
		}
	}
}

func TestDraftScriptHasNoBcc(t *testing.T) {
	script := buildDraftScript(ComposeInput{To: []string{"a@example.com"}, CC: []string{"b@example.com"}, Subject: "s", Body: "b"})
	if strings.Contains(script, "bcc") {
		t.Fatalf("drafts must not carry a Bcc:\n%s", script)
	}
}

func TestBuildMoveMessageScriptMatchesTargetExactly(t *testing.T) {
	script := buildMoveMessageScript("7", "Work", "INBOX", `Invoices "2026"`)
	if !strings.Contains(script, `set targetName to "Invoices \"2026\""`) {
		t.Fatalf("target not quoted: %s", script)
	}
	if !strings.Contains(script, `if (name of mb as text) is targetName then`) {
		t.Fatalf("expected exact target match: %s", script)
	}
	if strings.Contains(script, "contains targetName") {
		t.Fatalf("move target must not use fuzzy matching")
	}
	if !strings.Contains(script, "move msg to targetMailbox") {
		t.Fatalf("expected move command")
	}
}

// TestNoScriptCanSend is the fork's core guarantee: no generated script,
// and no non-test source file in this repo, contains an AppleScript send.
func TestNoScriptCanSend(t *testing.T) {
	scripts := []string{
		buildDraftScript(ComposeInput{To: []string{"a@example.com"}, Subject: "s", Body: "b"}),
		buildReplyDraftScript("1", "A", "INBOX", "b"),
		buildReplyTargetScript("1", "A", "INBOX"),
		buildOwnAddressesScript(),
		buildMoveMessageScript("1", "A", "INBOX", "Archive"),
	}
	sendCmd := regexp.MustCompile(`(?m)(^|\s)send(\s|$)`)
	for _, s := range scripts {
		if sendCmd.MatchString(s) {
			t.Fatalf("generated script contains a send command:\n%s", s)
		}
	}

	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "vendor") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, banned := range []string{"send newMessage", "send replyMsg", "send msg", "send_email", "SendEmail", "smtp"} {
			if strings.Contains(text, banned) {
				t.Errorf("%s contains banned send path %q", path, banned)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBuildGetMessageScriptReadsBodyInsideTell(t *testing.T) {
	script := buildGetMessageScript("123", "Personal", "INBOX", true)
	if !strings.Contains(script, `set includeBody to true`) {
		t.Fatalf("expected includeBody toggle in script")
	}
	if !strings.Contains(script, `set accountName to "Personal"`) {
		t.Fatalf("expected account hint in script")
	}
	if !strings.Contains(script, `set mailboxName to "INBOX"`) {
		t.Fatalf("expected mailbox hint in script")
	}
	if !strings.Contains(script, `set msgBody to (content of msg as text)`) {
		t.Fatalf("expected body extraction inside Mail tell block")
	}
	if !strings.Contains(script, `set cand to «class mssg» id messageIDNum of mb`) {
		t.Fatalf("expected direct by-id lookup")
	}
	if strings.Contains(script, `whose id is`) {
		t.Fatalf("a whose query scans the whole mailbox and takes minutes")
	}
	if strings.Contains(script, `& (content of msg as text) &`) {
		t.Fatalf("body must not be read inline outside tell block")
	}
	if !strings.Contains(script, `& fieldSep & msgBody & fieldSep &`) {
		t.Fatalf("expected final row to include msgBody variable")
	}
}

func TestSearchScriptNeverCoercesEpochSecondsToInteger(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, script := range []string{
		buildSearchMessagesScript(SearchQuery{Account: "A", UnreadOnly: true, DateFrom: &from}),
		buildGetMessageScript("1", "A", "INBOX", true),
	} {
		if strings.Contains(script, "epochDate") {
			t.Fatalf("script still does epoch maths that overflows AppleScript integers:\n%s", script)
		}
		if strings.Contains(script, "read status of it is false") {
			t.Fatalf("unread filter must not use a whose clause (hangs on large mailboxes)")
		}
	}
}

func TestParseMailDate(t *testing.T) {
	got := parseMailDate("2026-11-3-3600")
	want := time.Date(2026, 11, 3, 1, 0, 0, 0, time.Local).UTC()
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if !parseMailDate("").IsZero() || !parseMailDate("2026-13-1-0").IsZero() {
		t.Fatal("expected zero time for empty or invalid stamp")
	}
}

func TestSearchScriptUsesNoWhoseClause(t *testing.T) {
	script := buildSearchMessagesScript(SearchQuery{Account: "A", SenderContains: "x", SubjectContains: "y"})
	if strings.Contains(script, " whose ") {
		t.Fatalf("search must not use a whose clause (Mail builds the full match list and hangs):\n%s", script)
	}
}

func TestGetMessageScriptDefinesEveryRowField(t *testing.T) {
	script := buildGetMessageScript("1", "A", "INBOX", true)
	for _, v := range []string{"set recipList to", "set ccList to", "set msgID to", "set msgSubject to", "set msgSender to", "set msgUnixDate to", "set msgMailbox to", "set msgRead to", "set msgFlagged to", "set msgBody to"} {
		if !strings.Contains(script, v) {
			t.Fatalf("get_message script never sets %q", v)
		}
	}
}

func TestDraftScriptSetsSenderNotAccount(t *testing.T) {
	script := buildDraftScript(ComposeInput{To: []string{"a@example.com"}, Subject: "s", Body: "b", Account: "Google"})
	if strings.Contains(script, "set account of newMessage") {
		t.Fatal("outgoing messages have no account property")
	}
	if !strings.Contains(script, `set senderAddress to item 1 of (get email addresses of account "Google")`) || !strings.Contains(script, `content:contentText, sender:senderAddress}`) {
		t.Fatalf("expected sender from account address:\n%s", script)
	}
}
