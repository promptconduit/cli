package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	Short: "Recompute recorded Cursor cost from each event's raw hook payload",
	Long: `Cost is fixed when an event is recorded, so Cursor events captured before
v0.21.0 still carry the cache double-count fixed in #169. reprice re-derives
the cost enrichment of every recorded Cursor event in
~/.promptconduit/events.jsonl from the raw hook payload stored with it, using
the current parser and rate table. Request timestamps are kept.

It is idempotent: it always works from the raw payload, so a second run changes
nothing. Events whose cost doesn't change are left byte-for-byte. Before
rewriting, the original versions of changed lines are saved next to the log.
Use --dry-run to see the effect without writing.`,
	SilenceUsage: true,
	RunE:         runCostReprice,
}

// repriceResult is the effect of repricing one events.jsonl line.
type repriceResult struct {
	out           []byte
	changed       bool
	before, after float64
}

// repriceCursorLine re-derives the cost enrichment of one envelope if it is a
// Cursor event with a cost enrichment and a raw payload. Everything else
// (other tools, malformed lines, already-correct cost) is returned unchanged.
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

	ts := capturedAt
	if len(old.Requests) > 0 && old.Requests[0].Timestamp != "" {
		ts = old.Requests[0].Timestamp
	}
	fresh, ok := enrich.CursorCostFromRaw(raw, table, ts)
	if !ok {
		return repriceResult{}
	}
	if math.Abs(fresh.Totals.USD-old.Totals.USD) < 1e-9 && fresh.Totals.Tokens == old.Totals.Tokens {
		return repriceResult{} // already correct: keep the original bytes
	}

	costJSON, err := json.Marshal(fresh)
	if err != nil {
		return repriceResult{}
	}
	enr["cost"] = costJSON
	enrJSON, err := json.Marshal(enr)
	if err != nil {
		return repriceResult{}
	}
	top["enrichments"] = enrJSON
	out, err := json.Marshal(top)
	if err != nil {
		return repriceResult{}
	}
	return repriceResult{out: out, changed: true, before: old.Totals.USD, after: fresh.Totals.USD}
}

func runCostReprice(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	table, err := cost.LoadPriceTable()
	if err != nil {
		return fmt.Errorf("load pricing table: %w", err)
	}

	var n int
	var before, after float64
	tally := func(r repriceResult) {
		n++
		before += r.before
		after += r.after
	}

	if costRepriceDryRun {
		f, err := os.Open(eventlog.EventsJSONLPath())
		if os.IsNotExist(err) {
			_, _ = fmt.Fprintln(out, "No event log yet; nothing to reprice.")
			return nil
		}
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		r := bufio.NewReaderSize(f, 1<<20)
		for {
			line, readErr := r.ReadBytes('\n')
			if res := repriceCursorLine(bytes.TrimSuffix(line, []byte("\n")), table); res.changed {
				tally(res)
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
		if n == 0 {
			_, _ = fmt.Fprintln(out, "All recorded Cursor cost is already current; nothing to reprice.")
			return nil
		}
		_, _ = fmt.Fprintf(out, "Would reprice %d Cursor event(s): $%.2f → $%.2f.\n", n, before, after)
		_, _ = fmt.Fprintln(out, "Run `promptconduit cost reprice` to apply.")
		return nil
	}

	// Originals of changed lines go to a sidecar backup, opened lazily so a
	// no-op run leaves nothing behind.
	backupPath := filepath.Join(eventlog.Dir(), "events.jsonl.reprice-backup-"+time.Now().Format("20060102-150405"))
	var backup *os.File
	var backupErr error
	changed, err := eventlog.RewriteEvents(func(line []byte) ([]byte, bool) {
		res := repriceCursorLine(line, table)
		if !res.changed || backupErr != nil {
			return nil, false
		}
		if backup == nil {
			backup, backupErr = os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if backupErr != nil {
				return nil, false // no backup, no change
			}
		}
		if _, backupErr = backup.Write(append(append([]byte{}, line...), '\n')); backupErr != nil {
			return nil, false
		}
		tally(res)
		return res.out, true
	})
	if backup != nil {
		_ = backup.Close()
	}
	if errors.Is(err, eventlog.ErrRewriteBusy) {
		return err
	}
	if err != nil {
		return fmt.Errorf("rewrite event log: %w", err)
	}
	if backupErr != nil {
		return fmt.Errorf("write backup %s: %w", backupPath, backupErr)
	}
	if changed == 0 {
		_, _ = fmt.Fprintln(out, "All recorded Cursor cost is already current; nothing to reprice.")
		return nil
	}
	_, _ = fmt.Fprintf(out, "Repriced %d Cursor event(s): $%.2f → $%.2f.\n", changed, before, after)
	_, _ = fmt.Fprintf(out, "Original lines saved to %s\n", backupPath)
	return nil
}
