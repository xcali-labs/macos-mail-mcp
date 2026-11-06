package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/maximbilan/apple-mail-mcp-go/internal/mail"
	"github.com/maximbilan/apple-mail-mcp-go/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.3.0-draftonly"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "index-check":
			if err := runIndexCheck(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		case "tools-docs":
			if err := runToolsDocs(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		}
	}

	if err := runServer(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("apple-mail-mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	defaultReadOnly := parseEnvBool("APPLE_MAIL_MCP_READ_ONLY")
	defaultLogLevel := strings.TrimSpace(os.Getenv("APPLE_MAIL_MCP_LOG_LEVEL"))
	if defaultLogLevel == "" {
		defaultLogLevel = "info"
	}
	readOnly := fs.Bool("read-only", defaultReadOnly, "register only read-side tools")
	writeOnly := fs.Bool("write-only", false, "register only the tools that change something")
	logLevel := fs.String("log-level", defaultLogLevel, "log level: debug|info|warn|error")
	engine := fs.String("search-engine", "auto", "search engine: auto (index, else Mail scripting) | index | mail")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *readOnly && *writeOnly {
		return errors.New("--read-only and --write-only cannot be combined")
	}
	level, err := parseLogLevel(*logLevel)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	server := mcp.NewServer(&mcp.Implementation{Name: "apple-mail-mcp", Version: version}, nil)
	client := mail.NewClient(mail.NewOsaScriptRunnerFromEnv(), logger)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var index *mail.IndexEngine
	switch *engine {
	case "mail":
	case "auto", "index":
		path, err := mail.FindIndexPath(home)
		switch {
		case err == nil:
			index = mail.NewIndexEngine(path, client.AccountIDs)
			client.UseIndex(index, *engine == "index")
		case *engine == "index":
			return fmt.Errorf("--search-engine index: %w", err)
		default:
			logger.Warn("mail index not readable, using Mail scripting", slog.String("error", err.Error()))
		}
	default:
		return fmt.Errorf("invalid --search-engine %q", *engine)
	}
	allowFile := filepath.Join(home, "Library", "Application Support", "apple-mail-mcp", "allowed-recipients.txt")
	if index != nil {
		client.UseRecipientCheck(index, allowFile)
	} else {
		client.UseRecipientCheck(nil, allowFile)
	}
	client.UseProfile(mail.NewProfileStore(filepath.Join(home, "Library", "Application Support", "apple-mail-mcp", "profile.json")))
	if index != nil {
		client.UseSentLister(index)
	}
	client.UseAuditFile(filepath.Join(home, "Library", "Logs", "apple-mail-mcp", "actions.log"))
	tools.Register(server, client, tools.RegisterOptions{ReadOnly: *readOnly, WriteOnly: *writeOnly, Logger: logger})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("server run failed: %w", err)
	}
	return nil
}

// runIndexCheck prints the index's shape and search timings. It never prints
// a subject, a sender or a body, so it is safe to run on someone's mailbox.
func runIndexCheck(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path, err := mail.FindIndexPath(home)
	if err != nil {
		return err
	}
	ctx := context.Background()
	var engine *mail.IndexEngine
	engine = mail.NewIndexEngine(path, func(ctx context.Context) (map[string]string, error) {
		return engine.AccountHosts(ctx)
	})
	r, err := engine.Check(ctx)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}
	fmt.Printf("recipients columns: %s\nsent folders recognised: %d\n", strings.Join(r.RecipientCols, ","), r.SentFolders)
	fmt.Printf("index: %s\nmessages: %d\nmailboxes: %d\nopen+count: %s\nsender_addresses table: %v\nmessages columns: %s\naddresses columns: %s\n",
		r.Path, r.Messages, r.Mailboxes, r.OpenAndCount.Round(time.Millisecond), r.SenderTable, strings.Join(r.MessageColumns, ","), strings.Join(r.AddressColumns, ","))
	for _, c := range []struct {
		name string
		q    mail.SearchQuery
	}{
		{"latest 20, all mail", mail.SearchQuery{Limit: 20}},
		{"unread 20", mail.SearchQuery{Limit: 20, UnreadOnly: true}},
		{"subject word, no hits", mail.SearchQuery{Limit: 20, SubjectContains: "zzqqxxnomatch"}},
		{"sender word, no hits", mail.SearchQuery{Limit: 20, SenderContains: "zzqqxxnomatch"}},
	} {
		start := time.Now()
		res, err := engine.Search(ctx, c.q)
		if err != nil {
			fmt.Printf("%s: ERROR %v\n", c.name, err)
			continue
		}
		blankSender, blankDate := 0, 0
		for _, m := range res.Messages {
			if m.Sender == "" {
				blankSender++
			}
			if m.Date.Year() < 1990 {
				blankDate++
			}
		}
		fmt.Printf("%s: %d results in %s (blank senders %d, bad dates %d)\n", c.name, len(res.Messages), time.Since(start).Round(time.Millisecond), blankSender, blankDate)
	}
	if len(args) > 0 && args[0] == "--deep" {
		return runDeepCheck(ctx, path)
	}
	return nil
}

