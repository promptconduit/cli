package updater

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.3.2", "v0.3.1", 1, true},
		{"v0.3.1", "v0.3.2", -1, true},
		{"v0.3.2", "v0.3.2", 0, true},
		{"0.3.2", "v0.3.2", 0, true},
		{"v1.0.0", "v0.99.99", 1, true},
		{"v0.3.2-rc.1", "v0.3.2", 0, true}, // pre-release suffix stripped
		{"v0.3.3-rc.1", "v0.3.2", 1, true},
		{"dev", "v0.3.2", 0, false},  // unparseable -> ok=false
		{"v0.3", "v0.3.0", 0, false}, // not 3 parts
	}
	for _, c := range cases {
		got, ok := compareSemver(c.a, c.b)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("compareSemver(%q, %q) = (%d, %v), want (%d, %v)", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

func TestIsNewer(t *testing.T) {
	r := &CheckResult{LatestVersion: "v0.4.0", CurrentVersion: "v0.3.2"}
	if !r.IsNewer() {
		t.Error("expected IsNewer to be true")
	}
	r = &CheckResult{LatestVersion: "v0.3.2", CurrentVersion: "v0.3.2"}
	if r.IsNewer() {
		t.Error("expected IsNewer to be false for equal versions")
	}
	r = &CheckResult{LatestVersion: "v0.3.2", CurrentVersion: "dev"}
	if r.IsNewer() {
		t.Error("expected IsNewer to be false when current is unparseable")
	}
}

func TestShouldCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "update.json")

	if !ShouldCheck(path, "v0.3.2", time.Hour) {
		t.Error("expected ShouldCheck=true when cache missing")
	}

	fresh := &CheckResult{CheckedAt: time.Now(), CurrentVersion: "v0.3.2", LatestVersion: "v0.3.2"}
	if err := SaveCache(path, fresh); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if ShouldCheck(path, "v0.3.2", time.Hour) {
		t.Error("expected ShouldCheck=false with fresh cache")
	}

	stale := &CheckResult{CheckedAt: time.Now().Add(-25 * time.Hour), CurrentVersion: "v0.3.2", LatestVersion: "v0.3.2"}
	if err := SaveCache(path, stale); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if !ShouldCheck(path, "v0.3.2", time.Hour) {
		t.Error("expected ShouldCheck=true with stale cache")
	}

	// Version mismatch invalidates the cache (e.g. user did brew upgrade).
	if err := SaveCache(path, fresh); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if !ShouldCheck(path, "v0.4.0", time.Hour) {
		t.Error("expected ShouldCheck=true when current version differs from cache")
	}
}

func TestAssetForCurrent(t *testing.T) {
	rel := &Release{
		TagName: "v0.3.2",
		Assets: []Asset{
			{Name: "promptconduit_0.3.2_linux_amd64.tar.gz", BrowserDownloadURL: "https://example/linux"},
			{Name: "promptconduit_0.3.2_darwin_arm64.tar.gz", BrowserDownloadURL: "https://example/mac"},
			{Name: "promptconduit_0.3.2_windows_amd64.zip", BrowserDownloadURL: "https://example/win"},
			{Name: "checksums.txt", BrowserDownloadURL: "https://example/sums"},
		},
	}

	a, sums, err := AssetForCurrent(rel)
	if err != nil {
		// AssetForCurrent is platform-dependent; only fail when there's no
		// matching asset for the test runner's GOOS/GOARCH.
		t.Skipf("no asset for current platform: %v", err)
	}
	if a == nil || sums == nil {
		t.Fatalf("expected asset+sums, got %v %v", a, sums)
	}
	if sums.Name != "checksums.txt" {
		t.Errorf("checksums asset name = %q", sums.Name)
	}
}

func TestAssetForCurrent_NoChecksums(t *testing.T) {
	rel := &Release{
		TagName: "v0.3.2",
		Assets:  []Asset{{Name: "promptconduit_0.3.2_linux_amd64.tar.gz"}},
	}
	if _, _, err := AssetForCurrent(rel); err == nil {
		t.Error("expected error when checksums.txt is missing")
	}
}
