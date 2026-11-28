package cmd

import (
	"testing"
	"time"
)

func TestGithubSlugFromRemote(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:acme/web.git\n":     "acme/web",
		"https://github.com/acme/web":       "acme/web",
		"https://github.com/acme/web.git/":  "acme/web",
		"ssh://git@github.com/acme/my.repo": "acme/my.repo",
		"https://gitlab.com/acme/web":       "",
		"https://github.com/acme":           "",
	} {
		if got := githubSlugFromRemote(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestCIWindow(t *testing.T) {
	ciSince, ciUntil, ciDays = "", "", 30
	since, until := ciWindow(time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC))
	if since != "2026-11-01" || until != "2026-11-30" {
		t.Errorf("%s..%s", since, until)
	}
}