// runDeepCheck proves index ids are Mail's ids: it opens messages found in the
// index through Mail's scripting and compares subjects. It prints only counts
// and timings. It needs Automation of Mail for the app running it.
func runDeepCheck(ctx context.Context, path string) error {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := mail.NewClient(mail.NewOsaScriptRunnerFromEnv(), logger)
	engine := mail.NewIndexEngine(path, client.AccountIDs)
	ids, err := client.AccountIDs(ctx)
	if err != nil {
		return fmt.Errorf("read accounts through Mail: %w", err)
	}
	fmt.Printf("accounts known to Mail: %d\n", len(ids))

	fiveYearsAgo := time.Now().AddDate(-5, 0, 0)
	samples := []struct {
		name string
		q    mail.SearchQuery
	}{
		{"newest", mail.SearchQuery{Limit: 200}},
		{"older than 5 years", mail.SearchQuery{Limit: 200, DateTo: &fiveYearsAgo}},
	}
	for _, s := range samples {
		res, err := engine.Search(ctx, s.q)
		if err != nil {
			fmt.Printf("%s: search ERROR %v\n", s.name, err)
			continue
		}
		picked := map[string]int{}
		matched, wrong, failed := 0, 0, 0
		var slowest time.Duration
		for _, m := range res.Messages {
			key := m.Account + "/" + m.Mailbox
			if picked[key] >= 2 || len(picked) > 12 {
				continue
			}
			picked[key]++
			start := time.Now()
			got, err := client.GetMessage(ctx, m.ID, false)
			took := time.Since(start)
			if took > slowest {
				slowest = took
			}
			switch {
			case err != nil:
				failed++
				fmt.Printf("  open failed in folder #%d after %s: %s\n", len(picked), took.Round(time.Millisecond), firstWords(err.Error()))
			case sameSubject(got.Subject, m.Subject):
				matched++
			default:
				wrong++
			}
		}
		fmt.Printf("%s: %d folders sampled, %d matched, %d different, %d failed, slowest open %s\n", s.name, len(picked), matched, wrong, failed, slowest.Round(time.Millisecond))
	}

	res, err := engine.Search(ctx, mail.SearchQuery{Limit: 1})
	if err == nil && len(res.Messages) == 1 {
		needle := res.Messages[0].Sender
		if i := strings.Index(needle, "<"); i >= 0 {
			needle = strings.TrimRight(needle[i+1:], ">")
		}
		start := time.Now()
		hits, err := engine.Search(ctx, mail.SearchQuery{Limit: 5, SenderContains: needle})
		if err != nil {
			return err
		}
		fmt.Printf("sender search with a real address: %d results in %s\n", len(hits.Messages), time.Since(start).Round(time.Millisecond))
	}
	return nil
}

func sameSubject(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a == b || strings.HasSuffix(a, b) || strings.HasSuffix(b, a)
}

// firstWords keeps an error short and free of any script text.
func firstWords(s string) string {
	if i := strings.LastIndex(s, "execution error:"); i >= 0 {
		s = s[i:]
	}
	if len(s) > 90 {
		s = s[:90]
	}
	return s
}

func runToolsDocs(args []string) error {
	fs := flag.NewFlagSet("apple-mail-mcp tools-docs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	readOnly := fs.Bool("read-only", false, "generate docs for read-only mode")
	writeOnly := fs.Bool("write-only", false, "generate docs for write-only mode")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Println("| Name | Params | Description |")
	fmt.Println("|---|---|---|")
	for _, d := range tools.ToolDocs(*readOnly, *writeOnly) {
		fmt.Printf("| %s | %s | %s |\n", d.Name, strings.ReplaceAll(d.Params, "|", "\\|"), strings.ReplaceAll(d.Description, "|", "\\|"))
	}
	return nil
}

func parseLogLevel(raw string) (slog.Leveler, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return nil, fmt.Errorf("invalid --log-level %q", raw)
	}
}

func parseEnvBool(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
