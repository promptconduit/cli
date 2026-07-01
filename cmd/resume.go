package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/sessions"
	"github.com/spf13/cobra"
)

// resumeLookback is generous — `resume` is an explicit action, so a session from
// a couple of days ago is fair game (unlike the tighter default for `sessions`).
const resumeLookback = 72 * time.Hour

var resumePrintOnly bool

var resumeCmd = &cobra.Command{
	Use:   "resume [session-id]",
	Short: "Reopen an AI coding session in the directory it ran in",
	Long: `Reopen a Claude Code session, changing into the exact directory it ran in
(worktree-aware) and running 'claude --resume <id>'.

With no argument it resumes the most recently active session. A partial session
id is accepted as long as it's unambiguous. Use 'promptconduit sessions' to see
what's available, or --print to print the command instead of running it (handy
for shell integration).`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		list, err := sessions.ReadRecent(resumeLookback, time.Now())
		if err != nil {
			return fmt.Errorf("read sessions: %w", err)
		}
		if len(list) == 0 {
			return fmt.Errorf("no recent sessions found in the event log")
		}

		var target sessions.Session
		if len(args) == 0 {
			target = list[0] // newest-active first
		} else {
			match, err := pickSession(list, args[0])
			if err != nil {
				return err
			}
			target = match
		}

		if _, err := os.Stat(target.Cwd); err != nil {
			return fmt.Errorf("session directory %q is no longer present: %w", target.Cwd, err)
		}

		claude, err := exec.LookPath("claude")
		if err != nil {
			return fmt.Errorf("the `claude` CLI wasn't found on PATH — install Claude Code, then retry")
		}

		if resumePrintOnly {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cd %s && claude --resume %s\n",
				shellQuote(target.Cwd), target.SessionID)
			return nil
		}

		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Resuming %s in %s …\n",
			shortID(target.SessionID), prettyDir(target.Cwd))

		child := exec.Command(claude, "--resume", target.SessionID)
		child.Dir = target.Cwd
		child.Stdin = os.Stdin
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Run(); err != nil {
			// Propagate the child's exit code rather than wrapping it.
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			}
			return fmt.Errorf("run claude: %w", err)
		}
		return nil
	},
}

// pickSession resolves a user-supplied id (exact or unambiguous prefix) to a
// single session.
func pickSession(list []sessions.Session, id string) (sessions.Session, error) {
	var matches []sessions.Session
	for _, s := range list {
		if s.SessionID == id {
			return s, nil // exact wins outright
		}
		if strings.HasPrefix(s.SessionID, id) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return sessions.Session{}, fmt.Errorf("no session matches %q (try `promptconduit sessions`)", id)
	default:
		return sessions.Session{}, fmt.Errorf("%q is ambiguous — it matches %d sessions; use more characters", id, len(matches))
	}
}

// shellQuote single-quotes a path for the --print form so directories with
// spaces paste correctly.
func shellQuote(s string) string {
	if !strings.ContainsAny(s, " \t'\"\\$") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func init() {
	resumeCmd.Flags().BoolVar(&resumePrintOnly, "print", false, "print the resume command instead of running it")
	rootCmd.AddCommand(resumeCmd)
}
