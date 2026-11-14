package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	gosync "sync"
	"syscall"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/cost"
	"github.com/promptconduit/cli/internal/logger"
	"github.com/promptconduit/cli/internal/sync"
	"github.com/spf13/cobra"
)

var (
	syncDryRun bool
	syncForce  bool
	syncSince  string
	syncLimit  int
	syncFile   string
	syncDelay  int
)

// Chunking configuration
const (
	// CHUNK_SIZE is the number of messages per chunk (roughly 5-10MB per chunk)
	CHUNK_SIZE = 500
	// CHUNKED_THRESHOLD is the minimum number of messages to trigger chunked upload
	CHUNKED_THRESHOLD = 1000
	// CHUNKED_SIZE_THRESHOLD is the minimum file size in bytes to trigger chunked upload
	// This catches large files with few but huge messages (e.g., 71MB file with 832 messages)
	CHUNKED_SIZE_THRESHOLD = 5 * 1024 * 1024 // 5MB
)

var syncCmd = &cobra.Command{
	Use:   "sync [tool]",
	Short: "Sync AI assistant transcripts to PromptConduit",
	Long: `Sync conversation transcripts from AI coding assistants to PromptConduit.

This command reads transcript files from your local machine and uploads them
to PromptConduit for viewing and analysis. Already synced files are tracked
to avoid duplicate uploads.

Supported tools:
  - claude-code: Claude Code transcripts from ~/.claude/projects/
  - cursor: Cursor transcripts from ~/.cursor/projects/*/agent-transcripts/

Examples:
  promptconduit sync              # Sync all supported tools
  promptconduit sync claude-code  # Sync only Claude Code
  promptconduit sync cursor       # Sync only Cursor
  promptconduit sync --dry-run    # Show what would be synced
  promptconduit sync --force      # Re-sync already synced files
  promptconduit sync --since 2025-01-01
  promptconduit sync --limit 10   # Sync only 10 most recent

The sync state is tracked in ~/.config/promptconduit/sync_state.json`,
	RunE: runSync,
}

func init() {
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "Show what would be synced without uploading")
	syncCmd.Flags().BoolVar(&syncForce, "force", false, "Re-sync already synced files")
	syncCmd.Flags().StringVar(&syncSince, "since", "", "Only sync transcripts modified after this date (YYYY-MM-DD)")
	syncCmd.Flags().IntVar(&syncLimit, "limit", 0, "Maximum number of transcripts to sync (0 = unlimited)")
	syncCmd.Flags().StringVar(&syncFile, "file", "", "Sync a specific transcript file (for auto-sync)")
	syncCmd.Flags().IntVar(&syncDelay, "delay", 0, "Delay in seconds before syncing (for auto-sync to wait for file flush)")
	rootCmd.AddCommand(syncCmd)
}

// recordLockTimeout records a `sync --file` that gave up waiting for the
// transcript lock as a failed sync, keyed by session id (the transcript's file
// name without .jsonl — what a later successful sync clears), so
// retryFailedSyncs retries it.
func recordLockTimeout(sm *sync.StateManager, filePath string, cause error) error {
	sessionID := strings.TrimSuffix(filepath.Base(filePath), ".jsonl")
	sm.AddFailedSync(sessionID, filePath, cause.Error())
	return sm.Save()
}

// transcriptLockWait bounds how long a `sync --file` waits for another upload
// of the same transcript (large transcripts upload in chunks).
const transcriptLockWait = 10 * time.Minute

// checkSyncAllowed refuses an upload when the config forbids one. Local-only
// mode (config or PROMPTCONDUIT_LOCAL_ONLY) wins over an API key: the user
// asked for nothing to leave the machine, so a manual sync must not upload
// either — automatic sync is already gated the same way.
func checkSyncAllowed(cfg *client.Config) error {
	if cfg.LocalOnly {
		return fmt.Errorf("sync is disabled in local-only mode: transcripts stay on this machine\n\nTo enable cloud sync, run: promptconduit config set --local-only=false")
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("API key not configured. Run: promptconduit login")
	}
	return nil
}

