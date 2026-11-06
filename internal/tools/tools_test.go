package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/maximbilan/apple-mail-mcp-go/internal/mail"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeMailClient struct {
	accounts      []mail.Account
	mailboxes     []mail.Mailbox
	searchResults []mail.MessageSummary
	message       mail.Message
	unread        mail.UnreadCounts
	draftID       string

	lastSearchQuery mail.SearchQuery
	lastMessageID   string
	lastIncludeBody bool
	lastReply       mail.ReplyInput
	lastMoveTarget  string

	err error
}

func (f *fakeMailClient) ListAccounts(ctx context.Context) ([]mail.Account, error) {
	return f.accounts, f.err
}
func (f *fakeMailClient) ListMailboxes(ctx context.Context, account string) ([]mail.Mailbox, error) {
	return f.mailboxes, f.err
}
func (f *fakeMailClient) SearchMessages(ctx context.Context, q mail.SearchQuery) (mail.SearchResult, error) {
	f.lastSearchQuery = q
	return mail.SearchResult{Messages: f.searchResults, Scanned: len(f.searchResults), MailboxSize: len(f.searchResults)}, f.err
}
func (f *fakeMailClient) GetMessage(ctx context.Context, messageID string, includeBody bool) (mail.Message, error) {
	f.lastMessageID = messageID
	f.lastIncludeBody = includeBody
	return f.message, f.err
}
func (f *fakeMailClient) ReplyDraft(ctx context.Context, input mail.ReplyInput) (mail.ReplyResult, error) {
	f.lastReply = input
	return mail.ReplyResult{Recipients: []string{"ivan@example.com"}, Sender: "ivan@example.com"}, f.err
}
func (f *fakeMailClient) GetProfile() (mail.Profile, error)                 { return mail.Profile{}, f.err }
func (f *fakeMailClient) SaveVoice(ctx context.Context, voice string) error { return f.err }
func (f *fakeMailClient) Remember(ctx context.Context, text string) (mail.Rule, error) {
	return mail.Rule{Number: 1, Text: text}, f.err
}
func (f *fakeMailClient) Forget(number int) error { return f.err }
func (f *fakeMailClient) SampleSentMail(ctx context.Context, page int) ([]mail.SentSample, error) {
	return nil, f.err
}
func (f *fakeMailClient) MoveMessage(ctx context.Context, messageID, targetMailbox string) error {
	f.lastMoveTarget = targetMailbox
	return f.err
}
func (f *fakeMailClient) CreateDraft(ctx context.Context, input mail.ComposeInput) (string, error) {
	return f.draftID, f.err
}
func (f *fakeMailClient) GetUnreadCount(ctx context.Context, account string) (mail.UnreadCounts, error) {
	return f.unread, f.err
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestListAccountsHandler(t *testing.T) {
	fake := &fakeMailClient{accounts: []mail.Account{{Name: "Personal"}, {Name: "Work"}}}
	h := newListAccountsHandler(fake, testLogger())

	_, out, err := h(context.Background(), nil, listAccountsInput{})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if len(out.Accounts) != 2 || out.Accounts[0].Name != "Personal" || out.Accounts[1].Name != "Work" {
		t.Fatalf("unexpected accounts output: %#v", out)
	}
}

func TestSearchMessagesHandlerParsesDatesAndDefaults(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fake := &fakeMailClient{searchResults: []mail.MessageSummary{{ID: "1", Date: now}}}
	h := newSearchMessagesHandler(fake, testLogger())

	from := "2026-01-02T03:04:05Z"
	to := "2026-01-03T03:04:05Z"
	_, out, err := h(context.Background(), nil, searchMessagesInput{Account: "Personal", DateFrom: from, DateTo: to})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if len(out.Messages) != 1 || out.Messages[0].ID != "1" {
		t.Fatalf("unexpected output: %#v", out)
	}
	if fake.lastSearchQuery.Limit != 0 {
		t.Fatalf("expected handler to pass raw limit through, got %d", fake.lastSearchQuery.Limit)
	}
	if fake.lastSearchQuery.DateFrom == nil || fake.lastSearchQuery.DateTo == nil {
		t.Fatalf("expected parsed dates to be set")
	}
	if out.Messages[0].Date == "" {
		t.Fatalf("expected RFC3339 date output, got empty")
	}
}

func TestSearchMessagesHandlerReturnsEmptyDateForZeroTime(t *testing.T) {
	fake := &fakeMailClient{searchResults: []mail.MessageSummary{{ID: "1"}}}
	h := newSearchMessagesHandler(fake, testLogger())

	_, out, err := h(context.Background(), nil, searchMessagesInput{Account: "Personal"})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("unexpected output: %#v", out)
	}
	if out.Messages[0].Date != "" {
		t.Fatalf("expected empty date for zero time, got %q", out.Messages[0].Date)
	}
}

