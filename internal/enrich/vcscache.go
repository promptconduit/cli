package enrich

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/logger"
)

// The PR lookup needs a network call (`gh pr view`), which is far too slow for
// the hook's hot path. So the vcs enricher only ever READS a disk cache here;
// when an entry is missing or stale it spawns a detached `promptconduit
// vcs-refresh` subprocess that runs gh and rewrites the cache for the NEXT
// event. Steady state: events carry the PR link at the cost of zero added hook
// latency, refreshed at most once per prTTL per repo+branch.

const (
	vcsCacheFile = "enrich/vcs-cache.json"
	// prTTL is how long a resolved (or resolved-empty) PR entry is trusted.
	prTTL = 5 * time.Minute
	// refreshDebounce suppresses respawning a refresh that is already running.
	refreshDebounce = 60 * time.Second
	// ghTimeout bounds the gh invocation inside the detached refresh.
	ghTimeout = 15 * time.Second
)

type vcsCacheEntry struct {
	PR            *PRInfo `json:"pr,omitempty"`
	FetchedAt     string  `json:"fetched_at,omitempty"`
	RefreshingAt  string  `json:"refreshing_at,omitempty"`
	DefaultBranch string  `json:"default_branch,omitempty"` // reserved for future use
}

type vcsCache map[string]vcsCacheEntry

func vcsCachePath() string {
	if stateDirOverride != "" {
		return filepath.Join(stateDirOverride, vcsCacheFile)
	}
	return filepath.Join(client.ConfigDir(), vcsCacheFile)
}

func cacheKey(repoURL, branch string) string { return repoURL + "|" + branch }

func loadVCSCache() vcsCache {
	data, err := os.ReadFile(vcsCachePath())
	if err != nil {
		return vcsCache{}
	}
	var c vcsCache
	if err := json.Unmarshal(data, &c); err != nil || c == nil {
		return vcsCache{}
	}
	return c
}

func saveVCSCache(c vcsCache) {
	path := vcsCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
	}
}

// cachedPR returns the cached PR for repo+branch (nil when none known) and
// triggers a detached refresh when the entry is missing or stale. GitHub only
// for now; other providers always return nil without spawning anything.
func cachedPR(cwd, provider, repoURL, branch string) *PRInfo {
	if provider != "github" || repoURL == "" || branch == "" {
		return nil
	}
	c := loadVCSCache()
	entry, ok := c[cacheKey(repoURL, branch)]

	fresh := false
	if ok && entry.FetchedAt != "" {
		if t, err := time.Parse(time.RFC3339, entry.FetchedAt); err == nil {
			fresh = time.Since(t) < prTTL
		}
	}
	if !fresh {
		spawnVCSRefresh(cwd, repoURL, branch, c, entry)
	}
	if ok {
		return entry.PR // serve the last known value even while refreshing
	}
	return nil
}

// spawnVCSRefresh starts a detached `promptconduit vcs-refresh` unless one was
// spawned within refreshDebounce. It marks refreshing_at in the cache first so
// concurrent hooks don't pile up subprocesses.
func spawnVCSRefresh(cwd, repoURL, branch string, c vcsCache, entry vcsCacheEntry) {
	if entry.RefreshingAt != "" {
		if t, err := time.Parse(time.RFC3339, entry.RefreshingAt); err == nil && time.Since(t) < refreshDebounce {
			return
		}
	}
	entry.RefreshingAt = time.Now().UTC().Format(time.RFC3339)
	c[cacheKey(repoURL, branch)] = entry
	saveVCSCache(c)

	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "vcs-refresh", "--cwd", cwd, "--repo-url", repoURL, "--branch", branch)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		logger.Debug("enrich: vcs-refresh spawn failed: %v", err)
		return
	}
	_ = cmd.Process.Release()
}

// RefreshPR resolves the open PR for the branch checked out in cwd via
// `gh pr view` and writes the result (or a resolved-empty entry) to the cache.
// Run by the hidden `promptconduit vcs-refresh` command — never on the hook
// path. Any failure (gh missing/unauthenticated, no PR) still stamps
// fetched_at so the enricher doesn't respawn refreshes in a tight loop.
func RefreshPR(cwd, repoURL, branch string) {
	pr := lookupPRViaGh(cwd)

	c := loadVCSCache()
	c[cacheKey(repoURL, branch)] = vcsCacheEntry{
		PR:        pr,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
	}
	saveVCSCache(c)
}

func lookupPRViaGh(cwd string) *PRInfo {
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "pr", "view", "--json", "number,url,title,state")
	cmd.Dir = cwd
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil // no PR for the branch, gh absent, or unauthenticated
	}

	var out struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		Title  string `json:"title"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Number == 0 {
		return nil
	}
	return &PRInfo{
		Number: out.Number,
		URL:    out.URL,
		Title:  out.Title,
		State:  strings.ToLower(out.State),
	}
}
