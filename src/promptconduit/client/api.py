"""
HTTP client for PromptConduit API.

Handles sending canonical events to the PromptConduit API,
with support for both synchronous and asynchronous (forked) sending.
"""

import json
import os
import urllib.request
import urllib.error
from typing import Optional, Dict, Any, List
from dataclasses import dataclass

from promptconduit.schema.events import CanonicalEvent
from promptconduit.client.config import Config, get_config
from promptconduit.version import __version__


@dataclass
class ApiResponse:
    """
    Response from the PromptConduit API.

    Attributes:
        success: Whether the request succeeded
        status_code: HTTP status code
        data: Response body (parsed JSON)
        error: Error message if request failed
    """

    success: bool
    status_code: int = 0
    data: Optional[Dict[str, Any]] = None
    error: Optional[str] = None


class PromptConduitClient:
    """
    HTTP client for the PromptConduit API.

    This client handles sending canonical events to the API.
    It supports both blocking and non-blocking (forked) sends.

    Example:
        >>> client = PromptConduitClient()
        >>> if client.is_configured:
        ...     event = adapter.translate_event(native_event)
        ...     client.send_event_async(event)  # Non-blocking
    """

    def __init__(self, config: Optional[Config] = None):
        """
        Initialize the client.

        Args:
            config: Configuration object. If not provided, loads from environment.
        """
        self.config = config or get_config()
        self.user_agent = f"PromptConduit-Adapters/{__version__}"

    @property
    def is_configured(self) -> bool:
        """Check if the client is properly configured."""
        return self.config.is_configured

    def send_event(self, event: CanonicalEvent) -> ApiResponse:
        """
        Send a canonical event to the API (blocking).

        This method blocks until the request completes.
        For non-blocking behavior, use send_event_async().

        Args:
            event: The canonical event to send

        Returns:
            ApiResponse with result details
        """
        if not self.is_configured:
            return ApiResponse(success=False, error="API key not configured")

        return self._send_json(
            endpoint="/v1/events/ingest",
            data=event.to_dict(),
        )

    def send_event_async(self, event: CanonicalEvent) -> None:
        """
        Send a canonical event to the API (non-blocking).

        This method forks a child process to send the event,
        allowing the parent process to continue immediately.
        On platforms without fork (Windows), falls back to blocking send.

        Args:
            event: The canonical event to send
        """
        if not self.is_configured:
            return

        try:
            pid = os.fork()
            if pid == 0:
                # Child process - send and exit
                try:
                    self.send_event(event)
                finally:
                    os._exit(0)
            # Parent process continues immediately
        except (AttributeError, OSError):
            # os.fork() not available (Windows) - send synchronously
            self.send_event(event)

    def send_events_batch(self, events: List[CanonicalEvent]) -> ApiResponse:
        """
        Send multiple events in a single request (blocking).

        More efficient than sending events individually when you have
        multiple events to send at once.

        Args:
            events: List of canonical events to send

        Returns:
            ApiResponse with result details
        """
        if not self.is_configured:
            return ApiResponse(success=False, error="API key not configured")

        if not events:
            return ApiResponse(success=True, data={"count": 0})

        return self._send_json(
            endpoint="/v1/events/ingest-batch",
            data={"events": [e.to_dict() for e in events]},
        )

    def _send_json(self, endpoint: str, data: Dict[str, Any]) -> ApiResponse:
        """
        Send JSON data to an API endpoint.

        Args:
            endpoint: API endpoint path (e.g., "/v1/events/ingest")
            data: Dictionary to send as JSON body

        Returns:
            ApiResponse with result details
        """
        url = f"{self.config.api_url}{endpoint}"

        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {self.config.api_key}",
            "User-Agent": self.user_agent,
        }

        try:
            body = json.dumps(data).encode("utf-8")
            req = urllib.request.Request(
                url, data=body, headers=headers, method="POST"
            )

            with urllib.request.urlopen(
                req, timeout=self.config.timeout_seconds
            ) as response:
                status_code = response.status
                try:
                    response_data = json.loads(response.read().decode("utf-8"))
                except json.JSONDecodeError:
                    response_data = None

                return ApiResponse(
                    success=status_code in (200, 201),
                    status_code=status_code,
                    data=response_data,
                )

        except urllib.error.HTTPError as e:
            error_body = None
            try:
                error_body = e.read().decode("utf-8")
            except Exception:
                pass

            return ApiResponse(
                success=False,
                status_code=e.code,
                error=error_body or str(e),
            )

        except urllib.error.URLError as e:
            return ApiResponse(
                success=False,
                error=f"Connection error: {e.reason}",
            )

        except Exception as e:
            return ApiResponse(
                success=False,
                error=str(e),
            )


# Convenience functions for simple usage


def send_event(event: CanonicalEvent) -> ApiResponse:
    """
    Send a canonical event to the API (blocking).

    Convenience function that creates a client and sends the event.

    Args:
        event: The canonical event to send

    Returns:
        ApiResponse with result details
    """
    client = PromptConduitClient()
    return client.send_event(event)


def send_event_async(event: CanonicalEvent) -> None:
    """
    Send a canonical event to the API (non-blocking).

    Convenience function that creates a client and sends the event
    in a forked process.

    Args:
        event: The canonical event to send
    """
    client = PromptConduitClient()
    client.send_event_async(event)
