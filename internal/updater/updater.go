// Package updater checks for and applies new releases of the CLI from
// GitHub Releases. It is intentionally stdlib-only.
//
// Flow:
//  1. CheckLatest hits the GitHub releases API (with a 24h on-disk cache)
//     and returns release info if a newer semver tag is published.
//  2. Apply downloads the os/arch archive, verifies its SHA256 against the
//     release's checksums.txt, and atomically swaps the running binary.
//
// Self-replacement is safe on Linux/macOS because the kernel allows
// overwriting an executing binary (the running process keeps its inode).
// On Windows we rename the current binary aside first, then write the new
// one in its place — the old file is cleaned up on next invocation.
package updater

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// Repo is the GitHub repository for releases.
	Repo = "promptconduit/cli"

	// BinaryName is the released binary name (sans .exe).
	BinaryName = "promptconduit"

	// CheckTTL is how long a cached check is considered fresh.
	CheckTTL = 24 * time.Hour

	// CacheFileName is the on-disk cache for the last check result.
	CacheFileName = "update.json"

	// DefaultTimeout is the HTTP timeout for the version check.
	DefaultTimeout = 5 * time.Second

	// DownloadTimeout is the HTTP timeout for downloading a release archive.
	DownloadTimeout = 5 * time.Minute

	releaseAPIURL = "https://api.github.com/repos/%s/releases/latest"
)

// Release holds the metadata we need from a GitHub release.
type Release struct {
	TagName     string  `json:"tag_name"`
	HTMLURL     string  `json:"html_url"`
	PublishedAt string  `json:"published_at"`
	Assets      []Asset `json:"assets"`
}

// Asset is a single uploaded release artifact.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// CheckResult is the cached outcome of a check.
type CheckResult struct {
	CheckedAt      time.Time `json:"checked_at"`
	LatestVersion  string    `json:"latest_version"`
	CurrentVersion string    `json:"current_version"`
	ReleaseURL     string    `json:"release_url,omitempty"`
}

// IsNewer reports whether the cached LatestVersion is newer than current.
func (r *CheckResult) IsNewer() bool {
	if r == nil || r.LatestVersion == "" {
		return false
	}
	cmp, ok := compareSemver(r.LatestVersion, r.CurrentVersion)
	return ok && cmp > 0
}

// LoadCache reads the cache from cachePath. Returns (nil, nil) when missing
// or unreadable; callers should treat that as "no fresh result".
func LoadCache(cachePath string) (*CheckResult, error) {
	data, err := os.ReadFile(cachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var r CheckResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// SaveCache atomically writes the result to cachePath.
func SaveCache(cachePath string, r *CheckResult) error {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := cachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cachePath)
}

// ShouldCheck returns true when the cache is missing or older than ttl,
// or when the cached current_version no longer matches (e.g. user upgraded
// out-of-band via Homebrew).
func ShouldCheck(cachePath, currentVersion string, ttl time.Duration) bool {
	r, err := LoadCache(cachePath)
	if err != nil || r == nil {
		return true
	}
	if r.CurrentVersion != currentVersion {
		return true
	}
	return time.Since(r.CheckedAt) > ttl
}

// CheckLatest fetches the latest release from GitHub. It returns the
// release and whether it is strictly newer than currentVersion.
func CheckLatest(ctx context.Context, currentVersion string) (*Release, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(releaseAPIURL, Repo), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", BinaryName+"/"+currentVersion)

	client := &http.Client{Timeout: DefaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("github releases API returned %s", resp.Status)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, false, err
	}

	cmp, ok := compareSemver(rel.TagName, currentVersion)
	return &rel, ok && cmp > 0, nil
}

// AssetForCurrent picks the archive asset that matches the running OS/arch.
// It returns (asset, checksumsAsset, error). checksumsAsset is the
// checksums.txt asset for SHA256 verification.
func AssetForCurrent(rel *Release) (*Asset, *Asset, error) {
	if rel == nil {
		return nil, nil, errors.New("nil release")
	}

	osName := runtime.GOOS
	arch := runtime.GOARCH
	ext := "tar.gz"
	if osName == "windows" {
		ext = "zip"
	}

	// Asset names follow goreleaser pattern:
	//   promptconduit_<version>_<os>_<arch>.{tar.gz,zip}
	version := strings.TrimPrefix(rel.TagName, "v")
	wantArchive := fmt.Sprintf("%s_%s_%s_%s.%s", BinaryName, version, osName, arch, ext)

	var archive, checksums *Asset
	for i := range rel.Assets {
		a := &rel.Assets[i]
		switch {
		case a.Name == wantArchive:
			archive = a
		case a.Name == "checksums.txt":
			checksums = a
		}
	}
	if archive == nil {
		return nil, nil, fmt.Errorf("no release asset found for %s/%s (looking for %s)", osName, arch, wantArchive)
	}
	if checksums == nil {
		return archive, nil, errors.New("release has no checksums.txt; refusing to upgrade unverified")
	}
	return archive, checksums, nil
}

// Apply downloads the archive, verifies its SHA256, extracts the binary, and
// atomically replaces the running executable. Returns the path that was
// replaced.
func Apply(ctx context.Context, archive, checksums *Asset) (string, error) {
	if archive == nil || checksums == nil {
		return "", errors.New("missing archive or checksums asset")
	}

	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exePath)
	if err == nil {
		exePath = resolved
	}

	httpClient := &http.Client{Timeout: DownloadTimeout}

	// Fetch checksums.txt
	wantSum, err := fetchChecksum(ctx, httpClient, checksums.BrowserDownloadURL, archive.Name)
	if err != nil {
		return "", fmt.Errorf("fetch checksums: %w", err)
	}

	// Download archive into temp file while hashing it.
	tmpArchive, gotSum, err := downloadAndHash(ctx, httpClient, archive.BrowserDownloadURL)
	if err != nil {
		return "", fmt.Errorf("download archive: %w", err)
	}
	defer os.Remove(tmpArchive)

	if !strings.EqualFold(gotSum, wantSum) {
		return "", fmt.Errorf("checksum mismatch: got %s, want %s", gotSum, wantSum)
	}

	// Extract the binary into a temp file alongside the target so that the
	// final atomic rename stays on the same filesystem.
	targetDir := filepath.Dir(exePath)
	binaryName := BinaryName
	if runtime.GOOS == "windows" {
		binaryName = BinaryName + ".exe"
	}

	tmpBinary, err := extractBinary(tmpArchive, binaryName, targetDir)
	if err != nil {
		return "", fmt.Errorf("extract binary: %w", err)
	}
	defer os.Remove(tmpBinary)

	if err := os.Chmod(tmpBinary, 0o755); err != nil {
		return "", fmt.Errorf("chmod new binary: %w", err)
	}

	if err := replaceBinary(exePath, tmpBinary); err != nil {
		return "", fmt.Errorf("replace binary: %w", err)
	}

	return exePath, nil
}

