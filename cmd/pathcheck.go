package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// binariesOnPath returns every distinct executable named name on pathEnv, in
// PATH order (the first is what a typed command runs). Symlinks are resolved
// so one binary reachable through two PATH entries is listed once.
func binariesOnPath(pathEnv, name string) []string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var out []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			resolved = p
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, p)
	}
	return out
}

// printDuplicateBinaries warns when more than one promptconduit is on PATH.
// Typed commands run the first; hooks run the absolute path baked in at
// install. A stale copy that comes first (often a root-owned /usr/local/bin
// install that can't self-upgrade) silently runs old code for every typed
// command.
func printDuplicateBinaries(w io.Writer, copies []string, self string) {
	if len(copies) < 2 {
		return
	}
	selfResolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		selfResolved = self
	}
	_, _ = fmt.Fprintln(w, "Warning: multiple promptconduit binaries on PATH:")
	for i, p := range copies {
		var notes []string
		if i == 0 {
			notes = append(notes, "runs when you type `promptconduit`")
		}
		if r, err := filepath.EvalSymlinks(p); err == nil && r == selfResolved {
			notes = append(notes, "this one")
		}
		label := ""
		if len(notes) > 0 {
			label = " (" + strings.Join(notes, ", ") + ")"
		}
		_, _ = fmt.Fprintf(w, "  %s%s\n", p, label)
	}
	_, _ = fmt.Fprintln(w, "  Remove the extra copies so typed commands, hooks, and auto-update use one version")
	_, _ = fmt.Fprintf(w, "  (a root-owned copy needs sudo, e.g. `sudo rm %s`).\n", copies[0])
	_, _ = fmt.Fprintln(w)
}
