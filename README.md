# PromptConduit Adapters

Universal adapters for capturing prompts and events from AI coding assistants.

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Python](https://img.shields.io/badge/python-3.9+-blue.svg)](https://www.python.org/downloads/)

## Overview

PromptConduit Adapters provides a unified way to capture prompts, tool executions, and session events from various AI coding assistants. All events are normalized to a canonical schema, enabling consistent analysis regardless of the source tool.

### Supported Tools

| Tool | Status | Events Captured |
|------|--------|-----------------|
| [Claude Code](https://claude.ai/code) | ✅ Supported | Prompts, Tools, Sessions |
| [Cursor](https://cursor.com) | ✅ Supported | Prompts, Shell, MCP, Files |
| [Gemini CLI](https://geminicli.com) | ✅ Supported | Prompts, Tools, Sessions |
| OpenAI Codex | 🚧 Planned | - |
| GitHub Copilot | 🚧 Planned | - |

## Installation

### Using pip

```bash
pip install promptconduit
```

### From source

```bash
git clone https://github.com/promptconduit/promptconduit-adapters.git
cd promptconduit-adapters
pip install -e .
```

## Quick Start

### 1. Get your API key

Sign up at [promptconduit.dev](https://promptconduit.dev) and create an API key.

### 2. Set your API key

```bash
export PROMPTCONDUIT_API_KEY="your-api-key"
```

### 3. Install hooks for your tool

```bash
# For Claude Code
promptconduit install claude-code

# For Cursor
promptconduit install cursor

# For Gemini CLI
promptconduit install gemini
```

### 4. Start using your AI coding assistant

Events will be automatically captured and sent to PromptConduit.

## Configuration

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `PROMPTCONDUIT_API_KEY` | Yes | - | Your API key |
| `PROMPTCONDUIT_API_URL` | No | `https://api.promptconduit.dev` | API endpoint |
| `PROMPTCONDUIT_DEBUG` | No | `0` | Set to `1` to include raw events |
| `PROMPTCONDUIT_TIMEOUT` | No | `30` | HTTP timeout in seconds |
| `PROMPTCONDUIT_TOOL` | No | Auto-detect | Force specific adapter |

### Manual Hook Configuration

If you prefer manual setup, copy the appropriate config template:

**Claude Code** (`~/.claude/settings.json`):
```json
{
  "hooks": {
    "UserPromptSubmit": [{
      "hooks": [{
        "type": "command",
        "command": "python3 /path/to/promptconduit_hook.py",
        "timeout": 5000
      }]
    }]
  }
}
```

See `configs/` directory for complete templates.

## CLI Commands

```bash
# Install hooks for a tool
promptconduit install <tool>

# Uninstall hooks
promptconduit uninstall <tool>

# Check status
promptconduit status

# Test API connectivity
promptconduit test
```

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                         AI Coding Tools                              │
├─────────────┬─────────────┬─────────────┬─────────────┬─────────────┤
│ Claude Code │   Cursor    │ Gemini CLI  │   Codex     │  Future...  │
└──────┬──────┴──────┬──────┴──────┬──────┴──────┬──────┴──────┬──────┘
       │             │             │             │             │
       ▼             ▼             ▼             ▼             ▼
┌─────────────────────────────────────────────────────────────────────┐
│                    Tool-Specific Adapters                            │
│  Claude Code → ClaudeCodeAdapter                                     │
│  Cursor      → CursorAdapter                                         │
│  Gemini CLI  → GeminiAdapter                                         │
└──────────────────────────────────┬──────────────────────────────────┘
                                   │
                                   ▼
┌─────────────────────────────────────────────────────────────────────┐
│                    Canonical Event Schema                            │
│  { tool, event_type, session_id, timestamp, workspace, git, ... }   │
└──────────────────────────────────┬──────────────────────────────────┘
                                   │
                                   ▼
┌─────────────────────────────────────────────────────────────────────┐
│                    PromptConduit API                                 │
│  POST /v1/events/ingest                                              │
└─────────────────────────────────────────────────────────────────────┘
```

## Canonical Event Schema

All events are normalized to this schema:

```python
CanonicalEvent:
  tool: str              # "claude-code", "cursor", "gemini-cli"
  event_type: str        # "prompt_submit", "tool_pre", "tool_post", etc.
  event_id: str          # UUID
  timestamp: str         # ISO 8601
  adapter_version: str   # Adapter version that created this event
  session_id: str?       # Tool-specific session identifier
  workspace: {           # Project context
    repo_name: str?
    repo_path: str?
    working_directory: str?
  }
  git: {                 # Repository state
    commit_hash: str?
    branch: str?
    is_dirty: bool?
    remote_url: str?
  }
  prompt: {              # For prompt_submit events
    prompt: str
    attachments: list?
  }
  tool_event: {          # For tool_pre/tool_post events
    tool_name: str
    input: dict?
    output: dict?
    success: bool?
  }
  session: {             # For session_start/session_end events
    source: str?
    reason: str?
  }
```

## Adding a New Adapter

To add support for a new tool:

1. Create a new adapter in `src/promptconduit/adapters/`:

```python
from promptconduit.adapters.base import BaseAdapter
from promptconduit.schema.events import EventType, Tool

class MyToolAdapter(BaseAdapter):
    TOOL = Tool.MY_TOOL

    EVENT_MAPPING = {
        "NativeEventName": EventType.PROMPT_SUBMIT,
        # ... more mappings
    }

    def translate_event(self, native_event: dict) -> CanonicalEvent | None:
        # Translation logic here
        pass
```

2. Add the adapter to `ADAPTERS` in `scripts/promptconduit_hook.py`

3. Add a configuration template in `configs/`

4. Submit a PR!

See [CONTRIBUTING.md](CONTRIBUTING.md) for detailed guidelines.

## Privacy & Security

- **Transparent**: This is open source - you can see exactly what data is captured
- **Minimal**: Only captures what's needed for analysis
- **Secure**: Data is sent over HTTPS with API key authentication
- **Your data**: Use the managed service or self-host

## Development

```bash
# Install dev dependencies
pip install -e ".[dev]"

# Run tests
pytest

# Type checking
mypy src/

# Linting
ruff check src/
```

## License

Apache 2.0 - See [LICENSE](LICENSE) for details.

## Links

- [PromptConduit Website](https://promptconduit.dev)
- [Documentation](https://docs.promptconduit.dev)
- [Issue Tracker](https://github.com/promptconduit/promptconduit-adapters/issues)
