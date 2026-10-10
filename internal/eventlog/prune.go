package eventlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Local hook-history retention.
//
// Policy: TIME FLOOR + SIZE CEILING. Every record captured within the
// retention window (the floor) is kept — this is the "keep all hook history for
// at least N days" guarantee. Records older than the window are pruned only when
// a file grows past a generous size CEILING, and then oldest-first among the
// already-expired records, down to a TARGET below the ceiling (hysteresis, so
// the next rewrite is ceiling-target of new events away, not the next append).
// Data inside the window is never dropped, even if the file exceeds the ceiling.
// Retention of 0 (or negative) means keep forever.
//
// The prune is a full-file atomic rewrite (temp + rename), so it never leaves a
// half-written log. It never runs on the hook hot path: the hook only calls
// NeedsPrune (a few stats) and hands the rewrite to a detached subprocess. A
// cross-process lock keeps concurrent agents from stacking up rewrites, and a
// throttle stamp keeps an un-trimmable file (window alone over the ceiling)
// from being re-scanned on every event: the stamp records when the file's
// oldest record leaves the window, and no pass touches that file before then.

// EventsCeiling is the soft size ceiling for events.jsonl. The retention window
// always wins: records inside it are kept even past this. Older records are
// trimmed oldest-first only once the file grows beyond it.
const EventsCeiling int64 = 500 * 1024 * 1024 // 500 MB

// hookEventsCeiling bounds the lightweight ~/.promptconduit/hook-events status
// trace (consumed by the macOS menu-bar app), which otherwise grows unbounded.
const hookEventsCeiling int64 = 20 * 1024 * 1024 // 20 MB

// pruneTargetPercent: once a file trips its ceiling, expired records are
// trimmed down to this share of it.
const pruneTargetPercent = 80

// pruneMinInterval is the minimum gap between automatic passes.
const pruneMinInterval = 15 * time.Minute

// pruneLockStale: a lock older than this was left by a pruner that was killed
// mid-pass, and is broken. Comfortably above a full pass over a 500 MB log.
const pruneLockStale = 10 * time.Minute

// pruneTempStale: .prune-* temp files older than this are orphans of killed
// passes and are swept.
const pruneTempStale = time.Hour

// oldestProbeLines bounds how many leading timestamp-less lines
// oldestRecordTime looks past before giving up (and falling back to a scan).
const oldestProbeLines = 64

// ErrPruneBusy is returned by PruneExpired when another process holds the
// prune lock.
var ErrPruneBusy = errors.New("another prune is already running")

// HookEventsPath is the lightweight status trace written by the hook command
// (~/.promptconduit/hook-events).
func HookEventsPath() string { return filepath.Join(Dir(), "hook-events") }

// AppendHookEvent appends one status line (no trailing newline) to the
// hook-events trace. Not gated on Enabled(): the trace predates the event log
// and the menu-bar app relies on it regardless. Best-effort.
func AppendHookEvent(line []byte) { appendLine(HookEventsPath(), line, 0) }

func pruneLockPath() string  { return filepath.Join(Dir(), ".prune.lock") }
func pruneStampPath() string { return filepath.Join(Dir(), ".prune.stamp") }

func pruneTarget(ceiling int64) int64 { return ceiling / 100 * pruneTargetPercent }

// prunedFile is one log under automatic retention.
type prunedFile struct {
	path    string
	ceiling int64
	tsOf    timestampFn
}

func prunedFiles() []prunedFile {
	return []prunedFile{
		{EventsJSONLPath(), EventsCeiling, envelopeCapturedAt},
		{HookEventsPath(), hookEventsCeiling, hookEventTimestamp},
	}
}

// NeedsPrune reports whether an automatic retention pass is due: retention is
// on, a file is over its ceiling, that file's oldest record may already have
// expired (see pruneStamp), and no pass was attempted within pruneMinInterval.
// Only stats files, plus a tiny stamp read once a file is over its ceiling —
// safe on the hook hot path.
func NeedsPrune(retentionDays int) bool {
	if retentionDays <= 0 {
		return false
	}
	window := time.Duration(retentionDays) * 24 * time.Hour
	var st *pruneStamp
	due := false
	for _, pf := range prunedFiles() {
		if !fileOverCeiling(pf.path, pf.ceiling) {
			continue
		}
		if st == nil {
			st = loadPruneStamp()
		}
		// The last pass saw this file's oldest record; nothing in the file can
		// expire before that record leaves the CURRENT window (computed here, so
		// a retention change takes effect immediately).
		if oldest, ok := st.oldestOf(pf.path); ok && !oldest.Before(nowUTC().Add(-window)) {
			continue
		}
		due = true
		break
	}
	if !due {
		return false
	}
	if info, err := os.Stat(pruneStampPath()); err == nil && time.Since(info.ModTime()) < pruneMinInterval {
		return false
	}
	return true
}

