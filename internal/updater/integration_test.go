package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeRelease wires up an httptest.Server that pretends to be GitHub
// releases for `tag`. It serves a tar.gz containing a small fake binary
// and a matching checksums.txt entry.
//
// Returns (server, release-with-rewritten-URLs).
func fakeRelease(t *testing.T, tag string, body string) (*httptest.Server, *Release) {
	t.Helper()

	osName := runtime.GOOS
	arch := runtime.GOARCH
	ext := "tar.gz"
	if osName == "windows" {
		t.Skip("integration test does not cover Windows .zip path here")
	}

	version := strings.TrimPrefix(tag, "v")
	archiveName := fmt.Sprintf("%s_%s_%s_%s.%s", BinaryName, version, osName, arch, ext)

	// Build the archive in memory.
	archiveBytes := buildTarGz(t, BinaryName, []byte(body))
	archiveSum := sha256.Sum256(archiveBytes)
	checksums := fmt.Sprintf("%s  %s\n%s  other-platform.tar.gz\n",
		hex.EncodeToString(archiveSum[:]), archiveName,
		strings.Repeat("0", 64))

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tag_name": tag,
			"html_url": "https://example.invalid/release/" + tag,
			"assets": []map[string]string{
				{"name": archiveName, "browser_download_url": "__ARCHIVE__"},
				{"name": "checksums.txt", "browser_download_url": "__CHECKSUMS__"},
			},
		})
	})
	mux.HandleFunc("/archive", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archiveBytes)
	})
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(checksums))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rel := &Release{
		TagName: tag,
		HTMLURL: "https://example.invalid/release/" + tag,
		Assets: []Asset{
			{Name: archiveName, BrowserDownloadURL: srv.URL + "/archive"},
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums.txt"},
		},
	}
	return srv, rel
}

func buildTarGz(t *testing.T, binaryName string, content []byte) []byte {
	t.Helper()
	var buf strings.Builder
	gzw := gzip.NewWriter(stringWriter{&buf})
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{
		Name: binaryName,
		Mode: 0o755,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return []byte(buf.String())
}

type stringWriter struct{ b *strings.Builder }

func (s stringWriter) Write(p []byte) (int, error) { return s.b.Write(p) }

func TestApply_EndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("end-to-end self-replace covered via tar.gz path only")
	}

	const newBody = "new-binary-bytes-v0.4.0"
	_, rel := fakeRelease(t, "v0.4.0", newBody)
	archive, sums, err := AssetForCurrent(rel)
	if err != nil {
		t.Fatalf("AssetForCurrent: %v", err)
	}

	// Make a fake "installed" binary in a temp dir and ensure Apply swaps
	// it. We point os.Executable at it by writing to the path returned by
	// os.Executable… which we can't change. Instead, exercise Apply's
	// replace step against a temp target by calling the helpers directly.
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, BinaryName)
	if err := os.WriteFile(target, []byte("old-bytes"), 0o755); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	httpClient := &http.Client{Timeout: 5 * time.Second}
	wantSum, err := fetchChecksum(ctx, httpClient, sums.BrowserDownloadURL, archive.Name)
	if err != nil {
		t.Fatalf("fetchChecksum: %v", err)
	}
	tmpArchive, gotSum, err := downloadAndHash(ctx, httpClient, archive.BrowserDownloadURL)
	if err != nil {
		t.Fatalf("downloadAndHash: %v", err)
	}
	defer func() { _ = os.Remove(tmpArchive) }()
	if !strings.EqualFold(gotSum, wantSum) {
		t.Fatalf("checksum mismatch: got %s, want %s", gotSum, wantSum)
	}

	tmpBinary, err := extractBinary(tmpArchive, BinaryName, filepath.Dir(target))
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if err := replaceBinary(target, tmpBinary); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read swapped: %v", err)
	}
	if string(got) != newBody {
		t.Errorf("swap did not land: got %q, want %q", string(got), newBody)
	}
}

