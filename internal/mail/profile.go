package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxVoiceRunes = 6000
	maxRuleRunes  = 300
	maxRules      = 60
)

// Profile is one person's data: how they write and what they taught the
// assistant. It is the only place anything personal lives. Everything in it is
// a preference: it cannot change who a draft may go to or what the tools do.
type Profile struct {
	Voice   string    `json:"voice"`
	Rules   []Rule    `json:"rules"`
	Updated time.Time `json:"updated"`
}

// Rule is one thing the user taught, numbered so it can be forgotten.
type Rule struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}

// ProfileStore keeps the profile in one local file, readable only by its owner.
type ProfileStore struct {
	path string
	mu   sync.Mutex
}

func NewProfileStore(path string) *ProfileStore {
	return &ProfileStore{path: path}
}

// Load returns an empty profile when none exists yet.
func (s *ProfileStore) Load() (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *ProfileStore) load() (Profile, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Profile{}, nil
	}
	if err != nil {
		return Profile{}, err
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return Profile{}, fmt.Errorf("profile file is damaged: %w", err)
	}
	return p, nil
}

func (s *ProfileStore) save(p Profile) error {
	p.Updated = time.Now().UTC()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

var (
	urlLike   = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+|\b[a-z0-9-]+\.(?:com|net|org|io|ai|ru|cn|info|xyz|top|link)\b(?:/\S*)?`)
	emailLike = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

// checkTaught refuses text that could turn a preference into a route out: a web
// address, or an email address the user has never written to.
func (c *Client) checkTaught(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("nothing to save")
	}
	if hasControlChars(text) {
		return errors.New("text contains control characters")
	}
	if urlLike.MatchString(emailLike.ReplaceAllString(text, " ")) {
		return errors.New("nothing was saved: a preference cannot contain a web address")
	}
	return nil
}

// SaveVoice stores the writing profile made during onboarding.
func (c *Client) SaveVoice(ctx context.Context, voice string) error {
	if c.profile == nil {
		return errors.New("profile is not available")
	}
	voice = strings.TrimSpace(voice)
	if err := c.checkTaught(voice); err != nil {
		return err
	}
	if len([]rune(voice)) > maxVoiceRunes {
		return fmt.Errorf("the profile is too long: keep it under %d characters", maxVoiceRunes)
	}
	if err := c.checkRecipients(ctx, emailLike.FindAllString(voice, -1)); err != nil {
		return err
	}
	c.profile.mu.Lock()
	defer c.profile.mu.Unlock()
	p, err := c.profile.load()
	if err != nil {
		return err
	}
	p.Voice = voice
	if err := c.profile.save(p); err != nil {
		return err
	}
	c.audit("save_profile", map[string]any{"characters": len([]rune(voice))})
	return nil
}

// Remember adds one rule the user taught.
func (c *Client) Remember(ctx context.Context, text string) (Rule, error) {
	if c.profile == nil {
		return Rule{}, errors.New("profile is not available")
	}
	text = strings.TrimSpace(text)
	if err := c.checkTaught(text); err != nil {
		return Rule{}, err
	}
	if len([]rune(text)) > maxRuleRunes {
		return Rule{}, fmt.Errorf("the rule is too long: keep it under %d characters", maxRuleRunes)
	}
	if err := c.checkRecipients(ctx, emailLike.FindAllString(text, -1)); err != nil {
		return Rule{}, err
	}
	c.profile.mu.Lock()
	defer c.profile.mu.Unlock()
	p, err := c.profile.load()
	if err != nil {
		return Rule{}, err
	}
	if len(p.Rules) >= maxRules {
		return Rule{}, errors.New("there are too many saved rules: ask the user which one to forget first")
	}
	number := 1
	for _, r := range p.Rules {
		if r.Number >= number {
			number = r.Number + 1
		}
	}
	rule := Rule{Number: number, Text: text}
	p.Rules = append(p.Rules, rule)
	if err := c.profile.save(p); err != nil {
		return Rule{}, err
	}
	c.audit("remember", map[string]any{"rule": number, "text": text})
	return rule, nil
}

// Forget removes one rule by its number.
func (c *Client) Forget(number int) error {
	if c.profile == nil {
		return errors.New("profile is not available")
	}
	c.profile.mu.Lock()
	defer c.profile.mu.Unlock()
	p, err := c.profile.load()
	if err != nil {
		return err
	}
	kept := p.Rules[:0]
	found := false
	for _, r := range p.Rules {
		if r.Number == number {
			found = true
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		return fmt.Errorf("there is no rule number %d", number)
	}
	p.Rules = kept
	if err := c.profile.save(p); err != nil {
		return err
	}
	c.audit("forget", map[string]any{"rule": number})
	return nil
}

// GetProfile returns what is stored.
func (c *Client) GetProfile() (Profile, error) {
	if c.profile == nil {
		return Profile{}, errors.New("profile is not available")
	}
	return c.profile.Load()
}

// UseProfile sets where this person's profile lives.
func (c *Client) UseProfile(s *ProfileStore) {
	c.profile = s
}

// ownWordsCut are lines where someone else's text starts in a reply, in the
// languages the product serves. Nothing after one is the writer's own.
var ownWordsCut = regexp.MustCompile(`(?i)^\s*(?:-{2,}\s*(?:original|forwarded|message|ursprüngliche|pôvodná|mensagem).*|on .{4,120} wrote:\s*|le .{4,120} a écrit\s*:\s*|dňa .{4,120} napísal.{0,6}:\s*|em .{4,120} escreveu:\s*|am .{4,120} schrieb.{0,10}:\s*|(?:from|de|od|von)\s*:\s.+|begin forwarded message:?\s*|début du message réexpédié\s*:?\s*)$`)

var sentFromDevice = regexp.MustCompile(`(?i)^\s*(sent from my|envoyé de mon|odoslané z|enviado do meu|von meinem)\b.*$`)

// OwnWords keeps only what the writer typed: quotes and forwarded text are cut,
// so an incoming email can never shape the writing profile.
func OwnWords(body string) string {
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if ownWordsCut.MatchString(line) {
			break
		}
		if strings.HasPrefix(strings.TrimSpace(line), ">") || sentFromDevice.MatchString(line) {
			continue
		}
		kept = append(kept, strings.TrimRight(line, " \t"))
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// SentSample is one email the user wrote: their words only.
type SentSample struct {
	To    string `json:"to"`
	Date  string `json:"date"`
	Words string `json:"words"`
}

// SampleSentMail returns the user's own words from sent mail, spread over time.
// It needs the index, so it needs Full Disk Access.
func (c *Client) SampleSentMail(ctx context.Context, page int) ([]SentSample, error) {
	if c.sent == nil {
		return nil, errors.New("reading sent mail needs Full Disk Access for this app. Tell the user this must be set up first")
	}
	refs, err := c.sent.SentMessages(ctx, sampleSize, page)
	if err != nil {
		return nil, err
	}
	out := make([]SentSample, 0, len(refs))
	for _, r := range refs {
		m, err := c.GetMessage(ctx, r.ID, true)
		if err != nil {
			continue
		}
		words := []rune(OwnWords(m.Body))
		if len(words) < 20 {
			continue
		}
		if len(words) > sampleRunes {
			words = words[:sampleRunes]
		}
		out = append(out, SentSample{To: r.To, Date: r.Date.Format("2006-01-02"), Words: string(words)})
	}
	return out, nil
}

// SentRef points at one sent message.
type SentRef struct {
	ID   string
	To   string
	Date time.Time
}

// SentLister is the part of the index that finds sent mail.
type SentLister interface {
	SentMessages(ctx context.Context, size, page int) ([]SentRef, error)
}

const (
	sampleSize  = 15
	sampleRunes = 1500
)

// UseSentLister turns on sent-mail sampling.
func (c *Client) UseSentLister(s SentLister) {
	c.sent = s
}
