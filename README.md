# macos-mail-mcp

> **Unofficial MCP server for Apple Mail on macOS.** Combines fast mailbox reading and safe, draft-only composing in a single static binary. Nothing in this codebase can send an email: every compose action saves an unsent draft in Apple Mail for the user to review and send.

[![CI](https://github.com/xcali-labs/macos-mail-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/xcali-labs/macos-mail-mcp/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/xcali-labs/macos-mail-mcp)](https://github.com/xcali-labs/macos-mail-mcp/releases)
[![License](https://img.shields.io/github/license/xcali-labs/macos-mail-mcp?cacheSeconds=300)](https://github.com/xcali-labs/macos-mail-mcp/blob/main/LICENSE)

## Why reading and drafting live in one repository and one binary

1. **Draft safety depends on read-engine context.**
   - `create_draft` queries the read-only Envelope Index to verify that every `to` and `cc` address is someone the user has already sent mail to (preventing prompt-injected exfiltration to unknown addresses).
   - `reply_draft` reads the original message's `From` and `Reply-To` headers, creates a threaded quote in `Drafts`, and warns if the reply address differs from the sender or belongs to another organisation.
   - Voice and language onboarding (`sample_sent_mail`, `get_profile`, `save_profile`, `remember`, `forget`) samples the user's sent mail through the read engine to learn how they write drafts.
2. **One atomic binary, flexible MCP host permissions.**
   - Every Mac install deploys and verifies a single binary (`apple-mail-mcp`).
   - Pass `--read-only` to register only the 7 read tools, `--write-only` to register only the 6 draft/move/profile-write tools, or neither flag to expose all 13 tools on one MCP server entry. Hosts that approve permissions per server (such as Jan) can point two server entries (`mail` and `mail-write`) at the same binary.
3. **No send path anywhere.**
   - Upstream's `send_email` and `mark_as_read` tools and the AppleScript `send` command are removed. `TestNoScriptCanSend` fails the build if a send command reappears anywhere in the repository.

## Installation

### 1. One-line install (macOS)

```sh
curl -fsSL https://raw.githubusercontent.com/xcali-labs/macos-mail-mcp/main/install.sh | sh
```

This downloads the latest signed release for your Mac architecture (or builds from source if Go is installed), verifies its SHA256 checksum, and installs `~/.local/bin/apple-mail-mcp`.

### 2. Claude Desktop / Claude Code

In `~/Library/Application Support/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "mail": {
      "command": "/Users/<you>/.local/bin/apple-mail-mcp",
      "args": ["--read-only"]
    },
    "mail-write": {
      "command": "/Users/<you>/.local/bin/apple-mail-mcp",
      "args": ["--write-only"]
    }
  }
}
```

Or with Claude Code:

```sh
claude mcp add mail "$HOME/.local/bin/apple-mail-mcp" --scope user -- --read-only
claude mcp add mail-write "$HOME/.local/bin/apple-mail-mcp" --scope user -- --write-only
```

### 3. Build from source

```sh
make test
make release   # produces dist/apple-mail-mcp-darwin-arm64 and dist/SHA256SUMS (CGO_ENABLED=0)
cp dist/apple-mail-mcp-darwin-arm64 ~/.local/bin/apple-mail-mcp
~/.local/bin/apple-mail-mcp index-check --deep
```

## Tool Reference

Generated via `go run ./cmd/apple-mail-mcp tools-docs`:

| Mode | Name | Params | Description |
|---|---|---|---|
| Read (`--read-only`) | `list_accounts` | none | List account names configured in Mail.app (`local` for On My Mac folders). |
| Read (`--read-only`) | `list_mailboxes` | `account` (string, required) | List mailboxes for an account with unread counts. |
| Read (`--read-only`) | `search_messages` | `account`, `mailbox`, `sender_contains`, `subject_contains`, `unread_only`, `date_from`, `date_to`, `limit`, `scan_limit` | Find emails across accounts and folders via the SQLite Envelope Index with automatic AppleScript fallback. |
| Read (`--read-only`) | `get_message` | `message_id` (required), `include_body` | Get message metadata and optionally full body (cut at 20,000 characters). |
| Read (`--read-only`) | `get_unread_count` | `account` (optional) | Get total and per-mailbox unread counts. |
| Read (`--read-only`) | `get_profile` | none | Read the writing profile and taught preferences. |
| Read (`--read-only`) | `sample_sent_mail` | `page` | Read a sample of the user's own words from sent mail. |
| Write (`--write-only`) | `create_draft` | `to`, `subject`, `body`, `cc`, `account` | Save a new email unsent in Drafts, only to someone already written to. |
| Write (`--write-only`) | `reply_draft` | `message_id`, `body` | Save a quoted in-thread reply unsent in Drafts; returns its recipients. |
| Write (`--write-only`) | `move_message` | `message_id`, `target_mailbox` | Move one message to another mailbox (trash, junk, sent, drafts and outbox refused). |
| Write (`--write-only`) | `save_profile` | `profile` | Save the writing profile. |
| Write (`--write-only`) | `remember` | `preference` | Save one taught preference. |
| Write (`--write-only`) | `forget` | `number` | Delete one taught preference. |

See [FORK.md](FORK.md) for architecture details and [INSTALL-CHECKLIST.md](INSTALL-CHECKLIST.md) for the client installation runbook.

## License

MIT (forked from `maximbilan/apple-mail-mcp-go` at `1a843d8`).
