# Contributing to PromptConduit Adapters

Thank you for your interest in contributing! This document provides guidelines for contributing to the PromptConduit Adapters project.

## Getting Started

1. Fork the repository
2. Clone your fork:
   ```bash
   git clone https://github.com/YOUR_USERNAME/promptconduit-adapters.git
   cd promptconduit-adapters
   ```
3. Install development dependencies:
   ```bash
   pip install -e ".[dev]"
   ```

## Development Workflow

1. Create a feature branch:
   ```bash
   git checkout -b feat/my-feature
   ```

2. Make your changes

3. Run tests:
   ```bash
   pytest
   ```

4. Run type checking:
   ```bash
   mypy src/
   ```

5. Run linting:
   ```bash
   ruff check src/
   ```

6. Commit your changes:
   ```bash
   git commit -m "feat: description of changes"
   ```

7. Push and create a PR

## Adding a New Adapter

To add support for a new AI coding tool:

### 1. Add the Tool to the Enum

In `src/promptconduit/schema/events.py`, add your tool to the `Tool` enum:

```python
class Tool(str, Enum):
    # ... existing tools
    MY_TOOL = "my-tool"
    """Description of the tool."""
```

### 2. Create the Adapter

Create a new file `src/promptconduit/adapters/my_tool.py`:

```python
"""
MyTool adapter for PromptConduit.

Brief description of the tool and its hook system.
Link to documentation.
"""

from typing import Dict, Any, Optional

from promptconduit.adapters.base import BaseAdapter
from promptconduit.schema.events import (
    CanonicalEvent,
    EventType,
    Tool,
    PromptPayload,
    ToolPayload,
    SessionPayload,
)


class MyToolAdapter(BaseAdapter):
    """
    Adapter for MyTool hook events.

    Document the hook system and event structure here.
    """

    TOOL = Tool.MY_TOOL

    EVENT_MAPPING = {
        # Map native event names to canonical EventType
        "NativePromptEvent": EventType.PROMPT_SUBMIT,
        "NativeToolStart": EventType.TOOL_PRE,
        "NativeToolEnd": EventType.TOOL_POST,
        "NativeSessionStart": EventType.SESSION_START,
        "NativeSessionEnd": EventType.SESSION_END,
    }

    def translate_event(self, native_event: Dict[str, Any]) -> Optional[CanonicalEvent]:
        """Translate native event to canonical format."""
        native_event_name = native_event.get("event_name", "")

        event_type = self.get_event_type(native_event_name)
        if not event_type:
            return None

        event = self.create_base_event(event_type, native_event, native_event_name)

        # Populate event-specific payload
        if event_type == EventType.PROMPT_SUBMIT:
            event.prompt = self._translate_prompt(native_event)
        elif event_type in (EventType.TOOL_PRE, EventType.TOOL_POST):
            event.tool_event = self._translate_tool(native_event, event_type)
        elif event_type in (EventType.SESSION_START, EventType.SESSION_END):
            event.session = self._translate_session(native_event, event_type)

        return event

    def _translate_prompt(self, native_event: Dict[str, Any]) -> PromptPayload:
        """Translate prompt event."""
        return PromptPayload(
            prompt=native_event.get("prompt_text", ""),
        )

    def _translate_tool(
        self, native_event: Dict[str, Any], event_type: EventType
    ) -> ToolPayload:
        """Translate tool event."""
        return ToolPayload(
            tool_name=native_event.get("tool", "unknown"),
            input=native_event.get("input"),
            output=native_event.get("output") if event_type == EventType.TOOL_POST else None,
        )

    def _translate_session(
        self, native_event: Dict[str, Any], event_type: EventType
    ) -> SessionPayload:
        """Translate session event."""
        return SessionPayload(
            source=native_event.get("source") if event_type == EventType.SESSION_START else None,
            reason=native_event.get("reason") if event_type == EventType.SESSION_END else None,
        )
```

### 3. Register the Adapter

In `scripts/promptconduit_hook.py`, add your adapter:

```python
from promptconduit.adapters.my_tool import MyToolAdapter

ADAPTERS = {
    # ... existing adapters
    "my-tool": MyToolAdapter,
}

# Add event detection
MY_TOOL_EVENTS = {
    "NativePromptEvent",
    "NativeToolStart",
    # ... etc
}
```

Update `detect_tool()` to recognize your tool's events.

### 4. Add Configuration Template

Create `configs/my-tool/hooks.json` or similar with the hook configuration for your tool.

### 5. Add to CLI

Update `src/promptconduit/cli/main.py` to support installation for your tool.

### 6. Add Tests

Create `tests/test_adapters/test_my_tool.py` with test cases:

```python
import pytest
from promptconduit.adapters.my_tool import MyToolAdapter
from promptconduit.schema.events import EventType, Tool


class TestMyToolAdapter:
    def setup_method(self):
        self.adapter = MyToolAdapter()

    def test_translates_prompt_event(self):
        native_event = {
            "event_name": "NativePromptEvent",
            "prompt_text": "Hello world",
            "session_id": "test-123",
        }

        result = self.adapter.translate_event(native_event)

        assert result is not None
        assert result.tool == Tool.MY_TOOL
        assert result.event_type == EventType.PROMPT_SUBMIT
        assert result.prompt.prompt == "Hello world"

    def test_skips_unknown_event(self):
        native_event = {"event_name": "UnknownEvent"}

        result = self.adapter.translate_event(native_event)

        assert result is None
```

### 7. Update Documentation

- Update README.md with the new tool in the supported tools table
- Add any tool-specific documentation in `docs/`

## Code Style

- Follow PEP 8
- Use type hints
- Document public APIs with docstrings
- Keep functions focused and small
- Write tests for new functionality

## Commit Messages

Use conventional commits:

- `feat:` New features
- `fix:` Bug fixes
- `docs:` Documentation changes
- `test:` Adding or updating tests
- `refactor:` Code refactoring
- `chore:` Maintenance tasks

## Testing

- Ensure all tests pass: `pytest`
- Add tests for new functionality
- Include fixtures for sample events in `tests/fixtures/`

## Questions?

Open an issue or discussion on GitHub.
