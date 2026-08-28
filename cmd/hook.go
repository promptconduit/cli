package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/cost"
	"github.com/promptconduit/cli/internal/enrich"
	"github.com/promptconduit/cli/internal/envelope"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/promptconduit/cli/internal/logger"
	"github.com/promptconduit/cli/internal/sync"
	"github.com/promptconduit/cli/internal/transcript"
	"github.com/spf13/cobra"
)

var (
	sendEvent bool
	// toolOverride is set by the install command (`--tool codex`,
	// `--tool copilot`) when the host AI tool's hook payload can't be
	// reliably distinguished from other tools by inspecting fields
	// alone — Codex in particular sends `hook_event_name`, which would
	// otherwise be tagged as Claude Code.
	toolOverride string
)

var hookCmd = &cobra.Command{
	Use:    "hook",
	Short:  "Process hook events from AI tools",
	Long:   `Internal command called by AI tool hooks. Reads JSON events from stdin and sends to API.`,
	Hidden: true,
	RunE:   runHook,
}

func init() {
	hookCmd.Flags().BoolVar(&sendEvent, "send-event", false, "Send event data from stdin (internal use)")
	hookCmd.Flags().StringVar(&toolOverride, "tool", "", "Override tool name (codex, copilot, etc.); set by the install command")
}

func runHook(cmd *cobra.Command, args []string) error {
	if sendEvent {
		return sendEnvelopeFromStdin()
	}
	return processHookEvent()
}

