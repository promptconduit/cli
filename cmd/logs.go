package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/promptconduit/cli/internal/logger"
	"github.com/spf13/cobra"
)

var (
	logsTailN  int
	logsFollow bool
	logsPath   bool
	logsClear  bool
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "View the PromptConduit CLI log",
	Long: `Show recent errors and trace lines from the CLI log file.

Errors (failed sends, missing config, parse errors) are always recorded.
Debug-level trace lines are only recorded when ` + "`debug: true`" + ` is set in the
active environment of your config — see ` + "`promptconduit config show`" + `.

Examples:
  promptconduit logs              # last 50 lines
  promptconduit logs -n 200       # last 200 lines
  promptconduit logs --follow     # tail -f
  promptconduit logs --path       # just print the log file path
  promptconduit logs --clear      # truncate the log file`,
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().IntVarP(&logsTailN, "tail", "n", 50, "Number of lines to show from the end of the log")
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Tail the log file (like `tail -f`)")
	logsCmd.Flags().BoolVar(&logsPath, "path", false, "Print only the log file path and exit")
	logsCmd.Flags().BoolVar(&logsClear, "clear", false, "Truncate the log file and exit")
}

func runLogs(cmd *cobra.Command, args []string) error {
	if logsPath {
		cmd.Println(logger.Path())
		return nil
	}

	if logsClear {
		return clearLog(cmd)
	}

	if logsFollow {
		return followLog(cmd)
	}

	out, err := logger.Tail(logsTailN)
	if err != nil {
		return fmt.Errorf("read log: %w", err)
	}
	if out == "" {
		cmd.Printf("Log file is empty or does not exist yet.\n  Path: %s\n", logger.Path())
		return nil
	}
	cmd.Print(out)
	if out[len(out)-1] != '\n' {
		cmd.Println()
	}
	return nil
}

func clearLog(cmd *cobra.Command) error {
	path := logger.Path()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		cmd.Printf("Nothing to clear: %s does not exist.\n", path)
		return nil
	}
	if err := os.Truncate(path, 0); err != nil {
		return fmt.Errorf("truncate log: %w", err)
	}
	cmd.Printf("Cleared %s\n", path)
	return nil
}

// followLog shells out to `tail -f` when available (every supported
// platform except Windows ships it), and falls back to a simple polling
// loop otherwise. This keeps the implementation tiny while giving users
// the canonical UX for tailing a log.
func followLog(cmd *cobra.Command) error {
	path := logger.Path()
	// Ensure the file exists so tail/poll doesn't immediately fail.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}

	if tailPath, err := exec.LookPath("tail"); err == nil {
		c := exec.Command(tailPath, "-n", fmt.Sprintf("%d", logsTailN), "-F", path)
		c.Stdout = cmd.OutOrStdout()
		c.Stderr = cmd.ErrOrStderr()
		return c.Run()
	}

	return pollFollow(cmd, path)
}

func pollFollow(cmd *cobra.Command, path string) error {
	if out, err := logger.Tail(logsTailN); err == nil {
		cmd.Print(out)
	}
	var offset int64
	if info, err := os.Stat(path); err == nil {
		offset = info.Size()
	}
	for {
		time.Sleep(500 * time.Millisecond)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err == nil {
			data, _ := io.ReadAll(f)
			if len(data) > 0 {
				cmd.Print(string(data))
				offset += int64(len(data))
			}
		}
		f.Close()
	}
}
