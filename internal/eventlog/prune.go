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
// from being re-scanned on every event.

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

// ErrPruneBusy is returned by PruneExpired when another process holds the
// prune lock.
var ErrPruneBusy = errors.New("another prune is already running")

// HookEventsPath is the lightweight status trace written by the hook command
// (~/.promptconduit/hook-events).
func HookEventsPath() string { return filepath.Join(Dir(), "hook-events") }

func pruneLockPath() string  { return filepath.Join(Dir(), ".prune.lock") }
func pruneStampPath() string { return filepath.Join(Dir(), ".prune.stamp") }

func pruneTarget(ceiling int64) int64 { return ceiling / 100 * pruneTargetPercent }

// NeedsPrune reports whether an automatic retention pass is due: retention is
// on, a file is over its ceiling, and no pass was attempted within
// pruneMinInterval. Only stats files — safe on the hook hot path.
func NeedsPrune(retentionDays int) bool {
	if retentionDays <= 0 {
		return false
	}
	if !fileOverCeiling(EventsJSONLPath(), EventsCeiling) &&
		!fileOverCeiling(HookEventsPath(), hookEventsCeiling) {
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

// Prune runs one automatic retention pass over events.jsonl and the
// hook-events trace. Returns the total number of records removed; 0 when
// another process is already pruning. Best-effort: a failure on one file never
// affects the other or the caller.
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
	cutoff := nowUTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	writeMu.Lock()
	defer writeMu.Unlock()

	removed := pruneFile(EventsJSONLPath(), EventsCeiling, pruneTarget(EventsCeiling), cutoff, envelopeCapturedAt)
	removed += pruneFile(HookEventsPath(), hookEventsCeiling, pruneTarget(hookEventsCeiling), cutoff, hookEventTimestamp)
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
	sc := newLineScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		total++
		if cutoff.IsZero() {
			continue
		}
		if t, ok := tsOf(sc.Bytes()); ok && t.Before(cutoff) {
			expired++
		}
	}
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
// classifies each line, the second copies the survivors to the temp file.
func pruneFileFrom(path string, target int64, cutoff time.Time, tsOf timestampFn, startSize int64) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()

	// Pass 1. Read exactly the bytes present when we started, so appends that
	// arrive during the scan land strictly after startSize and are copied
	// verbatim below rather than double-counted here. events.jsonl lines always
	// end in '\n', so startSize is a clean line boundary.
	type lineInfo struct {
		size    int64 // including the newline
		expired bool  // unknown-ts lines are never expired
	}
	var lines []lineInfo
	var total int64
	sc := newLineScanner(io.LimitReader(f, startSize))
	for sc.Scan() {
		b := sc.Bytes()
		t, ok := tsOf(b)
		li := lineInfo{size: int64(len(b)) + 1, expired: ok && t.Before(cutoff)}
		lines = append(lines, li)
		total += li.size
	}
	if sc.Err() != nil {
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

	// Pass 2: stream the survivors into a temp file.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".prune-*")
	if err != nil {
		return 0
	}
	tmpName := tmp.Name()
	w := bufio.NewWriterSize(tmp, 1<<20)
	// Record + terminator go out as one write, mirroring appendLine (issue
	// #125). Nothing else holds this temp file's descriptor, so unlike the
	// live append path this was never a corruption vector — it is kept
	// single-write so the invariant "a line and its \n are never emitted
	// separately" holds everywhere a JSONL record is produced.
	line := make([]byte, 0, 4096)
	sc = newLineScanner(io.LimitReader(f, startSize))
	i := 0
	for ; sc.Scan(); i++ {
		if i >= len(drop) || drop[i] {
			continue
		}
		line = append(line[:0], sc.Bytes()...)
		line = append(line, '\n')
		_, _ = w.Write(line)
	}
	if sc.Err() != nil || i != len(lines) {
		cleanupTemp(tmp, tmpName) // the committed bytes changed between passes
		return 0
	}
	// Carry over any bytes appended after startSize by a concurrent writer, so no
	// in-flight capture is dropped by the rewrite.
	if _, err := f.Seek(startSize, io.SeekStart); err == nil {
		_, _ = io.Copy(w, f)
	}
	if err := w.Flush(); err != nil {
		cleanupTemp(tmp, tmpName)
		return 0
	}
	if err := tmp.Sync(); err != nil {
		cleanupTemp(tmp, tmpName)
		return 0
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return 0
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return 0
	}
	return removed
}

func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return sc
}

func cleanupTemp(f *os.File, name string) {
	_ = f.Close()
	_ = os.Remove(name)
}
