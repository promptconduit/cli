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
│   ├── enrich/       # Enrichment registry: slug-keyed normalized context on every event (see below)
│   ├── envelope/     # v2 event envelope types + per-tool id extraction
│   ├── extension/    # Bundled cost editor extension (.vsix go:embed) + sideload into Cursor
│   ├── git/          # Git context extraction
│   ├── outbound/     # http.RoundTripper that mirrors every outbound request to a local ndjson file (drives `promptconduit watch`)
│   ├── sync/         # Transcript + plan sync and parsing (Claude Code parser, state management)
│   ├── transcript/   # Transcript parsing and attachment extraction
│   └── updater/      # GitHub-release version check + self-replace upgrade
├── scripts/          # Install scripts
└── main.go           # Entry point
```

## Event envelope v2 + enrichments

Every hook event becomes ONE payload shape (`internal/envelope`): the line
appended to `~/.promptconduit/events.jsonl` at capture time is byte-identical
to what's POSTed to `/v1/events/raw` and stored in the platform bucket.

```jsonc
{
  "schema": 2, "event_id": "…", "session_id": "…", "prompt_id": "…",
  "tool": "claude-code", "hook_event": "PostToolUse", "captured_at": "…",
  "cli_version": "…",
  "raw_event": { /* native hook payload, untouched */ },
  "enrichments": { "env": {…}, "trace": {…}, "vcs": {…}, "prompt": {…}, "cost": {…},
                   "tools": {…}, "diff": {…}, "subagent": {…}, "turn": {…}, "permission": {…} }
}
```

Well-known slugs: `env` (host/os/os_version/arch/cwd, every event) · `trace`
(W3C correlation, every event) · `vcs` (normalized provider/repo/branch/PR,
git repos) · `prompt` (count/shape, UserPromptSubmit) · `cost` (priced
requests, Stop / cursor stop) · `tools` (normalized tool-call list,
PostToolUse/Failure/Batch) · `diff` (working-tree shortstat vs HEAD,
Stop/SessionEnd) · `subagent` (Start/Stop join with duration + per-agent
tokens/USD from the agent transcript, SubagentStart/Stop) · `turn`
(prompt→Stop wall-clock, Stop/StopFailure) · `permission`
(requested/denied + tool name, PermissionRequest/Denied). `prompt` also
carries `is_interrupt` (turn-open rule computed at the source).

**Adding an enrichment** = one new file in `internal/enrich/` implementing
`Enricher` (Slug/Applies/Enrich) plus `Register()` in its `init()`. Rules:
JSON-serializable payload; failure/panic drops the slug, never the event; keep
the hook fast — anything slow (network) must be disk-cached and refreshed by a
detached subprocess (see `vcscache.go` / `cmd/vcsrefresh.go` for the pattern).
Readers (editor extension, platform) ignore unknown slugs, so a new slug needs
no coordinated release. The v2 envelope + slug shapes are mirrored in
`platform/app/api/src/types/envelope.ts` and `editor-extension/src/envelope.ts`
— additive-only after v2.

The cost enrichment (`enrich/cost.go`) replaced the old `cost watch --json`
feed: Claude Code Stop events price the transcript lines appended since the
last Stop (per-session offset state under `~/.config/promptconduit/enrich/`),
Cursor stop/afterAgentResponse events price their own payload. The editor
extension reads it from events.jsonl.

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
6. Rides along plan files from `~/.claude/plans/*.md` via `POST /v1/plans/sync`,
   associating each plan to its session by finding the plan path inside a
   transcript (`internal/sync/plans.go`)

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

After a successful post-upgrade reconcile, the CLI writes an update marker
(`internal/extension/marker.go` → `~/.promptconduit/extension-update.json`) recording the newly
installed version. The running extension watches that file and, when it names a newer version than
the one it's running, offers a one-click **Reload Window** — a reload (not a restart) so the editor's
pty host survives and terminals running Claude Code keep their sessions. Keep the JSON field names in
sync with `editor-extension/src/updatePrompt.ts`.

## Live session graph (`promptconduit graph`)

`promptconduit graph` serves a live, "breathing" Session Graph in the browser —
one AI coding session as a tree of session → turns → subagents, updating in
place as your agent works. Free-tier and editor-independent: it reads
`~/.promptconduit/events.jsonl` locally, nothing is sent anywhere.

**Decoupled by design — the graph LOGIC lives in exactly one place.** The
rendering + state-building code is the editor-extension's *portable* TypeScript
core (`src/graphPanel/sessionTree.ts` + `webview/graphPanel/{render,mount,…}` —
zero vscode imports). Both the editor extension and this CLI consume it; neither
owns it. `internal/graph/` is a thin **tail-and-serve** shell:

- `internal/graph/server.go` — one `http.Server`: `GET /` serves the embedded
  page, `GET /api/events?after=<n>&limit=<m>` returns new raw envelope lines.
- `internal/graph/hub.go` — tails `events.jsonl` once (`outbound.Tail`) into an
  in-memory ring; the browser polls `/api/events` and builds the graph itself.
- `internal/graph/ui/graph.html` — the embedded page (`go:embed`), a
  self-contained build of the portable core. Committed artifact (like the
  `.vsix`); **not** in `dist/` (which `.gitignore` excludes).

Refresh the embedded page after the graph's TypeScript changes:
```bash
make refresh-graph                       # uses ../editor-extension by default
make refresh-graph EXTENSION_DIR=/path   # or point at a checkout
```
esbuild output is deterministic, so the **Refresh graph bundle** workflow
(`.github/workflows/refresh-graph.yml`) byte-diffs the rebuilt page and opens a
PR when it changed — no version bookkeeping needed (unlike the `.vsix` refresh).

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

ONE local event log under `~/.promptconduit/`:
- `events.jsonl` — v2 envelopes, one per line, written at capture time (before
  send) for *every* event. The stable substrate every local reader consumes.

(`events.ndjson` was removed in the v2 redesign; send outcomes live in
`status.json`/`errors.log`, raw HTTP diagnostics in `outbound.ndjson` via
`promptconduit watch`.) Gated by `PROMPTCONDUIT_EVENT_LOG` (on by default).

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