// MarkPruneAttempt stamps the throttle clock so NeedsPrune stays false for
// pruneMinInterval. The hook calls it before spawning a pruner so sibling hooks
// firing in the same instant don't each spawn one.
func MarkPruneAttempt() {
	p := pruneStampPath()
	now := time.Now()
	if err := os.Chtimes(p, now, now); err == nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, nil, 0o644)
}

// pruneStamp is the content of the throttle stamp file. Oldest maps a log's
// base name to the timestamp of its oldest record at the last pass. Until that
// record leaves the retention window (oldest + the window in effect when
// NeedsPrune runs) an automatic pass over that file cannot remove anything, so
// it is skipped without reading the file. Storing the record time rather than
// an absolute expiry keeps a lowered retention from being ignored. The stamp's mtime is the separate
// pruneMinInterval throttle clock.
type pruneStamp struct {
	Oldest map[string]time.Time `json:"oldest,omitempty"`
}

func (s *pruneStamp) oldestOf(path string) (time.Time, bool) {
	if s == nil || s.Oldest == nil {
		return time.Time{}, false
	}
	t, ok := s.Oldest[filepath.Base(path)]
	return t, ok
}

// loadPruneStamp reads the stamp. An absent, empty (legacy), or corrupt stamp
// yields an empty one, which never suppresses a pass.
func loadPruneStamp() *pruneStamp {
	st := &pruneStamp{}
	data, err := os.ReadFile(pruneStampPath())
	if err != nil || len(data) == 0 {
		return st
	}
	if json.Unmarshal(data, st) != nil {
		return &pruneStamp{}
	}
	return st
}

// savePruneStamp atomically replaces the stamp (which also refreshes its mtime,
// i.e. counts as an attempt).
func savePruneStamp(st *pruneStamp) {
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	p := pruneStampPath()
	dir := filepath.Dir(p)
	_ = os.MkdirAll(dir, 0o755)
	tmp, err := os.CreateTemp(dir, ".prune-stamp-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		cleanupTemp(tmp, name)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return
	}
	if err := os.Rename(name, p); err != nil {
		_ = os.Remove(name)
	}
}

// oldestRecordTime returns the timestamp of the first record in path that has
// one, reading only the head of the file. The logs are append-only in capture
// order (rewrites preserve order), so this is the oldest record that could be
// pruned; timestamp-less lines are never pruned, so looking past them is safe.
// ok=false when no timestamp is found within the first oldestProbeLines lines.
func oldestRecordTime(path string, tsOf timestampFn) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = f.Close() }()
	var (
		found time.Time
		ok    bool
		n     int
	)
	_ = scanLines(f, func(head []byte, _ int64, _ bool) bool {
		n++
		if t, good := tsOf(head); good {
			found, ok = t, true
			return false
		}
		return n < oldestProbeLines
	})
	return found, ok
}