// processHookEvent is the main hook entry point - wraps native event in envelope
func processHookEvent() error {
	defer outputContinueResponse()

	// Load config first so we can set the debug flag for the logger and the
	// enabled flag for the local event log before emitting any lines.
	cfg := client.LoadConfig()
	logger.SetDebug(cfg.Debug)
	eventlog.SetEnabled(cfg.EventLogEnabled())

	// Opportunistically enforce local hook-history retention. Runs after the
	// event is captured (deferred); cheap on the hot path — a couple of stats
	// unless the log has grown past its size ceiling, in which case expired
	// records (older than the retention window) are trimmed oldest-first.
	defer eventlog.MaybePrune(cfg.RetentionDays())

	logger.Debug("Hook started")

	// Read raw input from stdin
	rawInput, err := io.ReadAll(os.Stdin)
	if err != nil {
		debugLog("Failed to read stdin: %v", err)
		logger.Error("Failed to read stdin: %v", err)
		eventlog.RecordDrop("read_error", err.Error())
		return nil
	}

	if len(rawInput) == 0 {
		debugLog("Empty input, skipping")
		logger.Debug("Empty input, skipping")
		eventlog.RecordDrop("empty_stdin", "")
		return nil
	}

	previewLen := len(rawInput)
	if previewLen > 200 {
		previewLen = 200
	}
	logger.Debug("Received %d bytes: %s", len(rawInput), string(rawInput[:previewLen]))

	// Parse just enough to detect tool and event name
	var nativeEvent map[string]interface{}
	if err := json.Unmarshal(rawInput, &nativeEvent); err != nil {
		debugLog("Failed to parse JSON: %v", err)
		logger.Error("Failed to parse JSON: %v (raw=%q)", err, string(rawInput[:previewLen]))
		eventlog.RecordDrop("parse_error", err.Error())
		return nil
	}

	// Realtime cost tracking is local and unconditional: extract token cost from
	// Cursor agent-hook payloads here, BEFORE the platform-send gate below, so
	// the cost meter works for any Cursor hook install — with or without an API
	// key, and with no separate setup step. Best-effort; never blocks the hook.
	if _, isCursor := nativeEvent["cursor_version"]; isCursor {
		recordCursorCost(rawInput)
	}

	// Free / local-only mode: when no API key is set (or local_only is on) we
	// still build and capture every event to the local event log below — we just
	// never send it to the cloud. This is the legitimate Free experience, not an
	// error, so no drop is recorded and no error is logged.
	shouldSend := cfg.ShouldSend()
	if !shouldSend {
		debugLog("local-only mode — capturing locally, not sending")
		logger.Debug("local-only mode — events captured to %s, nothing sent", eventlog.EventsJSONLPath())
	}

	// Detect tool (simple heuristics)
	tool := detectTool(nativeEvent)
	hookEvent := getHookEventName(nativeEvent)
	sessionID, promptID := envelope.ExtractIDs(tool, nativeEvent)

	logger.Debug("Detected tool: %s, hook event: %s", tool, hookEvent)

	cwd := getWorkingDirectory(nativeEvent)

	// Write to local events file for macOS app
	writeLocalEvent(hookEvent, cwd, sessionID)

	// Trigger auto-sync on SessionEnd or Stop events
	// SessionEnd: Fires when user explicitly ends session (rare - users often just close terminal)
	// Stop: Fires after each Claude response - gives us incremental sync opportunities
	// The sync logic deduplicates via hash checking, so frequent triggers are safe
	// NOTE: Called directly (not in goroutine) since it spawns a subprocess and returns quickly
	// Auto-sync uploads transcripts to the platform, so it only runs in cloud
	// mode — in Free / local-only mode there is nothing to upload.
	if shouldSend && (hookEvent == "SessionEnd" || hookEvent == "Stop") {
		if sessionID != "" {
			triggerAutoSync(sessionID)
		}
	}

	// Compute the enrichments map — every slug is best-effort and isolated,
	// so a failing enricher can never block the hook or drop the event.
	transcriptPath, _ := nativeEvent["transcript_path"].(string)
	enrichments := enrich.Run(&enrich.Context{
		Tool:           tool,
		HookEvent:      hookEvent,
		SessionID:      sessionID,
		PromptID:       promptID,
		RawEvent:       nativeEvent,
		RawJSON:        rawInput,
		Cwd:            cwd,
		TranscriptPath: transcriptPath,
	})

	// For UserPromptSubmit events, check if the user's message includes attachments
	// We extract from the transcript which should have the message by now
	if hookEvent == "UserPromptSubmit" {
		extractor := transcript.GetExtractor(tool)
		if extractor.SupportsAttachments() {
			logger.Debug("UserPromptSubmit: checking for attachments in current message")
			attachments, _, err := extractor.ExtractAttachments(nativeEvent)
			if err != nil {
				logger.Error("Error extracting attachments: %v", err)
			} else if len(attachments) > 0 {
				logger.Debug("Found %d attachments in current message, sending with event", len(attachments))

				// Build attachment metadata for envelope and binary data for multipart
				envAttachments := make([]envelope.AttachmentMetadata, len(attachments))
				attachmentData := make([]client.AttachmentData, len(attachments))

				for i, att := range attachments {
					attachmentID := uuid.New().String()
					envAttachments[i] = envelope.AttachmentMetadata{
						AttachmentID: attachmentID,
						Filename:     att.Filename,
						ContentType:  att.MediaType,
						SizeBytes:    int64(len(att.Data)),
						Type:         att.Type,
					}
					attachmentData[i] = client.AttachmentData{
						AttachmentID: attachmentID,
						Filename:     att.Filename,
						ContentType:  att.MediaType,
						Data:         att.Data,
					}
					logger.Debug("Attachment %d: %s (%s, %d bytes)", i+1, att.Filename, att.MediaType, len(att.Data))
				}

				// Create envelope with attachment metadata
				env := envelope.NewWithAttachments(Version, tool, hookEvent, sessionID, promptID, rawInput, enrichments, envAttachments)

				// Always record the captured event locally, before any send.
				captureEnvelope(env)

				if shouldSend {
					// Send via multipart with binary attachments
					if err := client.NewClient(cfg, Version).SendEnvelopeWithAttachmentsAsync(env, attachmentData); err != nil {
						logger.Error("Failed to send envelope with attachments (event=%s, tool=%s): %v", hookEvent, tool, err)
					} else {
						logger.Debug("UserPromptSubmit with %d attachments sent successfully", len(attachments))
					}
				} else {
					logger.Debug("local-only: captured UserPromptSubmit with %d attachments, not sent", len(attachments))
				}
				// Return here - we've handled the event with attachments, don't process again below
				return nil
			}
		}
	}

	// Create envelope with raw payload (no attachments case, or non-UserPromptSubmit events)
	env := envelope.New(Version, tool, hookEvent, sessionID, promptID, rawInput, enrichments)

	logger.Debug("Created envelope: tool=%s, event=%s", tool, hookEvent)

	// Always record the captured event locally, before any send.
	captureEnvelope(env)

	if !shouldSend {
		return nil
	}

	// Send async
	if err := client.NewClient(cfg, Version).SendEnvelopeAsync(env); err != nil {
		debugLog("Failed to send envelope async: %v", err)
		logger.Error("Failed to spawn async sender (event=%s, tool=%s): %v", hookEvent, tool, err)
	}

	logger.Debug("Envelope queued for async send")
	return nil
}

// captureEnvelope writes the exact envelope JSON we would POST to the local
// capture log (~/.promptconduit/events.jsonl) before any network send. This is
// the unconditional local record of the event — it runs for both cloud and
// Free / local-only installs. Best-effort: serialization failure is logged at
// debug and the line is dropped rather than disturbing the hook.
func captureEnvelope(env *envelope.RawEventEnvelope) {
	data, err := env.ToJSON()
	if err != nil {
		logger.Debug("capture: failed to serialize envelope: %v", err)
		return
	}
	eventlog.RecordCapture(data)
}

