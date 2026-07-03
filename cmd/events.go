package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	Short:         "Inspect the local event log (captured v2 envelopes)",
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Show the events the CLI has captured, exactly as they are (or would be)
sent to the platform.

This reads ~/.promptconduit/events.jsonl — one v2 envelope per line, written at
capture time for every event, in cloud AND Free/local-only mode (secrets
scrubbed; the file rotates to events.jsonl.1 at 50MB). Send outcomes are in
'promptconduit status'; raw HTTP diagnostics in 'promptconduit watch'.

By default each event is shown as a one-line summary:

  16:55:02  PostToolUse        claude-code  9a402796  3.2KB  env,trace,vcs

The trailing list is the enrichment slugs attached to the event.

Use --raw to print the full envelope beneath each summary, or --errors to tail
the human-readable failure/drop log (errors.log) instead.

Examples:
  promptconduit events                # last 20 events, summary lines
  promptconduit events --raw          # include the full envelope per event
  promptconduit events -n 5           # last 5 events
  promptconduit events --follow       # stream new events live
  promptconduit events --errors       # tail errors.log (failures + drops)
  promptconduit events --path         # print the event-log file path`,
	RunE: runEvents,
}

func init() {
	eventsCmd.Flags().IntVarP(&eventsTailN, "lines", "n", 20, "Number of events to show from the end of the log")
	eventsCmd.Flags().BoolVarP(&eventsFollow, "follow", "f", false, "Stream new events as they are written (like `tail -f`)")
	eventsCmd.Flags().BoolVar(&eventsErrors, "errors", false, "Show the human-readable error log (errors.log) instead of the event log")
	eventsCmd.Flags().BoolVar(&eventsRaw, "raw", false, "Print the full JSON envelope beneath each summary line")
	eventsCmd.Flags().BoolVar(&eventsPath, "path", false, "Print only the log file path and exit")
}

func runEvents(cmd *cobra.Command, args []string) error {
	path := eventlog.EventsJSONLPath()
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

	out, err := eventlog.TailCaptured(eventsTailN)
	if err != nil {
		return fmt.Errorf("read event log: %w", err)
	}
	if out == "" {
		cmd.Printf("No events captured yet.\n  Path: %s\n", path)
		cmd.Println("  (events are captured whenever an installed hook fires; check `promptconduit status`)")
		return nil
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		cmd.Print(renderEventLine(line))
	}
	return nil
}

// renderEventLine turns one captured envelope line into a readable summary
// (plus the full envelope when --raw is set). Lines that don't parse are
// echoed verbatim so nothing is silently hidden.
func renderEventLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}
	var env struct {
		SessionID   string                     `json:"session_id"`
		Tool        string                     `json:"tool"`
		HookEvent   string                     `json:"hook_event"`
		CapturedAt  string                     `json:"captured_at"`
		Enrichments map[string]json.RawMessage `json:"enrichments"`
	}
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return trimmed + "\n"
	}

	ts := env.CapturedAt
	if t, err := time.Parse(time.RFC3339, env.CapturedAt); err == nil {
		ts = t.Local().Format("15:04:05")
	}

	slugs := make([]string, 0, len(env.Enrichments))
	for slug := range env.Enrichments {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	session := env.SessionID
	if len(session) > 8 {
		session = session[:8]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s  %-18s %-12s %-8s %7s  %s\n",
		ts, dashIfEmpty(env.HookEvent), dashIfEmpty(env.Tool), dashIfEmpty(session),
		humanBytes(len(trimmed)), strings.Join(slugs, ","))
	if eventsRaw {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, []byte(trimmed), "          ", "  "); err == nil {
			fmt.Fprintf(&b, "          %s\n", pretty.String())
		} else {
			fmt.Fprintf(&b, "          %s\n", trimmed)
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
	if out, err := eventlog.TailCaptured(eventsTailN); err == nil && !eventsErrors {
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
