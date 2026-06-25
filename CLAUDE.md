# CLAUDE.md

This file provides guidance to Claude Code when working with the CLI codebase.

## Commands

```bash
# Build
make build            # Build binary
make build-all        # Build for all platforms
make test             # Run tests
make lint             # Run linter (requires golangci-lint)

# Development
make dev              # Build and install hooks for testing
make tidy             # Run go mod tidy

# Release
make snapshot         # Create snapshot release with GoReleaser
make release          # Create release (requires GITHUB_TOKEN)
```

## Project Structure

```
cli/
├── cmd/              # CLI commands (install, uninstall, status, test, hook, config, sync)
├── internal/
│   ├── client/       # HTTP client, config loading
│   ├── correlation/  # W3C trace_id/span_id generation and per-session persistence
│   ├── envelope/     # Raw event envelope types
│   ├── extension/    # Bundled cost editor extension (.vsix go:embed) + sideload into Cursor
│   ├── git/          # Git context extraction
│   ├── outbound/     # http.RoundTripper that mirrors every outbound request to a local ndjson file (drives `promptconduit watch`)
│   ├── sync/         # Transcript sync and parsing (Claude Code parser, state management)
│   ├── transcript/   # Transcript parsing and attachment extraction
│   └── updater/      # GitHub-release version check + self-replace upgrade
├── scripts/          # Install scripts
└── main.go           # Entry point
```

## Auto-update

`promptconduit` checks GitHub Releases once every 24h on top-level commands
(skipped for `hook` to keep per-event latency tiny, and `upgrade` which
checks itself). When a newer version is found, it prints a one-line notice
and — unless `disable_auto_update` is set in config or
`PROMPTCONDUIT_AUTO_UPDATE=0` is set in the environment — spawns a detached
`promptconduit upgrade` subprocess that atomically replaces the binary. The
running process keeps its inode and finishes unaffected; the next invocation
runs the new build.

```bash
promptconduit upgrade           # explicit upgrade
promptconduit upgrade --check   # check-only
promptconduit config set --disable-auto-update=true   # opt out
```

Cache file: `~/.config/promptconduit/update.json`.

## Sync Command (Manual Only)

The `sync` command manually uploads AI assistant conversation transcripts to the platform. **There is no automatic sync** - you must run this command when you want to upload transcripts.

```bash
promptconduit sync              # Sync all supported tools
promptconduit sync claude-code  # Sync only Claude Code
promptconduit sync --dry-run    # Preview what would be synced
promptconduit sync --force      # Re-sync already synced files
promptconduit sync --since 2025-01-01  # Filter by date
promptconduit sync --limit 10   # Sync only N most recent
```

### How Sync Works

1. Discovers transcript files from `~/.claude/projects/**/*.jsonl`
2. Parses each file to extract conversation metadata and messages
3. Calculates SHA256 hash to detect changes
4. Uploads to platform via `POST /v1/transcripts/sync`
5. Tracks synced files in `~/.config/promptconduit/sync_state.json`

### Hooks vs Sync

- **Hooks** capture events in **real-time** during AI tool usage (no manual action needed)
- **Sync** uploads **historical transcripts** (must be run manually)

## Bundled cost editor extension

`internal/extension/` embeds the PromptConduit cost extension
(`embedded/promptconduit-cost.vsix`, built from the separate
`promptconduit/editor-extension` repo) into the CLI binary via `go:embed`.
`promptconduit install cursor` sideloads it with `cursor --install-extension`
(best-effort; `--no-extension` to skip). No marketplace/tokens — the extension version is
locked to the CLI's cost-feed schema, which is why it's distributed this way.

Refresh the bundled `.vsix` after the extension changes:
```bash
make refresh-extension                       # uses ../editor-extension by default
make refresh-extension EXTENSION_DIR=/path   # or point at a checkout
```
The `.vsix` lives in `internal/extension/embedded/` (deliberately NOT a `dist/` dir, which the
repo `.gitignore` excludes) and is a committed build artifact that `go:embed` requires.

Instead of refreshing locally, the **Refresh bundled extension** workflow
(`.github/workflows/refresh-extension.yml`) rebuilds the `.vsix` from `editor-extension` main and
opens a PR when the extension **version** changed — run it from the Actions tab
(`workflow_dispatch`), weekly via schedule, or via a `repository_dispatch` from the extension repo.
Detection is version-based (not a byte diff) because `vsce package` output isn't deterministic, so
**bump the extension's `package.json` version whenever you change it**. (Opening the PR needs the
repo's "Allow GitHub Actions to create and approve pull requests" setting.)

## Key Design Decisions

- **Server-side adapters**: The CLI sends raw events; all transformation happens in platform adapters
- **Config file over env vars**: Prefer `~/.config/promptconduit/config.json` for multi-environment setups
- **Async sending**: Events are sent asynchronously to avoid blocking the AI tool
- **Capture is decoupled from sending**: `processHookEvent` always builds the
  envelope and writes it to the local capture log (`eventlog.RecordCapture` →
  `~/.promptconduit/events.jsonl`) *before* deciding whether to send. The single
  send gate is `cfg.ShouldSend()` (`internal/client/config.go`) = API key set
  AND not local-only. This is what makes the Free tier work.

## Free / local-only tier

The **Free tier** is enforced entirely client-side: when `cfg.ShouldSend()` is
false — i.e. no API key, or `local_only` is set (config `local_only`, flag
`--local-only`, or `PROMPTCONDUIT_LOCAL_ONLY=1`) — events are captured to
`~/.promptconduit/events.jsonl` and **never POSTed**. No platform change is
required. A missing API key is a normal Free state, not an error (the old
`not_configured` drop/error path was removed).

Two distinct local logs under `~/.promptconduit/`:
- `events.jsonl` — raw event envelopes, one per line, written at capture time
  (before send) for *every* event. The stable substrate external tools read.
- `events.ndjson` — send-outcome diagnostics (status/latency), written only when
  an event is actually sent. Surfaced by `promptconduit status` / `events`.

Both are gated by the same `PROMPTCONDUIT_EVENT_LOG` knob (on by default).

## Branch Naming

- `feat/description` - New features
- `fix/description` - Bug fixes
- `chore/description` - Maintenance
- `refactor/description` - Code restructuring
- `docs/description` - Documentation

## Git Workflow

**IMPORTANT: Always use feature branches for code changes. Never commit directly to main.**

```bash
git checkout -b feat/my-feature
# make changes
git add . && git commit -m "feat: description"
git push -u origin feat/my-feature
gh pr create
```

## Testing

```bash
# Run all tests
make test

# Test with local API
PROMPTCONDUIT_API_URL=http://localhost:8787 ./promptconduit test
```
