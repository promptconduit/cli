# PromptConduit CLI

Capture prompts and events from AI coding assistants.

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Go](https://img.shields.io/badge/go-1.21+-blue.svg)](https://go.dev/dl/)

## Overview

PromptConduit CLI captures prompts, tool executions, and session events from various AI coding assistants. All events are normalized to a canonical schema and sent to the PromptConduit API for analysis.

### Supported Tools

| Tool | Events Captured |
|------|-----------------|
| [Claude Code](https://claude.ai/code) | Prompts, Tools, Sessions, Attachments |
| [Cursor](https://cursor.com) | Prompts, Shell, MCP, Files, Attachments |
| [Gemini CLI](https://geminicli.com) | Prompts, Tools, Sessions |

### Attachment Support

The CLI automatically extracts and uploads attachments from prompts:

- **Images**: JPEG, PNG, GIF, WebP, SVG, BMP, TIFF, HEIC
- **Documents**: PDF, Word (.doc/.docx), Excel (.xls/.xlsx), PowerPoint (.ppt/.pptx)
- **Text**: Plain text, CSV, HTML, Markdown, JSON, XML

Attachments are uploaded via multipart form data alongside the event metadata.

## Installation

### Quick Install (Recommended)

```bash
curl -fsSL https://promptconduit.dev/install | bash
```

Or with your API key:

```bash
curl -fsSL https://promptconduit.dev/install | bash -s -- YOUR_API_KEY
```

### Homebrew

```bash
brew tap promptconduit/tap
brew install promptconduit
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

## Quick Start

### 1. Get your API key

Sign up at [promptconduit.dev](https://promptconduit.dev) and create an API key.

### 2. Set your API key

```bash
export PROMPTCONDUIT_API_KEY="your-api-key"
```

Add this to your shell profile (`~/.zshrc`, `~/.bashrc`, etc.) for persistence.

### 3. Install hooks for your tool

```bash
# For Claude Code
promptconduit install claude-code

# For Cursor
promptconduit install cursor

# For Gemini CLI
promptconduit install gemini-cli
```

### 4. Verify installation

```bash
promptconduit status
```

### 5. Test API connectivity

```bash
promptconduit test
```

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

# Show version
promptconduit version
```

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
# Switch to local development (shortcut)
promptconduit config set-env local

# Switch to production
promptconduit config set-env prod

# Or use the full command
promptconduit config env use local

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
              │   promptconduit     │  ← Hook receives JSON
              │      hook           │    from stdin
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │   Tool Adapter      │  ← Translates to
              │ (Claude/Cursor/...) │    canonical format
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │  Canonical Event    │  ← Normalized schema
              │   + Git Context     │    with repo state
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │    Async Send       │  ← Non-blocking
              │  (subprocess)       │    to API
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │  PromptConduit API  │
              └─────────────────────┘
```

### Key Design Principles

- **Never blocks tools**: Hook always returns immediately with success
- **Async sending**: Events are sent in a detached subprocess
- **Rich context**: Captures git state (branch, commit, dirty files, etc.)
- **Attachment extraction**: Parses transcripts to extract images and documents
- **Multipart uploads**: Attachments sent efficiently via multipart form data
- **Graceful degradation**: Unknown events are silently skipped

## Canonical Event Schema

All events are normalized to this schema:

```json
{
  "tool": "claude-code",
  "event_type": "prompt_submit",
  "event_id": "uuid",
  "timestamp": "2024-01-01T00:00:00Z",
  "adapter_version": "1.0.0",
  "session_id": "...",
  "workspace": {
    "repo_name": "my-project",
    "repo_path": "/path/to/repo",
    "working_directory": "/path/to/repo"
  },
  "git": {
    "commit_hash": "abc123",
    "branch": "main",
    "is_dirty": true,
    "staged_count": 2,
    "unstaged_count": 1,
    "remote_url": "git@github.com:user/repo.git"
  },
  "prompt": {
    "prompt": "User's prompt text",
    "attachments": [
      {
        "filename": "image_1.png",
        "media_type": "image/png",
        "type": "image"
      }
    ]
  }
}
```

### Event Types

| Type | Description |
|------|-------------|
| `prompt_submit` | User submitted a prompt |
| `tool_pre` | Before tool execution |
| `tool_post` | After tool execution |
| `session_start` | Session started |
| `session_end` | Session ended |
| `shell_pre` | Before shell command (Cursor) |
| `shell_post` | After shell command (Cursor) |
| `file_read` | File read operation (Cursor) |
| `file_edit` | File edit operation (Cursor) |

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
│   ├── root.go            # Root command
│   ├── install.go         # Install command
│   ├── uninstall.go       # Uninstall command
│   ├── status.go          # Status command
│   ├── test.go            # Test command
│   └── hook.go            # Hook entry point
├── internal/
│   ├── adapters/          # Tool-specific adapters
│   │   ├── adapter.go     # Base adapter interface
│   │   ├── claudecode.go  # Claude Code adapter
│   │   ├── cursor.go      # Cursor adapter
│   │   ├── gemini.go      # Gemini adapter
│   │   └── registry.go    # Adapter registry
│   ├── client/            # HTTP client with multipart upload
│   ├── git/               # Git context extraction
│   ├── transcript/        # Transcript parsing & attachment extraction
│   └── schema/            # Event schemas
├── scripts/
│   └── install.sh         # Curl installer
├── .goreleaser.yaml       # Release configuration
├── Makefile               # Build commands
└── go.mod                 # Go module
```

## Privacy & Security

- **Open Source**: Full transparency on what data is captured
- **Minimal**: Only captures events needed for analysis
- **Secure**: HTTPS with API key authentication
- **Non-blocking**: Never interferes with your workflow

## License

Apache 2.0 - See [LICENSE](LICENSE) for details.

## Links

- [PromptConduit Website](https://promptconduit.dev)
- [Documentation](https://docs.promptconduit.dev)
- [Issue Tracker](https://github.com/promptconduit/cli/issues)
