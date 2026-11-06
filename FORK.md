# Architecture and Fork Notes

This is a draft-only fork of `maximbilan/apple-mail-mcp-go` at upstream commit `1a843d8` (MIT), built for macOS Apple Mail and an MCP client (such as Jan or Claude Desktop).

## Why reading and drafting share one binary

Reading and drafting are built into one repository and one static binary so that:
- `create_draft` and `reply_draft` share the Envelope Index and AppleScript layer used by `search_messages` and `get_message` (verifying known contacts from Sent folders and checking `Reply-To` headers before a draft is saved).
- Onboarding (`sample_writing_style`, `get_profile`, `save_profile`) reads sent mail and stores the user's language and tone profile in the same process.
- One binary on disk can run as a combined MCP server, or as two split entries (`--read-only` and `--write-only`) when the host manages permissions per server.

## Key changes from upstream

- **No send, anywhere.** The `send_email` tool, its client method and the AppleScript `send` command are deleted. `TestNoScriptCanSend` fails the build if a send command or a send path reappears in any script or source file.
- **New `reply_draft`.** Mail's own `reply` (threaded, Re: subject, reply headers), the new text above a `> ` quoted copy of the original, saved unsent in Drafts.
- **New `move_message`.** One message to another mailbox of the same account, exact name only. Trash, junk, deleted-items, sent, drafts and outbox names (EN, FR, SK, DE, PT) are refused before Mail is touched.
- **Writing profile and onboarding (`get_profile`, `save_profile`, `sample_writing_style`).** Samples sent messages so the assistant learns the user's languages, greetings, sign-offs and preferences without hardcoding any persona in the binary or skill.
- **Dates fixed.** Upstream coerced seconds-since-1970 to an AppleScript integer, which stops at 536870911 (1987): every date came back blank and every date filter returned nothing. Scripts now pass dates as calendar parts (`stamp()` / `mkDate()`).
- **Search rewritten for large mailboxes.** Upstream used Mail's `whose` queries, which build the full match list first: an unread or common-word search on a large inbox hung past 150 s. Search now reads pages of the newest messages (60, then 250), fetches only the property a filter needs, reads the rest for matches only, and stops at the result limit. `scan_limit` (default 5000, max 50000) reaches older mail. Every row is re-checked against the filters, so new mail arriving mid-search cannot return a wrong match.
- **Two search engines, chosen by the tool.** `search_messages` uses Mail's Envelope Index, read-only, when it can (engine `index`: every folder and account in milliseconds, accent-insensitive, Gmail duplicates merged, Trash and Junk skipped unless named). If the index cannot be read (no Full Disk Access, unknown layout), it falls back to Mail scripting (engine `mail`: inbox only by default, stops after 45 s) and adds a note. `--search-engine auto|index|mail` forces one; the model never chooses. `apple-mail-mcp index-check` prints the index's shape and timings, never content.
- **On My Mac folders** are reachable under the account name `local`.
- **`mark_as_read` removed.** It came from upstream, the skill never used it, and it could hide mail.
- **Replies report where they go.** `reply_draft` never replies to all, and returns the draft's real recipients. On macOS Mail, a scripted `reply` goes to the sender and ignores `Reply-To`. So a forged `Reply-To` cannot redirect a draft, while a cross-organisation `Reply-To` (a mailing list, an event host) is reported with both addresses so the user can adjust the `To` field in Mail if needed. If the draft recipient ever differs from the original sender by exact address, a warning is returned.
- **Drafts go only to known people.** No Bcc. `to` and `cc` must be the user's own addresses, in `~/Library/Application Support/apple-mail-mcp/allowed-recipients.txt`, or recipients of mail in a sent folder. Mail's address table alone will not do: a stranger's address enters it when their email arrives. With no index and no file, only the user's own addresses pass.
- **Audit log.** Every draft and move (including timed-out `*_unsure` attempts) is recorded in `~/Library/Logs/apple-mail-mcp/actions.log` (mode 600): action, time, folders, recipients, never message text.
- **No script text in logs.** Failures log the exit code and stderr only.
- **Message bodies are cut at 20,000 characters** with a note, so a 40,000-character host limit cannot lose the note.
- **Direct message lookup.** Upstream's `whose id is` scans the mailbox. Mail's by-id reference (`«class mssg» id N`, because `message id` parses as a property) opens messages in under a second.
- **Waits for a busy Mail.** Mail stops answering scripts for a minute or more while it syncs. Every call first waits, up to 4 minutes, until Mail answers a tiny probe. Reads keep retrying inside that time. A write runs once: if it times out mid-way it may have happened, so the tool returns `mail_unsure` and the skill checks before repeating. `mail_busy` means nothing changed and the skill calls again by itself. Set the host's tool call timeout (`toolCallTimeoutSeconds` in Jan) to `300`.
- **`/usr/bin/osascript` by absolute path**, so a minimal PATH from the client cannot substitute another binary.
- **Installer removed.** Upstream's installer shelled out to `claude` and `jq`; it is not needed.
- `TestRegisteredToolsAreExactlyTheAllowedSet` pins the tool list a real MCP client sees.

## Jan setup: two entries, one binary

Jan approves per server, so reads and writes run as two entries of this binary:

- `mail` with `--read-only`: pre-approved, no prompts.
- `mail-write` with `--write-only`: pre-approved alongside `mail` (`approvedServers: ["mail", "mail-write"]`) so drafts are created in Apple Mail without extra confirmation popups (the binary has no send path, so clicking Send in Apple Mail is the human gate), or left unapproved (`approvedServers: ["mail"]`) if per-call Jan prompts are preferred.

## Skill

`skill/mail-assistant.md` is the instruction sheet for the assistant, written for mid-size models (Qwen, GLM): short numbered steps, exact tool names, worked examples. `skill/eval-scenarios.md` is the 21-scenario pass list to run before a model serves a live mailbox.

## Network

This codebase opens no network connection. The MCP Go SDK links Go's HTTP packages for its HTTP transports; this binary runs the stdio transport only. Verify on the target with `lsof -a -p <pid> -i` while it runs (expected: nothing).

## Build

```sh
make test
make release   # dist/apple-mail-mcp-darwin-arm64 + dist/SHA256SUMS, CGO off, static
```

## Permissions on the target Mac

Automation of Mail, granted to the client app that launches the binary (for example Jan). Full Disk Access for that app turns on the fast index search; without it, search falls back to Mail scripting.