func runSync(cmd *cobra.Command, args []string) error {
	// Handle delay (used by auto-sync to wait for transcript file flush)
	if syncDelay > 0 {
		time.Sleep(time.Duration(syncDelay) * time.Second)
	}

	// Load config
	config := client.LoadConfig()
	if err := checkSyncAllowed(config); err != nil {
		return err
	}

	// Single-file (auto-)sync: never upload the same transcript from two
	// processes at once. Wait for an in-flight upload of this file to finish,
	// then proceed — the state is loaded only after, so the hash check below
	// sees what that upload recorded and skips unchanged content.
	releaseMain := func() {}
	if syncFile != "" {
		release, ok := sync.LockTranscript(client.ConfigDir(), syncFile, transcriptLockWait)
		var once gosync.Once
		releaseMain = func() { once.Do(release) }
		defer releaseMain()
		if !ok {
			err := fmt.Errorf("another sync of %s is still running; gave up after %s", syncFile, transcriptLockWait)
			// Don't drop it silently: record a failed sync so the next hook's
			// retryFailedSyncs picks this transcript up again.
			if sm, smErr := sync.NewStateManager(); smErr == nil {
				_ = recordLockTimeout(sm, syncFile, err)
			}
			return err
		}
	}

	// Initialize state manager
	stateManager, err := sync.NewStateManager()
	if err != nil {
		return fmt.Errorf("failed to initialize state manager: %w", err)
	}

	// Handle single-file sync (used by auto-sync from SessionEnd hook)
	if syncFile != "" {
		return runSingleFileSync(config, stateManager, syncFile, releaseMain)
	}

	// Determine which tools to sync
	toolsToSync := []string{"claude-code", "cursor"} // Default: all supported
	if len(args) > 0 {
		toolsToSync = args
	}

	// Parse since date if provided
	var sinceTime time.Time
	if syncSince != "" {
		parsed, err := time.Parse("2006-01-02", syncSince)
		if err != nil {
			return fmt.Errorf("invalid --since date format (use YYYY-MM-DD): %w", err)
		}
		sinceTime = parsed
	}

	// Create API client
	apiClient := client.NewClient(config, Version)

	totalSynced := 0
	totalSkipped := 0
	totalErrors := 0

	for _, tool := range toolsToSync {
		parser, err := getParser(tool)
		if err != nil {
			fmt.Printf("⚠️  Skipping %s: %v\n", tool, err)
			continue
		}

		files, err := parser.GetTranscriptPaths()
		if err != nil {
			fmt.Printf("⚠️  Failed to list transcripts for %s: %v\n", tool, err)
			continue
		}

		if len(files) == 0 {
			fmt.Printf("📁 No transcripts found for %s\n", tool)
			continue
		}

		fmt.Printf("📁 Found %d transcript(s) for %s\n", len(files), tool)

		syncedThisRun := 0
		// Per-transcript upload lock, held from parse through upload; released
		// at the top of the next iteration (every `continue` path included).
		releaseFile := func() {}
		for _, filePath := range files {
			releaseFile()
			releaseFile = func() {}

			// Apply limit
			if syncLimit > 0 && totalSynced >= syncLimit {
				break
			}

			// Never upload a transcript an auto-sync is uploading right now:
			// skip it without waiting and let that sync handle it.
			if !syncDryRun {
				release, ok := sync.LockTranscript(client.ConfigDir(), filePath, 0)
				if !ok {
					totalSkipped++
					continue
				}
				releaseFile = release
			}

			// Parse file first to get hash
			conversation, err := parser.ParseFile(filePath)
			if err != nil {
				// Show truncated path for parse errors
				displayName := filepath.Base(filepath.Dir(filePath)) + "/" + filepath.Base(filePath)
				if len(displayName) > 60 {
					displayName = "..." + displayName[len(displayName)-57:]
				}
				fmt.Printf("  ❌ Parse error: %s: %v\n", displayName, err)
				totalErrors++
				continue
			}

			// Check if already synced (unless force)
			if !syncForce && stateManager.IsSynced(filePath, conversation.SourceFileHash) {
				totalSkipped++
				continue
			}

			// Check since date
			if !sinceTime.IsZero() {
				fileTime, err := time.Parse(time.RFC3339, conversation.StartedAt)
				if err == nil && fileTime.Before(sinceTime) {
					totalSkipped++
					continue
				}
			}

			// Show what we're about to sync
			displayName := filepath.Base(filepath.Dir(filePath)) + "/" + filepath.Base(filePath)
			if len(displayName) > 60 {
				displayName = "..." + displayName[len(displayName)-57:]
			}

			// Skip empty transcripts (no valid messages) - warn instead of error
			if len(conversation.Messages) == 0 {
				fmt.Printf("  ⚠️  Skipping empty transcript: %s\n", displayName)
				totalSkipped++
				continue
			}

			// Check file size for chunking decision
			var fileSize int64
			if fileInfo, statErr := os.Stat(filePath); statErr == nil {
				fileSize = fileInfo.Size()
			}

			// Determine if we should use chunked upload (message count OR file size)
			useChunked := len(conversation.Messages) > CHUNKED_THRESHOLD || fileSize > CHUNKED_SIZE_THRESHOLD
			numChunks := (len(conversation.Messages) + CHUNK_SIZE - 1) / CHUNK_SIZE

			if syncDryRun {
				chunkInfo := ""
				if useChunked {
					reason := "messages"
					if len(conversation.Messages) <= CHUNKED_THRESHOLD {
						reason = fmt.Sprintf("%.1fMB", float64(fileSize)/(1024*1024))
					}
					chunkInfo = fmt.Sprintf(" [chunked: %d chunks, %s]", numChunks, reason)
				}
				fmt.Printf("  [dry-run] Would sync: %s (%d messages)%s\n", displayName, len(conversation.Messages), chunkInfo)
				totalSynced++
				continue
			}

			// Decide whether to use chunked or regular upload
			var resp *client.TranscriptSyncResponse
			if useChunked {
				// Use chunked upload for large files (by message count or file size)
				reason := "messages"
				if len(conversation.Messages) <= CHUNKED_THRESHOLD {
					reason = fmt.Sprintf("%.1fMB", float64(fileSize)/(1024*1024))
				}
				fmt.Printf("  📤 Uploading %s in %d chunks (%s)...\n", displayName, numChunks, reason)
				resp, err = syncChunked(apiClient, conversation, stateManager, filePath)
			} else {
				// Use regular upload for smaller files
				req := buildRawSyncRequest(conversation)
				resp, err = apiClient.SyncTranscriptRaw(req)
			}

			if err != nil {
				fmt.Printf("  ❌ %s: %v\n", displayName, err)
				totalErrors++
				continue
			}

			// Mark as synced
			stateManager.MarkSynced(filePath, sync.SyncedFileInfo{
				Hash:           conversation.SourceFileHash,
				ConversationID: resp.ConversationID,
				MessageCount:   resp.MessageCount,
			})

			statusIcon := "✓"
			if resp.Status == "updated" {
				statusIcon = "↻"
			}
			fmt.Printf("  %s %s: %s (%d messages)\n", statusIcon, resp.Status, displayName, resp.MessageCount)

			totalSynced++
			syncedThisRun++
		}
		releaseFile()
	}

	// Plan files ride along with every full sync.
	plansSynced := syncPlans(apiClient, stateManager, "")

	// Save state
	if !syncDryRun {
		if err := stateManager.Save(); err != nil {
			fmt.Printf("⚠️  Failed to save sync state: %v\n", err)
		}
	}

	// Print summary
	fmt.Println()
	if syncDryRun {
		fmt.Printf("Dry run complete: %d transcript(s) would be synced\n", totalSynced)
	} else {
		fmt.Printf("Sync complete: %d synced, %d skipped, %d errors", totalSynced, totalSkipped, totalErrors)
		if plansSynced > 0 {
			fmt.Printf(", %d plan(s)", plansSynced)
		}
		fmt.Println()
	}

	// Sync is the "push everything now" command: also replay any events
	// queued after failed sends. Best-effort — never fails the sync. Quiet
	// when there's nothing to do; Ctrl-C stops the drain cleanly so the
	// outbox lock is released.
	drainCtx, stopDrain := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	out := &blankLineBefore{w: os.Stdout}
	if err := flushOutboxNow(drainCtx, out, config, syncDryRun, true); err != nil {
		_, _ = fmt.Fprintf(out, "⚠️  Queued events not fully replayed: %v\n", err)
	}
	stopDrain()

	return nil
}

