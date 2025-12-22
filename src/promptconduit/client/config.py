"""
Configuration management for PromptConduit client.

Handles loading API keys and URLs from environment variables.
"""

import os
from dataclasses import dataclass
from typing import Optional


# Default API URL for PromptConduit managed service
DEFAULT_API_URL = "https://api.promptconduit.dev"


@dataclass
class Config:
    """
    Configuration for the PromptConduit client.

    Attributes:
        api_key: API key for authentication
        api_url: Base URL for the API
        debug: Enable debug mode (includes raw events)
        timeout_seconds: HTTP request timeout
    """

    api_key: str
    api_url: str = DEFAULT_API_URL
    debug: bool = False
    timeout_seconds: int = 30

    @property
    def is_configured(self) -> bool:
        """Check if the client is properly configured with an API key."""
        return bool(self.api_key)


def get_config() -> Config:
    """
    Load configuration from environment variables.

    Environment variables:
    - PROMPTCONDUIT_API_KEY: Required. Your API key.
    - PROMPTCONDUIT_API_URL: Optional. API URL (default: https://api.promptconduit.dev)
    - PROMPTCONDUIT_DEBUG: Optional. Set to "1" to enable debug mode.
    - PROMPTCONDUIT_TIMEOUT: Optional. Request timeout in seconds (default: 30).

    Returns:
        Config object with loaded settings

    Example:
        >>> config = get_config()
        >>> if config.is_configured:
        ...     client = PromptConduitClient(config)
    """
    api_key = os.environ.get("PROMPTCONDUIT_API_KEY", "")
    api_url = os.environ.get("PROMPTCONDUIT_API_URL", DEFAULT_API_URL)
    debug = os.environ.get("PROMPTCONDUIT_DEBUG", "") == "1"

    timeout_str = os.environ.get("PROMPTCONDUIT_TIMEOUT", "30")
    try:
        timeout_seconds = int(timeout_str)
    except ValueError:
        timeout_seconds = 30

    return Config(
        api_key=api_key,
        api_url=api_url.rstrip("/"),
        debug=debug,
        timeout_seconds=timeout_seconds,
    )


def get_api_key() -> Optional[str]:
    """
    Get the API key from environment.

    Returns:
        API key string or None if not set
    """
    key = os.environ.get("PROMPTCONDUIT_API_KEY", "")
    return key if key else None


def get_api_url() -> str:
    """
    Get the API URL from environment.

    Returns:
        API URL string (defaults to production URL)
    """
    url = os.environ.get("PROMPTCONDUIT_API_URL", DEFAULT_API_URL)
    return url.rstrip("/")
