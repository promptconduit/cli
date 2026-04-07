# Contributing to PromptConduit CLI

Thanks for your interest in contributing. This document covers how to get set up, what kinds of contributions are most valuable, and how to get your PR merged.

## Development Setup

**Prerequisites:** Go 1.21+, Git

```bash
git clone https://github.com/promptconduit/cli.git
cd cli
make build        # compile binary
make test         # run tests
make lint         # run golangci-lint (install: brew install golangci-lint)
```

To test hooks end-to-end against a running Claude Code session:

```bash
make dev          # builds and installs claude-code hooks pointing to your local binary
promptconduit status
PROMPTCONDUIT_DEBUG=1 promptconduit test
```

Debug logs land in `$TMPDIR/promptconduit-hook.log`.

## What We're Looking For

### Adding a New AI Tool (most valuable)

Supporting a new AI coding assistant is the highest-impact contribution. The CLI side requires four changes — a Claude Code skill walks through them step by step:

```
# In Claude Code, from this repo:
/add-tool <tool-name>
```

Or follow the steps manually:
1. Register the tool in `internal/envelope/envelope.go`
2. Add install logic in `cmd/install.go`
3. Add uninstall logic in `cmd/uninstall.go`
4. Add a sample config in `configs/<tool-name>/`

**Important:** Adding a tool also requires a server-side adapter in the platform repo (private). Open an issue first so we can coordinate — include the tool's hook event names and sample raw payloads.

### Bug Fixes

Check existing issues before opening a new one. Include the output of:
```bash
promptconduit --version
PROMPTCONDUIT_DEBUG=1 promptconduit status
```

### Sync Parsers

Each AI tool's transcript format is different. Parsers live in `internal/sync/`. Adding a parser for a new tool (e.g. a new CLI that writes JSONL transcripts) enables `promptconduit sync` to upload historical data.

## Pull Request Guidelines

1. **Branch naming:** `feat/description`, `fix/description`, `chore/description`
2. **Never commit directly to main** — always use a feature branch
3. **Tests:** `make test` must pass. Add tests for new parsers and envelope logic.
4. **Lint:** `make lint` must pass.
5. **Squash merge:** PRs are squash-merged — keep your commit history clean or we'll squash it.

## Open Source Guardrails

This repo is public. Please ensure your contributions:

- Do **not** reference platform internals, Cloudflare resource IDs, or private API endpoints
- Do **not** include API keys, tokens, or credentials of any kind
- Communicate with the platform only via the public `/v1/events/raw` endpoint
- Keep commit messages and PR descriptions free of proprietary business logic

When in doubt, open an issue and ask before implementing.

## License

By contributing, you agree your contributions will be licensed under the [MIT License](../LICENSE).
