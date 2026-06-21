package eventlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Outcome is the terminal disposition of an event the CLI handled.
type Outcome string

const (
	OutcomeSent    Outcome = "sent"    // accepted by the platform (2xx)
	OutcomeFailed  Outcome = "failed"  // attempted but the send errored / non-2xx
	OutcomeDropped Outcome = "dropped" // never sent (not configured, parse error, …)
)

// Status is the rolling health summary persisted to status.json. It answers
// "are my events actually reaching the platform?" at a glance and powers the
// counters section of `promptconduit status`.
type Status struct {
	Sent          int64  `json:"sent"`
	Failed        int64  `json:"failed"`
	Dropped       int64  `json:"dropped"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastErrorAt   string `json:"last_error_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

// statusMu serializes the read-modify-write of status.json within a process.
// Across processes (the hook parent and its async send subprocess), the
// temp-file + rename write keeps each update atomic; the worst case is a lost
// increment, never a corrupt file — acceptable for best-effort counters.
var statusMu sync.Mutex

// Bump increments the counter for outcome and, on failure, records the error
// detail. Best-effort: any error is swallowed so it never disturbs the caller.
func Bump(outcome Outcome, detail string) {
	if !Enabled() {
		return
	}
	statusMu.Lock()
	defer statusMu.Unlock()

	st := loadStatusLocked()
	now := nowUTC().Format(timeLayout)
	st.UpdatedAt = now

	switch outcome {
	case OutcomeSent:
		st.Sent++
		st.LastSuccessAt = now
	case OutcomeFailed:
		st.Failed++
		st.LastErrorAt = now
		if detail != "" {
			st.LastError = detail
		}
	case OutcomeDropped:
		st.Dropped++
		st.LastErrorAt = now
		if detail != "" {
			st.LastError = detail
		}
	}

	writeStatusLocked(st)
}

// LoadStatus returns the current counters, or a zero-value Status when the
// file is absent or unreadable. Safe to call regardless of Enabled().
func LoadStatus() Status {
	statusMu.Lock()
	defer statusMu.Unlock()
	return loadStatusLocked()
}

func loadStatusLocked() Status {
	var st Status
	data, err := os.ReadFile(StatusPath())
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st)
	return st
}

// writeStatusLocked writes status.json via a temp file + rename so a reader
// never observes a half-written file. Called with statusMu held.
func writeStatusLocked(st Status) {
	path := StatusPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, "status-*.tmp")
	if err != nil {
		// Fall back to a direct write rather than dropping the update.
		_ = os.WriteFile(path, data, 0o644)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
	}
}
