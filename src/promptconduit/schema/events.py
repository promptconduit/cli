"""
Canonical event schema for PromptConduit.

All tool-specific events are normalized to this schema before
being sent to the PromptConduit API. This ensures consistent
data across Claude Code, Cursor, Gemini CLI, and future tools.
"""

from dataclasses import dataclass, field
from typing import Optional, Dict, Any, List
from enum import Enum
import json

from promptconduit.schema.context import GitContext, WorkspaceContext


class EventType(str, Enum):
    """
    Normalized event types across all AI coding tools.

    These represent the canonical event categories that PromptConduit
    tracks, regardless of the source tool's native event naming.
    """

    # Prompt events
    PROMPT_SUBMIT = "prompt_submit"
    """User submitted a prompt to the AI assistant."""

    # Tool/action events
    TOOL_PRE = "tool_pre"
    """About to execute a tool (generic)."""

    TOOL_POST = "tool_post"
    """Completed executing a tool (generic)."""

    # Session lifecycle
    SESSION_START = "session_start"
    """A new session started or was resumed."""

    SESSION_END = "session_end"
    """Session ended (exit, logout, etc.)."""

    # Agent events
    AGENT_THOUGHT = "agent_thought"
    """Agent thinking/reasoning output."""

    AGENT_RESPONSE = "agent_response"
    """Agent completed its response."""

    # File operations (granular, optional)
    FILE_READ = "file_read"
    """File was read."""

    FILE_EDIT = "file_edit"
    """File was edited."""

    FILE_CREATE = "file_create"
    """File was created."""

    # Shell operations (granular, optional)
    SHELL_PRE = "shell_pre"
    """About to execute a shell command."""

    SHELL_POST = "shell_post"
    """Completed executing a shell command."""


class Tool(str, Enum):
    """
    Supported AI coding tools.

    Each tool has its own adapter that translates native events
    to the canonical PromptConduit schema.
    """

    CLAUDE_CODE = "claude-code"
    """Anthropic's Claude Code CLI."""

    CURSOR = "cursor"
    """Cursor IDE."""

    GEMINI_CLI = "gemini-cli"
    """Google's Gemini CLI."""

    CODEX = "codex"
    """OpenAI's Codex CLI."""

    WINDSURF = "windsurf"
    """Codeium's Windsurf IDE."""

    COPILOT = "copilot"
    """GitHub Copilot."""

    AIDER = "aider"
    """Aider CLI."""

    CONTINUE = "continue"
    """Continue.dev."""

    OTHER = "other"
    """Unknown or custom tool."""


@dataclass
class PromptPayload:
    """
    Payload for prompt_submit events.

    Contains the user's prompt and any associated metadata.
    """

    prompt: str
    """The user's prompt text."""

    response_summary: Optional[str] = None
    """Summary of the AI's response (if available)."""

    attachments: Optional[List[Dict[str, Any]]] = None
    """
    List of attachments included with the prompt.
    Each attachment has 'type', 'filename', 'content_type', etc.
    """

    def to_dict(self) -> Dict[str, Any]:
        """Convert to dictionary, excluding None values."""
        result: Dict[str, Any] = {"prompt": self.prompt}
        if self.response_summary is not None:
            result["response_summary"] = self.response_summary
        if self.attachments is not None:
            result["attachments"] = self.attachments
        return result


@dataclass
class ToolPayload:
    """
    Payload for tool_pre and tool_post events.

    Contains information about tool execution.
    """

    tool_name: str
    """
    Name of the tool being executed.
    Examples: "Bash", "Write", "Read", "shell", "read_file", "write_file"
    """

    tool_use_id: Optional[str] = None
    """Unique identifier for this tool invocation (if provided by the source)."""

    input: Optional[Dict[str, Any]] = None
    """Tool input parameters."""

    output: Optional[Dict[str, Any]] = None
    """Tool output/result (for post events)."""

    success: Optional[bool] = None
    """Whether the tool execution succeeded (for post events)."""

    duration_ms: Optional[int] = None
    """Execution time in milliseconds (for post events)."""

    error_message: Optional[str] = None
    """Error details if the tool failed."""

    def to_dict(self) -> Dict[str, Any]:
        """Convert to dictionary, excluding None values."""
        result: Dict[str, Any] = {"tool_name": self.tool_name}
        if self.tool_use_id is not None:
            result["tool_use_id"] = self.tool_use_id
        if self.input is not None:
            result["input"] = self.input
        if self.output is not None:
            result["output"] = self.output
        if self.success is not None:
            result["success"] = self.success
        if self.duration_ms is not None:
            result["duration_ms"] = self.duration_ms
        if self.error_message is not None:
            result["error_message"] = self.error_message
        return result


