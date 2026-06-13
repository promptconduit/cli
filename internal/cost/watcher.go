package cost

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/promptconduit/cli/internal/outbound"
)

// rescanInterval is how often the watcher looks for newly-created or recently
// touched transcripts (a new or resumed session) to start tailing.
const rescanInterval = 3 * time.Second

// recentWindow bounds which not-yet-tailed transcripts a rescan picks up: only
// those modified within this window count as "live" sessions worth tailing.
const recentWindow = 2 * time.Minute

// Watcher aggregates priced turns into per-session running totals and emits
// CostEvent + SessionSummary records. It owns all dedup and accumulation state;
// the file-tailing orchestration lives in Run.
type Watcher struct {
	table *PriceTable
	store *Store    // optional; nil disables on-disk persistence
	out   io.Writer // newline-delimited JSON sink (e.g. stdout)
	emit  bool      // emit per-turn CostEvents (false => summaries only)

	mu       sync.Mutex
	seen     map[string]bool
	sessions map[string]*sessionState
}

type sessionState struct {
	summary SessionSummary
	byModel map[string]*ModelTotal
}

// NewWatcher builds a watcher. Pass emitEvents=false for one-shot summaries
// (e.g. `cost session`), true for the live `cost watch` stream.
func NewWatcher(table *PriceTable, store *Store, out io.Writer, emitEvents bool) *Watcher {
	return &Watcher{
		table:    table,
		store:    store,
		out:      out,
		emit:     emitEvents,
		seen:     make(map[string]bool),
		sessions: make(map[string]*sessionState),
	}
}

// Run seeds the newest transcript per directory (to populate session totals
// without flooding the stream), tails it for new turns, and periodically
// rescans for new/resumed sessions. Returns when ctx is cancelled.
func (w *Watcher) Run(ctx context.Context, dirs []string) error {
	var wg sync.WaitGroup
	tailed := make(map[string]bool)
	var tmu sync.Mutex

	startTail := func(path string) {
		tmu.Lock()
		if tailed[path] {
			tmu.Unlock()
			return
		}
		tailed[path] = true
		tmu.Unlock()

		sessionID := sessionIDFromPath(path)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for raw := range outbound.Tail(ctx, path, 0) { // start at current end
				w.streamLine(raw, sessionID)
			}
		}()
	}

	// Initial pass: seed + tail the newest transcript in each directory.
	for _, dir := range dirs {
		files := jsonlFiles(dir)
		if len(files) == 0 {
			continue
		}
		w.seedFile(files[0])
		startTail(files[0])
	}

	// Rescan loop: pick up newly-created or recently-modified transcripts.
	ticker := time.NewTicker(rescanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-ticker.C:
			cutoff := nowUnixNano() - int64(recentWindow)
			for _, dir := range dirs {
				for _, path := range jsonlFiles(dir) {
					tmu.Lock()
					already := tailed[path]
					tmu.Unlock()
					if already {
						continue
					}
					if info, err := os.Stat(path); err == nil && info.ModTime().UnixNano() >= cutoff {
						startTail(path)
					}
				}
			}
		}
	}
}

// SeedNewest seeds the single newest transcript across the given directories
// and emits its SessionSummary. Used by the one-shot `cost session` command.
func (w *Watcher) SeedNewest(dirs []string) {
	var newest string
	var newestMod int64
	for _, dir := range dirs {
		files := jsonlFiles(dir)
		if len(files) == 0 {
			continue
		}
		if info, err := os.Stat(files[0]); err == nil && info.ModTime().UnixNano() > newestMod {
			newestMod = info.ModTime().UnixNano()
			newest = files[0]
		}
	}
	if newest != "" {
		w.seedFile(newest)
	}
}

// seedFile fully parses one transcript, accumulating its turns into the session
// aggregate without emitting per-turn events, then emits one SessionSummary.
func (w *Watcher) seedFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sessionFallback := sessionIDFromPath(path)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 100*1024*1024)

	var lastSession string
	for scanner.Scan() {
		ev, _, ok := parseClaudeCodeLine(scanner.Bytes(), w.table, sessionFallback)
		if !ok {
			continue
		}
		if w.apply(ev) {
			lastSession = ev.SessionID
			if w.store != nil {
				_ = w.store.AppendEvent(ev)
			}
		}
	}
	if lastSession != "" {
		w.emitSummary(lastSession)
	}
}

