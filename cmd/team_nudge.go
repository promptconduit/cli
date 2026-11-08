package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/outbound"
)

// The team nudge is one line under `cost` / `cost history` output pointing at
// the team view of the same numbers. It is deliberately hard to see twice:
//   - at most once per teamNudgeInterval,
//   - only on an interactive terminal (never in scripts or --json output),
//   - never with --quiet, PROMPTCONDUIT_QUIET or PROMPTCONDUIT_NO_TIPS,
//   - never again once the user has run any `team` command successfully.
// Its state is a small local file; showing it makes no network call.

const (
	envNoTips         = "PROMPTCONDUIT_NO_TIPS"
	teamNudgeFile     = "tips.json"
	teamNudgeInterval = 7 * 24 * time.Hour
)

type tipsState struct {
	TeamNudgeShownAt time.Time `json:"team_nudge_shown_at,omitempty"`
	TeamSeen         bool      `json:"team_seen,omitempty"`
}

func tipsPath() string {
	dir := client.ConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, teamNudgeFile)
}

func loadTips(path string) tipsState {
	var s tipsState
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

func saveTips(path string, s tipsState) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, data, 0o600)
}

// teamNudgeText is the tip for a user who is (signedIn) or isn't signed in.
func teamNudgeText(signedIn bool) string {
	if signedIn {
		return "Tip: see this per member, repo and PR for your whole team with `promptconduit team spend`, " +
			"or start a team (free for up to 3) with `promptconduit team create \"<name>\"`."
	}
	return "Tip: see this per member, repo and PR for your whole team, free for up to 3. " +
		"Run `promptconduit login`, then `promptconduit team create \"<name>\"`."
}

// shouldShowTeamNudge is the pure decision, separated for tests.
func shouldShowTeamNudge(s tipsState, now time.Time, interactive, suppressed bool) bool {
	if !interactive || suppressed || s.TeamSeen {
		return false
	}
	return s.TeamNudgeShownAt.IsZero() || now.Sub(s.TeamNudgeShownAt) >= teamNudgeInterval
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && outbound.IsTerminal(f)
}

// maybeTeamNudge prints the team tip under human cost output when it's due.
func maybeTeamNudge(out io.Writer) {
	path := tipsPath()
	if path == "" {
		return
	}
	s := loadTips(path)
	now := time.Now()
	if !shouldShowTeamNudge(s, now, isTerminalWriter(out), quiet() || envTrue(envNoTips)) {
		return
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, teamNudgeText(client.LoadConfig().IsConfigured()))
	s.TeamNudgeShownAt = now
	saveTips(path, s)
}

// markTeamSeen records that the user has used the team commands, which
// retires the nudge for good.
func markTeamSeen() {
	path := tipsPath()
	if path == "" {
		return
	}
	s := loadTips(path)
	if s.TeamSeen {
		return
	}
	s.TeamSeen = true
	saveTips(path, s)
}
