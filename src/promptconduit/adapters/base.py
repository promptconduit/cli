"""
Base adapter class for tool-specific event translation.

Each supported tool (Claude Code, Cursor, etc.) implements this interface
to translate its native hook events into the canonical PromptConduit schema.
"""

from abc import ABC, abstractmethod
from typing import Dict, Any, Optional, Type
import subprocess
import uuid
from datetime import datetime, timezone
from pathlib import Path

from promptconduit.schema.events import (
    CanonicalEvent,
    EventType,
    Tool,
)
from promptconduit.schema.context import GitContext, WorkspaceContext
from promptconduit.version import __version__


class BaseAdapter(ABC):
    """
    Abstract base class for tool adapters.

    Subclasses must implement:
    - TOOL: The Tool enum value for this adapter
    - EVENT_MAPPING: Dict mapping native event names to EventType
    - translate_event(): Convert native event to CanonicalEvent

    Example:
        class MyToolAdapter(BaseAdapter):
            TOOL = Tool.MY_TOOL
            EVENT_MAPPING = {
                "UserPromptSubmit": EventType.PROMPT_SUBMIT,
                "PreToolUse": EventType.TOOL_PRE,
            }

            def translate_event(self, native_event: Dict[str, Any]) -> Optional[CanonicalEvent]:
                # Implementation here
                pass
    """

    TOOL: Tool = NotImplemented
    """The Tool enum value for this adapter. Must be set by subclasses."""

    EVENT_MAPPING: Dict[str, EventType] = NotImplemented
    """
    Mapping from native event names to canonical EventType.
    Must be set by subclasses.
    """

    def __init__(self, include_raw_event: bool = False):
        """
        Initialize adapter.

        Args:
            include_raw_event: If True, include original event in raw_event field.
                              Useful for debugging but increases payload size.
        """
        self.include_raw_event = include_raw_event
        self.adapter_version = __version__

    @abstractmethod
    def translate_event(self, native_event: Dict[str, Any]) -> Optional[CanonicalEvent]:
        """
        Translate a native tool event to canonical format.

        This is the main method that subclasses must implement.
        It receives the raw event from the tool's hook system and
        should return a CanonicalEvent with all relevant fields populated.

        Args:
            native_event: The raw event from the tool's hook system

        Returns:
            CanonicalEvent if translation successful, None to skip event
        """
        pass

    def get_event_type(self, native_event_name: str) -> Optional[EventType]:
        """
        Map native event name to canonical EventType.

        Uses the EVENT_MAPPING defined by the subclass.

        Args:
            native_event_name: The event name from the native tool

        Returns:
            The corresponding EventType, or None if not mapped
        """
        return self.EVENT_MAPPING.get(native_event_name)

    def extract_git_context(self, working_dir: Optional[str]) -> Optional[GitContext]:
        """
        Extract git information from the working directory.

        Runs git commands to capture the current repository state.
        This is called automatically by create_base_event if a
        working directory is available.

        Args:
            working_dir: Path to the working directory

        Returns:
            GitContext with repository state, or None if not a git repo
        """
        if not working_dir:
            return None

        try:
            result = GitContext()

            # Check if this is a git repository
            repo_result = subprocess.run(
                ["git", "rev-parse", "--show-toplevel"],
                cwd=working_dir,
                capture_output=True,
                text=True,
                timeout=2,
            )
            if repo_result.returncode != 0:
                return None

            # Get commit hash
            hash_result = subprocess.run(
                ["git", "rev-parse", "HEAD"],
                cwd=working_dir,
                capture_output=True,
                text=True,
                timeout=2,
            )
            if hash_result.returncode == 0:
                result.commit_hash = hash_result.stdout.strip()

            # Get commit message
            msg_result = subprocess.run(
                ["git", "log", "-1", "--format=%s"],
                cwd=working_dir,
                capture_output=True,
                text=True,
                timeout=2,
            )
            if msg_result.returncode == 0:
                result.commit_message = msg_result.stdout.strip()

            # Get commit author
            author_result = subprocess.run(
                ["git", "log", "-1", "--format=%an"],
                cwd=working_dir,
                capture_output=True,
                text=True,
                timeout=2,
            )
            if author_result.returncode == 0:
                result.commit_author = author_result.stdout.strip()

            # Get branch
            branch_result = subprocess.run(
                ["git", "branch", "--show-current"],
                cwd=working_dir,
                capture_output=True,
                text=True,
                timeout=2,
            )
            if branch_result.returncode == 0:
                branch = branch_result.stdout.strip()
                result.branch = branch if branch else None
                result.is_detached_head = not bool(branch)

            # Check if dirty and get file counts
            status_result = subprocess.run(
                ["git", "status", "--porcelain"],
                cwd=working_dir,
                capture_output=True,
                text=True,
                timeout=2,
            )
            if status_result.returncode == 0:
                lines = status_result.stdout.strip().split("\n") if status_result.stdout.strip() else []
                result.is_dirty = len(lines) > 0

                # Count staged, unstaged, untracked
                staged = 0
                unstaged = 0
                untracked = 0
                for line in lines:
                    if len(line) >= 2:
                        index_status = line[0]
                        worktree_status = line[1]
                        if index_status == "?":
                            untracked += 1
                        elif index_status != " ":
                            staged += 1
                        if worktree_status not in (" ", "?"):
                            unstaged += 1

                result.staged_count = staged
                result.unstaged_count = unstaged
                result.untracked_count = untracked

            # Get remote URL
            remote_result = subprocess.run(
                ["git", "remote", "get-url", "origin"],
                cwd=working_dir,
                capture_output=True,
                text=True,
                timeout=2,
            )
            if remote_result.returncode == 0:
                result.remote_url = remote_result.stdout.strip()

            # Get ahead/behind counts
            tracking_result = subprocess.run(
                ["git", "rev-list", "--left-right", "--count", "@{upstream}...HEAD"],
                cwd=working_dir,
                capture_output=True,
                text=True,
                timeout=2,
            )
            if tracking_result.returncode == 0:
                parts = tracking_result.stdout.strip().split()
                if len(parts) == 2:
                    result.behind_count = int(parts[0])
                    result.ahead_count = int(parts[1])

            return result

        except (subprocess.TimeoutExpired, FileNotFoundError, OSError, ValueError):
            return None

    def create_base_event(
        self,
        event_type: EventType,
        native_event: Dict[str, Any],
        native_event_name: str,
    ) -> CanonicalEvent:
        """
        Create a CanonicalEvent with common fields populated.

        This is a helper method for subclasses to use when building
        canonical events. It handles:
        - Generating a unique event ID
        - Setting the timestamp
        - Extracting workspace and git context
        - Including raw event if configured

        Subclasses should call this first, then populate the
        event-specific payload fields (prompt, tool_event, session).

        Args:
            event_type: The canonical event type
            native_event: The raw event data from the tool
            native_event_name: The original event type name

        Returns:
            CanonicalEvent with common fields populated
        """
        working_dir = native_event.get("cwd") or native_event.get("workingDirectory")

        return CanonicalEvent(
            tool=self.TOOL,
            event_type=event_type,
            event_id=str(uuid.uuid4()),
            timestamp=datetime.now(timezone.utc).isoformat(),
            adapter_version=self.adapter_version,
            session_id=native_event.get("session_id"),
            workspace=self._extract_workspace(native_event),
            git=self.extract_git_context(working_dir),
            raw_event_type=native_event_name,
            raw_event=native_event if self.include_raw_event else None,
        )

    def _extract_workspace(self, native_event: Dict[str, Any]) -> Optional[WorkspaceContext]:
        """
        Extract workspace context from native event.

        Different tools structure their context differently.
        This method handles the common patterns.

        Args:
            native_event: The raw event data

        Returns:
            WorkspaceContext if context is available, None otherwise
        """
        # Try to get context from various possible locations
        context = native_event.get("context", {})
        cwd = native_event.get("cwd") or context.get("workingDirectory")

        if not cwd and not context:
            return None

        # Extract repo name from path if not provided
        repo_name = context.get("repoName")
        repo_path = context.get("repoPath")

        if not repo_name and repo_path:
            repo_name = Path(repo_path).name
        elif not repo_name and cwd:
            # Try to determine repo root
            try:
                result = subprocess.run(
                    ["git", "rev-parse", "--show-toplevel"],
                    cwd=cwd,
                    capture_output=True,
                    text=True,
                    timeout=2,
                )
                if result.returncode == 0:
                    repo_path = result.stdout.strip()
                    repo_name = Path(repo_path).name
            except (subprocess.TimeoutExpired, FileNotFoundError, OSError):
                pass

        return WorkspaceContext(
            repo_name=repo_name,
            repo_path=repo_path,
            working_directory=cwd,
            files_referenced=context.get("filesReferenced"),
        )


# Type alias for adapter classes
AdapterClass = Type[BaseAdapter]


def get_adapter_for_tool(tool: Tool) -> Optional[AdapterClass]:
    """
    Get the adapter class for a specific tool.

    This is used by the universal hook entry point to route
    events to the appropriate adapter.

    Args:
        tool: The Tool enum value

    Returns:
        The adapter class, or None if not supported
    """
    # Import here to avoid circular imports
    from promptconduit.adapters.claude_code import ClaudeCodeAdapter
    from promptconduit.adapters.cursor import CursorAdapter
    from promptconduit.adapters.gemini import GeminiAdapter

    adapters: Dict[Tool, AdapterClass] = {
        Tool.CLAUDE_CODE: ClaudeCodeAdapter,
        Tool.CURSOR: CursorAdapter,
        Tool.GEMINI_CLI: GeminiAdapter,
    }

    return adapters.get(tool)
