package cmd

// Examples shown under "Examples:" in --help, for commands whose Long text
// doesn't already carry them. Kept in one place so they're easy to audit
// against the real flags (TestVisibleCommandsHaveExamples guards coverage).
func init() {
	examples := map[*string]string{
		&uninstallCmd.Example: `  promptconduit uninstall cursor
  promptconduit uninstall claude-code`,

		&loginCmd.Example: `  promptconduit login
  promptconduit login --no-browser   # print the URL, e.g. over SSH`,

		&initCmd.Example: `  promptconduit init                                   # interactive wizard
  promptconduit init --local-only --yes                # no account, every detected tool
  promptconduit init --tools claude-code,cursor --yes
  promptconduit init --api-key-stdin --yes < key.txt   # CI / provisioning`,

		&costCmd.Example: `  promptconduit cost                 # current session in this directory
  promptconduit cost --json
  promptconduit cost history --days 30`,

		&costSessionCmd.Example: `  promptconduit cost session
  promptconduit cost session --cwd ~/code/my-repo --json`,

		&costHistoryCmd.Example: `  promptconduit cost history
  promptconduit cost history --days 30 --json`,

		&sessionsCmd.Example: `  promptconduit sessions                    # interrupted sessions, last 12h
  promptconduit sessions --all --since 48h
  promptconduit sessions --under ~/code/my-repo --json`,

		&resumeCmd.Example: `  promptconduit resume                 # most recent interrupted session
  promptconduit resume 3f2a9c1e
  promptconduit resume 3f2a9c1e --print   # show the command instead of running it`,

		&insightsCmd.Example: `  promptconduit insights
  promptconduit insights --period 30d --repo my-org/my-repo
  promptconduit insights spend --json`,

		&mcpCmd.Example: `  promptconduit mcp install           # register with every tool you use
  promptconduit mcp install cursor
  promptconduit mcp status --json
  promptconduit mcp tools`,

		&pruneCmd.Example: `  promptconduit prune --dry-run
  promptconduit prune`,

		&testCmd.Example: `  promptconduit test
  promptconduit test --debug`,

		&debugTraceCmd.Example: `  promptconduit debug trace 3f2a9c1e-…`,

		&teamSpendCmd.Example: `  promptconduit team spend
  promptconduit team spend --by member --period 7d
  promptconduit team spend --by client --limit 0 --json`,

		&teamSavingsCmd.Example: `  promptconduit team savings --period 90d`,

		&teamExportCmd.Example: `  promptconduit team export --by client --period 90d -o spend.csv
  promptconduit team export --by pr > spend.csv`,

		&teamInviteCmd.Example: `  promptconduit team invite dev@example.com
  promptconduit team invite lead@example.com --role admin`,

		&teamJoinCmd.Example: `  promptconduit team join https://app.promptconduit.dev/invite/abc123
  promptconduit team join abc123 --share-history`,

		&teamCreateCmd.Example: `  promptconduit team create "Acme Engineering"
  promptconduit team create Acme --slug acme --no-history`,

		&configCmd.Example: `  promptconduit config show
  promptconduit config set --local-only=true
  promptconduit config set --api-key-stdin < key.txt
  promptconduit config path`,

		&debugCmd.Example: `  promptconduit debug trace 3f2a9c1e-…`,

		&versionCmd.Example: `  promptconduit version
  promptconduit --version`,
	}
	for field, ex := range examples {
		if *field == "" {
			*field = ex
		}
	}
}
