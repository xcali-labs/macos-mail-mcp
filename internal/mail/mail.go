package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

var emailRegex = regexp.MustCompile(`^[A-Za-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$`)

const (
	messageRefPrefix = "mref:"
	maxBodyRunes     = 20000
)

type messageRef struct {
	Account   string
	Mailbox   string
	NumericID string
}

type Client struct {
	runner    ScriptRunner
	logger    *slog.Logger
	index     Searcher
	indexOnly bool

	accountsMu sync.Mutex
	accountIDs map[string]string

	recipients RecipientChecker
	allowFile  string
	auditFile  string

	waitBudget time.Duration
	sleep      func(context.Context, time.Duration) error

	profile *ProfileStore
	sent    SentLister
}

// mailWaitBudget is how long one tool call waits for a busy Mail. The client's
// own tool timeout must be longer (Jan: toolCallTimeoutSeconds 300).
const mailWaitBudget = 4 * time.Minute

const mailProbeScript = `with timeout of 8 seconds
	tell application "Mail" to count of every account
end timeout`

// The tool passes these to the model. The first means it is safe to call again;
// the second means it must look before repeating.
var (
	ErrMailBusy   = errors.New("mail_busy: Apple Mail is still busy syncing. Nothing was changed. Call this tool again")
	ErrMailUnsure = errors.New("mail_unsure: Apple Mail did not finish in time, so this may or may not have happened. Do not repeat it yet: search Drafts or the folder first, and repeat it only if it is not there")
)

// RecipientChecker answers whether an address is one the user has sent mail to.
// Mail's address table alone will not do: a stranger's address enters it the
// moment their email arrives, so an attacker would be on the list.
type RecipientChecker interface {
	KnownRecipient(ctx context.Context, address string) (bool, error)
}

// UseRecipientCheck sets who new drafts may go to: addresses from the checker,
// the user's own addresses, and one address per line in allowFile.
func (c *Client) UseRecipientCheck(r RecipientChecker, allowFile string) {
	c.recipients = r
	c.allowFile = allowFile
}

// UseAuditFile records every change to Mail in path, one JSON line each, with
// no message text. A failure to write it never blocks the action.
func (c *Client) UseAuditFile(path string) {
	c.auditFile = path
}

func (c *Client) audit(action string, fields map[string]any) {
	if c.auditFile == "" {
		return
	}
	fields["action"] = action
	fields["time"] = time.Now().UTC().Format(time.RFC3339)
	line, err := json.Marshal(fields)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(c.auditFile), 0o700)
	}
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(c.auditFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			defer f.Close()
			_, err = f.Write(append(line, '\n'))
		}
	}
	if err != nil {
		c.logger.Warn("audit log write failed", slog.String("error", err.Error()))
	}
}

// checkRecipients fails closed: with no checker and no allow file, only the user's
// own addresses pass.
func (c *Client) checkRecipients(ctx context.Context, addrs []string) error {
	allowed := map[string]bool{}
	if c.allowFile != "" {
		if data, err := os.ReadFile(c.allowFile); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if a := strings.ToLower(strings.TrimSpace(line)); a != "" && !strings.HasPrefix(a, "#") {
					allowed[a] = true
				}
			}
		}
	}
	if raw, err := c.runScript(ctx, buildOwnAddressesScript()); err == nil {
		for _, a := range strings.Split(raw, RecordSep) {
			allowed[strings.ToLower(strings.TrimSpace(a))] = true
		}
	}
	for _, addr := range addrs {
		a := strings.ToLower(strings.TrimSpace(addr))
		if allowed[a] {
			continue
		}
		known := false
		if c.recipients != nil {
			ok, err := c.recipients.KnownRecipient(ctx, a)
			if err != nil {
				c.logger.Warn("recipient check failed", slog.String("error", err.Error()))
			}
			known = ok && err == nil
		}
		if !known {
			return fmt.Errorf("nothing was saved: %s is not someone the user has written to before. Tell the user this. To write to a new address they must start the email themselves in Apple Mail", addr)
		}
	}
	return nil
}

