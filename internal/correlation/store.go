package correlation

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SpanKind names the lookup table inside a session's spans file.
type SpanKind string

const (
	SpanKindToolUse        SpanKind = "tool_uses"
	SpanKindSubagent       SpanKind = "subagents"
	SpanKindTask           SpanKind = "tasks"
	SpanKindElicitation    SpanKind = "elicitations"
	SpanKindContextCompact SpanKind = "context_compacts"
)

const (
	// LastPromptSubmitKey is the well-known key for the most recent
	// prompt_submit span in a session. Stored alongside the per-chain tables.
	LastPromptSubmitKey = "last_prompt_submit"
	// RootSpanKey is the well-known key for the session_start span ID,
	// used as the parent for session_end.
	RootSpanKey = "root_span"

	tracesSubdir = "traces"
	gcMaxAge     = 30 * 24 * time.Hour
	// gcProbability is the chance per hook fire that GC runs. 1/100 ≈ "~1%"
	// in the PRD; we use 256 as a power of two for a cheap bitmask.
	gcProbabilityDenominator = 100
)

// TraceRecord is persisted per session.
type TraceRecord struct {
	SessionID  string    `json:"session_id"`
	TraceID    string    `json:"trace_id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// SpansRecord is persisted per session: keyed parent span lookup tables.
type SpansRecord struct {
	ToolUses         map[string]string `json:"tool_uses,omitempty"`
	Subagents        map[string]string `json:"subagents,omitempty"`
	Tasks            map[string]string `json:"tasks,omitempty"`
	Elicitations     map[string]string `json:"elicitations,omitempty"`
	ContextCompacts  map[string]string `json:"context_compacts,omitempty"`
	LastPromptSubmit string            `json:"last_prompt_submit,omitempty"`
	RootSpan         string            `json:"root_span,omitempty"`
}

// Store persists trace and span lookup state under baseDir.
type Store struct {
	baseDir string
}

// NewStore returns a Store rooted at baseDir (typically the
// promptconduit config dir). Directories are created on demand.
func NewStore(baseDir string) *Store {
	return &Store{baseDir: baseDir}
}

func (s *Store) tracesDir() string { return filepath.Join(s.baseDir, tracesSubdir) }

func (s *Store) traceFile(sessionID string) string {
	return filepath.Join(s.tracesDir(), sessionID+".json")
}

func (s *Store) spansFile(sessionID string) string {
	return filepath.Join(s.tracesDir(), sessionID+".spans.json")
}

// LoadOrCreateTrace returns the trace ID for sessionID, creating and
// persisting a new one if none exists. The last_seen_at timestamp is
// refreshed on every call.
//
// Concurrent hook processes are reconciled via O_CREATE|O_EXCL: only the
// first writer wins; subsequent callers re-read the existing file.
func (s *Store) LoadOrCreateTrace(sessionID string) (*TraceRecord, error) {
	if sessionID == "" {
		return nil, errors.New("correlation: empty session id")
	}
	if err := os.MkdirAll(s.tracesDir(), 0700); err != nil {
		return nil, fmt.Errorf("correlation: mkdir traces: %w", err)
	}

	path := s.traceFile(sessionID)

	// Fast path: file already exists.
	if rec, err := readTrace(path); err == nil {
		rec.LastSeenAt = time.Now().UTC()
		_ = writeTraceAtomic(path, rec) // refresh; ignore errors (non-fatal)
		return rec, nil
	}

	// Slow path: write to a tempfile, then atomically link to the target.
	// os.Link fails with EEXIST when the target already exists, which is the
	// only race-safe primitive for "create-only" — unlike Rename, it never
	// overwrites, and unlike O_CREATE|O_EXCL + sequential write, the file is
	// fully populated at the moment any concurrent reader can see it.
	rec := &TraceRecord{
		SessionID:  sessionID,
		TraceID:    NewTraceID(),
		CreatedAt:  time.Now().UTC(),
		LastSeenAt: time.Now().UTC(),
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("correlation: marshal trace: %w", err)
	}

	tmp, err := os.CreateTemp(s.tracesDir(), ".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("correlation: tempfile: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("correlation: write tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("correlation: close tempfile: %w", err)
	}

	if err := os.Link(tmpName, path); err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("correlation: link trace: %w", err)
		}
		// Lost the race; another process won. Read theirs.
		existing, rerr := readTrace(path)
		if rerr != nil {
			return nil, fmt.Errorf("correlation: read after race: %w", rerr)
		}
		return existing, nil
	}
	return rec, nil
}

// RecordSpan stores a parent span ID under the given kind+key for sessionID.
// Returns nil on success. Errors are non-fatal at the call site.
func (s *Store) RecordSpan(sessionID string, kind SpanKind, key, spanID string) error {
	if sessionID == "" || spanID == "" {
		return errors.New("correlation: empty session or span id")
	}
	rec, err := s.loadSpans(sessionID)
	if err != nil {
		return err
	}
	switch kind {
	case SpanKindToolUse:
		ensureMap(&rec.ToolUses)[key] = spanID
	case SpanKindSubagent:
		ensureMap(&rec.Subagents)[key] = spanID
	case SpanKindTask:
		ensureMap(&rec.Tasks)[key] = spanID
	case SpanKindElicitation:
		ensureMap(&rec.Elicitations)[key] = spanID
	case SpanKindContextCompact:
		ensureMap(&rec.ContextCompacts)[key] = spanID
	default:
		return fmt.Errorf("correlation: unknown span kind %q", kind)
	}
	return s.writeSpans(sessionID, rec)
}

// RecordLastPromptSubmit stores the most recent prompt_submit span ID.
func (s *Store) RecordLastPromptSubmit(sessionID, spanID string) error {
	if sessionID == "" || spanID == "" {
		return errors.New("correlation: empty session or span id")
	}
	rec, err := s.loadSpans(sessionID)
	if err != nil {
		return err
	}
	rec.LastPromptSubmit = spanID
	return s.writeSpans(sessionID, rec)
}

// RecordRootSpan stores the session_start span ID.
func (s *Store) RecordRootSpan(sessionID, spanID string) error {
	if sessionID == "" || spanID == "" {
		return errors.New("correlation: empty session or span id")
	}
	rec, err := s.loadSpans(sessionID)
	if err != nil {
		return err
	}
	rec.RootSpan = spanID
	return s.writeSpans(sessionID, rec)
}

// LookupParent returns the parent span ID for kind+key, or "" if absent.
func (s *Store) LookupParent(sessionID string, kind SpanKind, key string) string {
	rec, err := s.loadSpans(sessionID)
	if err != nil {
		return ""
	}
	switch kind {
	case SpanKindToolUse:
		return rec.ToolUses[key]
	case SpanKindSubagent:
		return rec.Subagents[key]
	case SpanKindTask:
		return rec.Tasks[key]
	case SpanKindElicitation:
		return rec.Elicitations[key]
	case SpanKindContextCompact:
		return rec.ContextCompacts[key]
	}
	return ""
}

// LookupLastPromptSubmit returns the most recent prompt_submit span ID, or "".
func (s *Store) LookupLastPromptSubmit(sessionID string) string {
	rec, err := s.loadSpans(sessionID)
	if err != nil {
		return ""
	}
	return rec.LastPromptSubmit
}

// LookupRootSpan returns the session_start span ID, or "".
func (s *Store) LookupRootSpan(sessionID string) string {
	rec, err := s.loadSpans(sessionID)
	if err != nil {
		return ""
	}
	return rec.RootSpan
}

// LoadSpans returns the spans record for sessionID (or a fresh one).
// Exposed for the debug command.
func (s *Store) LoadSpans(sessionID string) (*SpansRecord, error) {
	return s.loadSpans(sessionID)
}

// LoadTrace returns the trace record for sessionID without updating it.
// Returns os.ErrNotExist if there is no record.
func (s *Store) LoadTrace(sessionID string) (*TraceRecord, error) {
	return readTrace(s.traceFile(sessionID))
}

// MaybeGC opportunistically deletes trace files older than gcMaxAge.
// Runs roughly once per gcProbabilityDenominator invocations; never blocks.
func (s *Store) MaybeGC() {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return
	}
	if binary.BigEndian.Uint32(b[:])%gcProbabilityDenominator != 0 {
		return
	}
	s.gc()
}

func (s *Store) gc() {
	entries, err := os.ReadDir(s.tracesDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-gcMaxAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Use mtime as a cheap proxy for last_seen_at — refreshed on every
		// LoadOrCreateTrace via atomic rewrite.
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(s.tracesDir(), e.Name()))
		}
	}
}

func (s *Store) loadSpans(sessionID string) (*SpansRecord, error) {
	if sessionID == "" {
		return nil, errors.New("correlation: empty session id")
	}
	path := s.spansFile(sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &SpansRecord{}, nil
		}
		return nil, err
	}
	var rec SpansRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		// Corrupt file: treat as empty rather than crashing the hook.
		return &SpansRecord{}, nil
	}
	return &rec, nil
}

func (s *Store) writeSpans(sessionID string, rec *SpansRecord) error {
	if err := os.MkdirAll(s.tracesDir(), 0700); err != nil {
		return err
	}
	return writeJSONAtomic(s.spansFile(sessionID), rec)
}

func readTrace(path string) (*TraceRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec TraceRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if !IsValidTraceID(rec.TraceID) {
		return nil, fmt.Errorf("correlation: invalid trace_id in %s", path)
	}
	return &rec, nil
}

func writeTraceAtomic(path string, rec *TraceRecord) error {
	return writeJSONAtomic(path, rec)
}

func writeJSONAtomic(path string, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func ensureMap(m *map[string]string) map[string]string {
	if *m == nil {
		*m = make(map[string]string)
	}
	return *m
}
