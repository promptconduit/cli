#!/usr/bin/env python3
"""
PromptConduit CLI.

Command-line interface for managing PromptConduit adapters.

Commands:
    promptconduit install <tool>    Install hooks for a tool
    promptconduit uninstall <tool>  Remove hooks for a tool
    promptconduit status            Show installed adapters
    promptconduit test              Test API connectivity
"""

import argparse
import json
import os
import shutil
import sys
from pathlib import Path
from typing import Optional

from promptconduit.version import __version__
from promptconduit.client.config import get_config
from promptconduit.client.api import PromptConduitClient


# Tool configuration paths
TOOL_CONFIGS = {
    "claude-code": {
        "settings_path": Path.home() / ".claude" / "settings.json",
        "hooks_key": "hooks",
    },
    "cursor": {
        "settings_path": Path.home() / ".cursor" / "hooks.json",
        "hooks_key": "hooks",
    },
    "gemini": {
        "settings_path": Path.home() / ".gemini" / "settings.json",
        "hooks_key": "hooks",
    },
}


def get_hook_script_path() -> str:
    """Get the path to the hook script."""
    # Check if running from installed package
    try:
        import promptconduit

        pkg_dir = Path(promptconduit.__file__).parent.parent.parent
        script_path = pkg_dir / "scripts" / "promptconduit_hook.py"
        if script_path.exists():
            return str(script_path)
    except Exception:
        pass

    # Fall back to relative path from this file
    cli_dir = Path(__file__).parent
    script_path = cli_dir.parent.parent.parent / "scripts" / "promptconduit_hook.py"
    if script_path.exists():
        return str(script_path)

    # Last resort - assume it's on PATH
    return "promptconduit_hook.py"


def cmd_install(args: argparse.Namespace) -> int:
    """Install hooks for a tool."""
    tool = args.tool.lower()

    if tool not in TOOL_CONFIGS:
        print(f"Error: Unknown tool '{tool}'")
        print(f"Supported tools: {', '.join(TOOL_CONFIGS.keys())}")
        return 1

    config = TOOL_CONFIGS[tool]
    settings_path = config["settings_path"]

    # Check if API key is configured
    api_config = get_config()
    if not api_config.is_configured:
        print("Warning: PROMPTCONDUIT_API_KEY is not set.")
        print("Set it before using the hooks:")
        print("  export PROMPTCONDUIT_API_KEY='your-api-key'")
        print()

    # Load existing settings or create new
    settings = {}
    if settings_path.exists():
        try:
            with open(settings_path, "r") as f:
                settings = json.load(f)
        except json.JSONDecodeError:
            print(f"Warning: Could not parse {settings_path}, creating new file")

    # Get hook script path
    script_path = get_hook_script_path()

    # Build hook configuration based on tool
    if tool == "claude-code":
        hooks = _build_claude_code_hooks(script_path)
    elif tool == "cursor":
        hooks = _build_cursor_hooks(script_path)
    elif tool == "gemini":
        hooks = _build_gemini_hooks(script_path)
    else:
        print(f"Error: No hook configuration for {tool}")
        return 1

    # Merge hooks into settings
    if "hooks" not in settings:
        settings["hooks"] = {}

    for event_name, event_hooks in hooks.items():
        if event_name not in settings["hooks"]:
            settings["hooks"][event_name] = []
        # Add our hooks (avoid duplicates)
        for hook in event_hooks:
            if hook not in settings["hooks"][event_name]:
                settings["hooks"][event_name].append(hook)

    # Ensure directory exists
    settings_path.parent.mkdir(parents=True, exist_ok=True)

    # Write settings
    with open(settings_path, "w") as f:
        json.dump(settings, f, indent=2)

    print(f"Installed PromptConduit hooks for {tool}")
    print(f"Configuration: {settings_path}")
    return 0


def cmd_uninstall(args: argparse.Namespace) -> int:
    """Uninstall hooks for a tool."""
    tool = args.tool.lower()

    if tool not in TOOL_CONFIGS:
        print(f"Error: Unknown tool '{tool}'")
        return 1

    config = TOOL_CONFIGS[tool]
    settings_path = config["settings_path"]

    if not settings_path.exists():
        print(f"No configuration found for {tool}")
        return 0

    # Load settings
    try:
        with open(settings_path, "r") as f:
            settings = json.load(f)
    except json.JSONDecodeError:
        print(f"Error: Could not parse {settings_path}")
        return 1

    # Remove our hooks
    if "hooks" in settings:
        for event_name in list(settings["hooks"].keys()):
            settings["hooks"][event_name] = [
                h
                for h in settings["hooks"][event_name]
                if "promptconduit" not in str(h).lower()
            ]
            # Remove empty lists
            if not settings["hooks"][event_name]:
                del settings["hooks"][event_name]

        # Remove empty hooks dict
        if not settings["hooks"]:
            del settings["hooks"]

    # Write settings
    with open(settings_path, "w") as f:
        json.dump(settings, f, indent=2)

    print(f"Uninstalled PromptConduit hooks for {tool}")
    return 0


