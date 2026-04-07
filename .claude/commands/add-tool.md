---
description: Guide for adding support for a new AI coding tool to PromptConduit
---

Walk me through adding support for a new AI tool to the PromptConduit CLI. The tool name I want to add is: $ARGUMENTS

## Steps to Add a New Tool

### 1. Register the tool name (`internal/envelope/envelope.go`)

Add the tool slug to `SupportedTools()` and handle any aliases in `IsValidTool()`:

```go
func SupportedTools() []string {
    return []string{"claude-code", "cursor", "gemini-cli", "<new-tool>"}
}

func IsValidTool(toolName string) bool {
    // add alias handling if needed (e.g. "gemini" → "gemini-cli")
}
```

### 2. Add install logic (`cmd/install.go`)

Create `installNewTool(exePath string) error` following the pattern of existing tools:
- Determine the tool's config file path (usually `~/<tool-config-dir>/settings.json` or similar)
- Build a hook configuration struct with the hooks the tool supports
- Load existing config, merge in PromptConduit hooks, write back
- Print confirmation with the config file path

Register it in the `install` command's switch statement.

### 3. Add uninstall logic (`cmd/uninstall.go`)

Create `uninstallNewTool() error` following the pattern of existing tools:
- Load the tool's config file
- Use `containsPromptConduit()` to find and remove only PromptConduit hooks
- Write the cleaned config back
- Handle the case where the file doesn't exist or no hooks were found

Register it in the `uninstall` command's switch statement.

### 4. Add a sample config (`configs/<new-tool>/`)

Add a sample hook configuration file showing what the installed hooks look like. Follow the pattern in `configs/claude-code/hooks.json` or `configs/cursor/hooks.json`.

### 5. Update the README

Add the new tool to the supported tools table in `README.md` under both the "Real-time hooks" section and the quick start guide.

### 6. Server-side adapter (cross-repo, platform is private)

The CLI is a thin client — all event normalization happens in the platform's adapter layer. To complete support for the new tool, a server-side adapter must be created in the platform repo. **Open a GitHub issue** with:
- The tool name and hook event names
- Sample raw event payloads (from the tool's docs or by running with `PROMPTCONDUIT_DEBUG=1`)
- Links to the tool's hook/plugin documentation

This lets the maintainers coordinate the platform-side work.

---

## Hook Event Pattern

Each tool has its own hook event names and config format. The CLI hook command (`promptconduit hook`) receives the raw event JSON on stdin and forwards it wrapped in a `RawEventEnvelope`. No parsing or normalization happens in the CLI.

Key principle: **the CLI never blocks the tool**. The hook command always exits immediately; sending is done in a detached subprocess.
