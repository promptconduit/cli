package cost

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	tools    ToolCounter // counts each tool_use once across a request's lines
	sessions map[string]*sessionState

	// from/to bound which requests count (zero = unbounded). A request is in
	// or out by its first line's timestamp; excluded remembers the ones left
	// out so their later lines (which repeat the usage) are skipped too.
	from, to time.Time
	excluded map[string]bool
}

type sessionState struct {
	summary SessionSummary
	byModel map[string]*ModelTotal
	// cost accumulates the per-component dollar breakdown across the session.
	// SessionTotal only keeps a flat CostTotal, but the session-level Signals
	// (cache-miss cost share) need the Input/CacheWrite components, so we sum
	// them here as turns are applied.
	cost Cost
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
		excluded: make(map[string]bool),
		sessions: make(map[string]*sessionState),
	}
}

// SetWindow limits the watcher to requests whose timestamp falls in
// [from, to] (both inclusive; a zero bound is open). A request without a
// parseable timestamp is excluded once either bound is set, since it can't be
// placed in the window. Call before seeding.
func (w *Watcher) SetWindow(from, to time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.from, w.to = from, to
}

// inWindow reports whether a request stamped ts falls in the window.
func (w *Watcher) inWindow(ts string) bool {
	if w.from.IsZero() && w.to.IsZero() {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return false
	}
	if !w.from.IsZero() && t.Before(w.from) {
		return false
	}
	if !w.to.IsZero() && t.After(w.to) {
		return false
	}
	return true
}

// Run watches two kinds of sources: Claude Code transcript directories (parsed
// per line) and Cursor cost-feed files (pre-priced CostEvent lines written by
// `cost hook`). It seeds each (to populate session totals without flooding the
// stream), tails for new entries, and periodically rescans Claude Code dirs for
// new/resumed sessions. Returns when ctx is cancelled.
func (w *Watcher) Run(ctx context.Context, dirs []string, cursorFeeds []string, cursorRescanDir string) error {
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

	// Cursor feeds carry already-priced CostEvent JSON lines, so they tail with
	// a different handler (unmarshal, not parse-transcript).
	startCursorTail := func(path string) {
		tmu.Lock()
		if tailed[path] {
			tmu.Unlock()
			return
		}
		tailed[path] = true
		tmu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for raw := range outbound.Tail(ctx, path, 0) {
				w.streamCostEventLine(raw)
			}
		}()
	}

	// Seed all sources first (accumulate only — no per-turn emits), so the
	// initial snapshot below reflects complete session totals.
	var ccNewest []string
	for _, dir := range dirs {
		files := jsonlFiles(dir)
		if len(files) == 0 {
			continue
		}
		w.seedFile(files[0])
		ccNewest = append(ccNewest, files[0])
	}
	for _, feed := range cursorFeeds {
		w.seedCursorFeed(feed)
	}

	// Emit one snapshot summary per known session, then start tailing for
	// live per-turn updates.
	w.emitAllSessions()
	for _, p := range ccNewest {
		startTail(p)
	}
	for _, feed := range cursorFeeds {
		startCursorTail(feed)
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
			// Under --all, pick up Cursor feeds for workspaces that started
			// after the watcher did (cli#63). New feed files are near-empty, so
			// tailing from the end captures everything without re-seeding.
			if cursorRescanDir != "" {
				if entries, err := os.ReadDir(cursorRescanDir); err == nil {
					for _, e := range entries {
						if e.IsDir() || !strings.HasSuffix(e.Name(), ".ndjson") {
							continue
						}
						p := filepath.Join(cursorRescanDir, e.Name())
						tmu.Lock()
						already := tailed[p]
						tmu.Unlock()
						if !already {
							startCursorTail(p)
						}
					}
				}
			}
		}
	}
}

// SeedNewest seeds the single newest transcript across the given directories,
// plus that session's subagent transcripts, and emits its SessionSummary. Used
// by the one-shot `cost session` command.
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
		for _, sub := range SubagentTranscripts(newest) {
			w.seedFile(sub)
		}
	}
}

// SeedSession seeds one session's transcript (and its subagent transcripts)
// by path, for `cost session --session <id>`.
func (w *Watcher) SeedSession(path string) {
	w.seedFile(path)
	for _, sub := range SubagentTranscripts(path) {
		w.seedFile(sub)
	}
}

