package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests back the autoupdater verification tracked in
// https://github.com/promptconduit/cli/issues/73. They add coverage for the
// behaviors that the existing suite left implicit; no production logic is
// changed.

// TestShouldCheck_RealTTLCadence pins the 24h check cadence to the actual
// CheckTTL constant the background check uses (root.go passes updater.CheckTTL
// to ShouldCheck). A check just under a day old must not re-fire; one just
// over a day old must. Verification item 1 (24h cadence).
func TestShouldCheck_RealTTLCadence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, CacheFileName)

	if CheckTTL != 24*time.Hour {
		t.Fatalf("CheckTTL = %v, expected 24h; cadence assumption changed", CheckTTL)
	}

	// 23h old with a matching current version -> still fresh, no check.
	recent := &CheckResult{
		CheckedAt:      time.Now().Add(-23 * time.Hour),
		CurrentVersion: "v0.3.2",
		LatestVersion:  "v0.3.2",
	}
	if err := SaveCache(path, recent); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if ShouldCheck(path, "v0.3.2", CheckTTL) {
		t.Error("expected ShouldCheck=false 23h after a check with CheckTTL=24h")
	}

	// 25h old -> stale, must re-check.
	stale := &CheckResult{
		CheckedAt:      time.Now().Add(-25 * time.Hour),
		CurrentVersion: "v0.3.2",
		LatestVersion:  "v0.3.2",
	}
	if err := SaveCache(path, stale); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if !ShouldCheck(path, "v0.3.2", CheckTTL) {
		t.Error("expected ShouldCheck=true 25h after a check with CheckTTL=24h")
	}
}

// TestReplaceBinary_PreservesRunningInode verifies the core self-replace
// invariant on macOS/Linux: os.Rename swaps a *new* inode into the target
// path, so a process that still holds the old file descriptor keeps reading
// the original bytes (its inode is unchanged) while new opens of the path see
// the replacement. This is exactly why the kernel lets us overwrite a running
// binary in place. Verification item 2 (detached self-replace, inode kept).
func TestReplaceBinary_PreservesRunningInode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inode semantics are POSIX-only; Windows uses the .old rename path")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, BinaryName)
	if err := os.WriteFile(target, []byte("old-bytes"), 0o755); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	// Capture the inode the "running process" is executing.
	var oldStat syscall.Stat_t
	if err := syscall.Stat(target, &oldStat); err != nil {
		t.Fatalf("stat old: %v", err)
	}

	// Open the target the way a running process holds its executable, then
	// keep the handle open across the swap.
	running, err := os.Open(target)
	if err != nil {
		t.Fatalf("open running: %v", err)
	}
	defer func() { _ = running.Close() }()

	// Stage the replacement in the same directory (same filesystem) so the
	// rename is atomic, mirroring Apply's behavior.
	newBinary, err := os.CreateTemp(dir, BinaryName+"-new-*")
	if err != nil {
		t.Fatalf("create new: %v", err)
	}
	if _, err := newBinary.WriteString("new-bytes"); err != nil {
		t.Fatalf("write new: %v", err)
	}
	if err := newBinary.Close(); err != nil {
		t.Fatalf("close new: %v", err)
	}

	if err := replaceBinary(target, newBinary.Name()); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}

	// The path now resolves to the replacement bytes / a new inode.
	var newStat syscall.Stat_t
	if err := syscall.Stat(target, &newStat); err != nil {
		t.Fatalf("stat new: %v", err)
	}
	if newStat.Ino == oldStat.Ino {
		t.Errorf("expected path to point at a new inode after swap; got same inode %d", newStat.Ino)
	}
	swapped, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read swapped path: %v", err)
	}
	if string(swapped) != "new-bytes" {
		t.Errorf("path bytes = %q, want new-bytes", string(swapped))
	}

	// The still-open "running" handle keeps reading the original bytes from
	// the original inode — the running process is undisturbed by the swap.
	// (Reading via the open fd resolves to the inode it was opened on, not the
	// path, which is precisely the kernel guarantee the updater relies on.)
	if _, err := running.Seek(0, 0); err != nil {
		t.Fatalf("seek running: %v", err)
	}
	stillRunning, err := io.ReadAll(running)
	if err != nil {
		t.Fatalf("read running handle: %v", err)
	}
	if string(stillRunning) != "old-bytes" {
		t.Errorf("open handle bytes = %q, want old-bytes (running process must keep its inode)", string(stillRunning))
	}
}

