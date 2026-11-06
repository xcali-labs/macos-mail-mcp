# Client Install Checklist

Verify each setting per target Mac; never copy ports or paths blindly from another machine.

## Before the install
- `apple-mail-mcp index-check --deep` passes on the target Mac (0 different, 0 failed).
- The 21 checks in `skill/eval-scenarios.md` pass on a test mailbox.
- Latest macOS security update installed.

## Target Mac settings
- Sleep: raise a 1-minute idle sleep on power so remote maintenance and scheduled runs are not cut off.
- Check free local ports before configuring any local model proxy (`lsof -nP -iTCP:<port> -sTCP:LISTEN`).

## Privatemode proxy (when using a sealed cloud model)
- API key: enter directly in the app, never in a chat.
- Launch at login: ON.
- Start the proxy when the app launches: ON.
- Port: default `8080` if free on the target Mac (or pick a free port and match Jan's base URL).
- HTTPS for browser clients: OFF (not needed for local Jan).
- Reasoning effort: below "max" for responsive chat.
- Upstream, Integration, Models: leave at defaults. Never point at the proxyless API.

## Jan
- Two entries of `~/.local/bin/apple-mail-mcp`, the only active tools: `mail` with `--read-only` and `mail-write` with `--write-only`, both pre-approved (`approvedServers: ["mail", "mail-write"]`) so creating a draft has no extra popup and Apple Mail's Send button is the human check. Details in [FORK.md](FORK.md).
- `toolCallTimeoutSeconds`: `300` (the tool waits up to 4 minutes for a busy Mail; Jan's default of 30 would cut that off).
- Create the "Mail" assistant from `skill/mail-assistant.md`, temperature `0.3`, selected as the default assistant in Jan.
- Model: `glm-5.3` (42/42 on the multilingual eval).
- Full Disk Access for Jan (enables the fast SQLite Envelope Index search).
- Automation of Mail for Jan: click Allow on the target screen at the first call.

## Current release build

- Tag `v0.3.1` (`make release` produces `dist/apple-mail-mcp-darwin-arm64` and `dist/SHA256SUMS` from a clean checkout).