// Searcher is the fast search engine. The client falls back to Mail's
// scripting when it fails, so a missing permission never stops search.
type Searcher interface {
	Search(ctx context.Context, q SearchQuery) (SearchResult, error)
}

// UseIndex turns on the fast engine. With indexOnly a failure is returned
// instead of falling back.
func (c *Client) UseIndex(s Searcher, indexOnly bool) {
	c.index = s
	c.indexOnly = indexOnly
}

// AccountIDs maps Mail's account ids to account names, cached after the first
// success. The index engine needs it: mailbox URLs carry only the id.
func (c *Client) AccountIDs(ctx context.Context) (map[string]string, error) {
	c.accountsMu.Lock()
	defer c.accountsMu.Unlock()
	if c.accountIDs != nil {
		return c.accountIDs, nil
	}
	raw, err := c.runScript(ctx, buildAccountIDsScript())
	if err != nil {
		return nil, err
	}
	ids := map[string]string{}
	for _, row := range strings.Split(raw, RecordSep) {
		parts := strings.Split(row, FieldSep)
		if len(parts) == 2 && parts[0] != "" {
			ids[parts[0]] = parts[1]
		}
	}
	c.accountIDs = ids
	return ids, nil
}

type API interface {
	ListAccounts(ctx context.Context) ([]Account, error)
	ListMailboxes(ctx context.Context, account string) ([]Mailbox, error)
	SearchMessages(ctx context.Context, q SearchQuery) (SearchResult, error)
	GetMessage(ctx context.Context, messageID string, includeBody bool) (Message, error)
	CreateDraft(ctx context.Context, input ComposeInput) (string, error)
	ReplyDraft(ctx context.Context, input ReplyInput) (ReplyResult, error)
	MoveMessage(ctx context.Context, messageID, targetMailbox string) error
	GetUnreadCount(ctx context.Context, account string) (UnreadCounts, error)
	GetProfile() (Profile, error)
	SaveVoice(ctx context.Context, voice string) error
	Remember(ctx context.Context, text string) (Rule, error)
	Forget(number int) error
	SampleSentMail(ctx context.Context, page int) ([]SentSample, error)
}

func NewClient(runner ScriptRunner, logger *slog.Logger) *Client {
	if runner == nil {
		runner = NewOsaScriptRunnerFromEnv()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{runner: runner, logger: logger, waitBudget: mailWaitBudget, sleep: sleepContext}
}

func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	raw, err := c.runScript(ctx, buildListAccountsScript())
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	names := strings.Split(raw, RecordSep)
	accounts := make([]Account, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		accounts = append(accounts, Account{Name: name})
	}
	return accounts, nil
}

func (c *Client) ListMailboxes(ctx context.Context, account string) ([]Mailbox, error) {
	if strings.TrimSpace(account) == "" {
		return nil, errors.New("account is required")
	}
	raw, err := c.runScript(ctx, buildListMailboxesScript(account))
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	rows := strings.Split(raw, RecordSep)
	out := make([]Mailbox, 0, len(rows))
	for _, row := range rows {
		parts := strings.Split(row, FieldSep)
		if len(parts) != 2 {
			continue
		}
		unread, _ := strconv.Atoi(parts[1])
		out = append(out, Mailbox{Account: account, Name: parts[0], UnreadCount: unread})
	}
	return out, nil
}

// SearchMessages uses the index when it can and Mail's scripting otherwise.
func (c *Client) SearchMessages(ctx context.Context, q SearchQuery) (SearchResult, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > maxSearchLimit {
		q.Limit = maxSearchLimit
	}
	if q.DateFrom != nil && q.DateTo != nil && q.DateFrom.After(*q.DateTo) {
		return SearchResult{}, errors.New("date_from must be before or equal to date_to")
	}

	fallback := false
	if c.index != nil {
		res, err := c.index.Search(ctx, q)
		if err == nil {
			return res, nil
		}
		if errors.Is(err, ErrBadSearch) || c.indexOnly {
			return SearchResult{}, err
		}
		c.logger.Warn("index search failed, using Mail scripting", slog.String("error", err.Error()))
		fallback = true
	}

	var res SearchResult
	var err error
	if strings.TrimSpace(q.Account) == "" {
		res, err = c.searchEveryAccountViaMail(ctx, q)
	} else {
		res, err = c.searchViaMail(ctx, q)
	}
	if err != nil {
		return SearchResult{}, err
	}
	res.Engine = "mail"
	res.Fallback = fallback
	return res, nil
}