// detectTool identifies which AI tool generated the event.
//
// Precedence: --tool flag (set by install for Codex/Copilot whose payloads
// can't be told apart from Claude Code by content) → PROMPTCONDUIT_TOOL
// env var → heuristic field-presence detection.
func detectTool(event map[string]interface{}) string {
	if toolOverride != "" {
		return toolOverride
	}
	// Check environment variable override next.
	if tool := os.Getenv(client.EnvTool); tool != "" {
		return tool
	}

	// Cursor: has cursor_version field. Must precede the hook_event_name check —
	// Cursor hooks also carry hook_event_name, so checking that first would
	// mislabel every Cursor event as claude-code.
	if _, ok := event["cursor_version"]; ok {
		return "cursor"
	}

	// Claude Code: has hook_event_name or hook_event field
	if _, ok := event["hook_event_name"]; ok {
		return "claude-code"
	}
	if _, ok := event["hook_event"]; ok {
		return "claude-code"
	}

	// Gemini: has gemini_session field
	if _, ok := event["gemini_session"]; ok {
		return "gemini-cli"
	}

	// Generic: has event field
	if _, ok := event["event"]; ok {
		return "unknown"
	}

	return "unknown"
}

// getHookEventName extracts the hook event name from native event
func getHookEventName(event map[string]interface{}) string {
	// Claude Code uses hook_event_name or hook_event
	if name, ok := event["hook_event_name"].(string); ok {
		return name
	}
	if name, ok := event["hook_event"].(string); ok {
		return name
	}

	// Generic event field
	if name, ok := event["event"].(string); ok {
		return name
	}

	return ""
}

// firstStringArrayEntry returns the first non-empty string in a JSON-decoded
// array (Unmarshal into map[string]interface{} yields []interface{}).
func firstStringArrayEntry(v interface{}) string {
	arr, ok := v.([]interface{})
	if !ok {
		return ""
	}
	for _, item := range arr {
		if s, ok := item.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// getWorkingDirectory extracts working directory from native event
func getWorkingDirectory(event map[string]interface{}) string {
	// Claude Code uses cwd
	if cwd, ok := event["cwd"].(string); ok && cwd != "" {
		return cwd
	}

	// Cursor might use workspace_dir
	if dir, ok := event["workspace_dir"].(string); ok && dir != "" {
		return dir
	}

	// Cursor agent hooks send workspace_roots (the open folder(s)), not cwd.
	// Prefer that over os.Getwd(): the hook subprocess cwd is often whichever
	// folder Cursor last launched from (e.g. a config repo), not the workspace
	// the agent is actually editing — which then mis-tags git/vcs on every event.
	if root := firstStringArrayEntry(event["workspace_roots"]); root != "" {
		return root
	}

	// Fallback to current directory
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}

	return ""
}

// sendEnvelopeFromStdin sends envelope data directly (called by async subprocess)
func sendEnvelopeFromStdin() error {
	cfg := client.LoadConfig()
	logger.SetDebug(cfg.Debug)
	// This runs as a fresh subprocess, so the enabled flag must be set here
	// too — it isn't inherited from the parent hook process.
	eventlog.SetEnabled(cfg.EventLogEnabled())

	logger.Debug("Async subprocess started")

	inputData, err := io.ReadAll(os.Stdin)
	if err != nil {
		logger.Error("Async subprocess failed to read stdin: %v", err)
		eventlog.RecordDrop("read_error", "async subprocess: "+err.Error())
		return fmt.Errorf("failed to read stdin: %w", err)
	}

	logger.Debug("Async subprocess received %d bytes", len(inputData))

	if !cfg.IsConfigured() {
		logger.Error("Async subprocess: API key not configured — envelope dropped")
		eventlog.RecordDrop("not_configured", "async subprocess")
		return fmt.Errorf("API key not configured")
	}

	logger.Debug("Async subprocess sending to API: %s", cfg.APIURL)
	apiClient := client.NewClient(cfg, Version)
	err = apiClient.SendEnvelopeDirect(inputData)
	if err != nil {
		logger.Error("Async subprocess API send failed (url=%s): %v", cfg.APIURL, err)
		return err
	}
	logger.Debug("Async subprocess: envelope sent successfully")
	return nil
}

// recordCursorCost extracts token cost from a Cursor agent-hook payload and
// appends it to the local cost feed. This is what makes realtime cost tracking
// part of the standard PromptConduit setup: the `stop`/`afterAgentResponse`
// hooks that `install cursor` already wires carry exact tokens, so the cost
// meter "just works" with no extra command. Local-only and best-effort — it
// reads no API key, sends nothing, and ignores all errors so the hook never
// blocks the editor.
func recordCursorCost(rawInput []byte) {
	table, err := cost.LoadBundledPriceTable()
	if err != nil {
		return
	}
	ev, cwd, ok := cost.ParseCursorHookPayload(rawInput, table)
	if !ok {
		return // not a token-bearing Cursor event
	}
	ev.Timestamp = time.Now().UTC().Format(time.RFC3339)
	_ = cost.AppendCursorEvent(ev, cwd)
}

// outputContinueResponse writes the success response to stdout
func outputContinueResponse() {
	response := map[string]interface{}{
		"continue": true,
	}
	data, _ := json.Marshal(response)
	fmt.Println(string(data))
}

// debugLog mirrors a debug-level message to stderr (real-time visibility
// when the user runs with debug mode). The persistent log file is handled
// separately by the logger package.
func debugLog(format string, args ...interface{}) {
	cfg := client.LoadConfig()
	if cfg.Debug {
		_, _ = fmt.Fprintf(os.Stderr, "[promptconduit] "+format+"\n", args...)
	}
}

// triggerAutoSync triggers automatic transcript sync after SessionEnd or Stop events
// Spawns a subprocess immediately to avoid being killed when the main process exits
// Uses hash-based deduplication so frequent triggers are efficient (only syncs if file changed)
func triggerAutoSync(sessionID string) {
	logger.Debug("Auto-sync: triggered for session %s", sessionID)

	// Find transcript file for this session (fast operation, do synchronously)
	transcriptPath, err := sync.FindTranscriptBySessionID(sessionID)
	if err != nil {
		logger.Debug("Auto-sync: could not find transcript for session %s: %v", sessionID, err)
		return
	}

	logger.Debug("Auto-sync: found transcript at %s", transcriptPath)

	// Spawn async subprocess to sync this file
	// Use --delay flag so the subprocess waits for transcript to be fully flushed
	exe, err := os.Executable()
	if err != nil {
		logger.Debug("Auto-sync: failed to get executable path: %v", err)
		return
	}

	cmd := exec.Command(exe, "sync", "--file", transcriptPath, "--delay", "1")
	if err := cmd.Start(); err != nil {
		logger.Debug("Auto-sync: failed to start sync subprocess: %v", err)
		return
	}

	// Release the process so it runs independently
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}

	logger.Debug("Auto-sync: sync subprocess started for session %s", sessionID)

	// Also retry any previously failed syncs (spawn as separate subprocess)
	go retryFailedSyncs(exe)
}

