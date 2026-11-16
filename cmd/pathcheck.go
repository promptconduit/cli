package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
		resolved := resolvePath(p)
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, p)
	}
	return out
}

func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// hookCommandRe matches the absolute binary path `install` bakes into hook
// configs, e.g. "/Users/x/.local/bin/promptconduit hook".
var hookCommandRe = regexp.MustCompile(`"([^"]*promptconduit(?:\.exe)?) hook`)

// hookBinaryPaths returns the distinct binaries the installed hooks call, read
// from each tool's hook config (the files `install` writes; see install.go).
func hookBinaryPaths(home string) []string {
	files := []string{
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".cursor", "hooks.json"),
		filepath.Join(home, ".gemini", "settings.json"),
		filepath.Join(home, ".codex", "hooks.json"),
		filepath.Join(home, ".copilot", "hooks", "promptconduit.json"),
		filepath.Join(home, ".grok", "hooks", "promptconduit.json"),
	}
	var out []string
	seen := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, m := range hookCommandRe.FindAllSubmatch(data, -1) {
			// The match is the raw JSON string body: unescape it (Windows
			// paths are stored with doubled backslashes).
			var p string
			if err := json.Unmarshal(append(append([]byte{'"'}, m[1]...), '"'), &p); err != nil {
				continue
			}
			if !seen[resolvePath(p)] {
				seen[resolvePath(p)] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// printDuplicateBinaries warns when more than one promptconduit is on PATH.
// Typed commands run the first; hooks run the absolute path baked in at
// install. A specific `sudo rm` is suggested only when exactly one copy on
// PATH is the hooks' copy AND it can self-update (its directory is writable).
// If the hooks use a copy that can't update, deleting the others would strand
// them on stale code, so the advice is to reinstall the hooks instead.
func printDuplicateBinaries(w io.Writer, copies []string, self string, hookPaths []string, writable func(dir string) bool) {
	if len(copies) < 2 {
		return
	}
	hookSet := map[string]bool{}
	for _, h := range hookPaths {
		hookSet[resolvePath(h)] = true
	}
	selfResolved := resolvePath(self)

	_, _ = fmt.Fprintln(w, "Warning: multiple promptconduit binaries on PATH:")
	var removable, hookCopies []string
	for i, p := range copies {
		var notes []string
		if i == 0 {
			notes = append(notes, "runs when you type `promptconduit`")
		}
		r := resolvePath(p)
		if hookSet[r] {
			notes = append(notes, "used by your hooks")
			hookCopies = append(hookCopies, p)
		} else {
			removable = append(removable, p)
		}
		if r == selfResolved {
			notes = append(notes, "this one")
		}
		label := ""
		if len(notes) > 0 {
			label = " (" + strings.Join(notes, ", ") + ")"
		}
		_, _ = fmt.Fprintf(w, "  %s%s\n", p, label)
	}
	switch {
	case len(hookCopies) == 1 && !writable(filepath.Dir(hookCopies[0])):
		_, _ = fmt.Fprintf(w, "  Your hooks use %s, which can't self-update (its directory isn't writable).\n", hookCopies[0])
		_, _ = fmt.Fprintln(w, "  Re-run `promptconduit install <tool>` from the copy you want to keep, then remove the others.")
	case len(hookCopies) == 1 && len(removable) > 0:
		_, _ = fmt.Fprintln(w, "  Keep the copy your hooks use and remove the others, so typed commands run the")
		_, _ = fmt.Fprintf(w, "  same version (a root-owned copy needs sudo): sudo rm %s\n", strings.Join(removable, " "))
	default:
		_, _ = fmt.Fprintln(w, "  Remove the copies you don't use so typed commands, hooks, and auto-update agree.")
	}
	_, _ = fmt.Fprintln(w)
}