func (c *Client) searchEveryAccountViaMail(ctx context.Context, q SearchQuery) (SearchResult, error) {
	accounts, err := c.ListAccounts(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	var all SearchResult
	for _, a := range accounts {
		one := q
		one.Account = a.Name
		res, err := c.searchViaMail(ctx, one)
		if err != nil {
			c.logger.Warn("account search failed", slog.String("account", a.Name), slog.String("error", err.Error()))
			all.Truncated = true
			continue
		}
		all.Messages = append(all.Messages, res.Messages...)
		all.Scanned += res.Scanned
		all.MailboxSize += res.MailboxSize
		all.Truncated = all.Truncated || res.Truncated
	}
	sort.SliceStable(all.Messages, func(i, j int) bool { return all.Messages[i].Date.After(all.Messages[j].Date) })
	if len(all.Messages) > q.Limit {
		all.Messages = all.Messages[:q.Limit]
		all.More = true
	}
	return all, nil
}

func (c *Client) searchViaMail(ctx context.Context, q SearchQuery) (SearchResult, error) {
	if q.Mailbox == "" {
		q.Mailbox = defaultMailbox
	}
	raw, err := c.runScript(ctx, buildSearchMessagesScript(q))
	if err != nil {
		return SearchResult{}, err
	}
	res := parseSearchResult(raw)
	// Re-check every row: a message that arrived mid-search shifts positions,
	// so the script's filter may have looked at a neighbour.
	kept := res.Messages[:0]
	for _, msg := range res.Messages {
		if matchesQuery(msg, q) {
			msg.Account = q.Account
			msg.ID = encodeMessageRef(q.Account, msg.Mailbox, msg.ID)
			kept = append(kept, msg)
		}
	}
	res.Messages = kept
	return res, nil
}

func matchesQuery(m MessageSummary, q SearchQuery) bool {
	if q.UnreadOnly && m.Read {
		return false
	}
	if q.SenderContains != "" && !strings.Contains(strings.ToLower(m.Sender), strings.ToLower(q.SenderContains)) {
		return false
	}
	if q.SubjectContains != "" && !strings.Contains(strings.ToLower(m.Subject), strings.ToLower(q.SubjectContains)) {
		return false
	}
	if (q.DateFrom != nil || q.DateTo != nil) && m.Date.IsZero() {
		return false
	}
	if q.DateFrom != nil && m.Date.Before(q.DateFrom.Truncate(time.Second)) {
		return false
	}
	if q.DateTo != nil && m.Date.After(*q.DateTo) {
		return false
	}
	return true
}

func parseSearchResult(raw string) SearchResult {
	var res SearchResult
	if raw == "" {
		return res
	}
	records := strings.Split(raw, RecordSep)
	if strings.HasPrefix(records[0], "META"+FieldSep) {
		meta := strings.Split(records[0], FieldSep)
		if len(meta) == 4 {
			res.Scanned, _ = strconv.Atoi(meta[1])
			res.MailboxSize, _ = strconv.Atoi(meta[2])
			res.Truncated = parseBool(meta[3])
		}
		records = records[1:]
	}
	res.Messages = parseSearchMessages(strings.Join(records, RecordSep))
	return res
}

func (c *Client) GetMessage(ctx context.Context, messageID string, includeBody bool) (Message, error) {
	if strings.TrimSpace(messageID) == "" {
		return Message{}, errors.New("message_id is required")
	}
	ref, err := decodeMessageRef(messageID)
	if err != nil {
		return Message{}, err
	}

	raw, err := c.runScript(ctx, buildGetMessageScript(ref.NumericID, ref.Account, ref.Mailbox, includeBody))
	if err != nil {
		return Message{}, err
	}
	parts := strings.Split(raw, FieldSep)
	if len(parts) != 10 {
		return Message{}, fmt.Errorf("unexpected get_message payload format")
	}
	messageOutID := parts[0]
	if strings.TrimSpace(ref.Account) != "" {
		messageOutID = encodeMessageRef(ref.Account, parts[7], parts[0])
	}
	body, cut := capBody(parts[6])
	return Message{
		BodyCut:    cut,
		ID:         messageOutID,
		Subject:    parts[1],
		Sender:     parts[2],
		Recipients: splitNonEmpty(parts[3], ListSep),
		CC:         splitNonEmpty(parts[4], ListSep),
		Date:       parseMailDate(parts[5]),
		Body:       body,
		Mailbox:    parts[7],
		Read:       parseBool(parts[8]),
		Flagged:    parseBool(parts[9]),
	}, nil
}

func (c *Client) CreateDraft(ctx context.Context, input ComposeInput) (string, error) {
	input.Subject = CleanDraftText(input.Subject)
	input.Body = CleanDraftText(input.Body)
	if err := validateComposeInput(input); err != nil {
		return "", err
	}
	if err := c.checkRecipients(ctx, append(append([]string{}, input.To...), input.CC...)); err != nil {
		return "", err
	}
	id, err := c.runScriptOnce(ctx, buildDraftScript(input))
	if err != nil {
		if errors.Is(err, ErrMailUnsure) {
			c.audit("create_draft_unsure", map[string]any{"to": input.To, "cc": input.CC})
		}
		return "", err
	}
	c.audit("create_draft", map[string]any{"to": input.To, "cc": input.CC})
	return id, nil
}

// ReplyDraft saves a quoted reply inside the message's thread, unsent, in
// Drafts, and says who it is addressed to. Measured on macOS Mail: a scripted
// reply goes to the sender and ignores Reply-To, so a forged Reply-To cannot
// redirect it, but a real one (a mailing list, an event host) is not honoured
// either. When Reply-To is another organisation the result says so.
func (c *Client) ReplyDraft(ctx context.Context, input ReplyInput) (ReplyResult, error) {
	ref, err := decodeMessageRef(input.MessageID)
	if err != nil {
		return ReplyResult{}, err
	}
	if strings.TrimSpace(input.Body) == "" {
		return ReplyResult{}, errors.New("body is required")
	}
	if hasControlChars(input.Body) {
		return ReplyResult{}, errors.New("body contains control characters")
	}
	raw, err := c.runScript(ctx, buildReplyTargetScript(ref.NumericID, ref.Account, ref.Mailbox))
	if err != nil {
		return ReplyResult{}, err
	}
	parts := strings.Split(raw, FieldSep)
	if len(parts) != 2 {
		return ReplyResult{}, errors.New("unexpected reply target format")
	}
	sender, replyTo := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])

	raw, err = c.runScriptOnce(ctx, buildReplyDraftScript(ref.NumericID, ref.Account, ref.Mailbox, CleanDraftText(input.Body)))
	if err != nil {
		if errors.Is(err, ErrMailUnsure) {
			c.audit("reply_draft_unsure", map[string]any{"message": ref.NumericID, "mailbox": ref.Mailbox, "sender": sender})
		}
		return ReplyResult{}, err
	}
	res := ReplyResult{Recipients: splitNonEmpty(raw, ListSep), Sender: sender}
	switch {
	case len(res.Recipients) == 0:
		res.Warning = "Mail did not report a recipient. Tell the user to check the To field before sending."
	default:
		for _, r := range res.Recipients {
			if !strings.EqualFold(strings.TrimSpace(r), sender) {
				res.Warning = "This draft is addressed to someone other than the sender. Tell the user exactly who it goes to."
			}
		}
	}
	if res.Warning == "" && replyTo != "" && !sameOrganisation(sender, replyTo) {
		res.Warning = fmt.Sprintf("The draft goes to the sender, %s, but this email asks for replies to go to %s. Tell the user both addresses. This is common in mailing lists and also in scams. If the second address is right the user can change the To field in Apple Mail before sending.", sender, replyTo)
	}
	c.audit("reply_draft", map[string]any{"message": ref.NumericID, "mailbox": ref.Mailbox, "to": res.Recipients, "sender": sender})
	return res, nil
}

