package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// StateManager manages the sync state file
type StateManager struct {
	statePath string
	state     *SyncState
}

// NewStateManager creates a new state manager
func NewStateManager() (*StateManager, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	configDir := filepath.Join(homeDir, ".config", "promptconduit")
	statePath := filepath.Join(configDir, "sync_state.json")

	sm := &StateManager{
		statePath: statePath,
		state: &SyncState{
			SyncedFiles:    make(map[string]SyncedFileInfo),
			PendingUploads: make(map[string]PendingUploadInfo),
		},
	}

	// Load existing state if available
	if data, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(data, sm.state)
		// Ensure maps are initialized even after loading
		if sm.state.SyncedFiles == nil {
			sm.state.SyncedFiles = make(map[string]SyncedFileInfo)
		}
		if sm.state.PendingUploads == nil {
			sm.state.PendingUploads = make(map[string]PendingUploadInfo)
		}
		if sm.state.FailedSyncs == nil {
			sm.state.FailedSyncs = make(map[string]FailedSyncInfo)
		}
	}

	return sm, nil
}

// IsSynced checks if a file with the given hash has been synced
func (sm *StateManager) IsSynced(path, hash string) bool {
	if info, ok := sm.state.SyncedFiles[path]; ok {
		return info.Hash == hash
	}
	return false
}

// MarkSynced marks a file as synced
func (sm *StateManager) MarkSynced(path string, info SyncedFileInfo) {
	if info.SyncedAt == "" {
		info.SyncedAt = time.Now().UTC().Format(time.RFC3339)
	}
	sm.state.SyncedFiles[path] = info
}

// Save persists the state to disk
func (sm *StateManager) Save() error {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(sm.statePath), 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(sm.state, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(sm.statePath, data, 0644)
}

// GetSyncedInfo returns info about a synced file
func (sm *StateManager) GetSyncedInfo(path string) (SyncedFileInfo, bool) {
	info, ok := sm.state.SyncedFiles[path]
	return info, ok
}

// ClearState clears all sync state
func (sm *StateManager) ClearState() {
	sm.state.SyncedFiles = make(map[string]SyncedFileInfo)
	sm.state.PendingUploads = make(map[string]PendingUploadInfo)
}

// GetPendingUpload returns pending upload info if one exists for the file with matching hash
func (sm *StateManager) GetPendingUpload(path, hash string) (PendingUploadInfo, bool) {
	if info, ok := sm.state.PendingUploads[path]; ok {
		// Only return if hash matches (file hasn't changed)
		if info.SourceFileHash == hash {
			return info, true
		}
		// Hash changed, remove stale pending upload
		delete(sm.state.PendingUploads, path)
	}
	return PendingUploadInfo{}, false
}

// SetPendingUpload tracks a pending chunked upload
func (sm *StateManager) SetPendingUpload(path string, info PendingUploadInfo) {
	if info.StartedAt == "" {
		info.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	sm.state.PendingUploads[path] = info
}

// UpdatePendingUploadProgress updates the chunks uploaded count
func (sm *StateManager) UpdatePendingUploadProgress(path string, chunksUploaded int) {
	if info, ok := sm.state.PendingUploads[path]; ok {
		info.ChunksUploaded = chunksUploaded
		sm.state.PendingUploads[path] = info
	}
}

// ClearPendingUpload removes a pending upload (after success or failure)
func (sm *StateManager) ClearPendingUpload(path string) {
	delete(sm.state.PendingUploads, path)
}

// AddFailedSync tracks a failed sync for retry
func (sm *StateManager) AddFailedSync(sessionID, filePath, errorMsg string) {
	if sm.state.FailedSyncs == nil {
		sm.state.FailedSyncs = make(map[string]FailedSyncInfo)
	}
	// Check if already exists to preserve retry count
	if existing, ok := sm.state.FailedSyncs[sessionID]; ok {
		existing.RetryCount++
		existing.LastError = errorMsg
		existing.FailedAt = time.Now().UTC().Format(time.RFC3339)
		sm.state.FailedSyncs[sessionID] = existing
	} else {
		sm.state.FailedSyncs[sessionID] = FailedSyncInfo{
			SessionID:  sessionID,
			FilePath:   filePath,
			FailedAt:   time.Now().UTC().Format(time.RFC3339),
			RetryCount: 0,
			LastError:  errorMsg,
		}
	}
}

// GetFailedSyncs returns all pending failed syncs
func (sm *StateManager) GetFailedSyncs() []FailedSyncInfo {
	result := make([]FailedSyncInfo, 0, len(sm.state.FailedSyncs))
	for _, info := range sm.state.FailedSyncs {
		result = append(result, info)
	}
	return result
}

// ClearFailedSync removes a failed sync after successful retry
func (sm *StateManager) ClearFailedSync(sessionID string) {
	if sm.state.FailedSyncs != nil {
		delete(sm.state.FailedSyncs, sessionID)
	}
}
