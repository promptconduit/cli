package enrich

import (
	"testing"
	"time"
)

// stubRefresh replaces the detached refresher with a counter.
func stubRefresh(t *testing.T) *int {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	SetStateDirForTest(t.TempDir())
	t.Cleanup(func() { SetStateDirForTest("") })
	n := 0
	prev := startVCSRefresh
	startVCSRefresh = func(string, string, string) { n++ }
	t.Cleanup(func() { startVCSRefresh = prev })
	return &n
}

const testRepo = "https://github.com/o/r"

func TestCachedPR_SpawnsOnceThenDebounces(t *testing.T) {
	spawns := stubRefresh(t)
	if pr := cachedPR("/x", "github", testRepo, "main"); pr != nil {
		t.Fatalf("empty cache serves nil, got %+v", pr)
	}
	if *spawns != 1 {
		t.Fatalf("missing entry should spawn one refresh, got %d", *spawns)
	}
	cachedPR("/x", "github", testRepo, "main")
	if *spawns != 1 {
		t.Fatalf("refresh in flight must debounce, got %d spawns", *spawns)
	}
}

func TestSpawnVCSRefresh_DoesNotClobberFreshEntry(t *testing.T) {
	spawns := stubRefresh(t)
	// A hook read the cache while it was stale ...
	storePR(testRepo, "main", &PRInfo{Number: 1, URL: "old"})
	updateVCSCache(time.Second, func(c vcsCache) bool {
		e := c[cacheKey(testRepo, "main")]
		e.FetchedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		c[cacheKey(testRepo, "main")] = e
		return true
	})
	// ... and meanwhile the refresher stored a fresh result.
	storePR(testRepo, "main", &PRInfo{Number: 42, URL: "new"})

	spawnVCSRefresh("/x", testRepo, "main")
	if *spawns != 0 {
		t.Fatalf("entry is fresh after re-read under lock: no refresh should spawn")
	}
	e := loadVCSCache()[cacheKey(testRepo, "main")]
	if e.PR == nil || e.PR.Number != 42 || e.RefreshingAt != "" {
		t.Fatalf("fresh entry clobbered: %+v", e)
	}
}

func TestVCSCache_DropsEntriesOlderThanAWeek(t *testing.T) {
	stubRefresh(t)
	old := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-2 * 24 * time.Hour).UTC().Format(time.RFC3339)
	updateVCSCache(time.Second, func(c vcsCache) bool {
		c["old|b"] = vcsCacheEntry{FetchedAt: old}
		c["oldrefresh|b"] = vcsCacheEntry{RefreshingAt: old}
		c["recent|b"] = vcsCacheEntry{FetchedAt: recent}
		c["revived|b"] = vcsCacheEntry{FetchedAt: old, RefreshingAt: recent}
		c["notime|b"] = vcsCacheEntry{}
		return true
	})
	c := loadVCSCache()
	for _, k := range []string{"old|b", "oldrefresh|b", "notime|b"} {
		if _, ok := c[k]; ok {
			t.Errorf("%s should have been dropped", k)
		}
	}
	for _, k := range []string{"recent|b", "revived|b"} {
		if _, ok := c[k]; !ok {
			t.Errorf("%s should have been kept", k)
		}
	}
}
