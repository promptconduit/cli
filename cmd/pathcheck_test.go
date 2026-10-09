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

func TestHookBinaryPaths(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"hooks":{"Stop":[{"hooks":[{"command":"/opt/pc/promptconduit hook"}]}],` +
		`"PreToolUse":[{"hooks":[{"command":"/opt/pc/promptconduit hook"}]}]}}`
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// Windows paths are JSON-escaped in the config and must come back unescaped.
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	win := `{"hooks":{"stop":[{"command":"C:\\\\pc\\\\promptconduit.exe hook"}]}}`
	if err := os.WriteFile(filepath.Join(home, ".cursor", "hooks.json"), []byte(win), 0o644); err != nil {
		t.Fatal(err)
	}
	got := hookBinaryPaths(home)
	if len(got) != 2 || got[0] != "/opt/pc/promptconduit" || got[1] != `C:\\pc\\promptconduit.exe` {
		t.Fatalf("got %q, want the unix path once and the unescaped Windows path", got)
	}
}

func writableAll(string) bool { return true }

func TestPrintDuplicateBinaries(t *testing.T) {
	stale, fresh := t.TempDir(), t.TempDir()
	staleBin, freshBin := writeExe(t, stale), writeExe(t, fresh)

	// Stale copy first on PATH, hooks use the fresh one: remove only the stale.
	var buf bytes.Buffer
	printDuplicateBinaries(&buf, []string{staleBin, freshBin}, freshBin, []string{freshBin}, writableAll)
	out := buf.String()
	if !strings.Contains(out, staleBin+" (runs when you type `promptconduit`)") ||
		!strings.Contains(out, freshBin+" (used by your hooks, this one)") ||
		!strings.Contains(out, "sudo rm "+staleBin+"\n") {
		t.Fatalf("unexpected advice:\n%s", out)
	}

	// The hooks' copy is FIRST on PATH: advice must never name it.
	buf.Reset()
	printDuplicateBinaries(&buf, []string{freshBin, staleBin}, freshBin, []string{freshBin}, writableAll)
	if strings.Contains(buf.String(), "sudo rm "+freshBin) || !strings.Contains(buf.String(), "sudo rm "+staleBin) {
		t.Fatalf("must suggest removing only the non-hook copy:\n%s", buf.String())
	}

	// Hooks' binary unknown: no specific rm command.
	buf.Reset()
	printDuplicateBinaries(&buf, []string{staleBin, freshBin}, staleBin, nil, writableAll)
	if strings.Contains(buf.String(), "sudo rm") {
		t.Fatalf("no specific removal without knowing the hooks' copy:\n%s", buf.String())
	}

	// Hooks use a copy that can't self-update: no rm; reinstall the hooks.
	buf.Reset()
	printDuplicateBinaries(&buf, []string{staleBin, freshBin}, freshBin, []string{staleBin},
		func(dir string) bool { return dir != stale })
	if strings.Contains(buf.String(), "sudo rm") || !strings.Contains(buf.String(), "can't self-update") {
		t.Fatalf("hooks on a non-updatable copy must not get rm advice:\n%s", buf.String())
	}

	// Hooks split across both copies: nothing is safely removable.
	buf.Reset()
	printDuplicateBinaries(&buf, []string{staleBin, freshBin}, freshBin, []string{staleBin, freshBin}, writableAll)
	if strings.Contains(buf.String(), "sudo rm") {
		t.Fatalf("no rm when every copy is used by hooks:\n%s", buf.String())
	}

	// A single copy prints nothing.
	buf.Reset()
	printDuplicateBinaries(&buf, []string{freshBin}, freshBin, []string{freshBin}, writableAll)
	if buf.Len() != 0 {
		t.Fatalf("single copy should print nothing, got %q", buf.String())
	}
}
