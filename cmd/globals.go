package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/promptconduit/cli/internal/logger"
	"github.com/promptconduit/cli/internal/outbound"
	"github.com/spf13/cobra"
)

// Global flags, shared by every command (https://clig.dev conventions).
var (
	// flagNoInput turns off every prompt. A command that needs an answer
	// fails with a hint naming the flag that supplies it instead.
	flagNoInput bool
	// flagNoColor turns off ANSI color. NO_COLOR and TERM=dumb do the same.
	flagNoColor bool
	// flagQuiet hides the extras a command prints on its own initiative:
	// update and upgrade notices, the first-run telemetry notice, and tips.
	flagQuiet bool
	// flagDebug turns on debug logging for this run and echoes log lines to
	// stderr, in addition to the log file (`promptconduit logs`).
	flagDebug bool
)

// Environment equivalents, for scripts that can't add flags.
const (
	envNoInput = "PROMPTCONDUIT_NO_INPUT"
	envQuiet   = "PROMPTCONDUIT_QUIET"
)

func registerGlobalFlags(root *cobra.Command) {
	pf := root.PersistentFlags()
	pf.BoolVar(&flagNoInput, "no-input", false, "Never prompt; fail with a hint instead (also $"+envNoInput+"=1)")
	pf.BoolVar(&flagNoColor, "no-color", false, "Disable color output (also $NO_COLOR)")
	pf.BoolVarP(&flagQuiet, "quiet", "q", false, "Hide update and telemetry notices and tips (also $"+envQuiet+"=1)")
	pf.BoolVar(&flagDebug, "debug", false, "Print debug logging (e.g. each HTTP request) to stderr")
}

// applyGlobalFlags runs after flag parsing (cobra.OnInitialize), before any
// hook or RunE. The flags stay in-process on purpose: they are NOT exported
// as environment variables, because `resume` execs the AI tool and anything
// in our environment would reach every hook of that whole session.
func applyGlobalFlags() {
	if flagNoColor {
		outbound.DisableColor()
	}
	if flagDebug {
		logger.SetDebug(true)
		logger.SetEcho(os.Stderr)
	}
}

func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// canPrompt reports whether the CLI may ask the user a question: stdin is a
// terminal and --no-input / PROMPTCONDUIT_NO_INPUT is not set.
func canPrompt() bool {
	if flagNoInput || envTrue(envNoInput) {
		return false
	}
	return outbound.IsTerminal(os.Stdin)
}

// quiet reports whether optional notices should be suppressed.
func quiet() bool {
	return flagQuiet || envTrue(envQuiet)
}

// usageError marks an error caused by how the command was invoked (bad
// flag, wrong number of arguments, missing required flag). Execute adds a
// one-line pointer to --help for these; other errors print only the message.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func isUsageError(err error) bool {
	var ue usageError
	if errors.As(err, &ue) {
		return true
	}
	// Cobra's required-flag and flag-group checks return plain errors.
	msg := err.Error()
	return strings.HasPrefix(msg, "required flag(s)") ||
		strings.HasPrefix(msg, "if any flags in the group") ||
		strings.HasPrefix(msg, "at least one of the flags in the group")
}

// markUsageErrors wraps every command's Args validator so its errors are
// recognised as usage errors.
func markUsageErrors(c *cobra.Command) {
	if c.Args != nil {
		orig := c.Args
		c.Args = func(cmd *cobra.Command, args []string) error {
			if err := orig(cmd, args); err != nil {
				return usageError{err}
			}
			return nil
		}
	}
	for _, sub := range c.Commands() {
		markUsageErrors(sub)
	}
}

// printUsageHint points at the command's help after a usage error.
func printUsageHint(w io.Writer, cmd *cobra.Command) {
	_, _ = fmt.Fprintf(w, "Run '%s --help' for usage.\n", cmd.CommandPath())
}