// syncPlans uploads new/changed plan files from ~/.claude/plans. When
// scopeTranscript is non-empty (the auto-sync path), association is checked
// against that transcript only and unmatched plans are left for the next full
// sync; otherwise association scans transcripts modified near the plan's
// mtime. Returns the number of plans uploaded. Dry-run lists without sending.
func syncPlans(apiClient *client.Client, stateManager *sync.StateManager, scopeTranscript string) int {
	plans, err := sync.DiscoverPlans()
	if err != nil || len(plans) == 0 {
		return 0
	}

	parser, err := sync.NewClaudeCodeParser()
	if err != nil {
		return 0
	}

	synced := 0
	for _, plan := range plans {
		if stateManager.IsPlanSynced(plan.Path, plan.Hash) {
			continue
		}

		var candidates []string
		if scopeTranscript != "" {
			candidates = []string{scopeTranscript}
		} else {
			candidates = sync.RecentTranscripts(parser, plan, 72)
		}
		sessionID := sync.AssociatePlanSession(plan, candidates)
		if scopeTranscript != "" && sessionID == "" {
			continue // not this session's plan; a full sync will pick it up
		}

		if syncDryRun {
			fmt.Printf("  [dry-run] Would sync plan: %s (session %s)\n", plan.Name, dashIfEmpty(sessionID))
			synced++
			continue
		}

		resp, err := apiClient.SyncPlan(&client.PlanSyncRequest{
			SessionID:      sessionID,
			Tool:           "claude-code",
			Filename:       plan.Name,
			Content:        string(plan.Content),
			SourceFileHash: plan.Hash,
			ModifiedAt:     plan.ModifiedAt,
		})
		if err != nil {
			fmt.Printf("  ❌ plan %s: %v\n", plan.Name, err)
			continue
		}
		stateManager.MarkPlanSynced(plan.Path, sync.SyncedPlanInfo{
			Hash:      plan.Hash,
			SessionID: sessionID,
			PlanID:    resp.PlanID,
		})
		fmt.Printf("  ✓ plan: %s\n", plan.Name)
		synced++
	}
	return synced
}

