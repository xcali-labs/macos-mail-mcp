package tools

import (
	"context"
	"log/slog"
	"time"

	"github.com/maximbilan/apple-mail-mcp-go/internal/mail"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type MailClient interface {
	ListAccounts(ctx context.Context) ([]mail.Account, error)
	ListMailboxes(ctx context.Context, account string) ([]mail.Mailbox, error)
	SearchMessages(ctx context.Context, q mail.SearchQuery) (mail.SearchResult, error)
	GetMessage(ctx context.Context, messageID string, includeBody bool) (mail.Message, error)
	CreateDraft(ctx context.Context, input mail.ComposeInput) (string, error)
	ReplyDraft(ctx context.Context, input mail.ReplyInput) (mail.ReplyResult, error)
	MoveMessage(ctx context.Context, messageID, targetMailbox string) error
	GetUnreadCount(ctx context.Context, account string) (mail.UnreadCounts, error)
	GetProfile() (mail.Profile, error)
	SaveVoice(ctx context.Context, voice string) error
	Remember(ctx context.Context, text string) (mail.Rule, error)
	Forget(number int) error
	SampleSentMail(ctx context.Context, page int) ([]mail.SentSample, error)
}

type RegisterOptions struct {
	ReadOnly  bool
	WriteOnly bool
	Logger    *slog.Logger
}

type ToolDoc struct {
	Name        string
	Params      string
	Description string
}

func Register(server *mcp.Server, client MailClient, opts RegisterOptions) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	// Reads and writes run as separate servers so the client can pre-approve
	// reads and still ask for every write: its approval is per server.
	if !opts.WriteOnly {
		registerAccountsTools(server, client, opts.Logger)
		registerMessageTools(server, client, opts.Logger)
	}
	if !opts.ReadOnly {
		registerComposeTools(server, client, opts.Logger)
	}
}

func ToolDocs(readOnly, writeOnly bool) []ToolDoc {
	var docs []ToolDoc
	if !writeOnly {
		docs = append(docs,
			ToolDoc{Name: "list_accounts", Params: "none", Description: "List account names configured in Mail.app."},
			ToolDoc{Name: "list_mailboxes", Params: "account (string, required)", Description: "List mailboxes for an account with unread counts."},
			ToolDoc{Name: "search_messages", Params: "account, mailbox, sender_contains, subject_contains, unread_only, date_from, date_to, limit, scan_limit", Description: "Find emails across accounts and folders."},
			ToolDoc{Name: "get_message", Params: "message_id (required), include_body", Description: "Get message metadata and optionally full body (cut at 20,000 characters)."},
			ToolDoc{Name: "get_unread_count", Params: "account (optional)", Description: "Get total and per-mailbox unread counts."},
			ToolDoc{Name: "get_profile", Params: "none", Description: "Read the writing profile and taught preferences."},
			ToolDoc{Name: "sample_sent_mail", Params: "page", Description: "Read a sample of the user's own words from sent mail."},
		)
	}
	if !readOnly {
		docs = append(docs,
			ToolDoc{Name: "create_draft", Params: "to, subject, body, cc, account", Description: "Save a new email unsent in Drafts, only to someone already written to."},
			ToolDoc{Name: "reply_draft", Params: "message_id, body", Description: "Save a quoted in-thread reply unsent in Drafts; returns its recipients."},
			ToolDoc{Name: "move_message", Params: "message_id, target_mailbox", Description: "Move one message to another mailbox (trash and junk refused)."},
			ToolDoc{Name: "save_profile", Params: "profile", Description: "Save the writing profile."},
			ToolDoc{Name: "remember", Params: "preference", Description: "Save one taught preference."},
			ToolDoc{Name: "forget", Params: "number", Description: "Delete one taught preference."},
		)
	}
	return docs
}

func startToolLog(logger *slog.Logger, name string) func() {
	started := time.Now()
	return func() {
		logger.Info("tool_call", slog.String("tool", name), slog.Duration("duration", time.Since(started)))
	}
}
