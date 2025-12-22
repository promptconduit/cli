#!/usr/bin/env python3
"""
Universal hook entry point for PromptConduit.

This script is called by all supported AI coding tools (Claude Code, Cursor,
Gemini CLI, etc.) and routes events to the appropriate adapter for translation
to the canonical PromptConduit event format.

Usage:
    This script is invoked by the tool's hook system. It reads a JSON event
    from stdin and outputs a JSON response to stdout.

    The script automatically detects which tool generated the event based
    on event structure and routes to the appropriate adapter.

Environment Variables:
    PROMPTCONDUIT_API_KEY: Required. Your PromptConduit API key.
    PROMPTCONDUIT_API_URL: Optional. API URL (default: https://api.promptconduit.dev)
    PROMPTCONDUIT_DEBUG: Optional. Set to "1" to include raw events in payload.
    PROMPTCONDUIT_TOOL: Optional. Force a specific tool adapter.

Exit Codes:
    0: Success (event processed or skipped)
    2: Blocking error (would cause the tool to block the action)
    Other: Non-blocking error (logged but continues)

Example hook configuration (Claude Code):
    {
        "hooks": {
            "UserPromptSubmit": [{
                "hooks": [{
                    "type": "command",
                    "command": "python3 /path/to/promptconduit_hook.py"
                }]
            }]
        }
    }
"""

import json
import os
import sys
from typing import Optional, Dict, Any

# Add src to path when running from development checkout
SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
SRC_DIR = os.path.join(SCRIPT_DIR, "..", "src")
if os.path.isdir(SRC_DIR):
    sys.path.insert(0, SRC_DIR)

from promptconduit.adapters.claude_code import ClaudeCodeAdapter
from promptconduit.adapters.cursor import CursorAdapter
from promptconduit.adapters.gemini import GeminiAdapter
from promptconduit.adapters.base import BaseAdapter
from promptconduit.client.api import PromptConduitClient
from promptconduit.client.config import get_config


# Adapter registry - maps tool identifiers to adapter classes
ADAPTERS: Dict[str, type] = {
    "claude-code": ClaudeCodeAdapter,
    "cursor": CursorAdapter,
    "gemini": GeminiAdapter,
    "gemini-cli": GeminiAdapter,
}

# Event names that indicate specific tools
CLAUDE_CODE_EVENTS = {
    "UserPromptSubmit",
    "PreToolUse",
    "PostToolUse",
    "SessionStart",
    "SessionEnd",
    "Stop",
    "SubagentStop",
    "PermissionRequest",
    "Notification",
    "PreCompact",
}

CURSOR_EVENTS = {
    "beforeSubmitPrompt",
    "beforeShellExecution",
    "afterShellExecution",
    "beforeMCPExecution",
    "afterMCPExecution",
    "beforeReadFile",
    "afterFileEdit",
    "afterAgentResponse",
    "afterAgentThought",
    "stop",
    "beforeTabFileRead",
    "afterTabFileEdit",
}

GEMINI_EVENTS = {
    "BeforeAgent",
    "AfterAgent",
    "BeforeModel",
    "AfterModel",
    "BeforeTool",
    "AfterTool",
    "BeforeToolSelection",
    "PreCompress",
}


def detect_tool(native_event: Dict[str, Any]) -> Optional[str]:
    """
    Detect which tool generated this event based on event structure.

    Different tools have different field patterns and event names.
    This function examines the event to determine the source.

    Args:
        native_event: The raw event from the tool's hook system

    Returns:
        Tool identifier string or None if unknown
    """
    # Check environment variable override first
    env_tool = os.environ.get("PROMPTCONDUIT_TOOL")
    if env_tool:
        return env_tool.lower()

    # Get event name from either field
    event_name = native_event.get("hook_event_name") or native_event.get("type", "")

    # Check for Cursor-specific marker
    if native_event.get("cursor_version"):
        return "cursor"

    # Check event names for each tool
    if event_name in CURSOR_EVENTS:
        return "cursor"

    if event_name in GEMINI_EVENTS:
        return "gemini"

    if event_name in CLAUDE_CODE_EVENTS:
        return "claude-code"

    # Unknown tool
    return None


def get_adapter(tool: str, debug: bool = False) -> Optional[BaseAdapter]:
    """
    Get an adapter instance for the specified tool.

    Args:
        tool: Tool identifier string
        debug: Whether to include raw events in output

    Returns:
        Adapter instance or None if tool not supported
    """
    adapter_class = ADAPTERS.get(tool)
    if adapter_class:
        return adapter_class(include_raw_event=debug)
    return None


def output_response(data: Dict[str, Any]) -> None:
    """
    Output a JSON response to stdout.

    Args:
        data: Response dictionary to output
    """
    print(json.dumps(data))


def main() -> int:
    """
    Main entry point for the hook script.

    Returns:
        Exit code (0 for success, 2 for blocking error)
    """
    try:
        # Read event from stdin
        try:
            native_event = json.load(sys.stdin)
        except json.JSONDecodeError:
            # Invalid JSON - return success to not block the tool
            output_response({"continue": True})
            return 0

        # Load configuration
        config = get_config()

        # Skip if not configured
        if not config.is_configured:
            output_response({"continue": True})
            return 0

        # Detect which tool this event came from
        tool = detect_tool(native_event)
        if not tool:
            # Unknown tool - skip silently
            output_response({"continue": True})
            return 0

        # Get appropriate adapter
        adapter = get_adapter(tool, debug=config.debug)
        if not adapter:
            # Tool not supported - skip silently
            output_response({"continue": True})
            return 0

        # Translate to canonical format
        canonical_event = adapter.translate_event(native_event)
        if not canonical_event:
            # Event type not supported - skip silently
            output_response({"continue": True})
            return 0

        # Send to API (non-blocking)
        client = PromptConduitClient(config)
        client.send_event_async(canonical_event)

        # Return success response
        output_response({"continue": True})
        return 0

    except Exception as e:
        # Any error - never block the tool
        # Output valid response so the tool can continue
        output_response({"continue": True})

        # Log to stderr for debugging (tools may capture this)
        if os.environ.get("PROMPTCONDUIT_DEBUG") == "1":
            print(f"PromptConduit error: {e}", file=sys.stderr)

        return 0


if __name__ == "__main__":
    sys.exit(main())