func getParser(tool string) (sync.Parser, error) {
	switch strings.ToLower(tool) {
	case "claude-code", "claude":
		return sync.NewClaudeCodeParser()
	case "cursor":
		return sync.NewCursorParser()
	default:
		return nil, fmt.Errorf("unsupported tool: %s", tool)
	}
}

// buildRawSyncRequest creates a request with raw JSONL for server-side categorization
func buildRawSyncRequest(conv *sync.ParsedConversation) *client.RawTranscriptSyncRequest {
	rawMessages := make([]client.RawTranscriptMessage, len(conv.Messages))
	for i, m := range conv.Messages {
		rawMessages[i] = client.RawTranscriptMessage{
			RawJSON:   m.RawJSON,
			Sequence:  m.SequenceNumber,
			Timestamp: m.Timestamp,
		}
	}

	return &client.RawTranscriptSyncRequest{
		SessionID:      conv.SessionID,
		Tool:           conv.Tool,
		SourceFileHash: conv.SourceFileHash,
		SourceFilePath: conv.SourceFilePath,
		RawMessages:    rawMessages,
	}
}

// syncChunked uploads a large transcript using chunked upload with resume support
func syncChunked(apiClient *client.Client, conv *sync.ParsedConversation, stateManager *sync.StateManager, filePath string) (*client.TranscriptSyncResponse, error) {
	// Calculate number of chunks
	totalMessages := len(conv.Messages)
	numChunks := (totalMessages + CHUNK_SIZE - 1) / CHUNK_SIZE

	var uploadID string
	startChunk := 0

	// Check for pending upload to resume
	if pending, found := stateManager.GetPendingUpload(filePath, conv.SourceFileHash); found {
		// Verify the upload is still valid (same chunk count)
		if pending.TotalChunks == numChunks && pending.ChunksUploaded < numChunks {
			uploadID = pending.UploadID
			startChunk = pending.ChunksUploaded
			fmt.Printf("    resuming from chunk %d/%d...\n", startChunk+1, numChunks)
		} else {
			// Upload params changed or completed, start fresh
			stateManager.ClearPendingUpload(filePath)
		}
	}

	// Step 1: Initialize upload session if not resuming
	if uploadID == "" {
		initReq := &client.ChunkedInitRequest{
			SessionID:      conv.SessionID,
			Tool:           conv.Tool,
			SourceFileHash: conv.SourceFileHash,
			SourceFilePath: conv.SourceFilePath,
			TotalChunks:    numChunks,
			TotalMessages:  totalMessages,
		}

		initResp, err := apiClient.InitChunkedUpload(initReq)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize upload: %w", err)
		}

		uploadID = initResp.UploadID

		// Track this pending upload
		stateManager.SetPendingUpload(filePath, sync.PendingUploadInfo{
			UploadID:       uploadID,
			SourceFileHash: conv.SourceFileHash,
			TotalChunks:    numChunks,
			ChunksUploaded: 0,
		})
		// Save state immediately so we can resume if interrupted
		_ = stateManager.Save()
	}

	// Step 2: Upload chunks (starting from where we left off)
	for chunkIndex := startChunk; chunkIndex < numChunks; chunkIndex++ {
		start := chunkIndex * CHUNK_SIZE
		end := start + CHUNK_SIZE
		if end > totalMessages {
			end = totalMessages
		}

		// Build chunk messages
		chunkMessages := make([]client.RawTranscriptMessage, end-start)
		for i, m := range conv.Messages[start:end] {
			chunkMessages[i] = client.RawTranscriptMessage{
				RawJSON:   m.RawJSON,
				Sequence:  m.SequenceNumber,
				Timestamp: m.Timestamp,
			}
		}

		chunkReq := &client.ChunkedUploadRequest{
			UploadID:    uploadID,
			ChunkIndex:  chunkIndex,
			RawMessages: chunkMessages,
		}

		_, err := apiClient.UploadChunk(chunkReq)
		if err != nil {
			// Save progress before returning error so we can resume later
			stateManager.UpdatePendingUploadProgress(filePath, chunkIndex)
			_ = stateManager.Save()
			return nil, fmt.Errorf("failed to upload chunk %d/%d: %w", chunkIndex+1, numChunks, err)
		}

		// Update progress
		stateManager.UpdatePendingUploadProgress(filePath, chunkIndex+1)

		// Progress indicator
		fmt.Printf("    chunk %d/%d ✓\n", chunkIndex+1, numChunks)
	}

	// Save final chunk progress before completing
	_ = stateManager.Save()

	// Step 3: Complete upload
	completeReq := &client.ChunkedCompleteRequest{
		UploadID: uploadID,
	}

	fmt.Printf("    assembling and processing...\n")
	resp, err := apiClient.CompleteChunkedUpload(completeReq)
	if err != nil {
		return nil, fmt.Errorf("failed to complete upload: %w", err)
	}

	// Success - clear the pending upload
	stateManager.ClearPendingUpload(filePath)

	return resp, nil
}