// streamLine handles one newly-appended transcript line: parse, dedup,
// aggregate, persist, and emit a CostEvent (if enabled) plus the updated
// SessionSummary.
func (w *Watcher) streamLine(line []byte, sessionFallback string) {
	ev, _, ok := parseClaudeCodeLine(line, w.table, sessionFallback)
	if !ok {
		return
	}
	if !w.apply(ev) {
		return // duplicate
	}
	if w.store != nil {
		_ = w.store.AppendEvent(ev)
	}
	if w.emit {
		w.emitJSON(ev)
	}
	w.emitSummary(ev.SessionID)
}

// apply folds an event into the session aggregate. Returns false if the event
// was already seen (deduped by request id).
func (w *Watcher) apply(ev CostEvent) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if ev.RequestID != "" {
		if w.seen[ev.RequestID] {
			return false
		}
		w.seen[ev.RequestID] = true
	}

	st := w.sessions[ev.SessionID]
	if st == nil {
		st = &sessionState{
			summary: SessionSummary{
				V:         SchemaVersion,
				Kind:      "session_summary",
				SessionID: ev.SessionID,
				Tool:      ev.Tool,
				StartedAt: ev.Timestamp,
			},
			byModel: make(map[string]*ModelTotal),
		}
		w.sessions[ev.SessionID] = st
	}

	t := &st.summary.Totals
	t.Input += ev.Tokens.Input
	t.Output += ev.Tokens.Output
	t.CacheRead += ev.Tokens.CacheRead
	t.CacheWrite += ev.Tokens.CacheWrite
	t.CostTotal += ev.Cost.Total
	t.Currency = Currency

	mt := st.byModel[ev.Model]
	if mt == nil {
		mt = &ModelTotal{Model: ev.Model}
		st.byModel[ev.Model] = mt
	}
	mt.Tokens.Input += ev.Tokens.Input
	mt.Tokens.Output += ev.Tokens.Output
	mt.Tokens.CacheRead += ev.Tokens.CacheRead
	mt.Tokens.CacheWrite += ev.Tokens.CacheWrite
	mt.CostTotal += ev.Cost.Total

	if st.summary.StartedAt == "" {
		st.summary.StartedAt = ev.Timestamp
	}
	st.summary.UpdatedAt = ev.Timestamp
	st.summary.Source = worstSource(st.summary.Source, ev.Source)
	return true
}

// emitSummary writes the current SessionSummary for a session.
func (w *Watcher) emitSummary(sessionID string) {
	w.mu.Lock()
	st := w.sessions[sessionID]
	if st == nil {
		w.mu.Unlock()
		return
	}
	summary := st.summary
	summary.ByModel = make([]ModelTotal, 0, len(st.byModel))
	for _, mt := range st.byModel {
		summary.ByModel = append(summary.ByModel, *mt)
	}
	w.mu.Unlock()

	sort.Slice(summary.ByModel, func(i, j int) bool {
		return summary.ByModel[i].CostTotal > summary.ByModel[j].CostTotal
	})
	w.emitJSON(summary)
}

// emitJSON writes one record as a JSON line, serializing writes so concurrent
// tail goroutines don't interleave output.
func (w *Watcher) emitJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.out.Write(append(data, '\n'))
}

// worstSource returns the least-accurate of two source labels for a session.
func worstSource(a, b string) string {
	if sourceRank(b) > sourceRank(a) {
		return b
	}
	if a == "" {
		return b
	}
	return a
}

func sourceRank(s string) int {
	switch s {
	case SourceEstimate:
		return 2
	case SourceReconciled:
		return 1
	case SourceExact:
		return 0
	default:
		return -1
	}
}

// nowUnixNano is a tiny indirection so the rescan loop's time read is the only
// wall-clock dependency and is easy to reason about in tests.
func nowUnixNano() int64 { return time.Now().UnixNano() }
