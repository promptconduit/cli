package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/promptconduit/cli/internal/collect"
	"github.com/spf13/cobra"
)

var (
	collectOTLPAddr      string
	collectDashboardAddr string
	collectDir           string
)

var collectCmd = &cobra.Command{
	Use:           "collect",
	Short:         "Run a local OTLP receiver, store, and dashboard for agent traces",
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Start a local-first agent observability stack:

  - An OTLP/HTTP receiver on 127.0.0.1:4318 (paths /v1/traces, /v1/logs).
  - A newline-delimited JSON store under ~/.config/promptconduit/collect/.
  - A read-only HTML+JSON dashboard on 127.0.0.1:4319.

This is the "weekend dogfood" stack: zero infra, point any OTLP-emitting
agent (Claude Code, Cursor on a recent build, anything using OpenInference
or OpenLLMetry) at the receiver and watch traces accumulate. The receiver
speaks OTLP/HTTP+JSON only — protobuf is on the roadmap.

Pointing Claude Code at the local receiver:

  export CLAUDE_CODE_ENABLE_TELEMETRY=1
  export OTEL_EXPORTER_OTLP_PROTOCOL=http/json
  export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318

Examples:
  promptconduit collect                          # run with defaults
  promptconduit collect --otlp :4318             # bind on all interfaces
  promptconduit collect --dir /tmp/pc-traces     # store somewhere else`,
	RunE: runCollect,
}

func init() {
	collectCmd.Flags().StringVar(&collectOTLPAddr, "otlp", "127.0.0.1:4318", "address for the OTLP/HTTP receiver")
	collectCmd.Flags().StringVar(&collectDashboardAddr, "dashboard", "127.0.0.1:4319", "address for the read-only dashboard")
	collectCmd.Flags().StringVar(&collectDir, "dir", "", "directory for NDJSON span files (defaults to ~/.config/promptconduit/collect)")
}

func runCollect(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv, err := collect.New(collect.Options{
		OTLPAddr:      collectOTLPAddr,
		DashboardAddr: collectDashboardAddr,
		StoreDir:      collectDir,
	})
	if err != nil {
		return fmt.Errorf("init collector: %w", err)
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "OTLP receiver:  http://%s/v1/traces\n", collectOTLPAddr)
	fmt.Fprintf(cmd.ErrOrStderr(), "Dashboard:      http://%s\n", collectDashboardAddr)
	fmt.Fprintf(cmd.ErrOrStderr(), "Store:          %s\n", srv.StoreDir())
	fmt.Fprintln(cmd.ErrOrStderr(), "Ctrl-C to stop.")

	return srv.Run(ctx)
}
