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
├── cmd/              # CLI commands (install, uninstall, status, test, hook, config)
├── internal/
│   ├── client/       # HTTP client, config loading
│   ├── envelope/     # Raw event envelope types
│   ├── git/          # Git context extraction
│   └── transcript/   # Transcript parsing and attachment extraction
├── scripts/          # Install scripts
└── main.go           # Entry point
```

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
