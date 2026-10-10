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

	"github.com/promptconduit/cli/internal/filelock"
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
	return filepath.Join(enrichBaseDir(), vcsCacheFile)
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
	entry, ok := loadVCSCache()[cacheKey(repoURL, branch)]
	if !entryFresh(entry, time.Now()) {
		spawnVCSRefresh(cwd, repoURL, branch)
	}
	if ok {
		return entry.PR // serve the last known value even while refreshing
	}
	return nil
}

func entryFresh(e vcsCacheEntry, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, e.FetchedAt)
	return err == nil && now.Sub(t) < prTTL
}

func entryRefreshing(e vcsCacheEntry, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, e.RefreshingAt)
	return err == nil && now.Sub(t) < refreshDebounce
}

// vcsCacheEntryMaxAge: entries untouched (neither fetched nor refreshing) for
// this long are dropped on the next write, so the cache doesn't accumulate
// every branch ever checked out.
const vcsCacheEntryMaxAge = 7 * 24 * time.Hour

const (
	// vcsCacheLockWaitHook bounds the wait on the hook path; on timeout the
	// refresh is skipped (whoever holds the lock is updating the same cache).
	vcsCacheLockWaitHook = 200 * time.Millisecond
	// vcsCacheLockWaitRefresh is the detached refresher's (off the hot path).
	vcsCacheLockWaitRefresh = 3 * time.Second
)

// updateVCSCache applies fn to a freshly re-read cache under an exclusive
// lock, drops stale entries, and saves. Re-reading under the lock means a
// writer never clobbers an entry another process wrote since it last looked
// (e.g. a hook marking refreshing_at over a just-fetched PR). fn returns
// false to skip the save. Returns whether the lock was taken and fn ran.
func updateVCSCache(wait time.Duration, fn func(c vcsCache) bool) bool {
	release, ok := filelock.Exclusive(vcsCachePath()+".lock", wait)
	defer release()
	if !ok {
		return false
	}
	c := loadVCSCache()
	if !fn(c) {
		return true
	}
	pruneVCSCache(c, time.Now())
	saveVCSCache(c)
	return true
}

// pruneVCSCache drops entries whose latest activity (fetch or refresh start)
// is older than vcsCacheEntryMaxAge, or that carry no parseable time at all.
func pruneVCSCache(c vcsCache, now time.Time) {
	for k, e := range c {
		var last time.Time
		for _, s := range []string{e.FetchedAt, e.RefreshingAt} {
			if t, err := time.Parse(time.RFC3339, s); err == nil && t.After(last) {
				last = t
			}
		}
		if last.IsZero() || now.Sub(last) > vcsCacheEntryMaxAge {
			delete(c, k)
		}
	}
}

// spawnVCSRefresh starts a detached `promptconduit vcs-refresh` unless the
// entry became fresh, or a refresh was started within refreshDebounce, since
// the caller read the cache. It marks refreshing_at (under the cache lock)
// before spawning so concurrent hooks don't pile up subprocesses.
func spawnVCSRefresh(cwd, repoURL, branch string) {
	key := cacheKey(repoURL, branch)
	spawn := false
	updateVCSCache(vcsCacheLockWaitHook, func(c vcsCache) bool {
		now := time.Now()
		entry := c[key]
		if entryFresh(entry, now) || entryRefreshing(entry, now) {
			return false
		}
		entry.RefreshingAt = now.UTC().Format(time.RFC3339)
		c[key] = entry
		spawn = true
		return true
	})
	if spawn {
		startVCSRefresh(cwd, repoURL, branch)
	}
}

// startVCSRefresh launches the detached refresher. A var so tests can stub it
// (the test binary must never re-exec itself as `vcs-refresh`).
var startVCSRefresh = func(cwd, repoURL, branch string) {
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
	storePR(repoURL, branch, lookupPRViaGh(cwd))
}

// storePR records a resolved lookup for repo+branch (clearing refreshing_at).
func storePR(repoURL, branch string, pr *PRInfo) {
	entry := vcsCacheEntry{PR: pr, FetchedAt: time.Now().UTC().Format(time.RFC3339)}
	if !updateVCSCache(vcsCacheLockWaitRefresh, func(c vcsCache) bool {
		c[cacheKey(repoURL, branch)] = entry
		return true
	}) {
		// Couldn't get the lock: still record the result (atomic write,
		// best-effort), as before the lock existed.
		c := loadVCSCache()
		c[cacheKey(repoURL, branch)] = entry
		pruneVCSCache(c, time.Now())
		saveVCSCache(c)
	}
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
