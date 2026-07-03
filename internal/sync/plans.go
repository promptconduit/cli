package sync

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Plan-file sync: Claude Code's plan mode writes approved plans to
// ~/.claude/plans/<slug>.md. These are first-class session artifacts the
// platform should hold next to the transcript, so `promptconduit sync`
// uploads them via POST /v1/plans/sync.
//
// Session association: the plan file's path appears verbatim inside the
// session's transcript (the plan-mode system reminders name it), so we find
// the owning session by scanning recent transcripts for the path. Best-effort
// — a plan whose transcript is gone uploads unassociated.

// PlanFile is one discovered local plan.
type PlanFile struct {
	Path       string
	Name       string // file basename, e.g. "swift-blazing-hopper.md"
	Content    []byte
	Hash       string // SHA256 of content, the dedup key
	ModifiedAt string // RFC3339
	SessionID  string // owning session, "" when unresolved
}

// PlansDir returns ~/.claude/plans.
func PlansDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "plans"), nil
}

// DiscoverPlans lists local plan files, newest-modified first. A missing plans
// directory yields an empty slice.
func DiscoverPlans() ([]PlanFile, error) {
	dir, err := PlansDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var plans []PlanFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		content, err := os.ReadFile(path)
		if err != nil || len(content) == 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		hash, err := CalculateFileHash(path)
		if err != nil {
			continue
		}
		plans = append(plans, PlanFile{
			Path:       path,
			Name:       e.Name(),
			Content:    content,
			Hash:       hash,
			ModifiedAt: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].ModifiedAt > plans[j].ModifiedAt })
	return plans, nil
}

// AssociatePlanSession resolves the session that produced the plan by scanning
// transcripts for the plan's path. candidates are transcript paths to search
// (pass the session's own transcript for the auto-sync case, or a recent set
// for a full sync). Returns "" when no transcript mentions the plan.
func AssociatePlanSession(plan PlanFile, candidates []string) string {
	needle := []byte(plan.Path)
	// The path may also appear ~-abbreviated or just by basename in reminders;
	// basename is unique enough (slugs are random) and cheap to scan for.
	baseNeedle := []byte(plan.Name)

	for _, tp := range candidates {
		if transcriptMentions(tp, needle, baseNeedle) {
			return strings.TrimSuffix(filepath.Base(tp), ".jsonl")
		}
	}
	return ""
}

// RecentTranscripts returns transcript paths modified within windowHours of
// the plan's mtime — the bounded candidate set for association during a full
// sync. Claude Code only.
func RecentTranscripts(parser Parser, plan PlanFile, windowHours int) []string {
	paths, err := parser.GetTranscriptPaths()
	if err != nil {
		return nil
	}
	planTime, err := time.Parse(time.RFC3339, plan.ModifiedAt)
	if err != nil {
		return paths // unparseable mtime: scan everything rather than nothing
	}
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		d := info.ModTime().Sub(planTime)
		if d < 0 {
			d = -d
		}
		if d.Hours() <= float64(windowHours) {
			out = append(out, p)
		}
	}
	return out
}

// transcriptMentions reports whether the transcript contains either needle.
// Streams line-by-line so huge transcripts don't load into memory.
func transcriptMentions(path string, needles ...[]byte) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		for _, n := range needles {
			if len(n) > 0 && bytes.Contains(line, n) {
				return true
			}
		}
	}
	return false
}