// Prune runs one automatic retention pass over events.jsonl and the
// hook-events trace. Returns the total number of records removed; 0 when
// another process is already pruning. Best-effort: a failure on one file never
// affects the other or the caller.
//
// A file whose oldest record is still inside the window is skipped after
// reading only its head — nothing in it can be removed — and that record's
// timestamp is saved in the stamp, so NeedsPrune stays false for the file
// until it leaves the window. Without this, a log over the ceiling purely with in-window data
// would be re-read in full on every pass while nothing is ever removed.
func Prune(retentionDays int) int {
	if retentionDays <= 0 {
		return 0
	}
	release, ok := tryPruneLock()
	if !ok {
		return 0
	}
	defer release()
	MarkPruneAttempt()
	sweepStaleTemps()
	window := time.Duration(retentionDays) * 24 * time.Hour
	cutoff := nowUTC().Add(-window)

	writeMu.Lock()
	defer writeMu.Unlock()

	st := &pruneStamp{Oldest: map[string]time.Time{}}
	removed := 0
	for _, pf := range prunedFiles() {
		if oldest, ok := oldestRecordTime(pf.path, pf.tsOf); ok && !oldest.Before(cutoff) {
			st.Oldest[filepath.Base(pf.path)] = oldest
			continue
		}
		removed += pruneFile(pf.path, pf.ceiling, pruneTarget(pf.ceiling), cutoff, pf.tsOf)
		// Whatever survived, its oldest record bounds the next useful pass.
		if oldest, ok := oldestRecordTime(pf.path, pf.tsOf); ok && !oldest.Before(cutoff) {
			st.Oldest[filepath.Base(pf.path)] = oldest
		}
	}
	savePruneStamp(st)
	return removed
}

// PruneExpired trims EVERY record older than the retention window right now,
// from events.jsonl and the hook-events trace, ignoring the size ceiling. It is
// the explicit "reclaim space" action behind `promptconduit prune`. Records
// inside the window are always kept (the floor), so it can only ever remove
// already-expired data. Returns the number of records removed, or ErrPruneBusy
// when another process is pruning.
func PruneExpired(retentionDays int) (int, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	release, ok := tryPruneLock()
	if !ok {
		return 0, ErrPruneBusy
	}
	defer release()
	MarkPruneAttempt()
	sweepStaleTemps()
	cutoff := nowUTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	writeMu.Lock()
	defer writeMu.Unlock()

	// trigger/target 0 force the trim loop to drop every expired record.
	removed := pruneFile(EventsJSONLPath(), 0, 0, cutoff, envelopeCapturedAt)
	removed += pruneFile(HookEventsPath(), 0, 0, cutoff, hookEventTimestamp)
	return removed, nil
}

// tryPruneLock takes the cross-process prune lock without blocking. O_EXCL
// create is portable (Windows included); a lock older than pruneLockStale was
// left by a killed pruner and is broken once.
func tryPruneLock() (release func(), ok bool) {
	p := pruneLockPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(p) }, true
		}
		if !os.IsExist(err) {
			return nil, false
		}
		info, err := os.Stat(p)
		if err != nil || time.Since(info.ModTime()) < pruneLockStale {
			return nil, false
		}
		_ = os.Remove(p)
	}
	return nil, false
}

// sweepStaleTemps removes .prune-* temp files orphaned by passes that were
// killed before their rename. Called with the prune lock held, so no live pass
// owns a temp file old enough to match.
func sweepStaleTemps() {
	matches, _ := filepath.Glob(filepath.Join(Dir(), ".prune-*"))
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && time.Since(info.ModTime()) > pruneTempStale {
			_ = os.Remove(m)
		}
	}
}

// RetentionStats summarizes local hook history against a retention window.
type RetentionStats struct {
	RetentionDays int   // effective window; 0 means keep forever
	EventsTotal   int   // records in events.jsonl
	EventsExpired int   // events.jsonl records older than the window
	EventsBytes   int64 // size of events.jsonl on disk
	HookTotal     int   // records in the hook-events trace
	HookExpired   int   // hook-events records older than the window
}

// Stats reports, read-only, how much local history exists and how much of it is
// older than the retention window. Used by `promptconduit prune --dry-run`.
func Stats(retentionDays int) RetentionStats {
	s := RetentionStats{RetentionDays: retentionDays}
	if info, err := os.Stat(EventsJSONLPath()); err == nil {
		s.EventsBytes = info.Size()
	}
	if retentionDays <= 0 {
		// Keep forever: nothing is ever expired; still report totals.
		s.EventsTotal, _ = countExpired(EventsJSONLPath(), time.Time{}, envelopeCapturedAt)
		s.HookTotal, _ = countExpired(HookEventsPath(), time.Time{}, hookEventTimestamp)
		return s
	}
	cutoff := nowUTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	s.EventsTotal, s.EventsExpired = countExpired(EventsJSONLPath(), cutoff, envelopeCapturedAt)
	s.HookTotal, s.HookExpired = countExpired(HookEventsPath(), cutoff, hookEventTimestamp)
	return s
}

