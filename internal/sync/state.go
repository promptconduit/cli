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
		state:     &SyncState{SyncedFiles: make(map[string]SyncedFileInfo)},
	}

	// Load existing state if available
	if data, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(data, sm.state)
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
}