// MoveMessage files one message into another mailbox of its own account.
// Trash, junk, deleted-items, sent, drafts and outbox mailboxes are refused.
func (c *Client) MoveMessage(ctx context.Context, messageID, targetMailbox string) error {
	ref, err := decodeMessageRef(messageID)
	if err != nil {
		return err
	}
	target := strings.TrimSpace(targetMailbox)
	if target == "" {
		return errors.New("target_mailbox is required")
	}
	if hasControlChars(target) {
		return errors.New("target_mailbox contains control characters")
	}
	if isProtectedMailbox(target) || isOutgoingMailbox(target) {
		return fmt.Errorf("moving to %q is not allowed: this tool never moves mail into trash, junk, sent, drafts or outbox folders", target)
	}
	if _, err = c.runScriptOnce(ctx, buildMoveMessageScript(ref.NumericID, ref.Account, ref.Mailbox, target)); err != nil {
		if errors.Is(err, ErrMailUnsure) {
			c.audit("move_message_unsure", map[string]any{"message": ref.NumericID, "from": ref.Mailbox, "to": target})
		}
		return err
	}
	c.audit("move_message", map[string]any{"message": ref.NumericID, "from": ref.Mailbox, "to": target})
	return nil
}

// protectedMailboxWords are lower-case fragments of trash, junk and
// deleted-items mailbox names in the languages the product serves
// (English, French, Slovak, German, Portuguese). A match anywhere in the
// target name refuses the move.
var protectedMailboxWords = []string{
	"trash", "deleted", "junk", "spam",
	"corbeille", "supprim", "indésirable", "indesirable", "pourriel",
	"kôš", "odstránen", "odstranen", "vymazan", "nevyžiadan", "nevyziadan",
	"papierkorb", "gelöscht", "geloscht",
	"lixo", "lixeira", "eliminad", "apagad", "excluíd", "excluid", "reciclagem",
}

