"""
Tool-specific adapters for PromptConduit.

Each adapter translates native hook events from a specific AI coding tool
into the canonical PromptConduit event schema.
"""

from promptconduit.adapters.base import BaseAdapter
from promptconduit.adapters.claude_code import ClaudeCodeAdapter
from promptconduit.adapters.cursor import CursorAdapter
from promptconduit.adapters.gemini import GeminiAdapter

__all__ = [
    "BaseAdapter",
    "ClaudeCodeAdapter",
    "CursorAdapter",
    "GeminiAdapter",
]
