"""
Cursor adapter for PromptConduit.

Translates Cursor hook events to canonical PromptConduit events.

Cursor Hook Events (Agent Hooks):
- beforeSubmitPrompt: User is about to submit a prompt
- beforeShellExecution: About to execute a shell command
- afterShellExecution: Completed executing a shell command
- beforeMCPExecution: About to execute an MCP tool
- afterMCPExecution: Completed executing an MCP tool
- beforeReadFile: About to read a file
- afterFileEdit: Completed editing a file
- afterAgentResponse: Agent finished responding
- afterAgentThought: Agent thinking output
- stop: Session/conversation stopped

Cursor Hook Events (Tab Hooks - inline completions):
- beforeTabFileRead: About to read a file for tab completion
- afterTabFileEdit: Completed a tab completion edit

See: https://cursor.com/docs/agent/hooks
"""

from typing import Dict, Any, Optional, List

from promptconduit.adapters.base import BaseAdapter
from promptconduit.schema.events import (
    CanonicalEvent,
    EventType,
    Tool,
    PromptPayload,
    ToolPayload,
    SessionPayload,
)


class CursorAdapter(BaseAdapter):
    """
    Adapter for Cursor hook events.

    Cursor uses a hook system similar to Claude Code, with JSON via stdin/stdout.
    Key differences:
    - Uses 'conversation_id' and 'generation_id' for session tracking
    - Has separate Agent and Tab hook categories
    - Includes 'cursor_version' in all events

    Example hook event (beforeSubmitPrompt):
        {
            "hook_event_name": "beforeSubmitPrompt",
            "conversation_id": "abc123",
            "generation_id": "gen456",
            "model": "gpt-4",
            "cursor_version": "0.42.0",
            "workspace_roots": ["/path/to/project"],
            "prompt": "Write a function",
            "attachments": [{"type": "file", "path": "/path/to/file.ts"}]
        }
    """

    TOOL = Tool.CURSOR

    EVENT_MAPPING = {
        # Agent hooks
        "beforeSubmitPrompt": EventType.PROMPT_SUBMIT,
        "beforeShellExecution": EventType.SHELL_PRE,
        "afterShellExecution": EventType.SHELL_POST,
        "beforeMCPExecution": EventType.TOOL_PRE,
        "afterMCPExecution": EventType.TOOL_POST,
        "beforeReadFile": EventType.FILE_READ,
        "afterFileEdit": EventType.FILE_EDIT,
        "afterAgentResponse": EventType.AGENT_RESPONSE,
        "afterAgentThought": EventType.AGENT_THOUGHT,
        "stop": EventType.SESSION_END,
        # Tab hooks (inline completions)
        "beforeTabFileRead": EventType.FILE_READ,
        "afterTabFileEdit": EventType.FILE_EDIT,
    }

    def translate_event(self, native_event: Dict[str, Any]) -> Optional[CanonicalEvent]:
        """
        Translate Cursor hook event to canonical format.

        Args:
            native_event: The raw event from Cursor's hook system

        Returns:
            CanonicalEvent if translation successful, None to skip event
        """
        native_event_name = native_event.get("hook_event_name", "")

        event_type = self.get_event_type(native_event_name)
        if not event_type:
            return None

        event = self.create_base_event(event_type, native_event, native_event_name)

        # Map Cursor's session tracking (uses conversation_id or generation_id)
        event.session_id = native_event.get("conversation_id") or native_event.get(
            "generation_id"
        )

        # Handle workspace roots (Cursor may have multiple)
        workspace_roots = native_event.get("workspace_roots", [])
        if workspace_roots and event.workspace:
            # Use the first workspace root as the repo path
            event.workspace.repo_path = workspace_roots[0]
            if not event.workspace.working_directory:
                event.workspace.working_directory = workspace_roots[0]

        # Populate event-specific payload
        if event_type == EventType.PROMPT_SUBMIT:
            event.prompt = self._translate_prompt(native_event)
        elif event_type in (EventType.SHELL_PRE, EventType.SHELL_POST):
            event.tool_event = self._translate_shell(native_event, event_type)
        elif event_type in (EventType.TOOL_PRE, EventType.TOOL_POST):
            event.tool_event = self._translate_mcp(native_event, event_type)
        elif event_type in (EventType.FILE_READ, EventType.FILE_EDIT):
            event.tool_event = self._translate_file_op(native_event, event_type)

        return event

    def _translate_prompt(self, native_event: Dict[str, Any]) -> PromptPayload:
        """
        Translate beforeSubmitPrompt to PromptPayload.

        Cursor's prompt event includes:
        - prompt: The user's input text
        - attachments: Array of file/rule attachments

        Args:
            native_event: The raw event data

        Returns:
            PromptPayload with prompt content and attachments
        """
        raw_attachments = native_event.get("attachments", [])
        attachments = None

        if raw_attachments:
            attachments = [
                {
                    "type": a.get("type"),
                    "path": a.get("path"),
                }
                for a in raw_attachments
                if isinstance(a, dict)
            ]

        return PromptPayload(
            prompt=native_event.get("prompt", ""),
            attachments=attachments if attachments else None,
        )

    def _translate_shell(
        self, native_event: Dict[str, Any], event_type: EventType
    ) -> ToolPayload:
        """
        Translate shell execution events.

        beforeShellExecution includes:
        - command: The shell command
        - cwd: Working directory

        afterShellExecution includes:
        - command: The executed command
        - output: Full terminal output
        - duration: Execution time in milliseconds

        Args:
            native_event: The raw event data
            event_type: Whether this is pre or post execution

        Returns:
            ToolPayload with shell execution details
        """
        is_post = event_type == EventType.SHELL_POST

        return ToolPayload(
            tool_name="shell",
            input={
                "command": native_event.get("command"),
                "cwd": native_event.get("cwd"),
            },
            output={"stdout": native_event.get("output")} if is_post else None,
            success=True if is_post else None,  # Cursor doesn't provide explicit success
            duration_ms=native_event.get("duration"),
        )

    def _translate_mcp(
        self, native_event: Dict[str, Any], event_type: EventType
    ) -> ToolPayload:
        """
        Translate MCP (Model Context Protocol) execution events.

        beforeMCPExecution includes:
        - tool_name: Name of the MCP tool
        - params: Tool parameters (JSON)
        - server_url or server_command: MCP server identifier

        afterMCPExecution includes:
        - tool_name: Name of the MCP tool
        - params: Input parameters
        - result: Tool output (JSON)
        - duration: Execution time in milliseconds

        Args:
            native_event: The raw event data
            event_type: Whether this is pre or post execution

        Returns:
            ToolPayload with MCP tool execution details
        """
        is_post = event_type == EventType.TOOL_POST

        return ToolPayload(
            tool_name=native_event.get("tool_name", "mcp"),
            input=native_event.get("params"),
            output=native_event.get("result") if is_post else None,
            success=True if is_post else None,
            duration_ms=native_event.get("duration"),
        )

    def _translate_file_op(
        self, native_event: Dict[str, Any], event_type: EventType
    ) -> ToolPayload:
        """
        Translate file read/edit events.

        beforeReadFile / beforeTabFileRead includes:
        - file_path: Path to the file
        - contents: File contents (for read)

        afterFileEdit / afterTabFileEdit includes:
        - file_path: Path to the file
        - edits: Array of edit objects with old_string/new_string

        Tab hooks also include range info (line numbers, columns).

        Args:
            native_event: The raw event data
            event_type: Whether this is read or edit

        Returns:
            ToolPayload with file operation details
        """
        is_edit = event_type == EventType.FILE_EDIT
        tool_name = "file_edit" if is_edit else "file_read"

        input_data: Dict[str, Any] = {
            "file_path": native_event.get("file_path"),
        }

        if not is_edit:
            # Include contents for read operations
            input_data["contents"] = native_event.get("contents")

        output_data = None
        if is_edit:
            edits = native_event.get("edits", [])
            output_data = {"edits": edits}

        return ToolPayload(
            tool_name=tool_name,
            input=input_data,
            output=output_data,
        )