@dataclass
class SessionPayload:
    """
    Payload for session_start and session_end events.

    Contains session lifecycle information.
    """

    source: Optional[str] = None
    """
    How the session started (for session_start).
    Examples: "startup", "resume", "clear", "compact"
    """

    reason: Optional[str] = None
    """
    Why the session ended (for session_end).
    Examples: "exit", "logout", "clear", "prompt_input_exit", "other"
    """

    def to_dict(self) -> Dict[str, Any]:
        """Convert to dictionary, excluding None values."""
        result: Dict[str, Any] = {}
        if self.source is not None:
            result["source"] = self.source
        if self.reason is not None:
            result["reason"] = self.reason
        return result


@dataclass
class CanonicalEvent:
    """
    The canonical event format for PromptConduit.

    All tool-specific events are normalized to this schema by adapters.
    This provides a consistent interface for storage and analysis
    regardless of the source tool.
    """

    # Required fields
    tool: Tool
    """The AI coding tool that generated this event."""

    event_type: EventType
    """The type of event (normalized across tools)."""

    event_id: str
    """Unique identifier for this event (UUID)."""

    timestamp: str
    """ISO 8601 timestamp when the event was captured."""

    adapter_version: str
    """Version of the adapter that created this event."""

    # Session tracking
    session_id: Optional[str] = None
    """Session identifier (tool-specific format)."""

    # Context
    workspace: Optional[WorkspaceContext] = None
    """Workspace/project context."""

    git: Optional[GitContext] = None
    """Git repository context."""

    # Event-specific payloads (one of these based on event_type)
    prompt: Optional[PromptPayload] = None
    """Payload for prompt_submit events."""

    tool_event: Optional[ToolPayload] = None
    """Payload for tool_pre, tool_post, shell_pre, shell_post events."""

    session: Optional[SessionPayload] = None
    """Payload for session_start, session_end events."""

    # Debugging: preserve original event
    raw_event_type: Optional[str] = None
    """Original event type name from the source tool."""

    raw_event: Optional[Dict[str, Any]] = None
    """
    Original event data (for debugging).
    Only included if adapter is configured with include_raw_event=True.
    """

    def to_dict(self) -> Dict[str, Any]:
        """
        Convert to dictionary for JSON serialization.

        Only includes non-None fields to minimize payload size.
        """
        result: Dict[str, Any] = {
            "tool": self.tool.value if isinstance(self.tool, Tool) else self.tool,
            "event_type": (
                self.event_type.value
                if isinstance(self.event_type, EventType)
                else self.event_type
            ),
            "event_id": self.event_id,
            "timestamp": self.timestamp,
            "adapter_version": self.adapter_version,
        }

        if self.session_id is not None:
            result["session_id"] = self.session_id
        if self.workspace is not None:
            result["workspace"] = self.workspace.to_dict()
        if self.git is not None:
            result["git"] = self.git.to_dict()
        if self.prompt is not None:
            result["prompt"] = self.prompt.to_dict()
        if self.tool_event is not None:
            result["tool_event"] = self.tool_event.to_dict()
        if self.session is not None:
            result["session"] = self.session.to_dict()
        if self.raw_event_type is not None:
            result["raw_event_type"] = self.raw_event_type
        if self.raw_event is not None:
            result["raw_event"] = self.raw_event

        return result

    def to_json(self) -> str:
        """Serialize to JSON string."""
        return json.dumps(self.to_dict())

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "CanonicalEvent":
        """
        Create a CanonicalEvent from a dictionary.

        Useful for deserializing events received from the API.
        """
        # Parse enums
        tool = Tool(data["tool"]) if isinstance(data["tool"], str) else data["tool"]
        event_type = (
            EventType(data["event_type"])
            if isinstance(data["event_type"], str)
            else data["event_type"]
        )

        # Parse nested objects
        workspace = None
        if "workspace" in data and data["workspace"]:
            workspace = WorkspaceContext(**data["workspace"])

        git = None
        if "git" in data and data["git"]:
            git = GitContext(**data["git"])

        prompt = None
        if "prompt" in data and data["prompt"]:
            prompt = PromptPayload(**data["prompt"])

        tool_event = None
        if "tool_event" in data and data["tool_event"]:
            tool_event = ToolPayload(**data["tool_event"])

        session = None
        if "session" in data and data["session"]:
            session = SessionPayload(**data["session"])

        return cls(
            tool=tool,
            event_type=event_type,
            event_id=data["event_id"],
            timestamp=data["timestamp"],
            adapter_version=data["adapter_version"],
            session_id=data.get("session_id"),
            workspace=workspace,
            git=git,
            prompt=prompt,
            tool_event=tool_event,
            session=session,
            raw_event_type=data.get("raw_event_type"),
            raw_event=data.get("raw_event"),
        )
