package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/sync"
	"github.com/spf13/cobra"
)

var (
	syncDryRun bool
	syncForce  bool
	syncSince  string
	syncLimit  int
	syncFile   string
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

Examples:
  promptconduit sync              # Sync all supported tools
  promptconduit sync claude-code  # Sync only Claude Code
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
	rootCmd.AddCommand(syncCmd)
}

func runSync(cmd *cobra.Command, args []string) error {
	// Load config
	config := client.LoadConfig()
	if config.APIKey == "" {
		return fmt.Errorf("API key not configured. Run: promptconduit config set --api-key=\"your-key\"")
	}

	// Initialize state manager
	stateManager, err := sync.NewStateManager()
	if err != nil {
		return fmt.Errorf("failed to initialize state manager: %w", err)
	}

	// Handle single-file sync (used by auto-sync from SessionEnd hook)
	if syncFile != "" {
		return runSingleFileSync(config, stateManager, syncFile)
	}

	// Determine which tools to sync
	toolsToSync := []string{"claude-code"} // Default: all supported
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

	// Track results
	var results []sync.SyncResult
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
		for _, filePath := range files {
			// Apply limit
			if syncLimit > 0 && totalSynced >= syncLimit {
				break
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
				results = append(results, sync.SyncResult{
					FilePath: filePath,
					Status:   "error",
					Error:    err,
				})
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
				results = append(results, sync.SyncResult{
					FilePath:     filePath,
					MessageCount: len(conversation.Messages),
					Status:       "dry-run",
				})
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
				results = append(results, sync.SyncResult{
					FilePath: filePath,
					Status:   "error",
					Error:    err,
				})
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

			results = append(results, sync.SyncResult{
				FilePath:       filePath,
				ConversationID: resp.ConversationID,
				MessageCount:   resp.MessageCount,
				Status:         resp.Status,
			})
			totalSynced++
			syncedThisRun++
		}
	}

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
		fmt.Printf("Sync complete: %d synced, %d skipped, %d errors\n", totalSynced, totalSkipped, totalErrors)
	}

	return nil
}

func getParser(tool string) (sync.Parser, error) {
	switch strings.ToLower(tool) {
	case "claude-code", "claude":
		return sync.NewClaudeCodeParser()
	default:
		return nil, fmt.Errorf("unsupported tool: %s", tool)
	}
}

func buildSyncRequest(conv *sync.ParsedConversation) *client.TranscriptSyncRequest {
	messages := make([]client.TranscriptMessage, len(conv.Messages))
	for i, m := range conv.Messages {
		messages[i] = client.TranscriptMessage{
			UUID:              m.UUID,
			ParentUUID:        m.ParentUUID,
			Type:              m.Type,
			Role:              m.Role,
			Content:           m.Content,
			Model:             m.Model,
			Thinking:          m.Thinking,
			ToolName:          m.ToolName,
			ToolUseID:         m.ToolUseID,
			ToolInput:         m.ToolInput,
			ToolResult:        m.ToolResult,
			ToolResultSuccess: m.ToolResultSuccess,
			Timestamp:         m.Timestamp,
			SequenceNumber:    m.SequenceNumber,
			GitBranch:         m.GitBranch,
			GitCommit:         m.GitCommit,
			Cwd:               m.Cwd,
			AttachmentCount:   m.AttachmentCount,
		}
	}

	return &client.TranscriptSyncRequest{
		Conversation: client.TranscriptConversation{
			SessionID:        conv.SessionID,
			Tool:             conv.Tool,
			Title:            conv.Title,
			Summary:          conv.Summary,
			StartedAt:        conv.StartedAt,
			EndedAt:          conv.EndedAt,
			RepoName:         conv.RepoName,
			Branch:           conv.Branch,
			WorkingDirectory: conv.WorkingDirectory,
			PrimaryModel:     conv.PrimaryModel,
			CLIVersion:       conv.CLIVersion,
			SourceFilePath:   conv.SourceFilePath,
			SourceFileHash:   conv.SourceFileHash,
		},
		Messages: messages,
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
		stateManager.Save()
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
			stateManager.Save()
			return nil, fmt.Errorf("failed to upload chunk %d/%d: %w", chunkIndex+1, numChunks, err)
		}

		// Update progress
		stateManager.UpdatePendingUploadProgress(filePath, chunkIndex+1)

		// Progress indicator
		fmt.Printf("    chunk %d/%d ✓\n", chunkIndex+1, numChunks)
	}

	// Save final chunk progress before completing
	stateManager.Save()

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

// runSingleFileSync syncs a single transcript file (used by auto-sync)
func runSingleFileSync(config *client.Config, stateManager *sync.StateManager, filePath string) error {
	// Verify file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return fmt.Errorf("transcript file not found: %s", filePath)
	}

	// Determine tool from path (claude-code for now)
	parser, err := sync.NewClaudeCodeParser()
	if err != nil {
		return fmt.Errorf("failed to create parser: %w", err)
	}

	// Parse file
	conversation, err := parser.ParseFile(filePath)
	if err != nil {
		stateManager.AddFailedSync(filepath.Base(filePath), filePath, err.Error())
		stateManager.Save()
		return fmt.Errorf("failed to parse transcript: %w", err)
	}

	// Check if already synced (unless force)
	if !syncForce && stateManager.IsSynced(filePath, conversation.SourceFileHash) {
		// Already synced, nothing to do
		return nil
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
		stateManager.Save()
		return fmt.Errorf("failed to sync: %w", err)
	}

	// Success - mark as synced and clear any previous failure
	stateManager.MarkSynced(filePath, sync.SyncedFileInfo{
		Hash:           conversation.SourceFileHash,
		ConversationID: resp.ConversationID,
		MessageCount:   resp.MessageCount,
	})
	stateManager.ClearFailedSync(conversation.SessionID)
	stateManager.Save()

	return nil
}