// protectedMailboxNames are short names refused only on an exact match, so a
// folder such as "Cabinet" or "Kostol" is still allowed.
var protectedMailboxNames = []string{"bin", "kos", "koš"}

// draftAndOutboxNames are lower-case, accent-free names of drafts and outbox
// folders in English, French, Slovak, German and Portuguese.
var draftAndOutboxNames = map[string]bool{
	"draft": true, "drafts": true,
	"brouillon": true, "brouillons": true,
	"koncept": true, "koncepty": true, "rozpisane": true, "rozpisane spravy": true,
	"entwurf": true, "entwurfe": true,
	"rascunho": true, "rascunhos": true,
	"outbox": true,
	"boite d'envoi": true, "boite denvoi": true, "boite d’envoi": true,
	"na odoslanie": true, "odosielane": true,
	"postausgang": true,
	"caixa de saida": true, "a enviar": true,
}

func isProtectedMailbox(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, exact := range protectedMailboxNames {
		if n == exact {
			return true
		}
	}
	for _, w := range protectedMailboxWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// isOutgoingMailbox refuses moves into sent, drafts or outbox folders so an
// incoming message cannot be moved into a sent folder to launder its recipients
// onto the known-recipient list.
func isOutgoingMailbox(name string) bool {
	trimmed := strings.TrimSpace(name)
	leaf := trimmed
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		leaf = trimmed[i+1:]
	}
	for _, cand := range []string{foldText(trimmed), foldText(leaf)} {
		if sentMailboxNames[cand] || draftAndOutboxNames[cand] {
			return true
		}
	}
	return false
}

// sameOrganisation compares registered domains using the public suffix list, so
// mail.brand.com matches brand.com but evil.co.uk does not match bank.co.uk.
// Anything it cannot parse counts as different.
func sameOrganisation(a, b string) bool {
	if strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) {
		return true
	}
	da, errA := publicsuffix.EffectiveTLDPlusOne(emailDomain(a))
	db, errB := publicsuffix.EffectiveTLDPlusOne(emailDomain(b))
	return errA == nil && errB == nil && strings.EqualFold(da, db)
}

func emailDomain(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(addr[i+1:]))
	}
	return ""
}

