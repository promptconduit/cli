package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/promptconduit/cli/internal/filelock"
)

// StateManager manages the sync state file.
//
// Several sync processes can run at once (auto-sync fires on every Stop of
// every concurrent agent), so the file is never blindly overwritten with this
// process's in-memory copy. Instead every mutation is recorded as an op;
// Save takes an exclusive lock, re-reads the file, replays this process's ops
// on top of what other processes have saved since, and writes the result
// atomically (temp + rename). A reader therefore never sees a torn file, and
// concurrent syncs never erase each other's records.
type StateManager struct {
	statePath string
	state     *SyncState
	ops       []func(*SyncState)
}

// stateLockWait bounds how long Save waits for another process's save. Saves
// hold the lock for milliseconds; after the wait Save proceeds unlocked
// (still atomic, at worst losing a concurrent save's update).
const stateLockWait = 5 * time.Second

// ErrStateCorrupt marks an on-disk state that can't be parsed. Save first
// retries briefly (a torn read from a pre-atomic writer settles), so a
// transient read never blanks good state; if the file stays unparseable it is
// moved aside to sync_state.json.corrupt-<unix> and fresh state is written.
var ErrStateCorrupt = errors.New("sync state file is corrupt")

// NewStateManager creates a new state manager
func NewStateManager() (*StateManager, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	configDir := filepath.Join(homeDir, ".config", "promptconduit")
	return newStateManagerAt(filepath.Join(configDir, "sync_state.json")), nil
}

// newStateManagerAt loads (best-effort) the state at statePath.
func newStateManagerAt(statePath string) *StateManager {
	sm := &StateManager{statePath: statePath, state: emptyState()}
	if st, err := readState(statePath); err == nil {
		sm.state = st
	}
	// A corrupt file loads as empty here (sync can still proceed), but Save
	// refuses to overwrite it — see ErrStateCorrupt.
	return sm
}

func emptyState() *SyncState {
	return &SyncState{
		SyncedFiles:    make(map[string]SyncedFileInfo),
		PendingUploads: make(map[string]PendingUploadInfo),
		FailedSyncs:    make(map[string]FailedSyncInfo),
	}
}

// readState parses the state file. A missing file is an empty state; a file
// that doesn't parse is ErrStateCorrupt.
func readState(path string) (*SyncState, error) {
	st := emptyState()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("%w (%s): %v", ErrStateCorrupt, path, err)
	}
	// Ensure maps are initialized even after loading
	if st.SyncedFiles == nil {
		st.SyncedFiles = make(map[string]SyncedFileInfo)
	}
	if st.PendingUploads == nil {
		st.PendingUploads = make(map[string]PendingUploadInfo)
	}
	if st.FailedSyncs == nil {
		st.FailedSyncs = make(map[string]FailedSyncInfo)
	}
	return st, nil
}

// apply runs op on the in-memory state now and records it for Save to replay.
func (sm *StateManager) apply(op func(*SyncState)) {
	op(sm.state)
	sm.ops = append(sm.ops, op)
}

// IsSynced checks if a file with the given hash has been synced
func (sm *StateManager) IsSynced(path, hash string) bool {
	if info, ok := sm.state.SyncedFiles[path]; ok {
		return info.Hash == hash
	}
	return false
}

// MarkSynced marks a file as synced. When Save replays it, a record another
// process saved for the same file with a LATER synced_at (it uploaded a newer
// version after this upload) is kept rather than overwritten with this,
// older, hash — otherwise the next sync would re-upload needlessly, or worse
// a long full sync would roll back a newer auto-sync record.
func (sm *StateManager) MarkSynced(path string, info SyncedFileInfo) {
	if info.SyncedAt == "" {
		info.SyncedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	sm.apply(func(s *SyncState) {
		if cur, ok := s.SyncedFiles[path]; ok && syncedAfter(cur.SyncedAt, info.SyncedAt) {
			return
		}
		s.SyncedFiles[path] = info
	})
}

// syncedAfter reports whether timestamp a is strictly after b. Unparseable
// values never win (so a legacy or garbled record is overwritten).
func syncedAfter(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339Nano, a)
	tb, errB := time.Parse(time.RFC3339Nano, b)
	return errA == nil && errB == nil && ta.After(tb)
}

