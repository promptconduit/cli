package collect

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	spansFile = "spans.ndjson"
	logsFile  = "logs.ndjson"

	// maxFileBytes is the soft cap before the file is rotated to .1. Keeps
	// `collect` from filling a disk during a long-running session.
	maxFileBytes int64 = 50 * 1024 * 1024
)

// Store is an append-only NDJSON store for spans and logs. One file each,
// rotated to <name>.1 when they exceed maxFileBytes. Safe for concurrent
// writers within a single process.
type Store struct {
	dir       string
	mu        sync.Mutex
	spans     *os.File
	logs      *os.File
	spansSize int64
	logsSize  int64
}

// OpenStore opens (or creates) the spans/logs NDJSON files under dir.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	s := &Store{dir: dir}
	sp, spSize, err := openAppend(filepath.Join(dir, spansFile))
	if err != nil {
		return nil, err
	}
	lg, lgSize, err := openAppend(filepath.Join(dir, logsFile))
	if err != nil {
		_ = sp.Close()
		return nil, err
	}
	s.spans, s.spansSize = sp, spSize
	s.logs, s.logsSize = lg, lgSize
	return s, nil
}

func openAppend(path string) (*os.File, int64, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// Dir returns the directory the store writes to.
func (s *Store) Dir() string { return s.dir }

// Close flushes and closes the underlying files.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err1, err2 error
	if s.spans != nil {
		err1 = s.spans.Close()
		s.spans = nil
	}
	if s.logs != nil {
		err2 = s.logs.Close()
		s.logs = nil
	}
	return errors.Join(err1, err2)
}

// AppendSpan writes one span as a JSON line to spans.ndjson.
func (s *Store) AppendSpan(row SpanRow) error {
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLine(&s.spans, &s.spansSize, spansFile, data)
}

// AppendLog writes one log record as a JSON line to logs.ndjson.
func (s *Store) AppendLog(row LogRow) error {
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLine(&s.logs, &s.logsSize, logsFile, data)
}

func (s *Store) writeLine(fp **os.File, size *int64, name string, data []byte) error {
	if *fp == nil {
		f, sz, err := openAppend(filepath.Join(s.dir, name))
		if err != nil {
			return err
		}
		*fp = f
		*size = sz
	}
	n, err := (*fp).Write(append(data, '\n'))
	*size += int64(n)
	if err != nil {
		return err
	}
	if *size > maxFileBytes {
		if err := (*fp).Close(); err != nil {
			return err
		}
		*fp = nil
		_ = os.Rename(filepath.Join(s.dir, name), filepath.Join(s.dir, name+".1"))
		f, _, err := openAppend(filepath.Join(s.dir, name))
		if err != nil {
			return err
		}
		*fp = f
		*size = 0
	}
	return nil
}

// ReadSpans returns up to limit recent spans, newest first. Only the active
// (non-rotated) file is consulted — that's plenty for dogfooding and keeps
// the dashboard cheap. If traceID is non-empty, only spans with that
// trace_id are returned. limit <= 0 means no limit.
func (s *Store) ReadSpans(limit int, traceID string) ([]SpanRow, error) {
	path := filepath.Join(s.dir, spansFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var rows []SpanRow
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var r SpanRow
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			continue
		}
		if traceID != "" && r.TraceID != traceID {
			continue
		}
		rows = append(rows, r)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ReceivedAt.After(rows[j].ReceivedAt) })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// TraceSummary is a compact view of one trace for the trace-list page.
type TraceSummary struct {
	TraceID     string    `json:"trace_id"`
	ServiceName string    `json:"service_name,omitempty"`
	RootName    string    `json:"root_name,omitempty"`
	SpanCount   int       `json:"span_count"`
	StartedAt   time.Time `json:"started_at"`
	DurationMs  float64   `json:"duration_ms,omitempty"`
	Status      int       `json:"status,omitempty"`
}

// ListTraces returns recent traces ordered by newest first, up to limit.
func (s *Store) ListTraces(limit int) ([]TraceSummary, error) {
	rows, err := s.ReadSpans(0, "")
	if err != nil {
		return nil, err
	}
	byTrace := map[string]*TraceSummary{}
	for _, r := range rows {
		t, ok := byTrace[r.TraceID]
		if !ok {
			t = &TraceSummary{
				TraceID:     r.TraceID,
				ServiceName: r.ServiceName,
				StartedAt:   r.ReceivedAt,
			}
			byTrace[r.TraceID] = t
		}
		t.SpanCount++
		if r.ReceivedAt.Before(t.StartedAt) {
			t.StartedAt = r.ReceivedAt
		}
		if r.ParentSpanID == "" && r.Name != "" {
			t.RootName = r.Name
			t.DurationMs = r.DurationMs
			t.Status = r.StatusCode
		}
	}
	out := make([]TraceSummary, 0, len(byTrace))
	for _, t := range byTrace {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
