package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeExe(t *testing.T, dir string) string {
	t.Helper()
	name := "promptconduit"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBinariesOnPath(t *testing.T) {
	stale, fresh, empty := t.TempDir(), t.TempDir(), t.TempDir()
	staleBin := writeExe(t, stale)
	freshBin := writeExe(t, fresh)
	// The same binary reachable via a symlinked dir is listed once.
	link := filepath.Join(t.TempDir(), "linkdir")
	if err := os.Symlink(fresh, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	path := strings.Join([]string{stale, empty, fresh, link}, string(os.PathListSeparator))

	got := binariesOnPath(path, "promptconduit")
	if len(got) != 2 || got[0] != staleBin || got[1] != freshBin {
		t.Fatalf("got %v, want [%s %s] in PATH order", got, staleBin, freshBin)
	}
}

func TestPrintDuplicateBinaries(t *testing.T) {
	stale, fresh := t.TempDir(), t.TempDir()
	staleBin, freshBin := writeExe(t, stale), writeExe(t, fresh)

	var buf bytes.Buffer
	printDuplicateBinaries(&buf, []string{staleBin, freshBin}, freshBin)
	out := buf.String()
	if !strings.Contains(out, staleBin+" (runs when you type `promptconduit`)") {
		t.Fatalf("first copy not flagged as the typed one:\n%s", out)
	}
	if !strings.Contains(out, freshBin+" (this one)") {
		t.Fatalf("running copy not flagged:\n%s", out)
	}

	buf.Reset()
	printDuplicateBinaries(&buf, []string{freshBin}, freshBin)
	if buf.Len() != 0 {
		t.Fatalf("single copy should print nothing, got %q", buf.String())
	}
}
