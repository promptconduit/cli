package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestShouldShowTeamNudge(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		s           tipsState
		interactive bool
		suppressed  bool
		want        bool
	}{
		{"first time on a terminal", tipsState{}, true, false, true},
		{"not a terminal", tipsState{}, false, false, false},
		{"quiet or no-tips", tipsState{}, true, true, false},
		{"already uses team", tipsState{TeamSeen: true}, true, false, false},
		{"shown 6 days ago", tipsState{TeamNudgeShownAt: now.Add(-6 * 24 * time.Hour)}, true, false, false},
		{"shown 7 days ago", tipsState{TeamNudgeShownAt: now.Add(-7 * 24 * time.Hour)}, true, false, true},
	}
	for _, c := range cases {
		if got := shouldShowTeamNudge(c.s, now, c.interactive, c.suppressed); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTeamNudgeText(t *testing.T) {
	if s := teamNudgeText(true); !strings.Contains(s, "promptconduit team spend") || strings.Contains(s, "login") {
		t.Errorf("signed-in tip should point at team spend, got %q", s)
	}
	if s := teamNudgeText(false); !strings.Contains(s, "promptconduit login") || !strings.Contains(s, "team create") {
		t.Errorf("signed-out tip should say login then team create, got %q", s)
	}
}

func TestTipsStateRoundTripAndTeamSeen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := tipsPath()
	if path == "" {
		t.Fatal("no tips path")
	}
	if s := loadTips(path); s.TeamSeen || !s.TeamNudgeShownAt.IsZero() {
		t.Fatalf("fresh state should be empty, got %+v", s)
	}
	shown := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	saveTips(path, tipsState{TeamNudgeShownAt: shown})
	markTeamSeen()
	s := loadTips(path)
	if !s.TeamSeen || !s.TeamNudgeShownAt.Equal(shown) {
		t.Fatalf("markTeamSeen should keep the shown time and set team_seen, got %+v", s)
	}
}

func TestMaybeTeamNudgeSilentOffTerminal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var buf bytes.Buffer // not a terminal: scripts and pipes never see the tip
	maybeTeamNudge(&buf)
	if buf.Len() != 0 {
		t.Fatalf("expected no output off a terminal, got %q", buf.String())
	}
	if s := loadTips(tipsPath()); !s.TeamNudgeShownAt.IsZero() {
		t.Fatal("an unshown tip must not be recorded as shown")
	}
}