// capBody keeps a message inside Jan's tool output limit, so the note saying it
// was cut is not itself cut off.
func capBody(body string) (string, bool) {
	r := []rune(body)
	if len(r) <= maxBodyRunes {
		return body, false
	}
	return string(r[:maxBodyRunes]), true
}

func encodeMessageRef(account, mailbox, numericID string) string {
	account = strings.TrimSpace(account)
	mailbox = strings.TrimSpace(mailbox)
	numericID = strings.TrimSpace(numericID)
	if account == "" || mailbox == "" || numericID == "" {
		return numericID
	}
	return messageRefPrefix + url.QueryEscape(account) + ":" + url.QueryEscape(mailbox) + ":" + numericID
}

func decodeMessageRef(value string) (messageRef, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return messageRef{}, errors.New("message_id is required")
	}
	if strings.HasPrefix(value, messageRefPrefix) {
		parts := strings.SplitN(strings.TrimPrefix(value, messageRefPrefix), ":", 3)
		if len(parts) != 3 {
			return messageRef{}, fmt.Errorf("invalid message_id %q", value)
		}
		account, err := url.QueryUnescape(parts[0])
		if err != nil {
			return messageRef{}, fmt.Errorf("invalid message_id account component: %w", err)
		}
		mailbox, err := url.QueryUnescape(parts[1])
		if err != nil {
			return messageRef{}, fmt.Errorf("invalid message_id mailbox component: %w", err)
		}
		numericID := strings.TrimSpace(parts[2])
		if _, err := strconv.Atoi(numericID); err != nil {
			return messageRef{}, errors.New("message_id must contain a numeric id")
		}
		return messageRef{Account: account, Mailbox: mailbox, NumericID: numericID}, nil
	}
	if _, err := strconv.Atoi(value); err != nil {
		return messageRef{}, errors.New("message_id must be an integer string")
	}
	return messageRef{NumericID: value}, nil
}

func (c *Client) GetUnreadCount(ctx context.Context, account string) (UnreadCounts, error) {
	raw, err := c.runScript(ctx, buildUnreadCountScript(account))
	if err != nil {
		return UnreadCounts{}, err
	}
	if raw == "" {
		return UnreadCounts{Account: account, Mailboxes: nil}, nil
	}
	rows := strings.Split(raw, RecordSep)
	if len(rows) == 0 {
		return UnreadCounts{Account: account, Mailboxes: nil}, nil
	}
	total, _ := strconv.Atoi(rows[0])
	counts := make([]MailboxCount, 0, len(rows)-1)
	for _, row := range rows[1:] {
		parts := strings.Split(row, FieldSep)
		if len(parts) != 3 {
			continue
		}
		unread, _ := strconv.Atoi(parts[2])
		counts = append(counts, MailboxCount{Account: parts[0], Mailbox: parts[1], Unread: unread})
	}
	return UnreadCounts{Account: account, Total: total, Mailboxes: counts}, nil
}

func (c *Client) runScript(ctx context.Context, script string) (string, error) {
	return c.run(ctx, script, true)
}

// runScriptOnce never retries. Every script that changes Mail uses it.
func (c *Client) runScriptOnce(ctx context.Context, script string) (string, error) {
	return c.run(ctx, script, false)
}

func (c *Client) run(ctx context.Context, script string, retryOnTimeout bool) (string, error) {
	deadline := time.Now().Add(c.waitBudget)
	for attempt := 1; ; attempt++ {
		if err := c.waitForMail(ctx, deadline); err != nil {
			return "", err
		}
		out, err := c.runner.Run(ctx, script)
		if err == nil {
			return out, nil
		}
		if isTimeoutErr(err) {
			if !retryOnTimeout {
				// A write that timed out may still have happened, so it is
				// never repeated here: a repeat could make a second draft.
				return "", ErrMailUnsure
			}
			if attempt == 1 || time.Now().Before(deadline) {
				c.logger.Warn("mail busy, reading again", slog.Int("attempt", attempt))
				continue
			}
			return "", ErrMailBusy
		}
		var execErr *ExecError
		if errors.As(err, &execErr) {
			c.logger.Error("apple script execution failed",
				slog.Int("exit_code", execErr.ExitCode),
				slog.String("stderr", execErr.Stderr),
			)
		}
		return "", err
	}
}

