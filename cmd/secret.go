package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/promptconduit/cli/internal/outbound"
	"github.com/spf13/cobra"
)

const (
	apiKeyFlagHelp      = "API key (visible in shell history and ps; prefer --api-key-stdin)"
	apiKeyStdinFlagHelp = "Read the API key from stdin, e.g. `pbpaste | promptconduit ... --api-key-stdin`"
)

// secretInput is where --api-key-stdin reads from. A var so tests can swap it.
var secretInput io.Reader = os.Stdin

// addAPIKeyStdinFlag gives c an --api-key-stdin flag that fills *target from
// stdin before RunE. Secrets passed as flags leak into shell history and the
// process list (https://clig.dev/#arguments-and-flags), so every command that
// takes --api-key also takes it on stdin.
func addAPIKeyStdinFlag(c *cobra.Command, target *string) {
	var fromStdin bool
	c.Flags().BoolVar(&fromStdin, "api-key-stdin", false, apiKeyStdinFlagHelp)
	c.MarkFlagsMutuallyExclusive("api-key", "api-key-stdin")

	prev := c.PreRunE
	c.PreRunE = func(cmd *cobra.Command, args []string) error {
		// Cobra checks mutually exclusive flags only after PreRunE; check
		// first so a conflict never blocks on stdin.
		if fromStdin && cmd.Flags().Changed("api-key") {
			return usageError{fmt.Errorf("use either --api-key or --api-key-stdin, not both")}
		}
		if fromStdin {
			key, err := readSecret(secretInput, "API key")
			if err != nil {
				return err
			}
			*target = key
		}
		if prev != nil {
			return prev(cmd, args)
		}
		return nil
	}
}

// readSecret reads one line from r and trims it. When r is a terminal it
// prompts on stderr first.
func readSecret(r io.Reader, what string) (string, error) {
	if f, ok := r.(*os.File); ok && outbound.IsTerminal(f) {
		_, _ = fmt.Fprintf(os.Stderr, "Paste the %s and press Enter: ", what)
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading %s from stdin: %w", what, err)
	}
	secret := strings.TrimSpace(line)
	if secret == "" {
		return "", fmt.Errorf("no %s on stdin", what)
	}
	return secret, nil
}