func TestSearchMessagesHandlerRejectsInvalidDate(t *testing.T) {
	fake := &fakeMailClient{}
	h := newSearchMessagesHandler(fake, testLogger())

	_, _, err := h(context.Background(), nil, searchMessagesInput{Account: "Personal", DateFrom: "not-a-date"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGetMessageHandlerDefaultsIncludeBodyTrue(t *testing.T) {
	fake := &fakeMailClient{message: mail.Message{ID: "1"}}
	h := newGetMessageHandler(fake, testLogger())

	_, out, err := h(context.Background(), nil, getMessageInput{MessageID: "1"})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if out.Message.ID != "1" {
		t.Fatalf("unexpected output: %#v", out)
	}
	if !fake.lastIncludeBody {
		t.Fatal("expected includeBody default true")
	}
}

func TestComposeHandlersPassThroughErrors(t *testing.T) {
	fake := &fakeMailClient{err: errors.New("boom")}
	draftHandler := newCreateDraftHandler(fake, testLogger())
	replyHandler := newReplyDraftHandler(fake, testLogger())
	moveHandler := newMoveMessageHandler(fake, testLogger())

	if _, _, err := draftHandler(context.Background(), nil, createDraftInput{To: []string{"alice@example.com"}, Subject: "s", Body: "b"}); err == nil {
		t.Fatal("expected draft error")
	}
	if _, _, err := replyHandler(context.Background(), nil, replyDraftInput{MessageID: "1", Body: "b"}); err == nil {
		t.Fatal("expected reply error")
	}
	if _, _, err := moveHandler(context.Background(), nil, moveMessageInput{MessageID: "1", TargetMailbox: "Archive"}); err == nil {
		t.Fatal("expected move error")
	}
}

func TestReplyDraftHandlerReportsRecipients(t *testing.T) {
	fake := &fakeMailClient{}
	h := newReplyDraftHandler(fake, testLogger())
	_, out, err := h(context.Background(), nil, replyDraftInput{MessageID: "mref:iCloud:INBOX:5", Body: "Ďakujem"})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if len(out.Recipients) != 1 || out.Recipients[0] != "ivan@example.com" || !strings.Contains(out.Status, "unsent") {
		t.Fatalf("unexpected output: %+v", out)
	}
	if fake.lastReply.Body != "Ďakujem" {
		t.Fatalf("input not passed through: %+v", fake.lastReply)
	}
}

// TestRegisteredToolsAreExactlyTheAllowedSet lists the tools a real MCP
// client sees. Any new tool (above all a send tool) fails this test.
func TestRegisteredToolsAreExactlyTheAllowedSet(t *testing.T) {
	reads := []string{"get_message", "get_profile", "get_unread_count", "list_accounts", "list_mailboxes", "sample_sent_mail", "search_messages"}
	writes := []string{"create_draft", "forget", "move_message", "remember", "reply_draft", "save_profile"}
	all := append(append([]string{}, writes...), reads...)
	sort.Strings(all)
	cases := []struct {
		name string
		opts RegisterOptions
		want []string
	}{
		{"full", RegisterOptions{}, all},
		{"read-only", RegisterOptions{ReadOnly: true}, reads},
		{"write-only", RegisterOptions{WriteOnly: true}, writes},
	}
	for _, c := range cases {
		ctx := context.Background()
		server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		c.opts.Logger = testLogger()
		Register(server, &fakeMailClient{}, c.opts)
		st, ct := mcp.NewInMemoryTransports()
		if _, err := server.Connect(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
		session, err := client.Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(res.Tools))
		for _, tool := range res.Tools {
			got = append(got, tool.Name)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Fatalf("%s tools = %v, want %v", c.name, got, c.want)
		}
		session.Close()
	}
}
