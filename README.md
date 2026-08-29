# PromptConduit CLI

Every great workflow you build in Claude Code disappears when the session ends. PromptConduit captures your AI coding sessions and turns repeated patterns into reusable slash commands — automatically, and without a platform account.

[![CI](https://github.com/promptconduit/cli/actions/workflows/test.yml/badge.svg)](https://github.com/promptconduit/cli/actions/workflows/test.yml)
[![GitHub Stars](https://img.shields.io/github/stars/promptconduit/cli?style=social)](https://github.com/promptconduit/cli/stargazers)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go](https://img.shields.io/badge/go-1.21+-blue.svg)](https://go.dev/dl/)

## No Account? Start Here

Turn your existing AI coding transcripts into reusable Claude Code skills — **no sign-up required**.

```bash
# Install
curl -fsSL https://promptconduit.dev/install | bash

# Generate skills from your transcripts (uses your Claude Code subscription)
promptconduit skills generate --local
```

Skills are written directly to `~/.claude/skills/` and show up in Claude Code's `/` autocomplete immediately. Reads from Claude Code, OpenAI Codex CLI, and GitHub Copilot CLI — whichever you have installed.

[Jump to full local skills docs →](#local-skill-generation)

---

## Overview

PromptConduit CLI captures prompts, tool executions, and session events from AI coding assistants and forwards them to the PromptConduit platform for analysis and insights.

### Supported Tools

**Real-time hooks** (requires [platform account](https://promptconduit.dev)):

| Tool | Events Captured |
|------|-----------------|
| [Claude Code](https://claude.ai/code) | Prompts, Tools, Sessions, Attachments |
| [Cursor](https://cursor.com) | Prompts, Shell, MCP, Files, Attachments |
| [Gemini CLI](https://geminicli.com) | Prompts, Tools, Sessions |
| [OpenAI Codex CLI](https://github.com/openai/codex) | Prompts, Tools, Permissions, Sessions |
| [GitHub Copilot CLI](https://github.com/github/copilot-cli) | Prompts, Tools, Subagents, Errors, Sessions |
| [Grok Build](https://docs.x.ai/build/features/hooks) | Prompts, Tools, Sessions, Subagents |

**Local skill generation** (no account required):

| Tool | Transcript Location |
|------|---------------------|
| Claude Code | `~/.claude/projects/**/*.jsonl` |
| OpenAI Codex CLI | `~/.codex/**/*.jsonl` |
| GitHub Copilot CLI | `~/.copilot/session-state/**/*.jsonl` |

### Attachment Support

The CLI automatically extracts and uploads attachments from prompts:

- **Images**: JPEG, PNG, GIF, WebP, SVG, BMP, TIFF, HEIC
- **Documents**: PDF, Word (.doc/.docx), Excel (.xls/.xlsx), PowerPoint (.ppt/.pptx)
- **Text**: Plain text, CSV, HTML, Markdown, JSON, XML

## Installation

### Quick Install

```bash
curl -fsSL https://promptconduit.dev/install | bash
```

Or with your API key pre-configured:

```bash
curl -fsSL https://promptconduit.dev/install | bash -s -- YOUR_API_KEY
```

### From Source

```bash
git clone https://github.com/promptconduit/cli.git
cd cli
make build
make install
```

### Download Binary

Download the latest release for your platform from the [releases page](https://github.com/promptconduit/cli/releases).

### Staying up to date

Once installed, `promptconduit` checks GitHub releases at most once every 24 hours
in the background. When a newer version is published it prints a one-line notice
to stderr and applies the upgrade in the background (atomic self-replace; the
running process is unaffected). The next invocation runs the new version.

```bash
promptconduit upgrade         # upgrade now (interactive)
promptconduit upgrade --check # only check; don't download

# Opt out of background upgrades:
promptconduit config set --disable-auto-update=true
# Or via environment:
PROMPTCONDUIT_AUTO_UPDATE=0 promptconduit ...
```

The check uses the unauthenticated GitHub API (60 requests/hour/IP) and is
cached at `~/.config/promptconduit/update.json`.

## Platform Quick Start

> **No account?** See [No Account? Start Here](#no-account-start-here) above.

![PromptConduit dashboard — sessions, skills, and recent activity](docs/screenshot-dashboard.png)

### 1. Set up

```bash
promptconduit init
```

The interactive wizard signs you in through your browser, detects your installed
AI tools, and installs their hooks — the whole setup in one command. Sign up at
[promptconduit.dev](https://promptconduit.dev) first if you don't have an account.

### 2. Verify

```bash
promptconduit status
```

<details>
<summary>Prefer to set it up manually?</summary>

```bash
promptconduit login                  # browser sign-in; saves your key to config
promptconduit install claude-code    # or: cursor | gemini-cli | codex | copilot | grok
promptconduit status
```

For CI or scripted setups, set the key directly instead of the browser flow:

```bash
promptconduit config set --api-key="your-api-key"   # ~/.config/promptconduit/config.json
```

</details>

## Commands

```bash
# Install hooks for a tool
promptconduit install <tool>

# Uninstall hooks from a tool
promptconduit uninstall <tool>

# Show installation status
promptconduit status

# Test API connectivity
promptconduit test

# Generate skills locally (no account needed)
promptconduit skills generate --local [flags]

# Generate skills via platform (requires account)
promptconduit skills generate [flags]

# Sync transcripts (manual)
promptconduit sync [tool] [flags]

# Show version
promptconduit version

# Upgrade to the latest release
promptconduit upgrade

# Tail outbound HTTP traffic in real time (great for debugging hooks)
promptconduit watch
promptconduit watch --verbose    # also pretty-print request/response bodies
promptconduit watch --lines 20   # backfill the last 20 entries before going live
```

### Tail outbound traffic

`promptconduit watch` streams every HTTP request the CLI makes to the
platform — hook envelope sends, transcript syncs, insights queries,
skills traffic — to your terminal as it happens. Useful for answering
"is my hook actually sending anything when Claude Code fires?" without
spelunking the debug log.

Each request appears as a one-line summary by default:

```
15:30:42  POST  /v1/events/raw  3.2KB  → 200 (87ms)
```

Add `--verbose` to also see the pretty-printed JSON body underneath
each summary.

Implementation note: every request the CLI makes is mirrored to
`~/.config/promptconduit/outbound.ndjson` (mode `0600`). Authorization,
Cookie, and similar credential headers are redacted before write;
bodies are capped at 64KB per row and the file rotates to
`outbound.ndjson.1` when it crosses 50MB. Update-check traffic from
`internal/updater` uses a separate HTTP client and is **not** mirrored
— that traffic is predictable and noisy and isn't what `watch` is for.

### Sync Command

The `sync` command uploads historical conversation transcripts to the platform. **This is a manual process** - there is no automatic syncing of transcripts.

```bash
# Sync all supported tools
promptconduit sync

# Sync only Claude Code transcripts
promptconduit sync claude-code

# Preview what would be synced (no uploads)
promptconduit sync --dry-run

# Force re-sync already synced files
promptconduit sync --force

# Only sync transcripts newer than a date
promptconduit sync --since 2025-01-01

# Limit to N most recent transcripts
promptconduit sync --limit 10
```

**How it works:**
- Discovers JSONL transcript files from `~/.claude/projects/`
- Extracts conversation metadata, messages, and git context
- Tracks synced files in `~/.config/promptconduit/sync_state.json` to avoid duplicates
- Use `--dry-run` first to preview, then run without flags to sync

**Hooks vs Sync:**
- **Hooks** capture events in real-time during AI tool usage (automatic after installation)
- **Sync** uploads historical transcripts (must be run manually when needed)

### Local Skill Generation

Extract reusable skills from your AI coding transcripts — **no platform account required**. Uses your existing Claude Code subscription (or an API key) to analyze patterns and write skill files directly to `~/.claude/skills/`.

```bash
# Analyze all tools, write skills globally
promptconduit skills generate --local

# Analyze only Claude Code transcripts
promptconduit skills generate --local --tool claude-code

# Scope skills to current git repo
promptconduit skills generate --local --repo owner/repo

# Re-analyze previously seen transcripts
promptconduit skills generate --local --force

# Preview what would be generated
promptconduit skills generate --local --dry-run
```

**AI provider auto-detection** (in priority order):
1. `claude` CLI binary in PATH — uses your existing [Claude Code](https://claude.ai/code) / claude.ai Pro subscription
2. `ANTHROPIC_API_KEY` env var — direct Anthropic API access
3. `OPENAI_API_KEY` env var — OpenAI fallback (gpt-4o-mini)

**How it works:**
- Reads transcripts from Claude Code, OpenAI Codex CLI, and GitHub Copilot (whichever are installed)
- Requires ≥ 5 new conversations to detect patterns (use `--force` to override)
- Writes `SKILL.md` files with YAML frontmatter to `~/.claude/skills/<name>/`
- Tracks analyzed transcripts by SHA256 hash in `~/.config/promptconduit/local_skills_state.json`
- Skills appear immediately in Claude Code's `/` autocomplete

**Output example:**
```
Analyzing 47 local transcripts (global)...
Using Claude Code (claude CLI).

Detected 3 skills:

  /ci-monitor  (workflow, 90%)
    CI Monitor
    Check CI status and fix failures until green.
    → Written to: /Users/you/.claude/skills/ci-monitor/SKILL.md

3 skills written. Use them with /ci-monitor, etc.
```

## Free Tier — Local-Only Mode

PromptConduit has a **Free tier that is 100% local**: every event is captured to
a file on your machine and **nothing is ever sent to the cloud**. You register
for free, but no telemetry leaves your computer.

Local-only mode is the default whenever no API key is configured — installing
hooks and capturing events works with no account at all. You can also opt in
explicitly (useful if you have a key but want to stay local):

```bash
# Capture locally, never send — even if an API key is set
promptconduit config set --local-only=true

# Or enable it at install time
promptconduit install claude-code --local-only

# Or per-invocation via environment variable
PROMPTCONDUIT_LOCAL_ONLY=1 ...
```

`promptconduit status` shows whether you're in **Free (local-only)** or
**Cloud sync** mode. To enable cloud sync later, set an API key and turn
local-only off (`promptconduit config set --local-only=false`).

### The local event capture log (`events.jsonl`)

Every captured event is written — **before any network send** — to:

```
~/.promptconduit/events.jsonl
```

This file is the durable, send-independent record of your events. It is written
in **both** Free and Cloud modes (in Cloud mode the same envelope is then also
sent to the platform). Each line is exactly one event envelope — the same JSON
the CLI would POST to `/v1/events/raw`:

```json
{"envelope_version":"1.2","tool":"claude-code","hook_event":"UserPromptSubmit","captured_at":"2026-06-23T21:11:57Z","native_payload":{ ... },"enrichment":{ ... }}
```

This format is a stable contract for local tooling (e.g. an editor extension
that reads `events.jsonl` to show a live cost estimator). Secrets matching
well-known patterns (API keys, bearer tokens) are redacted before write; the
file rotates to `events.jsonl.1` at 50MB. Disable all local logging — including
this file — with `PROMPTCONDUIT_EVENT_LOG=0` or
`promptconduit config set --disable-event-log=true`.

> Note: `events.jsonl` (raw events, captured before send) is distinct from
> `events.ndjson` (the send-outcome diagnostics log, written only when an event
> is actually sent).

## Cost status-bar extension (Cursor)

The CLI **bundles** the PromptConduit cost editor extension and sideloads it into
Cursor automatically when you install hooks:

```bash
promptconduit install cursor   # wires hooks AND installs the cost status-bar extension
```

The extension is embedded in the CLI binary (no marketplace, no separate
download) and installed via Cursor's `cursor --install-extension` CLI, so its
version is always locked to the CLI's cost-feed schema. It reads the same local
cost feed `promptconduit cost` produces and shows live request/session cost — and
the cost-reduction signals — right in the status bar.

- **Skip it:** `promptconduit install cursor --no-extension`
- **Cursor CLI not found?** The install step is skipped with a hint — open Cursor
  and run *“Shell Command: Install 'cursor' command in PATH”*, then re-run
  `promptconduit install cursor`. The hooks are installed either way.
- The extension source lives in
  [promptconduit/editor-extension](https://github.com/promptconduit/editor-extension);
  the bundled `.vsix` is refreshed with `make refresh-extension`.

## Configuration

The CLI supports multiple configuration methods with the following priority:

1. **Environment variables** (highest priority)
2. **Config file** (`~/.config/promptconduit/config.json`)
3. **Defaults** (lowest priority)

### Config File (Recommended)

The config file supports multiple environments, making it easy to switch between local development, staging, and production:

```bash
# Show current configuration
promptconduit config show

# Set API key for current environment
promptconduit config set --api-key=sk_your_key_here

# Set API URL (for local development)
promptconduit config set --api-url=http://localhost:8787

# Enable debug mode
promptconduit config set --debug=true

# Show config file path
promptconduit config path
```

### Multi-Environment Setup

Create environments for local, dev, and prod:

```bash
# Create local environment
promptconduit config env add local \
  --api-key=sk_local_key \
  --api-url=http://localhost:8787 \
  --debug

# Create dev environment
promptconduit config env add dev \
  --api-key=sk_dev_key \
  --api-url=https://dev-api.promptconduit.dev

# Create prod environment
promptconduit config env add prod \
  --api-key=sk_prod_key \
  --api-url=https://api.promptconduit.dev
```

### Switching Environments

```bash
# Switch environments
promptconduit config env use local
promptconduit config env use prod

# Shorthand alias
promptconduit config set-env local

# List all environments
promptconduit config env list

# Show current environment
promptconduit config show
```

**Important**: After switching environments, start a **new Claude Code session** for the hooks to pick up the new configuration. The `--continue` flag preserves the old environment.

### Environment Variables

Environment variables override config file settings:

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `PROMPTCONDUIT_API_KEY` | Yes | - | Your API key |
| `PROMPTCONDUIT_API_URL` | No | `https://api.promptconduit.dev` | API endpoint |
| `PROMPTCONDUIT_DEBUG` | No | `false` | Enable debug logging |
| `PROMPTCONDUIT_TIMEOUT` | No | `30` | HTTP timeout in seconds |
| `PROMPTCONDUIT_TOOL` | No | Auto-detect | Force specific adapter |

> **Warning**: If using multi-environment config, avoid setting `PROMPTCONDUIT_API_KEY` in your shell profile. The env var will override the config file's environment-specific key, which can cause mismatches (e.g., prod API key with local URL). Use the config file exclusively or env vars exclusively, but not both.

### Debug Mode

Enable debug mode to log hook activity:

```bash
# Via config
promptconduit config set --debug=true

# Via environment variable
export PROMPTCONDUIT_DEBUG=1
```

Debug logs are written to `$TMPDIR/promptconduit-hook.log` (on macOS this is typically `/var/folders/.../promptconduit-hook.log`).

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     AI Coding Tools                          │
├─────────────┬─────────────────────┬─────────────────────────┤
│ Claude Code │       Cursor        │      Gemini CLI         │
└──────┬──────┴──────────┬──────────┴───────────┬─────────────┘
       │                 │                      │
       └─────────────────┼──────────────────────┘
                         ▼
              ┌─────────────────────┐
              │   promptconduit     │  ← Hook receives raw JSON
              │      hook           │    from stdin
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │   Raw Envelope      │  ← Wraps event with
              │   + Git Context     │    metadata & repo state
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │    Async Send       │  ← Non-blocking POST to
              │  (subprocess)       │    /v1/events/raw
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │  PromptConduit API  │  ← Server-side adapters
              │  (normalization)    │    transform to canonical
              └─────────────────────┘
```

### Key Design Principles

- **Thin client**: The CLI wraps raw events in an envelope — all normalization happens server-side
- **Never blocks tools**: Hook always returns immediately with success
- **Async sending**: Events are sent in a detached subprocess
- **Rich context**: Captures git state (branch, commit, dirty files, etc.)
- **Attachment extraction**: Parses transcripts to extract images and documents
- **Multipart uploads**: Attachments sent efficiently via multipart form data
- **Graceful degradation**: Unknown events are silently skipped

## Wire format

The CLI is intentionally thin: it forwards each AI tool's raw hook event
untouched in `native_payload`, plus an `enrichment` block of locally
computed context (git, host, correlation IDs). The platform's server-side
adapters do the categorization and normalization. That keeps the CLI
small and lets the platform evolve event taxonomy without forcing CLI
upgrades.

The envelope is currently at version `1.2`:

```json
{
  "envelope_version": "1.2",
  "cli_version": "v0.5.0",
  "tool": "claude-code",
  "hook_event": "UserPromptSubmit",
  "captured_at": "2026-05-16T13:04:11Z",
  "native_payload": {
    "session_id": "abc123",
    "hook_event_name": "UserPromptSubmit",
    "prompt": "Refactor the auth module",
    "cwd": "/Users/me/my-project"
  },
  "enrichment": {
    "git": {
      "repo_name": "my-project",
      "branch": "main",
      "commit_hash": "abc123",
      "is_dirty": true,
      "remote_url": "git@github.com:me/my-project.git"
    },
    "source": "github",
    "correlation": {
      "trace_id": "151a9441671718243f44d5b0fa183894",
      "span_id": "143855046e61e7f8"
    },
    "host": "my-laptop",
    "os": "darwin",
    "arch": "arm64"
  }
}
```

Field reference:

| Field | Description |
| --- | --- |
| `envelope_version` | Schema version. Currently `1.2`. |
| `cli_version` | Version of `promptconduit` that produced the event. |
| `tool` | `claude-code`, `cursor`, `gemini-cli`, `codex`, `copilot`, `grok`, or `unknown`. |
| `hook_event` | The tool's native hook event name (e.g. `UserPromptSubmit`, `preToolUse`). Not canonicalized — that happens server-side. |
| `captured_at` | ISO 8601 timestamp at the moment the hook fired. |
| `native_payload` | Raw JSON the tool wrote to the hook's stdin. Pass-through. |
| `enrichment.git` | Repo metadata derived from `cwd` (branch, dirty state, commit hash, remote). Omitted when not in a git repo. |
| `enrichment.source` | Git provider derived from the remote URL (`github`, `gitlab`, `bitbucket`, `azure`). |
| `enrichment.correlation` | W3C Trace Context-compatible IDs so events stitch into a single trace. `trace_id` is stable per session; `span_id` is unique per event. |
| `enrichment.host`, `os`, `arch` | Machine identity, runtime GOOS, runtime GOARCH. |
| `attachments` | When present: array of `{attachment_id, filename, content_type, size_bytes, type}`. Binary data is sent as a separate multipart field. |

Image and document bytes ride alongside the envelope as multipart parts
on the same `/v1/events/raw` request — never embedded as base64 in the
JSON.

### Headless Mode (claude -p)

PromptConduit hooks work in Claude Code's headless/print mode (`claude -p "..."`) without any changes. Hooks fire and capture events identically in both interactive and non-interactive sessions.

**`--bare` mode limitation:** When running `claude --bare -p "..."`, Claude Code skips auto-discovery of hooks and settings, so PromptConduit hooks will not fire. To capture events in bare mode, explicitly pass your settings file:

```bash
claude --bare -p "your prompt" --settings ~/.claude/settings.json
```

Note: `--bare` is expected to become the default for `-p` in a future Claude Code release. The `--settings` workaround ensures capture until that changes.

## Development

### Prerequisites

- Go 1.21+
- Git

### Building

```bash
# Build binary
make build

# Run tests
make test

# Build for all platforms
make build-all

# Create snapshot release
make snapshot
```

### Project Structure

```
.
├── cmd/                    # CLI commands
│   ├── root.go             # Root command + background update-check wiring
│   ├── install.go          # Install hooks for AI tools (5 tools)
│   ├── uninstall.go        # Remove hooks
│   ├── status.go           # Show installation status
│   ├── test.go             # Test API connectivity
│   ├── hook.go             # Hook entry point (receives raw events)
│   ├── config.go           # Config management
│   ├── sync.go             # Manual transcript sync
│   ├── upgrade.go          # `promptconduit upgrade` (self-replace)
│   ├── watch.go            # `promptconduit watch` (tail outbound traffic)
│   ├── insights.go         # Personal usage insights
│   ├── skills.go           # Skill detection (platform)
│   ├── skills_local.go     # Skill detection (local, no account)
│   ├── correlation.go      # W3C trace_id/span_id helpers
│   └── debug.go            # Internal debugging commands
├── internal/
│   ├── envelope/           # Raw event envelope types (v1.2)
│   ├── client/             # HTTP client with multipart upload
│   ├── correlation/        # W3C trace_id/span_id generation + persistence
│   ├── git/                # Git context extraction
│   ├── outbound/           # http.RoundTripper that mirrors outbound traffic
│   │                       # to outbound.ndjson (drives `watch`)
│   ├── sync/               # Transcript sync and parsing
│   │   ├── claudecode.go   # Claude Code transcript parser
│   │   ├── state.go        # Sync state management
│   │   └── types.go        # Parser interface and types
│   ├── transcript/         # Transcript parsing & attachment extraction
│   └── updater/            # GitHub-release version check + atomic self-replace
├── scripts/
│   └── install.sh          # Curl installer
├── .goreleaser.yaml        # Release configuration
├── Makefile                # Build commands
└── go.mod                  # Go module
```

## Privacy & Security

- **Open Source**: Full transparency on what data is captured
- **Minimal**: Only captures events needed for analysis
- **Secure**: HTTPS with API key authentication
- **Non-blocking**: Never interferes with your workflow
- **Free / local-only tier**: With no API key (or `--local-only`), events are
  captured to `~/.promptconduit/events.jsonl` and **nothing is sent to the
  cloud**. See [Free Tier — Local-Only Mode](#free-tier--local-only-mode).
- **Local event capture log**: Every event is written to
  `~/.promptconduit/events.jsonl` (mode `0644`) before any send, with secrets
  redacted; rotates at 50MB. Disable with `PROMPTCONDUIT_EVENT_LOG=0`.
- **Local outbound mirror**: Every HTTP request the CLI makes is also
  written to `~/.config/promptconduit/outbound.ndjson` (mode `0600`,
  owner-only) so you can tail it with `promptconduit watch`.
  Authorization, Cookie, and any header matching `token` / `secret` /
  `key` is redacted to `***` before write. Bodies are capped at 64KB
  per row; the file rotates to `outbound.ndjson.1` at 50MB.
- **Background self-upgrade**: A once-per-24h GitHub releases check
  fetches `/repos/promptconduit/cli/releases/latest` (no auth, no
  payload uploaded). Disable with
  `promptconduit config set --disable-auto-update=true` or
  `PROMPTCONDUIT_AUTO_UPDATE=0`.

## License

[MIT](LICENSE) - Copyright (c) 2026 PromptConduit

## Links

- [PromptConduit Website](https://promptconduit.dev)
- [Documentation](https://promptconduit.dev/docs)
- [Issue Tracker](https://github.com/promptconduit/cli/issues)
