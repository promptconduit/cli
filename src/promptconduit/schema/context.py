"""
Context dataclasses for PromptConduit canonical events.

These classes capture the workspace and git context at the time
an event is captured.
"""

from dataclasses import dataclass, asdict
from typing import Optional, List, Dict, Any


@dataclass
class GitContext:
    """
    Git repository context at the time of the event.

    This captures the state of the git repository when an event occurred,
    enabling correlation of prompts and tool usage with specific code states.
    """

    # Commit information
    commit_hash: Optional[str] = None
    """The current HEAD commit SHA."""

    commit_message: Optional[str] = None
    """The commit message of HEAD."""

    commit_author: Optional[str] = None
    """The author of the HEAD commit."""

    commit_timestamp: Optional[str] = None
    """ISO 8601 timestamp of the HEAD commit."""

    # Branch information
    branch: Optional[str] = None
    """Current branch name (empty if detached HEAD)."""

    is_detached_head: Optional[bool] = None
    """True if HEAD is detached from any branch."""

    # Working tree state
    is_dirty: Optional[bool] = None
    """True if there are uncommitted changes."""

    staged_count: Optional[int] = None
    """Number of staged files."""

    unstaged_count: Optional[int] = None
    """Number of modified but unstaged files."""

    untracked_count: Optional[int] = None
    """Number of untracked files."""

    # Remote tracking
    ahead_count: Optional[int] = None
    """Number of commits ahead of upstream."""

    behind_count: Optional[int] = None
    """Number of commits behind upstream."""

    upstream_branch: Optional[str] = None
    """The upstream branch being tracked (e.g., 'origin/main')."""

    remote_url: Optional[str] = None
    """The URL of the 'origin' remote."""

    def to_dict(self) -> Dict[str, Any]:
        """Convert to dictionary, excluding None values."""
        return {k: v for k, v in asdict(self).items() if v is not None}


@dataclass
class WorkspaceContext:
    """
    Workspace and project context for an event.

    This captures information about the project/repository being worked on
    and any files that were referenced in the prompt or action.
    """

    repo_name: Optional[str] = None
    """Name of the repository (directory name)."""

    repo_path: Optional[str] = None
    """Absolute path to the repository root."""

    working_directory: Optional[str] = None
    """Current working directory when the event occurred."""

    files_referenced: Optional[List[str]] = None
    """List of file paths referenced in the prompt or action."""

    def to_dict(self) -> Dict[str, Any]:
        """Convert to dictionary, excluding None values."""
        return {k: v for k, v in asdict(self).items() if v is not None}