// CleanupOldBinary removes a leftover .old file from a prior Windows
// upgrade. Safe to call on any platform; a no-op when nothing to clean.
func CleanupOldBinary() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	old := exePath + ".old"
	_ = os.Remove(old)
}

// fetchChecksum downloads checksums.txt and returns the hex sum for
// assetName. The file format is `<sum>  <name>` per goreleaser default.
func fetchChecksum(ctx context.Context, c *http.Client, url, assetName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %s", resp.Status)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[len(fields)-1] == assetName {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no checksum entry for %s", assetName)
}

func downloadAndHash(ctx context.Context, c *http.Client, url string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("status %s", resp.Status)
	}

	f, err := os.CreateTemp("", BinaryName+"-update-*.archive")
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hash), resp.Body); err != nil {
		os.Remove(f.Name())
		return "", "", err
	}
	return f.Name(), hex.EncodeToString(hash.Sum(nil)), nil
}

func extractBinary(archivePath, binaryName, targetDir string) (string, error) {
	if strings.HasSuffix(archivePath, ".zip") || isZip(archivePath) {
		return extractZipBinary(archivePath, binaryName, targetDir)
	}
	return extractTarGzBinary(archivePath, binaryName, targetDir)
}

func isZip(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var sig [4]byte
	if _, err := io.ReadFull(f, sig[:]); err != nil {
		return false
	}
	return sig[0] == 0x50 && sig[1] == 0x4b && (sig[2] == 0x03 || sig[2] == 0x05) && (sig[3] == 0x04 || sig[3] == 0x06)
}

func extractTarGzBinary(archivePath, binaryName, targetDir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(hdr.Name) != binaryName {
			continue
		}
		out, err := os.CreateTemp(targetDir, BinaryName+"-new-*")
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			os.Remove(out.Name())
			return "", err
		}
		if err := out.Close(); err != nil {
			os.Remove(out.Name())
			return "", err
		}
		return out.Name(), nil
	}
	return "", fmt.Errorf("binary %q not found in archive", binaryName)
}

func extractZipBinary(archivePath, binaryName, targetDir string) (string, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", err
	}
	defer zr.Close()

	for _, file := range zr.File {
		if filepath.Base(file.Name) != binaryName {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return "", err
		}
		out, err := os.CreateTemp(targetDir, BinaryName+"-new-*")
		if err != nil {
			rc.Close()
			return "", err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			os.Remove(out.Name())
			return "", err
		}
		rc.Close()
		if err := out.Close(); err != nil {
			os.Remove(out.Name())
			return "", err
		}
		return out.Name(), nil
	}
	return "", fmt.Errorf("binary %q not found in archive", binaryName)
}

// replaceBinary atomically swaps newPath into target. On Windows, where a
// running binary cannot be overwritten, the existing file is renamed to
// target+".old" first; that stale file is removed by CleanupOldBinary on
// the next invocation.
func replaceBinary(target, newPath string) error {
	if runtime.GOOS == "windows" {
		old := target + ".old"
		_ = os.Remove(old)
		if err := os.Rename(target, old); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("move current aside: %w", err)
		}
		return os.Rename(newPath, target)
	}
	return os.Rename(newPath, target)
}

// compareSemver compares two semver strings (with optional leading "v" and
// optional pre-release/build suffixes). Returns (-1,0,1) and ok=false if
// either side is not parseable.
func compareSemver(a, b string) (int, bool) {
	pa, ok := parseSemver(a)
	if !ok {
		return 0, false
	}
	pb, ok := parseSemver(b)
	if !ok {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1, true
			}
			return -1, true
		}
	}
	return 0, true
}

func parseSemver(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// Strip any pre-release / build metadata suffix.
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
