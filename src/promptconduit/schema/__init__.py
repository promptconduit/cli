"""
Canonical event schema for PromptConduit.

All tool-specific events are normalized to this schema before
being sent to the PromptConduit API.
"""

from promptconduit.schema.events import (
    CanonicalEvent,
    EventType,
    Tool,
    PromptPayload,
    ToolPayload,
    SessionPayload,
)
from promptconduit.schema.context import (
    GitContext,
    WorkspaceContext,
)

__all__ = [
    "CanonicalEvent",
    "EventType",
    "Tool",
    "PromptPayload",
    "ToolPayload",
    "SessionPayload",
    "GitContext",
    "WorkspaceContext",
]