// retryFailedSyncs attempts to sync any previously failed transcripts
func retryFailedSyncs(exe string) {
	stateManager, err := sync.NewStateManager()
	if err != nil {
		logger.Debug("Auto-sync retry: failed to load state: %v", err)
		return
	}

	failedSyncs := stateManager.GetFailedSyncs()
	if len(failedSyncs) == 0 {
		return
	}

	logger.Debug("Auto-sync retry: found %d failed syncs to retry", len(failedSyncs))

	for _, failed := range failedSyncs {
		// Max 3 retries per file
		if failed.RetryCount >= 3 {
			logger.Debug("Auto-sync retry: skipping %s (exceeded max retries)", failed.SessionID)
			continue
		}

		cmd := exec.Command(exe, "sync", "--file", failed.FilePath)
		if err := cmd.Start(); err != nil {
			logger.Debug("Auto-sync retry: failed to start sync for %s: %v", failed.SessionID, err)
			continue
		}

		if cmd.Process != nil {
			_ = cmd.Process.Release()
		}

		logger.Debug("Auto-sync retry: started retry for session %s", failed.SessionID)
	}
}

// writeLocalEvent appends a lightweight status trace to
// ~/.promptconduit/hook-events so the macOS menu-bar app can tell when a
// session starts/stops. This is NOT the API payload — only {event, cwd,
// session_id, timestamp}. The full envelope is recorded separately by the
// eventlog package (~/.promptconduit/events.jsonl) at capture time.
func writeLocalEvent(hookEvent, cwd, sessionID string) {
	// Only write status-relevant events
	switch hookEvent {
	case "SessionStart", "UserPromptSubmit", "Stop":
		// Continue
	default:
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	eventsPath := filepath.Join(home, ".promptconduit", "hook-events")

	// Ensure directory exists
	dir := filepath.Dir(eventsPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}

	// Build event JSON
	event := fmt.Sprintf(`{"event":"%s","cwd":"%s","session_id":"%s","timestamp":"%s"}`,
		hookEvent, cwd, sessionID, time.Now().Format(time.RFC3339))

	// Append to file
	f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(event + "\n")
}
