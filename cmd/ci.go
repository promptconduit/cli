package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/ci"
	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/spf13/cobra"
)

var (
	ciSince      string
	ciUntil      string
	ciDays       int
	ciJSON       bool
	ciTop        int
	ciNoSessions bool
	ciOutput     string
)

var ciCmd = &cobra.Command{
	Use:   "ci",
	Short: "What your GitHub Actions minutes cost, and which agent sessions caused them",
	Long: `Estimate a repo's GitHub Actions spend, find CI waste (re-tests after merge,
bursts of agent PRs, push storms, superseded runs, slow jobs) and match each
run to the agent session whose commit triggered it.

Reads only Actions metadata (runs and jobs: no logs, artifacts or code) with
your own GitHub token (GITHUB_TOKEN, GH_TOKEN, or ` + "`gh auth token`" + `). Never
writes to the repo. ` + "`ci audit`" + ` runs fully on this machine.

Examples:
  promptconduit ci audit                          # this repo, last 30 days
  promptconduit ci audit acme/api --since 2026-11-01 --until 2026-11-30
  promptconduit ci audit --json                   # for an agent
  promptconduit ci sync --since 2026-11-01        # add CI $ to your team dashboard`,
	Args: cobra.NoArgs,
}

var ciAuditCmd = &cobra.Command{
	Use:   "audit [owner/repo]",
	Short: "One-screen CI cost and waste report for a repo (runs locally)",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runCIAudit,
}

var ciSyncCmd = &cobra.Command{
	Use:   "sync [owner/repo]",
	Short: "Upload a repo's Actions job metadata so CI $ shows next to token $ for your team",
	Long: `Reads the repo's Actions runs and jobs (metadata only) with your GitHub token
and sends them to PromptConduit, which prices them and matches each job to the
agent session that pushed its commit. Requires ` + "`promptconduit login`" + `.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runCISync,
}

func init() {
	for _, c := range []*cobra.Command{ciAuditCmd, ciSyncCmd} {
		c.Flags().StringVar(&ciSince, "since", "", "first UTC day, YYYY-MM-DD (default: --days ago)")
		c.Flags().StringVar(&ciUntil, "until", "", "last UTC day, YYYY-MM-DD (default: today)")
		c.Flags().IntVar(&ciDays, "days", 30, "days to read when --since is not set")
		c.Flags().BoolVar(&ciJSON, "json", false, "machine-readable output")
	}
	ciAuditCmd.Flags().IntVar(&ciTop, "top", 10, "rows per table")
	ciAuditCmd.Flags().BoolVar(&ciNoSessions, "no-sessions", false, "skip matching runs to sessions in the local event log")
	ciAuditCmd.Flags().StringVarP(&ciOutput, "output", "o", "", "also write the JSON report to this file")
	ciCmd.AddCommand(ciAuditCmd, ciSyncCmd)
	silenceUsage(ciCmd)
	rootCmd.AddCommand(ciCmd)
}

var repoSlugRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// githubSlugFromRemote turns a GitHub remote URL into "owner/name".
func githubSlugFromRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	for _, p := range []string{"git@github.com:", "ssh://git@github.com/", "https://github.com/", "http://github.com/"} {
		if strings.HasPrefix(remote, p) {
			slug := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(remote, p), "/"), ".git")
			if repoSlugRe.MatchString(slug) {
				return slug
			}
		}
	}
	return ""
}

func ciRepo(args []string) (string, error) {
	if len(args) == 1 {
		if !repoSlugRe.MatchString(args[0]) {
			return "", fmt.Errorf("repo must be owner/name, got %q", args[0])
		}
		return args[0], nil
	}
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err == nil {
		if slug := githubSlugFromRemote(string(out)); slug != "" {
			return slug, nil
		}
	}
	return "", fmt.Errorf("not in a GitHub repo: pass owner/name")
}

func ciWindow(now time.Time) (string, string) {
	until := ciUntil
	if until == "" {
		until = now.UTC().Format("2006-01-02")
	}
	since := ciSince
	if since == "" {
		days := ciDays
		if days < 1 {
			days = 1
		}
		since = now.UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	}
	return since, until
}

func collectCI(cmd *cobra.Command, args []string) (*ci.Dataset, error) {
	repo, err := ciRepo(args)
	if err != nil {
		return nil, err
	}
	token, err := ci.Token()
	if err != nil {
		return nil, err
	}
	since, until := ciWindow(time.Now())
	progress := func(s string) {
		if !ciJSON {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), s)
		}
	}
	progress(fmt.Sprintf("Reading GitHub Actions for %s, %s to %s...", repo, since, until))
	return ci.NewGitHub(token).Collect(repo, since, until, progress)
}

func runCIAudit(cmd *cobra.Command, args []string) error {
	ds, err := collectCI(cmd, args)
	if err != nil {
		return err
	}
	var idx *ci.SessionIndex
	if !ciNoSessions {
		if i, err := ci.IndexSessions(eventlog.EventsJSONLPath(), ds.Repo); err == nil {
			idx = i
		}
	}
	rep := ci.Analyze(ds, idx)
	if ciOutput != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(ciOutput, b, 0o644); err != nil {
			return err
		}
	}
	if ciJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	ci.WriteText(cmd.OutOrStdout(), rep, ciTop)
	if !client.LoadConfig().IsConfigured() {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nSee CI $ per member, repo and PR for your team: promptconduit login, then promptconduit ci sync")
	} else {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nAdd this to your team dashboard: promptconduit ci sync")
	}
	return nil
}

func runCISync(cmd *cobra.Command, args []string) error {
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return fmt.Errorf("not signed in. Run: promptconduit login")
	}
	ds, err := collectCI(cmd, args)
	if err != nil {
		return err
	}
	c := client.NewClient(cfg, Version)
	var jobs, stored, minutes int
	var usdTotal float64
	for _, up := range ci.Uploads(ds) {
		resp := c.UploadCIJobs(up)
		if !resp.Success {
			return teamAPIError("upload CI jobs", resp)
		}
		jobs += len(up.Jobs)
		stored += int(num(resp.Data["stored"]))
		minutes += int(num(resp.Data["minutes"]))
		usdTotal += num(resp.Data["usd"])
	}
	if ciJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]interface{}{
			"repo": ds.Repo, "since": ds.Since, "until": ds.Until, "runs": len(ds.Runs),
			"jobs": jobs, "stored": stored, "minutes": minutes, "usd": usdTotal,
		})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Uploaded %d jobs from %d runs (%s min, $%.2f) for %s, %s to %s.\n",
		jobs, len(ds.Runs), commasInt(minutes), usdTotal, ds.Repo, ds.Since, ds.Until)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "PromptConduit prices each job and matches it to the agent session that pushed its commit.")
	return nil
}

func num(v interface{}) float64 {
	f, _ := v.(float64)
	return f
}

func commasInt(n int) string {
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
