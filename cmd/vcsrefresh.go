package cmd

import (
	"github.com/promptconduit/cli/internal/enrich"
	"github.com/spf13/cobra"
)

// vcs-refresh is the detached worker behind the vcs enrichment's PR link: the
// hook only reads a disk cache, and spawns this command to do the slow
// `gh pr view` lookup out-of-band (see internal/enrich/vcscache.go).
var (
	vcsRefreshCwd     string
	vcsRefreshRepoURL string
	vcsRefreshBranch  string
)

var vcsRefreshCmd = &cobra.Command{
	Use:           "vcs-refresh",
	Short:         "Refresh the cached PR lookup for a repo+branch (internal use)",
	Hidden:        true,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if vcsRefreshCwd == "" || vcsRefreshRepoURL == "" || vcsRefreshBranch == "" {
			return nil // nothing to do; spawned with incomplete context
		}
		enrich.RefreshPR(vcsRefreshCwd, vcsRefreshRepoURL, vcsRefreshBranch)
		return nil
	},
}

func init() {
	vcsRefreshCmd.Flags().StringVar(&vcsRefreshCwd, "cwd", "", "repository working directory")
	vcsRefreshCmd.Flags().StringVar(&vcsRefreshRepoURL, "repo-url", "", "normalized repo URL (cache key)")
	vcsRefreshCmd.Flags().StringVar(&vcsRefreshBranch, "branch", "", "branch name (cache key)")
	rootCmd.AddCommand(vcsRefreshCmd)
}
