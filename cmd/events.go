package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/spf13/cobra"
)

var (
	eventsTailN  int
	eventsFollow bool
	eventsErrors bool
	eventsRaw    bool
	eventsPath   bool
)

var eventsCmd = &cobra.Command{
	Use:           "events",
	Short:         "Inspect the local event log of payloads sent to the platform",
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Show the full JSON payloads the CLI has sent to the platform, recorded
locally before/at send time — the actual envelope plus the HTTP outcome
(status, latency) for each event.

This reads ~/.promptconduit/events.ndjson (full-fidelity, one record per send;
secrets scrubbed; the file rotates to events.ndjson.1 at 50MB). Use it to
answer "what did my hook actually send?" and "did it reach the platform?".

By default each record is shown as a one-line summary:

  16:55:02  sent    UserPromptSubmit  claude-code  3.2KB  → 200 (87ms)

Use --raw to print the full envelope payload beneath each summary, or
--errors to tail the human-readable failure/drop log (errors.log) instead.

Examples:
  promptconduit events                # last 20 records, summary lines
  promptconduit events --raw          # include the full payload per record
  promptconduit events -n 5           # last 5 records
  promptconduit events --follow       # stream new records live
  promptconduit events --errors       # tail errors.log (failures + drops)
  promptconduit events --path         # print the event-log file path`,
	RunE: runEvents,
}

func init() {
	eventsCmd.Flags().IntVarP(&eventsTailN, "lines", "n", 20, "Number of records to show from the end of the log")
	eventsCmd.Flags().BoolVarP(&eventsFollow, "follow", "f", false, "Stream new records as they are written (like `tail -f`)")
	eventsCmd.Flags().BoolVar(&eventsErrors, "errors", false, "Show the human-readable error log (errors.log) instead of the event log")
	eventsCmd.Flags().BoolVar(&eventsRaw, "raw", false, "Print the full JSON payload beneath each summary line")
	eventsCmd.Flags().BoolVar(&eventsPath, "path", false, "Print only the log file path and exit")
}

func runEvents(cmd *cobra.Command, args []string) error {
	path := eventlog.EventsPath()
	if eventsErrors {
		path = eventlog.ErrorsPath()
	}

	if eventsPath {
		cmd.Println(path)
		return nil
	}

	if eventsFollow {
		return followFile(cmd, path, renderEventLine)
	}

	// errors.log is already human-readable; print it verbatim.
	if eventsErrors {
		out, err := eventlog.TailErrors(eventsTailN)
		if err != nil {
			return fmt.Errorf("read error log: %w", err)
		}
		if out == "" {
			cmd.Printf("No errors recorded yet.\n  Path: %s\n", path)
			return nil
		}
		cmd.Print(out)
		if !strings.HasSuffix(out, "\n") {
			cmd.Println()
		}
		return nil
	}

	out, err := eventlog.TailEvents(eventsTailN)
	if err != nil {
		return fmt.Errorf("read event log: %w", err)
	}
	if out == "" {
		cmd.Printf("No events recorded yet.\n  Path: %s\n", path)
		cmd.Println("  (events are logged when the CLI sends to the platform; check `promptconduit status`)")
		return nil
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		cmd.Print(renderEventLine(line))
	}
	return nil
}

// renderEventLine turns one NDJSON event record into a readable summary line
// (plus the full payload when --raw is set). Lines that don't parse are echoed
// verbatim so nothing is silently hidden.
func renderEventLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	var rec struct {
		TS        string          `json:"ts"`
		Tool      string          `json:"tool"`
		HookEvent string          `json:"hook_event"`
		Outcome   string          `json:"outcome"`
		Status    int             `json:"status"`
		LatencyMs int64           `json:"latency_ms"`
		Error     string          `json:"error"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return line + "\n"
	}

	ts := rec.TS
	if t, err := time.Parse(time.RFC3339, rec.TS); err == nil {
		ts = t.Local().Format("15:04:05")
	}

	size := humanBytes(len(rec.Payload))
	statusPart := fmt.Sprintf("→ %d (%dms)", rec.Status, rec.LatencyMs)
	if rec.Status == 0 {
		statusPart = fmt.Sprintf("→ no response (%dms)", rec.LatencyMs)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s  %-7s %-18s %-12s %7s  %s\n",
		ts, rec.Outcome, dashIfEmpty(rec.HookEvent), dashIfEmpty(rec.Tool), size, statusPart)
	if rec.Error != "" {
		fmt.Fprintf(&b, "          error: %s\n", rec.Error)
	}
	if eventsRaw && len(rec.Payload) > 0 {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, rec.Payload, "          ", "  "); err == nil {
			fmt.Fprintf(&b, "          %s\n", pretty.String())
		} else {
			fmt.Fprintf(&b, "          %s\n", string(rec.Payload))
		}
	}
	return b.String()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// followFile tails path, rendering each newly appended line with render. It
// backfills the last eventsTailN lines first, then polls for growth. Kept
// dependency-free (no external `tail`) so it behaves identically everywhere,
// including reset-to-zero on rotation.
func followFile(cmd *cobra.Command, path string, render func(string) string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create event-log dir: %w", err)
	}

	// Backfill recent context.
	if out, err := eventlog.TailEvents(eventsTailN); err == nil && !eventsErrors {
		for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			cmd.Print(render(line))
		}
	}

	var offset int64
	if info, err := os.Stat(path); err == nil {
		offset = info.Size()
	}
	for {
		time.Sleep(500 * time.Millisecond)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		// Rotation (or truncate) shrank the file — start over from the top.
		if info.Size() < offset {
			offset = 0
		}
		if info.Size() == offset {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err == nil {
			r := bufio.NewReader(f)
			for {
				lineBytes, err := r.ReadBytes('\n')
				if len(lineBytes) > 0 {
					offset += int64(len(lineBytes))
					if eventsErrors {
						cmd.Print(string(lineBytes))
					} else {
						cmd.Print(render(string(lineBytes)))
					}
				}
				if err != nil {
					break
				}
			}
		}
		_ = f.Close()
	}
}