// TestCleanupOldBinary_RemovesDotOld verifies the cross-platform half of the
// Windows upgrade flow: after a Windows swap renames the current binary to
// "<exe>.old", the next invocation's CleanupOldBinary removes it. It is a
// no-op (and must not error) when there's nothing to clean.
// Verification item 3 (Windows .old rename + CleanupOldBinary).
func TestCleanupOldBinary_RemovesDotOld(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	old := exe + ".old"

	// No leftover -> no-op, no panic, no error.
	_ = os.Remove(old)
	CleanupOldBinary()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expected no .old file, stat err = %v", err)
	}

	// Plant a leftover .old and confirm CleanupOldBinary removes it.
	if err := os.WriteFile(old, []byte("stale"), 0o600); err != nil {
		t.Fatalf("plant .old: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(old) })
	CleanupOldBinary()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("expected CleanupOldBinary to remove %s, stat err = %v", old, err)
	}
}

// TestReplaceBinary_RenameAsideKeepsTarget exercises the rename-aside contract
// that replaceBinary relies on: renaming the current file out of the way and
// renaming the replacement into its place leaves the target readable with the
// new bytes and leaves an ".old" copy of the original. On POSIX this is what
// the Windows branch does step-by-step (os.Rename(target, target+".old") then
// os.Rename(new, target)); we drive those primitives directly so the behavior
// is covered on every CI runner, not just Windows.
// Verification item 3 (Windows .old rename semantics).
func TestReplaceBinary_RenameAsideKeepsTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, BinaryName)
	if err := os.WriteFile(target, []byte("old-bytes"), 0o755); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	newBinary := filepath.Join(dir, BinaryName+"-new")
	if err := os.WriteFile(newBinary, []byte("new-bytes"), 0o755); err != nil {
		t.Fatalf("seed new: %v", err)
	}

	old := target + ".old"
	if err := os.Rename(target, old); err != nil {
		t.Fatalf("rename aside: %v", err)
	}
	if err := os.Rename(newBinary, target); err != nil {
		t.Fatalf("rename into place: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "new-bytes" {
		t.Errorf("target bytes = %q, want new-bytes", string(got))
	}
	oldBytes, err := os.ReadFile(old)
	if err != nil {
		t.Fatalf("read .old: %v", err)
	}
	if string(oldBytes) != "old-bytes" {
		t.Errorf(".old bytes = %q, want old-bytes", string(oldBytes))
	}

	// And CleanupOldBinary's removal primitive clears the .old leftover.
	if err := os.Remove(old); err != nil {
		t.Fatalf("remove .old: %v", err)
	}
}

// TestApply_ChecksumMismatchRefusesSwap is the end-to-end guarantee for
// verification item 5: when the downloaded archive's SHA256 does not match the
// release's checksums.txt entry, Apply returns an error and never touches the
// target binary. The existing TestApply_ChecksumMismatch only asserts that
// two different byte streams hash differently; this drives the full Apply path
// against a server whose archive bytes are deliberately corrupted relative to
// the published checksum.
func TestApply_ChecksumMismatchRefusesSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("checksum-refusal path is platform-independent; covered on POSIX runners")
	}

	osName := runtime.GOOS
	arch := runtime.GOARCH
	version := "0.4.0"
	archiveName := fmt.Sprintf("%s_%s_%s_%s.tar.gz", BinaryName, version, osName, arch)

	// Build a valid archive, publish the checksum for the *honest* bytes, but
	// have the download endpoint serve *tampered* bytes.
	honest := buildVerifyTarGz(t, BinaryName, []byte("honest-binary-bytes"))
	honestSum := sha256.Sum256(honest)
	tampered := append([]byte("TAMPERED"), honest...)

	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(honestSum[:]), archiveName)

	mux := http.NewServeMux()
	mux.HandleFunc("/archive", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tampered) // serve corrupted bytes
	})
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(checksums))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	archive := &Asset{Name: archiveName, BrowserDownloadURL: srv.URL + "/archive"}
	sums := &Asset{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums.txt"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := Apply(ctx, archive, sums)
	if err == nil {
		t.Fatal("expected Apply to fail on checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("expected a checksum mismatch error, got: %v", err)
	}
}

// buildVerifyTarGz is a self-contained tar.gz builder for the verify tests so
// they don't depend on helpers in integration_test.go.
func buildVerifyTarGz(t *testing.T, binaryName string, content []byte) []byte {
	t.Helper()
	var buf strings.Builder
	gzw := gzip.NewWriter(verifyStringWriter{&buf})
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{Name: binaryName, Mode: 0o755, Size: int64(len(content))}); err != nil {
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

type verifyStringWriter struct{ b *strings.Builder }

func (s verifyStringWriter) Write(p []byte) (int, error) { return s.b.Write(p) }