// SubagentTranscripts lists a session's subagent transcripts. Claude Code
// writes each subagent's turns to <session>/subagents/agent-*.jsonl next to
// the session's own <session>.jsonl, and workflow agents one level deeper
// (<session>/subagents/workflows/wf_*/agent-*.jsonl). Those lines carry the
// parent sessionId, so seeding them folds subagent spend into the parent
// session's summary. The main transcript never repeats those requests.
//
// It returns every *.jsonl under <session>/subagents/,
// recursively, in a stable (lexical) order. Empty when there are none.
func SubagentTranscripts(sessionPath string) []string {
	root := filepath.Join(strings.TrimSuffix(sessionPath, ".jsonl"), "subagents")
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // missing root or unreadable entry: skip, never fail
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// SeedCursorFeeds seeds each Cursor cost feed and emits its session summaries.
// Used by the one-shot `cost session` command so a Cursor-only workspace still
// shows a total.
func (w *Watcher) SeedCursorFeeds(feeds []string) {
	for _, feed := range feeds {
		w.seedCursorFeed(feed)
	}
}

// seedFile fully parses one transcript, accumulating its turns into the session
// aggregate without emitting per-turn events, then emits one SessionSummary.
func (w *Watcher) seedFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	sessionFallback := sessionIDFromPath(path)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 100*1024*1024)

	for scanner.Scan() {
		ev, _, ok := parseClaudeCodeLine(scanner.Bytes(), w.table, sessionFallback)
		if !ok {
			continue
		}
		if w.apply(ev) && w.store != nil {
			_ = w.store.AppendEvent(ev)
		}
	}
}

// seedCursorFeed fully reads a Cursor cost feed (already-priced CostEvent
// lines), accumulating into session aggregates without per-turn emits, then
// emits one SessionSummary per session it touched.
func (w *Watcher) seedCursorFeed(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var ev CostEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil || ev.Kind != "cost_event" {
			continue
		}
		if w.apply(ev) && w.store != nil {
			_ = w.store.AppendEvent(ev)
		}
	}
}

// streamCostEventLine handles one newly-appended Cursor feed line: unmarshal a
// pre-priced CostEvent, dedup, aggregate, persist, and emit it plus the updated
// SessionSummary.
func (w *Watcher) streamCostEventLine(line []byte) {
	var ev CostEvent
	if err := json.Unmarshal(line, &ev); err != nil || ev.Kind != "cost_event" {
		return
	}
	if !w.apply(ev) {
		return
	}
	if w.store != nil {
		_ = w.store.AppendEvent(ev)
	}
	if w.emit {
		w.emitJSON(ev)
	}
	w.emitSummary(ev.SessionID)
}