// runSingleFileSync syncs a single transcript file (used by auto-sync), plus
// the subagent transcripts of a Claude Code session. Subagents (and workflow
// agents) write their own transcripts under <session>/subagents/ and never
// fire a Stop hook of their own, so before this they were only uploaded by a
// manual `promptconduit sync`. The parent session's auto-sync carries them.
// A subagent failure is recorded for retry, but the result reflects the main
// transcript.
//
// releaseMain drops the main transcript's upload lock once the main file is
// done, so the next Stop's sync isn't held up behind a long run of subagent
// uploads. Each subagent upload takes that subagent file's own lock instead.
func runSingleFileSync(config *client.Config, stateManager *sync.StateManager, filePath string, releaseMain func()) error {
	err := syncOneTranscript(config, stateManager, filePath)
	releaseMain()
	if !isSubagentTranscript(filePath) {
		syncSubagentTranscripts(config, stateManager, filePath)
	}
	return err
}

// syncSubagentTranscripts uploads the changed subagent transcripts of a
// session. Each upload holds that file's own upload lock, the same lock a
// full `promptconduit sync` or a retry of that file takes, so two processes
// never upload one file at once. A file another process is uploading right now
// is skipped; that process covers it. After taking the lock, the sync state is
// re-read so the hash check sees what that other process recorded.
func syncSubagentTranscripts(config *client.Config, stateManager *sync.StateManager, sessionPath string) {
	for _, sub := range cost.SubagentTranscripts(sessionPath) {
		if unchangedSinceSync(stateManager, sub) {
			continue
		}
		release, ok := sync.LockTranscript(client.ConfigDir(), sub, 0)
		if !ok {
			release()
			logger.Debug("Auto-sync: subagent transcript %s is being synced elsewhere; skipping", sub)
			continue
		}
		subState, err := sync.NewStateManager()
		if err == nil {
			err = syncOneTranscript(config, subState, sub)
		}
		release()
		if err != nil {
			logger.Debug("Auto-sync: subagent transcript %s: %v", sub, err)
		}
	}
}

