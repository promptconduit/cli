package eventlog

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
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
// already-expired records. Data inside the window is never dropped, even if the
// file exceeds the ceiling. Retention of 0 (or negative) means keep forever.
//
// The prune is a full-file atomic rewrite (temp + rename), so it never leaves a
// half-written log. It runs opportunistically (see MaybePrune) and preserves any
// lines appended concurrently while it works.

// EventsCeiling is the soft size ceiling for events.jsonl. The retention window
// always wins: records inside it are kept even past this. Older records are
// trimmed oldest-first only once the file grows beyond it.
const EventsCeiling int64 = 500 * 1024 * 1024 // 500 MB

// hookEventsCeiling bounds the lightweight ~/.promptconduit/hook-events status
// trace (consumed by the macOS menu-bar app), which otherwise grows unbounded.
const hookEventsCeiling int64 = 20 * 1024 * 1024 // 20 MB

// pruneProbabilityDenominator: MaybePrune runs a full pass ~1/N invocations
// (plus always when a file is already over its ceiling). Mirrors the
// correlation store's opportunistic GC so retention costs nothing per event.
const pruneProbabilityDenominator = 100

// HookEventsPath is the lightweight status trace written by the hook command
// (~/.promptconduit/hook-events).
func HookEventsPath() string { return filepath.Join(Dir(), "hook-events") }

// MaybePrune opportunistically enforces retention on the local event files.
// retentionDays is the effective window; 0 or negative means keep forever (a
// no-op). It runs a full pass ~1/pruneProbabilityDenominator invocations, but
// always when a file already exceeds its ceiling, so disk can't run away on an
// unlucky roll. Best-effort and safe to call on the hook hot path.
func MaybePrune(retentionDays int) {
	if retentionDays <= 0 {
		return
	}
	forced := fileOverCeiling(EventsJSONLPath(), EventsCeiling) ||
		fileOverCeiling(HookEventsPath(), hookEventsCeiling)
	if !forced && !pruneDiceHit() {
		return
	}
	Prune(retentionDays)
}

// Prune runs one retention pass over events.jsonl and the hook-events trace.
// Returns the total number of records removed. Best-effort: a failure on one
// file never affects the other or the caller.
func Prune(retentionDays int) int {
	if retentionDays <= 0 {
		return 0
	}
	cutoff := nowUTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	writeMu.Lock()
	defer writeMu.Unlock()

	removed := pruneFile(EventsJSONLPath(), EventsCeiling, cutoff, envelopeCapturedAt)
	removed += pruneFile(HookEventsPath(), hookEventsCeiling, cutoff, hookEventTimestamp)
	return removed
}

// PruneExpired trims EVERY record older than the retention window right now,
// from events.jsonl and the hook-events trace, ignoring the size ceiling. It is
// the explicit "reclaim space" action behind `promptconduit prune`. Records
// inside the window are always kept (the floor), so it can only ever remove
// already-expired data. Returns the number of records removed.
func PruneExpired(retentionDays int) int {
	if retentionDays <= 0 {
		return 0
	}
	cutoff := nowUTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	writeMu.Lock()
	defer writeMu.Unlock()

	// ceiling 0 forces the trim loop to drop every expired record.
	removed := pruneFile(EventsJSONLPath(), 0, cutoff, envelopeCapturedAt)
	removed += pruneFile(HookEventsPath(), 0, cutoff, hookEventTimestamp)
	return removed
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
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
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

func pruneDiceHit() bool {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return false
	}
	return binary.BigEndian.Uint32(b[:])%pruneProbabilityDenominator == 0
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

func envelopeCapturedAt(line []byte) (time.Time, bool) {
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

// pruneFile rewrites path keeping (a) every record younger than cutoff and (b)
// as many older records as fit under ceiling, oldest dropped first. When nothing
// needs dropping it returns without touching the file. Any lines appended by
// another writer while the rewrite is in flight are carried over, so a
// concurrent capture is never lost. Called with writeMu held; returns the number
// of records removed.
func pruneFile(path string, ceiling int64, cutoff time.Time, tsOf timestampFn) int {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	// Fast path: under the ceiling → keep everything (the floor is already
	// satisfied, and we only ever trim expired records to honour the ceiling).
	// No read, no rewrite.
	if info.Size() <= ceiling {
		return 0
	}
	return pruneFileFrom(path, ceiling, cutoff, tsOf, info.Size())
}

// pruneFileFrom is pruneFile's core with an explicit startSize (the byte length
// to treat as the file's committed content). Only the first startSize bytes are
// scanned; anything appended past it by a concurrent writer is copied over
// verbatim, so an in-flight capture is never lost. Split out for testability.
func pruneFileFrom(path string, ceiling int64, cutoff time.Time, tsOf timestampFn, startSize int64) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	// Read exactly the bytes present when we started, so appends that arrive
	// during the scan land strictly after startSize and are copied verbatim
	// below rather than double-counted here. events.jsonl lines always end in
	// '\n', so startSize is a clean line boundary.
	type record struct {
		data  []byte
		young bool
		size  int64
	}
	var records []record
	var total int64
	sc := bufio.NewScanner(io.LimitReader(f, startSize))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		b := append([]byte(nil), sc.Bytes()...)
		t, ok := tsOf(b)
		young := !ok || !t.Before(cutoff) // unknown-ts lines are kept
		size := int64(len(b)) + 1         // +1 for the newline
		records = append(records, record{data: b, young: young, size: size})
		total += size
	}
	scanErr := sc.Err()
	_ = f.Close()
	if scanErr != nil {
		return 0 // never rewrite from a partial read
	}

	// Drop oldest EXPIRED records first until under the ceiling. Young records
	// are never candidates, so the retention window is always preserved — even
	// if the window alone exceeds the ceiling (the floor wins).
	keep := make([]bool, len(records))
	for i := range records {
		keep[i] = true
	}
	removed := 0
	for i := 0; i < len(records) && total > ceiling; i++ {
		if !records[i].young {
			keep[i] = false
			total -= records[i].size
			removed++
		}
	}
	if removed == 0 {
		return 0 // everything over the ceiling is still within the window — keep it
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".prune-*")
	if err != nil {
		return 0
	}
	tmpName := tmp.Name()
	w := bufio.NewWriter(tmp)
	// Record + terminator go out as one write, mirroring appendLine (issue
	// #125). Nothing else holds this temp file's descriptor, so unlike the
	// live append path this was never a corruption vector — it is kept
	// single-write so the invariant "a line and its \n are never emitted
	// separately" holds everywhere a JSONL record is produced.
	line := make([]byte, 0, 4096)
	for i, rec := range records {
		if !keep[i] {
			continue
		}
		line = append(line[:0], rec.data...)
		line = append(line, '\n')
		_, _ = w.Write(line)
	}
	// Carry over any bytes appended after startSize by a concurrent writer, so no
	// in-flight capture is dropped by the rewrite.
	if src, err := os.Open(path); err == nil {
		if _, err := src.Seek(startSize, io.SeekStart); err == nil {
			_, _ = io.Copy(w, src)
		}
		_ = src.Close()
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

func cleanupTemp(f *os.File, name string) {
	_ = f.Close()
	_ = os.Remove(name)
}