// waitForMail holds until Mail answers a tiny script, because Mail stops
// answering scripts for a minute or more while it syncs. Nothing has been
// changed yet at this point, so waiting here is always safe.
func (c *Client) waitForMail(ctx context.Context, deadline time.Time) error {
	if c.waitBudget <= 0 {
		return nil
	}
	delay := 2 * time.Second
	for {
		_, err := c.runner.Run(ctx, mailProbeScript)
		if err == nil || !isTimeoutErr(err) {
			return nil // any other problem surfaces on the real script
		}
		if time.Now().Add(delay).After(deadline) {
			return ErrMailBusy
		}
		c.logger.Warn("mail busy, waiting", slog.Duration("next_try", delay))
		if err := c.sleep(ctx, delay); err != nil {
			return err
		}
		if delay *= 2; delay > 15*time.Second {
			delay = 15 * time.Second
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "timeout") || strings.Contains(text, "timed out") || strings.Contains(text, "-1712")
}

func splitNonEmpty(s, sep string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	items := strings.Split(s, sep)
	out := make([]string, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		out = append(out, it)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseMailDate reads the "Y-M-D-S" stamp the scripts return (local time,
// S = seconds since midnight). A plain integer is still read as Unix seconds.
func parseMailDate(s string) time.Time {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, "-")
	if len(parts) == 4 {
		n := make([]int, 4)
		for i, p := range parts {
			v, err := strconv.Atoi(p)
			if err != nil {
				return time.Time{}
			}
			n[i] = v
		}
		if n[0] < 1900 || n[1] < 1 || n[1] > 12 || n[2] < 1 || n[2] > 31 || n[3] < 0 || n[3] > 86400 {
			return time.Time{}
		}
		return time.Date(n[0], time.Month(n[1]), n[2], 0, 0, 0, 0, time.Local).Add(time.Duration(n[3]) * time.Second).UTC()
	}
	return parseUnix(s)
}

func parseUnix(s string) time.Time {
	sec, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

func parseBool(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "true" || s == "yes" || s == "1"
}

func parseSearchMessages(raw string) []MessageSummary {
	if raw == "" {
		return nil
	}
	rows := strings.Split(raw, RecordSep)
	msgs := make([]MessageSummary, 0, len(rows))
	for _, row := range rows {
		parts := strings.Split(row, FieldSep)
		if len(parts) != 6 {
			continue
		}
		msgs = append(msgs, MessageSummary{
			ID:      parts[0],
			Subject: parts[1],
			Sender:  parts[2],
			Date:    parseMailDate(parts[3]),
			Read:    parseBool(parts[4]),
			Mailbox: parts[5],
		})
	}
	return msgs
}

func filterMessagesByDate(msgs []MessageSummary, from, to *time.Time) []MessageSummary {
	if from == nil && to == nil {
		return msgs
	}
	out := make([]MessageSummary, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Date.IsZero() {
			continue
		}
		if from != nil && msg.Date.Before(*from) {
			continue
		}
		if to != nil && msg.Date.After(*to) {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func isDateSentQueryError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "date sent")
}

func validateComposeInput(input ComposeInput) error {
	if len(input.To) == 0 {
		return errors.New("to is required")
	}
	if strings.TrimSpace(input.Subject) == "" {
		return errors.New("subject is required")
	}
	if input.Body == "" {
		return errors.New("body is required")
	}
	for _, addr := range append(append([]string{}, input.To...), input.CC...) {
		if strings.TrimSpace(addr) == "" {
			return errors.New("email addresses cannot be empty")
		}
		if hasControlChars(addr) {
			return fmt.Errorf("invalid email address %q: contains control characters", addr)
		}
		if !emailRegex.MatchString(addr) {
			return fmt.Errorf("invalid email address %q", addr)
		}
	}
	if hasControlChars(input.Subject) || hasControlChars(input.Body) {
		return errors.New("subject/body contain control characters")
	}
	return nil
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			if r == '\n' || r == '\r' || r == '\t' {
				continue
			}
			return true
		}
	}
	return false
}