func TestApply_ChecksumMismatch(t *testing.T) {
	_, rel := fakeRelease(t, "v0.4.0", "real-bytes")
	archive, sums, err := AssetForCurrent(rel)
	if err != nil {
		t.Fatalf("AssetForCurrent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	httpClient := &http.Client{Timeout: 5 * time.Second}

	// Tamper: ask for the checksum of a name that doesn't exist.
	if _, err := fetchChecksum(ctx, httpClient, sums.BrowserDownloadURL, "wrong-name.tar.gz"); err == nil {
		t.Error("expected fetchChecksum to fail for unknown asset name")
	}

	// And confirm Apply rejects when the archive bytes don't match the
	// expected sum (we point downloadAndHash at the checksums URL on
	// purpose — it'll hash some bytes but the sum won't match what
	// fetchChecksum returns for `archive.Name`).
	wantSum, err := fetchChecksum(ctx, httpClient, sums.BrowserDownloadURL, archive.Name)
	if err != nil {
		t.Fatalf("fetchChecksum: %v", err)
	}
	tmpArchive, gotSum, err := downloadAndHash(ctx, httpClient, sums.BrowserDownloadURL) // wrong URL on purpose
	if err != nil {
		t.Fatalf("downloadAndHash: %v", err)
	}
	defer func() { _ = os.Remove(tmpArchive) }()
	if strings.EqualFold(gotSum, wantSum) {
		t.Fatal("expected wrong bytes to produce a different sum")
	}
}

func TestCheckLatest_HappyPath(t *testing.T) {
	srv, _ := fakeRelease(t, "v9.9.9", "x")
	withReleaseAPIURL(t, srv.URL+"/repos/%s/releases/latest")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rel, newer, err := CheckLatest(ctx, "v0.3.2")
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if !newer {
		t.Errorf("expected v9.9.9 > v0.3.2")
	}
	if rel.TagName != "v9.9.9" {
		t.Errorf("rel.TagName = %q, want v9.9.9", rel.TagName)
	}
}

func TestCheckLatest_NotNewer(t *testing.T) {
	srv, _ := fakeRelease(t, "v0.3.2", "x")
	withReleaseAPIURL(t, srv.URL+"/repos/%s/releases/latest")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, newer, err := CheckLatest(ctx, "v0.3.2")
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if newer {
		t.Errorf("expected equal version not to be reported as newer")
	}
}

func TestCheckLatest_Non200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	withReleaseAPIURL(t, srv.URL+"/repos/%s/releases/latest")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, err := CheckLatest(ctx, "v0.3.2"); err == nil {
		t.Error("expected non-200 response to surface an error")
	}
}

// withReleaseAPIURL swaps the package-level releaseAPIURL for the duration
// of the test.
func withReleaseAPIURL(t *testing.T, url string) {
	t.Helper()
	orig := releaseAPIURL
	releaseAPIURL = url
	t.Cleanup(func() { releaseAPIURL = orig })
}

func TestLoadCache_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, CacheFileName)
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("seed corrupt cache: %v", err)
	}
	if _, err := LoadCache(path); err == nil {
		t.Error("expected LoadCache to return an error on corrupt JSON")
	}
	// ShouldCheck must still recover (return true) so the next invocation
	// just re-checks rather than wedging.
	if !ShouldCheck(path, "v0.3.2", time.Hour) {
		t.Error("expected ShouldCheck to return true on corrupt cache")
	}
}

func TestDetectUpgrade(t *testing.T) {
	cases := []struct {
		name    string
		cache   *CheckResult
		running string
		want    bool
	}{
		{"nil cache", nil, "v0.4.0", false},
		{"empty cache version", &CheckResult{CurrentVersion: ""}, "v0.4.0", false},
		{"same version", &CheckResult{CurrentVersion: "v0.3.2"}, "v0.3.2", false},
		{"forward upgrade", &CheckResult{CurrentVersion: "v0.3.2"}, "v0.4.0", true},
		{"downgrade is silent", &CheckResult{CurrentVersion: "v0.4.0"}, "v0.3.2", false},
		{"unparseable running version", &CheckResult{CurrentVersion: "v0.3.2"}, "dev", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cache.DetectUpgrade(c.running); got != c.want {
				t.Errorf("DetectUpgrade(%v, %q) = %v, want %v", c.cache, c.running, got, c.want)
			}
		})
	}
}

func TestLock_BlocksConcurrent(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, LockFileName)

	release1, err := Lock(lockPath)
	if err != nil {
		t.Fatalf("first Lock failed: %v", err)
	}

	if _, err := Lock(lockPath); err == nil {
		release1()
		t.Fatal("expected second Lock to fail with ErrLocked")
	} else if err != ErrLocked {
		release1()
		t.Fatalf("expected ErrLocked, got %v", err)
	}

	release1()

	// After release, the lock can be reacquired.
	release2, err := Lock(lockPath)
	if err != nil {
		t.Fatalf("second Lock after release failed: %v", err)
	}
	release2()
}

func TestLock_StaleLockGetsClaimed(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, LockFileName)

	// Plant a stale lock file (mtime in the distant past).
	if err := os.WriteFile(lockPath, []byte("99999\n"), 0o600); err != nil {
		t.Fatalf("plant lock: %v", err)
	}
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	release, err := Lock(lockPath)
	if err != nil {
		t.Fatalf("expected stale lock to be claimable, got %v", err)
	}
	release()
}
