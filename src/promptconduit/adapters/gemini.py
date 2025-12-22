"""
Gemini CLI adapter for PromptConduit.

Translates Gemini CLI hook events to canonical PromptConduit events.

Gemini CLI Hook Events:
- SessionStart: Session initialization
- SessionEnd: Session termination
- BeforeAgent: After user prompt, before planning
- AfterAgent: When agent loop concludes
- BeforeModel: Pre-LLM request submission
- AfterModel: Post-LLM response receipt
- BeforeToolSelection: Post-model, pre-tool filtering
- BeforeTool: About to execute a tool
- AfterTool: Completed executing a tool
- PreCompress: Before context compression
- Notification: Permission events

See: https://geminicli.com/docs/hooks/
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


class GeminiAdapter(BaseAdapter):
    """
    Adapter for Gemini CLI hook events.

    Gemini CLI has a hook system very similar to Claude Code.
    In fact, Gemini CLI provides a migration tool:
        `gemini hooks migrate --from-claude`

    Key differences:
    - Uses BeforeTool/AfterTool instead of PreToolUse/PostToolUse
    - Has BeforeAgent/AfterAgent for agent lifecycle
    - Has BeforeModel/AfterModel for LLM request/response

    Example hook event (BeforeTool):
        {
            "session_id": "abc123",
            "transcript_path": "/path/to/transcript.jsonl",
            "cwd": "/path/to/project",
            "hook_event_name": "BeforeTool",
            "timestamp": "2025-01-01T00:00:00Z",
            "tool_name": "write_file",
            "tool_input": {"file_path": "...", "content": "..."}
        }
    """

    TOOL = Tool.GEMINI_CLI

    EVENT_MAPPING = {
        # Session lifecycle
        "SessionStart": EventType.SESSION_START,
        "SessionEnd": EventType.SESSION_END,
        # Agent lifecycle
        "BeforeAgent": EventType.PROMPT_SUBMIT,  # Closest equivalent
        "AfterAgent": EventType.AGENT_RESPONSE,
        # Model events (optional tracking)
        "BeforeModel": EventType.AGENT_THOUGHT,
        "AfterModel": EventType.AGENT_THOUGHT,
        # Tool events
        "BeforeTool": EventType.TOOL_PRE,
        "AfterTool": EventType.TOOL_POST,
        "BeforeToolSelection": EventType.TOOL_PRE,
        # Other
        "PreCompress": EventType.AGENT_THOUGHT,
        "Notification": EventType.AGENT_THOUGHT,
    }

    # Gemini tool name mappings (Gemini uses different names than Claude Code)
    TOOL_NAME_MAPPING = {
        "read_file": "Read",
        "write_file": "Write",
        "replace": "Edit",
        "read_many_files": "Read",
        "list_directory": "Glob",
        "glob": "Glob",
        "search_file_content": "Grep",
        "run_shell_command": "Bash",
        "google_web_search": "WebSearch",
        "web_fetch": "WebFetch",
        "write_todos": "TodoWrite",
        "save_memory": "Memory",
        "delegate_to_agent": "Task",
    }

    def translate_event(self, native_event: Dict[str, Any]) -> Optional[CanonicalEvent]:
        """
        Translate Gemini CLI hook event to canonical format.

        Args:
            native_event: The raw event from Gemini CLI's hook system

        Returns:
            CanonicalEvent if translation successful, None to skip event
        """
        native_event_name = native_event.get("hook_event_name", "")

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
        """
        Translate BeforeAgent to PromptPayload.

        BeforeAgent is triggered after the user submits a prompt but before
        the agent starts planning. The actual prompt text may be in various
        fields depending on the event structure.

        Args:
            native_event: The raw event data

        Returns:
            PromptPayload with prompt content
        """
        # Try different possible locations for prompt text
        prompt = (
            native_event.get("prompt")
            or native_event.get("user_message")
            or native_event.get("message")
            or ""
        )

        return PromptPayload(
            prompt=prompt,
            response_summary=native_event.get("response_summary"),
        )

    def _translate_tool(
        self, native_event: Dict[str, Any], event_type: EventType
    ) -> ToolPayload:
        """
        Translate BeforeTool/AfterTool to ToolPayload.

        Gemini CLI tool events include:
        - tool_name: Name of the tool (Gemini-style names like write_file)
        - tool_input: Input parameters
        - tool_output: Output (AfterTool only)

        Args:
            native_event: The raw event data
            event_type: Whether this is pre or post execution

        Returns:
            ToolPayload with tool execution details
        """
        gemini_tool_name = native_event.get("tool_name", "unknown")

        # Normalize tool name to Claude Code style for consistency
        normalized_name = self.TOOL_NAME_MAPPING.get(
            gemini_tool_name, gemini_tool_name
        )

        is_post = event_type == EventType.TOOL_POST
        tool_output = native_event.get("tool_output") or native_event.get("output")

        # Determine success
        success = None
        error_message = None
        if is_post:
            if isinstance(tool_output, dict):
                has_error = tool_output.get("error") or tool_output.get("is_error")
                success = not has_error
                if not success:
                    error_message = (
                        tool_output.get("error")
                        or tool_output.get("message")
                        or str(tool_output)
                    )
            else:
                success = True

        return ToolPayload(
            tool_name=normalized_name,
            tool_use_id=native_event.get("tool_use_id"),
            input=native_event.get("tool_input") or native_event.get("input"),
            output=tool_output if is_post else None,
            success=success,
            error_message=error_message,
            duration_ms=native_event.get("duration_ms"),
        )

    def _translate_session(
        self, native_event: Dict[str, Any], event_type: EventType
    ) -> SessionPayload:
        """
        Translate SessionStart/SessionEnd to SessionPayload.

        Gemini CLI session events are similar to Claude Code's format.

        Args:
            native_event: The raw event data
            event_type: Whether this is start or end

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