// Save persists the state to disk: lock, re-read, replay this process's
// changes, write atomically. On a corrupt on-disk file it returns
// ErrStateCorrupt and leaves the file as is.
func (sm *StateManager) Save() error {
	if err := os.MkdirAll(filepath.Dir(sm.statePath), 0755); err != nil {
		return err
	}
	release, _ := filelock.Exclusive(sm.statePath+".lock", stateLockWait)
	defer release()

	merged, err := readState(sm.statePath)
	// A CLI from before atomic saves may still be mid-write; give a torn read a
	// couple of chances to settle before declaring the file corrupt.
	for retry := 0; retry < 3 && errors.Is(err, ErrStateCorrupt); retry++ {
		time.Sleep(50 * time.Millisecond)
		merged, err = readState(sm.statePath)
	}
	if errors.Is(err, ErrStateCorrupt) {
		// Persistently unparseable: self-heal rather than block every future
		// save (and re-upload on every sync). Keep the bad file for inspection.
		backup := fmt.Sprintf("%s.corrupt-%d", sm.statePath, time.Now().Unix())
		if rerr := os.Rename(sm.statePath, backup); rerr != nil {
			return fmt.Errorf("%w; could not move it aside: %v", err, rerr)
		}
		merged, err = emptyState(), nil
	}
	if err != nil {
		return err
	}
	for _, op := range sm.ops {
		op(merged)
	}

	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(sm.statePath, data, 0644); err != nil {
		return err
	}
	sm.state = merged
	sm.ops = nil
	return nil
}

// writeFileAtomic writes data to a temp file in path's directory and renames
// it over path, so readers see either the old or the new content, never a mix.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sync_state-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// GetSyncedInfo returns info about a synced file
func (sm *StateManager) GetSyncedInfo(path string) (SyncedFileInfo, bool) {
	info, ok := sm.state.SyncedFiles[path]
	return info, ok
}

// ClearState clears all sync state
func (sm *StateManager) ClearState() {
	sm.apply(func(s *SyncState) {
		s.SyncedFiles = make(map[string]SyncedFileInfo)
		s.PendingUploads = make(map[string]PendingUploadInfo)
	})
}

// GetPendingUpload returns pending upload info if one exists for the file with matching hash
func (sm *StateManager) GetPendingUpload(path, hash string) (PendingUploadInfo, bool) {
	if info, ok := sm.state.PendingUploads[path]; ok {
		// Only return if hash matches (file hasn't changed)
		if info.SourceFileHash == hash {
			return info, true
		}
		// Hash changed, remove stale pending upload
		sm.apply(func(s *SyncState) {
			if cur, ok := s.PendingUploads[path]; ok && cur.SourceFileHash != hash {
				delete(s.PendingUploads, path)
			}
		})
	}
	return PendingUploadInfo{}, false
}

// SetPendingUpload tracks a pending chunked upload
func (sm *StateManager) SetPendingUpload(path string, info PendingUploadInfo) {
	if info.StartedAt == "" {
		info.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	sm.apply(func(s *SyncState) { s.PendingUploads[path] = info })
}

// UpdatePendingUploadProgress updates the chunks uploaded count
func (sm *StateManager) UpdatePendingUploadProgress(path string, chunksUploaded int) {
	sm.apply(func(s *SyncState) {
		if info, ok := s.PendingUploads[path]; ok {
			info.ChunksUploaded = chunksUploaded
			s.PendingUploads[path] = info
		}
	})
}

// ClearPendingUpload removes a pending upload (after success or failure)
func (sm *StateManager) ClearPendingUpload(path string) {
	sm.apply(func(s *SyncState) { delete(s.PendingUploads, path) })
}

// IsPlanSynced checks whether a plan file with the given hash has been synced.
func (sm *StateManager) IsPlanSynced(path, hash string) bool {
	if info, ok := sm.state.SyncedPlans[path]; ok {
		return info.Hash == hash
	}
	return false
}

// MarkPlanSynced records a plan file as synced.
func (sm *StateManager) MarkPlanSynced(path string, info SyncedPlanInfo) {
	if info.SyncedAt == "" {
		info.SyncedAt = time.Now().UTC().Format(time.RFC3339)
	}
	sm.apply(func(s *SyncState) {
		if s.SyncedPlans == nil {
			s.SyncedPlans = make(map[string]SyncedPlanInfo)
		}
		s.SyncedPlans[path] = info
	})
}

// AddFailedSync tracks a failed sync for retry
func (sm *StateManager) AddFailedSync(sessionID, filePath, errorMsg string) {
	now := time.Now().UTC().Format(time.RFC3339)
	sm.apply(func(s *SyncState) {
		if s.FailedSyncs == nil {
			s.FailedSyncs = make(map[string]FailedSyncInfo)
		}
		// Check if already exists to preserve retry count
		if existing, ok := s.FailedSyncs[sessionID]; ok {
			existing.RetryCount++
			existing.LastError = errorMsg
			existing.FailedAt = now
			s.FailedSyncs[sessionID] = existing
		} else {
			s.FailedSyncs[sessionID] = FailedSyncInfo{
				SessionID:  sessionID,
				FilePath:   filePath,
				FailedAt:   now,
				RetryCount: 0,
				LastError:  errorMsg,
			}
		}
	})
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
	sm.apply(func(s *SyncState) {
		if s.FailedSyncs != nil {
			delete(s.FailedSyncs, sessionID)
		}
	})
}
