package cost

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/promptconduit/cli/internal/client"
)

// Modeled on internal/collect/store.go: an append-only NDJSON file that rotates
// to <name>.1 past a size cap. This store is strictly local — it is the cost
// feature's durable history and is never read by any code that talks to the
// platform API.
const (
	costDirName    = "cost"
	eventsFileName = "events.ndjson"
	maxFileBytes   = int64(50 * 1024 * 1024)
)

// Store appends cost events to a local NDJSON file. Safe for concurrent
// writers within a process.
type Store struct {
	path string
	mu   sync.Mutex
	f    *os.File
	size int64
}

// StoreDir returns the on-disk directory the cost store lives in
// (~/.config/promptconduit/cost), reusing the CLI's existing config-dir logic.
func StoreDir() string {
	return filepath.Join(client.ConfigDir(), costDirName)
}

// OpenStore opens (creating if needed) the cost events file under StoreDir.
func OpenStore() (*Store, error) {
	dir := StoreDir()
	if dir == "" {
		return nil, errors.New("cost: could not resolve config dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cost: mkdir store dir: %w", err)
	}
	path := filepath.Join(dir, eventsFileName)
	f, size, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, f: f, size: size}, nil
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

// AppendEvent marshals and appends one cost event as a JSON line.
func (s *Store) AppendEvent(e CostEvent) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLine(data)
}

func (s *Store) writeLine(data []byte) error {
	if s.f == nil {
		f, sz, err := openAppend(s.path)
		if err != nil {
			return err
		}
		s.f, s.size = f, sz
	}
	n, err := s.f.Write(append(data, '\n'))
	s.size += int64(n)
	if err != nil {
		return err
	}
	if s.size > maxFileBytes {
		if err := s.f.Close(); err != nil {
			return err
		}
		s.f = nil
		_ = os.Rename(s.path, s.path+".1")
		f, _, err := openAppend(s.path)
		if err != nil {
			return err
		}
		s.f, s.size = f, 0
	}
	return nil
}

// Close flushes and closes the underlying file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// ReadEvents returns every cost event in the active (non-rotated) file. Used by
// `cost history`/`cost session` for aggregation. Malformed lines are skipped.
func ReadEvents() ([]CostEvent, error) {
	dir := StoreDir()
	if dir == "" {
		return nil, nil
	}
	f, err := os.Open(filepath.Join(dir, eventsFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	// Dedup by request id: separate `cost watch` runs each re-seed and re-append
	// a session's turns, so the same request can appear many times on disk.
	// Request ids are globally unique, so first-seen-wins yields correct totals
	// regardless of how often the watcher ran.
	var out []CostEvent
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var e CostEvent
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if e.Kind != "cost_event" {
			continue
		}
		if e.RequestID != "" {
			if seen[e.RequestID] {
				continue
			}
			seen[e.RequestID] = true
		}
		out = append(out, e)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return out, nil
}
