package mail

import "time"

const (
	FieldSep  = "\x1f" // Unit Separator
	RecordSep = "\x1e" // Record Separator
	ListSep   = "\x1d" // Group Separator
)

type Account struct {
	Name string `json:"name"`
}

type Mailbox struct {
	Account     string `json:"account"`
	Name        string `json:"name"`
	UnreadCount int    `json:"unread_count"`
}

type MessageSummary struct {
	ID      string    `json:"id"`
	Subject string    `json:"subject"`
	Sender  string    `json:"sender"`
	Date    time.Time `json:"date"`
	Read    bool      `json:"read"`
	Mailbox string    `json:"mailbox"`
	Account string    `json:"account"`
}

type Message struct {
	ID         string    `json:"id"`
	Subject    string    `json:"subject"`
	Sender     string    `json:"sender"`
	Recipients []string  `json:"recipients"`
	CC         []string  `json:"cc"`
	Date       time.Time `json:"date"`
	Body       string    `json:"body"`
	BodyCut    bool      `json:"-"`
	Mailbox    string    `json:"mailbox"`
	Read       bool      `json:"read"`
	Flagged    bool      `json:"flagged"`
}

// SearchResult says how much of the mailbox a search covered, so the model can
// tell the user when older mail was not looked at.
type SearchResult struct {
	Messages    []MessageSummary
	Scanned     int
	MailboxSize int
	Truncated   bool
	Engine      string // "index" or "mail"
	More        bool   // more matches exist beyond the limit
	Fallback    bool   // the index was wanted but could not be used
}

type SearchQuery struct {
	Account         string
	Mailbox         string
	SenderContains  string
	SubjectContains string
	UnreadOnly      bool
	DateFrom        *time.Time
	DateTo          *time.Time
	Limit           int
	ScanLimit       int
}

type ComposeInput struct {
	To      []string
	Subject string
	Body    string
	CC      []string
	Account string
}

type ReplyInput struct {
	MessageID string
	Body      string
}

// ReplyResult says where the saved draft is addressed.
type ReplyResult struct {
	Recipients []string
	Sender     string
	Warning    string
}

type UnreadCounts struct {
	Account   string         `json:"account,omitempty"`
	Total     int            `json:"total"`
	Mailboxes []MailboxCount `json:"mailboxes"`
}

type MailboxCount struct {
	Account string `json:"account"`
	Mailbox string `json:"mailbox"`
	Unread  int    `json:"unread"`
}