// isSubagentTranscript reports whether path lives under a session's
// subagents/ directory (and so has no subagents of its own to carry).
func isSubagentTranscript(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/subagents/")
}

// unchangedSinceSync is a cheap pre-check for subagent transcripts: a file
// whose mtime and size both still match the fingerprint recorded when it was
// last hashed is skipped without re-hashing. Workflow-heavy sessions hold
// hundreds of them, and auto-sync runs on every Stop (debounced to once a
// minute). The fingerprint is read BEFORE hashing, so an append during a slow
// upload changes the size and is never mistaken for "unchanged". Comparing to
// synced_at instead would be wrong for exactly that case. Anything uncertain
// falls through to the hash check in syncOneTranscript.
func unchangedSinceSync(stateManager *sync.StateManager, path string) bool {
	info, ok := stateManager.GetSyncedInfo(path)
	if !ok || syncForce || info.SourceModTime == 0 {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.ModTime().UnixNano() == info.SourceModTime && st.Size() == info.SourceSize
}

// syncOneTranscript uploads one transcript file if it changed since its last
// sync (or always, with --force).
func syncOneTranscript(config *client.Config, stateManager *sync.StateManager, filePath string) error {
	// Verify file exists. The stat doubles as the fingerprint recorded with a
	// successful sync (see unchangedSinceSync); it is read before hashing.
	pre, statErr := os.Stat(filePath)
	if os.IsNotExist(statErr) {
		// Nothing left to retry: drop any failed-sync record for it, or every
		// later Stop hook would respawn a sync of a file that is gone.
		if stateManager.HasFailedSyncForFile(filePath) {
			stateManager.ClearFailedSyncsForFile(filePath)
			_ = stateManager.Save()
		}
		return fmt.Errorf("transcript file not found: %s", filePath)
	}

	// Fast-path: Check file hash before expensive parsing
	// This is critical for Stop events which fire frequently
	if !syncForce {
		currentHash, err := sync.CalculateFileHash(filePath)
		if err == nil && stateManager.IsSynced(filePath, currentHash) {
			// Unchanged since its last upload: nothing to send. A failed-sync
			// record left from before that upload (e.g. a full `promptconduit
			// sync` succeeded later) is stale; without clearing it here every
			// Stop hook keeps respawning this retry.
			save := false
			if stateManager.HasFailedSyncForFile(filePath) {
				stateManager.ClearFailedSyncsForFile(filePath)
				save = true
			}
			// Refresh the fingerprint when it is missing (a record from before
			// it existed) or stale (a write landed between the stat and the
			// hash of the last upload), so the next auto-sync can skip this
			// file without hashing it.
			if info, ok := stateManager.GetSyncedInfo(filePath); ok && statErr == nil &&
				(info.SourceModTime != pre.ModTime().UnixNano() || info.SourceSize != pre.Size()) {
				info.SourceModTime, info.SourceSize = pre.ModTime().UnixNano(), pre.Size()
				stateManager.MarkSynced(filePath, info)
				save = true
			}
			if save {
				_ = stateManager.Save()
			}
			return nil
		}
	}

	parser, err := sync.NewParserForPath(filePath)
	if err != nil {
		return fmt.Errorf("failed to create parser: %w", err)
	}

	// Parse file (only if hash changed or force sync)
	conversation, err := parser.ParseFile(filePath)
	if err != nil {
		stateManager.AddFailedSync(filepath.Base(filePath), filePath, err.Error())
		_ = stateManager.Save()
		return fmt.Errorf("failed to parse transcript: %w", err)
	}

	// Skip empty transcripts
	if len(conversation.Messages) == 0 {
		return nil
	}

	// Create API client
	apiClient := client.NewClient(config, Version)

	// Check file size for chunking decision
	var fileSize int64
	if fileInfo, statErr := os.Stat(filePath); statErr == nil {
		fileSize = fileInfo.Size()
	}

	// Determine if we should use chunked upload
	useChunked := len(conversation.Messages) > CHUNKED_THRESHOLD || fileSize > CHUNKED_SIZE_THRESHOLD

	// Upload
	var resp *client.TranscriptSyncResponse
	if useChunked {
		resp, err = syncChunked(apiClient, conversation, stateManager, filePath)
	} else {
		req := buildRawSyncRequest(conversation)
		resp, err = apiClient.SyncTranscriptRaw(req)
	}

	if err != nil {
		// Track failure for retry
		stateManager.AddFailedSync(conversation.SessionID, filePath, err.Error())
		_ = stateManager.Save()
		return fmt.Errorf("failed to sync: %w", err)
	}

	// Success - mark as synced and clear any previous failure
	synced := sync.SyncedFileInfo{
		Hash:           conversation.SourceFileHash,
		ConversationID: resp.ConversationID,
		MessageCount:   resp.MessageCount,
	}
	if statErr == nil {
		synced.SourceModTime, synced.SourceSize = pre.ModTime().UnixNano(), pre.Size()
	}
	stateManager.MarkSynced(filePath, synced)
	stateManager.ClearFailedSync(conversation.SessionID)

	// Ride-along: upload any new plan this session's transcript references.
	syncPlans(apiClient, stateManager, filePath)

	_ = stateManager.Save()

	return nil
}
