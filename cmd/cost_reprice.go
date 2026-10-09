package cmd

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/promptconduit/cli/internal/cost"
	"github.com/promptconduit/cli/internal/enrich"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/spf13/cobra"
)

var costRepriceDryRun bool

var costRepriceCmd = &cobra.Command{
	Use:   "reprice",
	Short: "Fix recorded Cursor cost that double-counted cached tokens",
	Long: `Cost is fixed when an event is recorded, so Cursor events captured before
v0.21.0 still carry the cache double-count fixed in #169 (Cursor's
input_tokens already includes cache reads and writes). reprice finds those
events in ~/.promptconduit/events.jsonl and re-derives their cost enrichment
from the raw hook payload stored with each one, keeping the request timestamp.

Only events that show the bug are touched (stored input equals the raw,
cache-inclusive input_tokens), so events captured after the fix are never
changed and a second run does nothing. The original lines are written to a
backup file and fsynced before the log is rewritten. Use --dry-run to see the
effect without writing.`,
	SilenceUsage: true,
	RunE:         runCostReprice,
}

// repriceResult is the effect of repricing one events.jsonl line.
type repriceResult struct {
	out           []byte
	changed       bool
	before, after float64
}

// cursorRawTokens is the token block of a raw Cursor hook payload.
type cursorRawTokens struct {
	Input      int64 `json:"input_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
}

// repriceCursorLine re-derives the cost enrichment of one envelope when it is a
// Cursor event recorded with the cache double-count. Everything else (other
// tools, malformed lines, events already priced correctly) is unchanged.
func repriceCursorLine(line []byte, table *cost.PriceTable) repriceResult {
	// Cheap pre-filter: most lines are not Cursor cost events.
	if !bytes.Contains(line, []byte(`"cursor"`)) || !bytes.Contains(line, []byte(`"cost":`)) {
		return repriceResult{}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(line, &top); err != nil {
		return repriceResult{}
	}
	var tool, capturedAt string
	_ = json.Unmarshal(top["tool"], &tool)
	_ = json.Unmarshal(top["captured_at"], &capturedAt)
	raw := top["raw_event"]
	if tool != "cursor" || len(raw) == 0 {
		return repriceResult{}
	}
	var enr map[string]json.RawMessage
	if err := json.Unmarshal(top["enrichments"], &enr); err != nil || len(enr["cost"]) == 0 {
		return repriceResult{}
	}
	var old enrich.CostEnrichment
	if err := json.Unmarshal(enr["cost"], &old); err != nil {
		return repriceResult{}
	}

	// Only the bug: the raw payload is cache-inclusive and the stored input is
	// that inclusive count (pre-fix parser). Post-fix events store input minus
	// cache and never match, so reprice is idempotent no matter how the rate
	// table changes later.
	var rt cursorRawTokens
	if err := json.Unmarshal(raw, &rt); err != nil {
		return repriceResult{}
	}
	cached := rt.CacheRead + rt.CacheWrite
	if cached == 0 || rt.Input < cached || old.Totals.Tokens.Input != rt.Input {
		return repriceResult{}
	}

	ts := capturedAt
	if len(old.Requests) > 0 && old.Requests[0].Timestamp != "" {
		ts = old.Requests[0].Timestamp
	}
	fresh, ok := enrich.CursorCostFromRaw(raw, table, ts)
	if !ok {
		return repriceResult{}
	}
	costJSON, err := marshalNoEscape(fresh)
	if err != nil {
		return repriceResult{}
	}
	enr["cost"] = costJSON
	enrJSON, err := marshalNoEscape(enr)
	if err != nil {
		return repriceResult{}
	}
	top["enrichments"] = enrJSON
	out, err := marshalNoEscape(top)
	if err != nil {
		return repriceResult{}
	}
	return repriceResult{out: out, changed: true, before: old.Totals.USD, after: fresh.Totals.USD}
}

// marshalNoEscape is json.Marshal without HTML escaping, so <, >, & in
// preserved payloads (prompts, shell commands) keep their captured form.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// repriceScan is one read-only pass over the log: it tallies what would change
// and calls onChange with each original line that would be repriced.
type repriceScan struct {
	n             int
	before, after float64
}

func scanReprice(path string, table *cost.PriceTable, onChange func(line []byte, res repriceResult) error) (repriceScan, error) {
	var s repriceScan
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, readErr := r.ReadBytes('\n')
		body := bytes.TrimSuffix(line, []byte("\n"))
		if res := repriceCursorLine(body, table); res.changed {
			s.n++
			s.before += res.before
			s.after += res.after
			if onChange != nil {
				if err := onChange(body, res); err != nil {
					return s, err
				}
			}
		}
		if readErr == io.EOF {
			return s, nil
		}
		if readErr != nil {
			return s, readErr
		}
	}
}

func runCostReprice(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	table, err := cost.LoadPriceTable()
	if err != nil {
		return fmt.Errorf("load pricing table: %w", err)
	}
	path := eventlog.EventsJSONLPath()

	if costRepriceDryRun {
		s, err := scanReprice(path, table, nil)
		if err != nil {
			return fmt.Errorf("read event log: %w", err)
		}
		if s.n == 0 {
			_, _ = fmt.Fprintln(out, "No recorded Cursor cost needs repricing.")
			return nil
		}
		_, _ = fmt.Fprintf(out, "Would reprice %d Cursor event(s): $%.2f → $%.2f.\n", s.n, s.before, s.after)
		_, _ = fmt.Fprintln(out, "Run `promptconduit cost reprice` to apply.")
		return nil
	}

	// Pass 1 (under the rewrite lock): back up every line that will change and
	// fsync the backup BEFORE the log is touched; any failure aborts with the
	// log untouched. Pass 2 (same lock, so nothing changed in between) swaps in
	// the outputs computed here, only for lines whose original was backed up.
	backupPath := filepath.Join(eventlog.Dir(), "events.jsonl.reprice-backup-"+time.Now().Format("20060102-150405"))
	planned := map[[sha256.Size]byte]repriceResult{}
	prepare := func(path string) error {
		var backup *os.File
		_, err := scanReprice(path, table, func(line []byte, res repriceResult) error {
			if backup == nil {
				var err error
				if backup, err = os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600); err != nil {
					return err
				}
			}
			if _, err := backup.Write(append(append([]byte{}, line...), '\n')); err != nil {
				return err
			}
			planned[sha256.Sum256(line)] = res
			return nil
		})
		if backup != nil {
			if err == nil {
				err = backup.Sync()
			}
			if cerr := backup.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			return fmt.Errorf("write backup %s (event log not modified): %w", backupPath, err)
		}
		if len(planned) == 0 {
			return errNothingToReprice
		}
		return nil
	}

	var before, after float64
	changed, err := eventlog.LockedRewrite(prepare, func(line []byte) ([]byte, bool) {
		// Cheap filter first: only Cursor cost lines can be in the plan.
		if !bytes.Contains(line, []byte(`"cursor"`)) {
			return nil, false
		}
		res, ok := planned[sha256.Sum256(line)]
		if !ok {
			return nil, false
		}
		before += res.before
		after += res.after
		return res.out, true
	})
	if errors.Is(err, errNothingToReprice) {
		_, _ = fmt.Fprintln(out, "No recorded Cursor cost needs repricing.")
		return nil
	}
	if err != nil {
		if len(planned) > 0 {
			return fmt.Errorf("rewrite event log (backup kept at %s): %w", backupPath, err)
		}
		return err
	}
	_, _ = fmt.Fprintf(out, "Repriced %d Cursor event(s): $%.2f → $%.2f.\n", changed, before, after)
	_, _ = fmt.Fprintf(out, "Original lines saved to %s\n", backupPath)
	return nil
}

// errNothingToReprice aborts the locked rewrite when the backup pass found
// nothing to change, so no temp file or backup is created.
var errNothingToReprice = errors.New("nothing to reprice")
