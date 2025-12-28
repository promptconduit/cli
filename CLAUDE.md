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
│   ├── envelope/     # Raw event envelope types
│   ├── git/          # Git context extraction
│   ├── sync/         # Transcript sync and parsing (Claude Code parser, state management)
│   └── transcript/   # Transcript parsing and attachment extraction
├── scripts/          # Install scripts
└── main.go           # Entry point
```

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

## Key Design Decisions

- **Server-side adapters**: The CLI sends raw events; all transformation happens in platform adapters
- **Config file over env vars**: Prefer `~/.config/promptconduit/config.json` for multi-environment setups
- **Async sending**: Events are sent asynchronously to avoid blocking the AI tool

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
