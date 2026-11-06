package tools

import (
	"context"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type listAccountsInput struct{}

type listAccountsOutput struct {
	Accounts []accountItem `json:"accounts"`
}

type accountItem struct {
	Name string `json:"name"`
}

type listMailboxesInput struct {
	Account string `json:"account" jsonschema:"Mail account name"`
}

type listMailboxesOutput struct {
	Account   string        `json:"account"`
	Mailboxes []mailboxItem `json:"mailboxes"`
}

type mailboxItem struct {
	Name        string `json:"name"`
	UnreadCount int    `json:"unread_count"`
}

type getUnreadCountInput struct {
	Account string `json:"account,omitempty" jsonschema:"Optional account name; if omitted, all accounts"`
}

type getUnreadCountOutput struct {
	Account   string             `json:"account,omitempty"`
	Total     int                `json:"total"`
	Mailboxes []unreadMailboxRow `json:"mailboxes"`
}

type unreadMailboxRow struct {
	Account string `json:"account"`
	Mailbox string `json:"mailbox"`
	Unread  int    `json:"unread"`
}

func registerAccountsTools(server *mcp.Server, client MailClient, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{Name: "list_accounts", Description: "List the names of the mail accounts. Use only if you need an exact account name."},
		newListAccountsHandler(client, logger))

	mcp.AddTool(server, &mcp.Tool{Name: "list_mailboxes", Description: "List the folders of one account with their unread counts. Use before move_message to get the exact folder name."},
		newListMailboxesHandler(client, logger))

	mcp.AddTool(server, &mcp.Tool{Name: "get_unread_count", Description: "Count unread emails per folder. Use when the user asks how many new emails they have."},
		newGetUnreadCountHandler(client, logger))
}

func newListAccountsHandler(client MailClient, logger *slog.Logger) func(context.Context, *mcp.CallToolRequest, listAccountsInput) (*mcp.CallToolResult, listAccountsOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ listAccountsInput) (*mcp.CallToolResult, listAccountsOutput, error) {
		defer startToolLog(logger, "list_accounts")()
		accounts, err := client.ListAccounts(ctx)
		if err != nil {
			return nil, listAccountsOutput{}, err
		}
		out := make([]accountItem, 0, len(accounts))
		for _, a := range accounts {
			out = append(out, accountItem{Name: a.Name})
		}
		return nil, listAccountsOutput{Accounts: out}, nil
	}
}

func newListMailboxesHandler(client MailClient, logger *slog.Logger) func(context.Context, *mcp.CallToolRequest, listMailboxesInput) (*mcp.CallToolResult, listMailboxesOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input listMailboxesInput) (*mcp.CallToolResult, listMailboxesOutput, error) {
		defer startToolLog(logger, "list_mailboxes")()
		mailboxes, err := client.ListMailboxes(ctx, input.Account)
		if err != nil {
			return nil, listMailboxesOutput{}, err
		}
		out := make([]mailboxItem, 0, len(mailboxes))
		for _, mb := range mailboxes {
			out = append(out, mailboxItem{Name: mb.Name, UnreadCount: mb.UnreadCount})
		}
		return nil, listMailboxesOutput{Account: input.Account, Mailboxes: out}, nil
	}
}

func newGetUnreadCountHandler(client MailClient, logger *slog.Logger) func(context.Context, *mcp.CallToolRequest, getUnreadCountInput) (*mcp.CallToolResult, getUnreadCountOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input getUnreadCountInput) (*mcp.CallToolResult, getUnreadCountOutput, error) {
		defer startToolLog(logger, "get_unread_count")()
		counts, err := client.GetUnreadCount(ctx, input.Account)
		if err != nil {
			return nil, getUnreadCountOutput{}, err
		}
		rows := make([]unreadMailboxRow, 0, len(counts.Mailboxes))
		for _, row := range counts.Mailboxes {
			rows = append(rows, unreadMailboxRow{Account: row.Account, Mailbox: row.Mailbox, Unread: row.Unread})
		}
		return nil, getUnreadCountOutput{Account: counts.Account, Total: counts.Total, Mailboxes: rows}, nil
	}
}