// streamLine handles one newly-appended transcript line: parse, dedup,
// aggregate, persist, and emit a CostEvent (if enabled) plus the updated
// SessionSummary.
func (w *Watcher) streamLine(line []byte, sessionFallback string) {
	ev, _, ok := parseClaudeCodeLine(line, w.table, sessionFallback)
	if !ok {
		return
	}
	fresh, toolsAdded := w.applyLine(ev)
	if !fresh {
		// A later line of an already-counted request: no new tokens or
		// cost, but it may carry more of the request's tool calls.
		if toolsAdded {
			w.emitSummary(ev.SessionID)
		}
		return
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
	fresh, _ := w.applyLine(ev)
	return fresh
}

// applyLine is apply that also reports whether a duplicate line added tool
// calls. Claude Code writes one transcript line per content block, all sharing
// the request's requestId: tokens and cost count once (first line), while
// each line's tool_use blocks are counted once each by id.
func (w *Watcher) applyLine(ev CostEvent) (fresh, toolsAdded bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if ev.RequestID != "" {
		if w.seen[ev.RequestID] {
			if w.excluded[ev.RequestID] {
				return false, false
			}
			if st := w.sessions[ev.SessionID]; st != nil {
				toolsAdded = w.tools.Fold(&st.summary.Tools, ev) > 0
			}
			return false, toolsAdded
		}
		w.seen[ev.RequestID] = true
	}
	if !w.inWindow(ev.Timestamp) {
		if ev.RequestID != "" {
			w.excluded[ev.RequestID] = true
		}
		return false, false
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

	st.summary.Requests++
	t := &st.summary.Totals
	t.Input += ev.Tokens.Input
	t.Output += ev.Tokens.Output
	t.CacheRead += ev.Tokens.CacheRead
	t.CacheWrite += ev.Tokens.CacheWrite
	t.CostTotal += ev.Cost.Total
	t.Currency = Currency

	// Sum the per-component cost so session Signals can derive cache-miss cost
	// share (which needs the input + cache-write components, not just the total).
	st.cost.Input += ev.Cost.Input
	st.cost.Output += ev.Cost.Output
	st.cost.CacheRead += ev.Cost.CacheRead
	st.cost.CacheWrite += ev.Cost.CacheWrite
	st.cost.Total += ev.Cost.Total

	if ev.toolCalls != nil {
		w.tools.Fold(&st.summary.Tools, ev) // transcript line: keyed calls
	} else {
		st.summary.Tools.add(ev.Tools) // pre-summarized (Cursor feed)
	}

	mt := st.byModel[ev.Model]
	if mt == nil {
		mt = &ModelTotal{Model: ev.Model, ModelPriced: ev.ModelPriced}
		st.byModel[ev.Model] = mt
	}
	mt.Tokens.Input += ev.Tokens.Input
	mt.Tokens.Output += ev.Tokens.Output
	mt.Tokens.CacheRead += ev.Tokens.CacheRead
	mt.Tokens.CacheWrite += ev.Tokens.CacheWrite
	mt.CostTotal += ev.Cost.Total

	// Min/max, not first/last: a session is seeded file by file (main
	// transcript, then each subagent transcript), so the last event applied is
	// often an older subagent turn. UpdatedAt also picks the newest session in
	// LatestSummary, so it must be the session's real latest activity.
	if ev.Timestamp != "" {
		if st.summary.StartedAt == "" || ev.Timestamp < st.summary.StartedAt {
			st.summary.StartedAt = ev.Timestamp
		}
		if ev.Timestamp > st.summary.UpdatedAt {
			st.summary.UpdatedAt = ev.Timestamp
		}
	}
	st.summary.Source = worstSource(st.summary.Source, ev.Source)
	return true, false
}

// emitAllSessions emits a snapshot summary for every known session.
func (w *Watcher) emitAllSessions() {
	w.mu.Lock()
	ids := make([]string, 0, len(w.sessions))
	for id := range w.sessions {
		ids = append(ids, id)
	}
	w.mu.Unlock()
	for _, id := range ids {
		w.emitSummary(id)
	}
}

// LatestSummary returns the most-recently-updated session's summary (with its
// by-model breakdown populated and sorted), for the one-shot `cost`/`cost
// session` command. A Cursor workspace feed may hold many past conversations;
// this returns just the current one. ok is false if no session was seen.
func (w *Watcher) LatestSummary() (SessionSummary, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var latestID, latestTs string
	for id, st := range w.sessions {
		if latestID == "" || st.summary.UpdatedAt > latestTs {
			latestID, latestTs = id, st.summary.UpdatedAt
		}
	}
	if latestID == "" {
		return SessionSummary{}, false
	}
	return w.summaryLocked(latestID), true
}

// SummaryFor returns one session's summary (by-model breakdown populated and
// sorted). ok is false when no request of that session was applied, e.g.
// because none fell in the window.
func (w *Watcher) SummaryFor(sessionID string) (SessionSummary, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sessions[sessionID] == nil {
		return SessionSummary{}, false
	}
	return w.summaryLocked(sessionID), true
}

// summaryLocked snapshots a known session's summary. Caller holds w.mu.
func (w *Watcher) summaryLocked(id string) SessionSummary {
	st := w.sessions[id]
	summary := st.summary
	summary.ByModel = make([]ModelTotal, 0, len(st.byModel))
	for _, mt := range st.byModel {
		summary.ByModel = append(summary.ByModel, *mt)
	}
	sort.Slice(summary.ByModel, func(i, j int) bool {
		return summary.ByModel[i].CostTotal > summary.ByModel[j].CostTotal
	})
	summary.Signals = sessionSignals(summary, st.cost)
	return summary
}

// sessionSignals derives the session-level Signals from accumulated totals (not
// by averaging per-turn signals): the cache/input rates are recomputed from the
// summed token counts, and cache-miss cost share from the summed cost
// components. Tier follows the dominant (costliest) model, which is ByModel[0]
// after the caller sorts. accCost is the per-component cost sum kept in
// sessionState. Must be called after ByModel is populated and sorted.
func sessionSignals(s SessionSummary, accCost Cost) Signals {
	tok := Tokens{
		Input:      s.Totals.Input,
		Output:     s.Totals.Output,
		CacheRead:  s.Totals.CacheRead,
		CacheWrite: s.Totals.CacheWrite,
	}
	model, priced := "", false
	if len(s.ByModel) > 0 {
		model, priced = s.ByModel[0].Model, s.ByModel[0].ModelPriced
	}
	return computeSignals(tok, accCost, model, priced, s.Tools.Total)
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
	accCost := st.cost
	w.mu.Unlock()

	sort.Slice(summary.ByModel, func(i, j int) bool {
		return summary.ByModel[i].CostTotal > summary.ByModel[j].CostTotal
	})
	summary.Signals = sessionSignals(summary, accCost)
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