def cmd_status(args: argparse.Namespace) -> int:
    """Show status of installed adapters."""
    print(f"PromptConduit Adapters v{__version__}")
    print()

    # Check API configuration
    config = get_config()
    if config.is_configured:
        print(f"API Key: {'*' * 8}...{config.api_key[-4:]}")
        print(f"API URL: {config.api_url}")
    else:
        print("API Key: Not configured")
        print("  Set PROMPTCONDUIT_API_KEY environment variable")
    print()

    # Check each tool
    print("Installed hooks:")
    for tool, tool_config in TOOL_CONFIGS.items():
        settings_path = tool_config["settings_path"]
        if settings_path.exists():
            try:
                with open(settings_path, "r") as f:
                    settings = json.load(f)
                if "hooks" in settings:
                    # Check if any hook contains promptconduit
                    has_pc = any(
                        "promptconduit" in str(hook).lower()
                        for hooks in settings["hooks"].values()
                        for hook in hooks
                    )
                    if has_pc:
                        print(f"  {tool}: Installed")
                        continue
            except Exception:
                pass
        print(f"  {tool}: Not installed")

    return 0


def cmd_test(args: argparse.Namespace) -> int:
    """Test API connectivity."""
    config = get_config()

    if not config.is_configured:
        print("Error: PROMPTCONDUIT_API_KEY is not set")
        return 1

    print(f"Testing connection to {config.api_url}...")

    client = PromptConduitClient(config)

    # Create a test event
    from promptconduit.schema.events import CanonicalEvent, EventType, Tool
    import uuid
    from datetime import datetime, timezone

    test_event = CanonicalEvent(
        tool=Tool.OTHER,
        event_type=EventType.SESSION_START,
        event_id=str(uuid.uuid4()),
        timestamp=datetime.now(timezone.utc).isoformat(),
        adapter_version=__version__,
    )

    response = client.send_event(test_event)

    if response.success:
        print("Connection successful!")
        return 0
    else:
        print(f"Connection failed: {response.error}")
        return 1


def _build_claude_code_hooks(script_path: str) -> dict:
    """Build hook configuration for Claude Code."""
    hook_command = f'python3 "{script_path}"'
    hook_entry = {"type": "command", "command": hook_command, "timeout": 5000}

    return {
        "UserPromptSubmit": [{"hooks": [hook_entry]}],
        "PreToolUse": [{"matcher": "*", "hooks": [hook_entry]}],
        "PostToolUse": [{"matcher": "*", "hooks": [hook_entry]}],
        "SessionStart": [{"hooks": [hook_entry]}],
        "SessionEnd": [{"hooks": [hook_entry]}],
    }


def _build_cursor_hooks(script_path: str) -> dict:
    """Build hook configuration for Cursor."""
    hook_command = f'python3 "{script_path}"'
    hook_entry = {"command": hook_command}

    return {
        "beforeSubmitPrompt": [hook_entry],
        "beforeShellExecution": [hook_entry],
        "afterShellExecution": [hook_entry],
        "afterFileEdit": [hook_entry],
    }


def _build_gemini_hooks(script_path: str) -> dict:
    """Build hook configuration for Gemini CLI."""
    hook_command = f'python3 "{script_path}"'
    hook_entry = {
        "type": "command",
        "command": hook_command,
        "name": "promptconduit",
        "timeout": 5000,
    }

    return {
        "BeforeAgent": [{"hooks": [hook_entry]}],
        "BeforeTool": [{"matcher": "*", "hooks": [hook_entry]}],
        "AfterTool": [{"matcher": "*", "hooks": [hook_entry]}],
        "SessionStart": [{"hooks": [hook_entry]}],
        "SessionEnd": [{"hooks": [hook_entry]}],
    }


def main() -> int:
    """Main entry point."""
    parser = argparse.ArgumentParser(
        description="PromptConduit - Universal AI coding assistant event capture"
    )
    parser.add_argument(
        "--version", action="version", version=f"promptconduit {__version__}"
    )

    subparsers = parser.add_subparsers(dest="command", help="Commands")

    # install command
    install_parser = subparsers.add_parser("install", help="Install hooks for a tool")
    install_parser.add_argument(
        "tool", choices=["claude-code", "cursor", "gemini"], help="Tool to install"
    )

    # uninstall command
    uninstall_parser = subparsers.add_parser(
        "uninstall", help="Uninstall hooks for a tool"
    )
    uninstall_parser.add_argument(
        "tool", choices=["claude-code", "cursor", "gemini"], help="Tool to uninstall"
    )

    # status command
    subparsers.add_parser("status", help="Show installed adapters")

    # test command
    subparsers.add_parser("test", help="Test API connectivity")

    args = parser.parse_args()

    if args.command == "install":
        return cmd_install(args)
    elif args.command == "uninstall":
        return cmd_uninstall(args)
    elif args.command == "status":
        return cmd_status(args)
    elif args.command == "test":
        return cmd_test(args)
    else:
        parser.print_help()
        return 0


if __name__ == "__main__":
    sys.exit(main())