// countExpired reads path and returns (total records, records older than
// cutoff). A zero cutoff counts nothing as expired. Best-effort: an unreadable
// file reports zeros.
func countExpired(path string, cutoff time.Time, tsOf timestampFn) (total, expired int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer func() { _ = f.Close() }()
	_ = scanLines(f, func(head []byte, _ int64, _ bool) bool {
		if len(head) == 0 {
			return true
		}
		total++
		if cutoff.IsZero() {
			return true
		}
		if t, ok := tsOf(head); ok && t.Before(cutoff) {
			expired++
		}
		return true
	})
	return total, expired
}

func fileOverCeiling(path string, ceiling int64) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Size() > ceiling
}

// timestampFn extracts a record's time from a JSONL line. ok=false when the line
// has no parseable timestamp — such records are treated as "young" (kept), so a
// malformed or foreign line is never silently discarded.
type timestampFn func(line []byte) (t time.Time, ok bool)

// capturedAtKey prefixes the envelope's own timestamp. The envelope serializes
// captured_at ahead of raw_event, and every earlier field is a plain string (in
// which a literal quote is always escaped), so the first match is the
// envelope's — never a key nested in the native payload.
var capturedAtKey = []byte(`"captured_at":"`)

func envelopeCapturedAt(line []byte) (time.Time, bool) {
	// Fast path: slice out just the value instead of decoding a multi-KB line.
	if i := bytes.Index(line, capturedAtKey); i >= 0 {
		rest := line[i+len(capturedAtKey):]
		if j := bytes.IndexByte(rest, '"'); j > 0 {
			if t, err := time.Parse(time.RFC3339, string(rest[:j])); err == nil {
				return t, true
			}
		}
	}
	var rec struct {
		CapturedAt string `json:"captured_at"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.CapturedAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, rec.CapturedAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func hookEventTimestamp(line []byte) (time.Time, bool) {
	var rec struct {
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Timestamp == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, rec.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// pruneFile rewrites path once it exceeds trigger, keeping (a) every record
// younger than cutoff and (b) as many older records as fit under target, oldest
// dropped first. When nothing needs dropping it returns without touching the
// file. Any lines appended by another writer while the rewrite is in flight are
// carried over, so a concurrent capture is never lost. Called with writeMu and
// the prune lock held; returns the number of records removed.
func pruneFile(path string, trigger, target int64, cutoff time.Time, tsOf timestampFn) int {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	// Fast path: under the trigger → keep everything (the floor is already
	// satisfied, and we only ever trim expired records to honour the ceiling).
	// No read, no rewrite.
	if info.Size() <= trigger {
		return 0
	}
	return pruneFileFrom(path, target, cutoff, tsOf, info.Size())
}

// pruneFileFrom is pruneFile's core with an explicit startSize (the byte length
// to treat as the file's committed content). Only the first startSize bytes are
// scanned; anything appended past it by a concurrent writer is copied over
// verbatim, so an in-flight capture is never lost. Split out for testability.
//
// Two streaming passes keep memory O(lines) rather than O(file): the first
// classifies each line, the second copies the survivors to the temp file. Lines
// may be arbitrarily long (raw tool payloads can be tens of MB): only a line's
// head is inspected for its timestamp and survivors are copied as exact byte
// ranges. A line whose timestamp isn't in its head is treated as young (kept).
func pruneFileFrom(path string, target int64, cutoff time.Time, tsOf timestampFn, startSize int64) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0
	}

	// Pass 1. Read exactly the bytes present when we started, so appends that
	// arrive during the scan land strictly after startSize and are copied
	// verbatim below rather than double-counted here.
	type lineInfo struct {
		size    int64 // including the newline, when present
		expired bool  // unknown-ts lines are never expired
	}
	var lines []lineInfo
	var total int64
	err = scanLines(io.LimitReader(f, startSize), func(head []byte, size int64, _ bool) bool {
		t, ok := tsOf(head)
		lines = append(lines, lineInfo{size: size, expired: ok && t.Before(cutoff)})
		total += size
		return true
	})
	if err != nil {
		return 0 // never rewrite from a partial read
	}

	// Drop oldest EXPIRED records first until under the target. Young records
	// are never candidates, so the retention window is always preserved — even
	// if the window alone exceeds the ceiling (the floor wins).
	drop := make([]bool, len(lines))
	removed := 0
	for i := 0; i < len(lines) && total > target; i++ {
		if lines[i].expired {
			drop[i] = true
			total -= lines[i].size
			removed++
		}
	}
	if removed == 0 {
		return 0 // everything over the target is still within the window — keep it
	}

	// Pass 2: stream the survivors into a temp file as exact byte ranges, so a
	// record and its terminator always travel together (issue #125) and an
	// unterminated final record stays unterminated until the carried-over tail
	// completes it.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".prune-*")
	if err != nil {
		return 0
	}
	tmpName := tmp.Name()
	w := bufio.NewWriterSize(tmp, 1<<20)
	r := bufio.NewReaderSize(io.LimitReader(f, startSize), 1<<20)
	for i, li := range lines {
		dst := io.Writer(w)
		if drop[i] {
			dst = io.Discard
		}
		if n, err := io.CopyN(dst, r, li.size); err != nil || n != li.size {
			cleanupTemp(tmp, tmpName) // the committed bytes changed between passes
			return 0
		}
	}
	_ = tmp.Chmod(info.Mode().Perm())
	if err := finishRewrite(f, w, tmp, tmpName, path, startSize); err != nil {
		return 0
	}
	return removed
}

// finishRewrite carries over everything a concurrent writer appended to src
// past startSize, then atomically replaces path with tmp. Shared by the pruner
// and LockedRewrite. On error the temp file is removed and path is untouched.
//
// Two carry rounds: the first copies the bulk and pays for the slow fsync of
// the large temp file without blocking anyone; the second runs under the
// exclusive append lock (lockAppendsForRename), so no append can land in the
// old file between the final copy and the rename — such a line would be lost
// with the old inode.
func finishRewrite(src *os.File, w *bufio.Writer, tmp *os.File, tmpName, path string, startSize int64) error {
	copied := startSize
	carry := func() error {
		if _, err := src.Seek(copied, io.SeekStart); err != nil {
			return err
		}
		n, err := io.Copy(w, src)
		copied += n
		if err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
		return tmp.Sync()
	}
	if err := carry(); err != nil {
		cleanupTemp(tmp, tmpName)
		return err
	}
	release, ok := lockAppendsForRename(path)
	if !ok {
		cleanupTemp(tmp, tmpName)
		return errAppendLockBusy
	}
	defer release()
	if err := carry(); err != nil {
		cleanupTemp(tmp, tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	// Windows can't replace a file that is still open; close the source first.
	_ = src.Close()
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// lineHeadMax is how much of each line scanLines hands to its callback. Every
// timestamp we look for sits within the first few hundred bytes of a record.
const lineHeadMax = 64 * 1024

// scanLines calls fn for each line in r with the line's head (up to
// lineHeadMax bytes, without the trailing newline), its full size in bytes
// (including the newline, if any), and whether it was newline-terminated. A
// final unterminated fragment is reported too. Lines of any length are
// supported — unlike bufio.Scanner there is no cap that makes a whole scan fail
// on one oversized record. fn returns false to stop early; head is only valid
// during the call.
func scanLines(r io.Reader, fn func(head []byte, size int64, terminated bool) bool) error {
	br := bufio.NewReaderSize(r, lineHeadMax)
	headBuf := make([]byte, 0, lineHeadMax)
	for {
		chunk, err := br.ReadSlice('\n')
		size := int64(len(chunk))
		head := chunk
		if err == bufio.ErrBufferFull {
			// Oversized line: keep its head, skip through the rest.
			headBuf = append(headBuf[:0], chunk...)
			head = headBuf
			for err == bufio.ErrBufferFull {
				chunk, err = br.ReadSlice('\n')
				size += int64(len(chunk))
			}
		}
		if err != nil && err != io.EOF {
			return err
		}
		if size == 0 {
			return nil // clean EOF
		}
		terminated := err == nil
		if n := len(head); terminated && n > 0 && head[n-1] == '\n' {
			head = head[:n-1]
		}
		if !fn(head, size, terminated) {
			return nil
		}
		if err == io.EOF {
			return nil
		}
	}
}

func cleanupTemp(f *os.File, name string) {
	_ = f.Close()
	_ = os.Remove(name)
}
