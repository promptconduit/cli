package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/promptconduit/cli/internal/sessions"
	"github.com/spf13/cobra"
)

var (
	sessionsSince     time.Duration
	sessionsJSON      bool
	sessionsAll       bool // include sessions that are still running
	sessionsUnderPath string
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "List recent, resumable AI coding sessions",
	Long: `List AI coding sessions reconstructed from the local event log, newest first.

Each session records the exact working directory it ran in (worktree-aware) and
its session id, so it can be reopened with 'promptconduit resume <id>' or
'claude --resume <id>'. Sessions with a live process are marked (running) and,
unless --all is given, hidden — the common use is finding sessions that were
interrupted (e.g. when the editor restarted and closed their terminals).

This is the engine behind the editor extension's automatic session restore;
--json emits the machine-readable form it consumes.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		list, err := sessions.ReadRecent(sessionsSince, time.Now())
		if err != nil {
			return fmt.Errorf("read sessions: %w", err)
		}
		sessions.MarkAlive(list, sessions.LiveClaudeCwds())

		if sessionsUnderPath != "" {
			base, err := filepath.Abs(sessionsUnderPath)
			if err == nil {
				list = filterUnder(list, base)
			}
		}
		if !sessionsAll {
			list = filterInterrupted(list)
		}

		if sessionsJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if list == nil {
				list = []sessions.Session{}
			}
			return enc.Encode(list)
		}
		printSessionsTable(cmd, list)
		return nil
	},
}

// filterUnder keeps sessions whose cwd is base or a descendant of it — used to
// scope restore to the current workspace.
func filterUnder(list []sessions.Session, base string) []sessions.Session {
	out := make([]sessions.Session, 0, len(list))
	for _, s := range list {
		if s.Cwd == base || isUnder(base, s.Cwd) {
			out = append(out, s)
		}
	}
	return out
}

func isUnder(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel != ".." && !hasDotDotPrefix(rel) && !filepath.IsAbs(rel)
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[0] == '.' && rel[1] == '.' && (rel[2] == filepath.Separator)
}

func filterInterrupted(list []sessions.Session) []sessions.Session {
	out := make([]sessions.Session, 0, len(list))
	for _, s := range list {
		if !s.Alive {
			out = append(out, s)
		}
	}
	return out
}

func printSessionsTable(cmd *cobra.Command, list []sessions.Session) {
	if len(list) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No recent resumable sessions.")
		return
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ACTIVE\tBRANCH\tDIRECTORY\tSESSION\tPROMPT")
	now := time.Now()
	for _, s := range list {
		branch := s.Branch
		if branch == "" {
			branch = "-"
		}
		state := humanAgo(now.Sub(s.LastActive))
		if s.Alive {
			state += " (running)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			state, branch, prettyDir(s.Cwd), shortID(s.SessionID), s.LastPrompt)
	}
	_ = tw.Flush()
}

func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func prettyDir(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if isUnder(home, p) {
			if rel, err := filepath.Rel(home, p); err == nil {
				return "~/" + rel
			}
		}
	}
	return p
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func init() {
	sessionsCmd.Flags().DurationVar(&sessionsSince, "since", 12*time.Hour, "only sessions active within this window")
	sessionsCmd.Flags().BoolVar(&sessionsJSON, "json", false, "emit machine-readable JSON")
	sessionsCmd.Flags().BoolVar(&sessionsAll, "all", false, "include sessions that are still running")
	sessionsCmd.Flags().StringVar(&sessionsUnderPath, "under", "", "only sessions whose directory is under this path")
	rootCmd.AddCommand(sessionsCmd)
}
