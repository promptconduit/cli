"""
Claude Code adapter for PromptConduit.

Translates Claude Code hook events to canonical PromptConduit events.

Claude Code Hook Events:
- UserPromptSubmit: User submitted a prompt
- PreToolUse: About to execute a tool (Bash, Write, Read, etc.)
- PostToolUse: Completed executing a tool
- SessionStart: Session started or resumed
- SessionEnd: Session ended
- Stop: Agent finished responding
- SubagentStop: Subagent (Task tool) finished

See: https://docs.anthropic.com/en/docs/claude-code/hooks
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


class ClaudeCodeAdapter(BaseAdapter):
    """
    Adapter for Claude Code hook events.

    Claude Code uses a hook system that sends JSON via stdin and expects
    JSON responses via stdout. This adapter translates those events into
    the canonical PromptConduit format.

    Example hook event (UserPromptSubmit):
        {
            "hook_event_name": "UserPromptSubmit",
            "session_id": "abc123",
            "cwd": "/path/to/project",
            "prompt": "Write a function to calculate fibonacci"
        }
    """

    TOOL = Tool.CLAUDE_CODE

    EVENT_MAPPING = {
        # Prompt events
        "UserPromptSubmit": EventType.PROMPT_SUBMIT,
        # Tool events
        "PreToolUse": EventType.TOOL_PRE,
        "PostToolUse": EventType.TOOL_POST,
        # Session events
        "SessionStart": EventType.SESSION_START,
        "SessionEnd": EventType.SESSION_END,
        # Agent completion events
        "Stop": EventType.AGENT_RESPONSE,
        "SubagentStop": EventType.AGENT_RESPONSE,
        # Permission events (optional tracking)
        "PermissionRequest": EventType.TOOL_PRE,
        # Notification events (optional tracking)
        "Notification": EventType.AGENT_THOUGHT,
    }

    def translate_event(self, native_event: Dict[str, Any]) -> Optional[CanonicalEvent]:
        """
        Translate Claude Code hook event to canonical format.

        Args:
            native_event: The raw event from Claude Code's hook system.
                         Contains hook_event_name (or type) and event-specific fields.

        Returns:
            CanonicalEvent if translation successful, None to skip event
        """
        # Get event type from either 'type' or 'hook_event_name'
        native_event_name = native_event.get("type") or native_event.get(
            "hook_event_name", ""
        )

        event_type = self.get_event_type(native_event_name)
        if not event_type:
            return None  # Unknown event type, skip

        # Create base event with common fields
        event = self.create_base_event(event_type, native_event, native_event_name)

        # Populate event-specific payload
        if event_type == EventType.PROMPT_SUBMIT:
            event.prompt = self._translate_prompt(native_event)
        elif event_type in (EventType.TOOL_PRE, EventType.TOOL_POST):
            event.tool_event = self._translate_tool(native_event, event_type)
        elif event_type in (EventType.SESSION_START, EventType.SESSION_END):
            event.session = self._translate_session(native_event, event_type)
        # AGENT_RESPONSE events don't need additional payload

        return event

    def _translate_prompt(self, native_event: Dict[str, Any]) -> PromptPayload:
        """
        Translate UserPromptSubmit to PromptPayload.

        Claude Code's UserPromptSubmit event includes:
        - prompt: The user's input text
        - (attachments are handled separately via multipart upload)

        Args:
            native_event: The raw event data

        Returns:
            PromptPayload with prompt content
        """
        return PromptPayload(
            prompt=native_event.get("prompt", ""),
            response_summary=native_event.get("responseSummary"),
            # Attachments are handled via multipart in the HTTP client
            attachments=None,
        )

    def _translate_tool(
        self, native_event: Dict[str, Any], event_type: EventType
    ) -> ToolPayload:
        """
        Translate PreToolUse/PostToolUse to ToolPayload.

        Claude Code tool events include:
        - tool_name: Name of the tool (Bash, Write, Read, Edit, etc.)
        - tool_use_id: Unique ID for this tool invocation
        - tool_input: Input parameters for the tool
        - tool_response: Output from the tool (PostToolUse only)

        Args:
            native_event: The raw event data
            event_type: Whether this is a pre or post event

        Returns:
            ToolPayload with tool execution details
        """
        tool_response = native_event.get("tool_response", {})

        # Determine success from response
        success = None
        error_message = None
        if event_type == EventType.TOOL_POST:
            if isinstance(tool_response, dict):
                # Check various error indicators
                has_error = tool_response.get("error") or tool_response.get("is_error")
                success = not has_error
                if not success:
                    error_message = (
                        tool_response.get("error")
                        or tool_response.get("message")
                        or str(tool_response)
                    )
            else:
                success = True

        return ToolPayload(
            tool_name=native_event.get("tool_name", "unknown"),
            tool_use_id=native_event.get("tool_use_id"),
            input=native_event.get("tool_input"),
            output=tool_response if event_type == EventType.TOOL_POST else None,
            success=success,
            error_message=error_message,
        )

    def _translate_session(
        self, native_event: Dict[str, Any], event_type: EventType
    ) -> SessionPayload:
        """
        Translate SessionStart/SessionEnd to SessionPayload.

        SessionStart includes:
        - source: How the session started ("startup", "resume", "clear", "compact")

        SessionEnd includes:
        - reason: Why the session ended ("exit", "clear", "logout", "prompt_input_exit")

        Args:
            native_event: The raw event data
            event_type: Whether this is a start or end event

        Returns:
            SessionPayload with session lifecycle details
        """
        return SessionPayload(
            source=(
                native_event.get("source")
                if event_type == EventType.SESSION_START
                else None
            ),
            reason=(
                native_event.get("reason")
                if event_type == EventType.SESSION_END
                else None
            ),
        )
