package eventlog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteFileTransformsAndKeepsOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	huge := strings.Repeat("x", 9*1024*1024) // over the 8 MiB scanner cap the pruner uses
	in := "a\n" + `{"big":"` + huge + `"}` + "\nb\nc\n"
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := rewriteFile(path, func(line []byte) ([]byte, bool) {
		if bytes.Equal(line, []byte("b")) {
			return []byte("B"), true
		}
		return nil, false
	})
	if err != nil || n != 1 {
		t.Fatalf("rewriteFile = %d, %v; want 1, nil", n, err)
	}
	got, _ := os.ReadFile(path)
	want := "a\n" + `{"big":"` + huge + `"}` + "\nB\nc\n"
	if string(got) != want {
		t.Fatalf("content mismatch (len got %d want %d)", len(got), len(want))
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644 preserved", info.Mode().Perm())
	}
}

func TestRewriteFileNoChangeLeavesFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	n, err := rewriteFile(path, func([]byte) ([]byte, bool) { return nil, false })
	if err != nil || n != 0 {
		t.Fatalf("rewriteFile = %d, %v; want 0, nil", n, err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("file was replaced even though nothing changed")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

// Lines appended by a concurrent writer after the pass starts must survive.
func TestRewriteFileCarriesConcurrentAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appended := false
	n, err := rewriteFile(path, func(line []byte) ([]byte, bool) {
		if !appended {
			appended = true
			f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.WriteString("late\n")
			_ = f.Close()
		}
		return append([]byte("x"), line...), true
	})
	if err != nil || n != 2 {
		t.Fatalf("rewriteFile = %d, %v; want 2, nil", n, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "xa\nxb\nlate\n" {
		t.Fatalf("got %q, want transformed lines plus the verbatim late append", got)
	}
}

func TestRewriteFileMissingIsNoop(t *testing.T) {
	n, err := rewriteFile(filepath.Join(t.TempDir(), "nope.jsonl"), func([]byte) ([]byte, bool) { return nil, true })
	if err != nil || n != 0 {
		t.Fatalf("missing file: %d, %v; want 0, nil", n, err)
	}
}

// A record still being written at the start of the pass (no trailing newline)
// is copied verbatim and completed by the carry-over, never split.
func TestRewriteFileKeepsPartialFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("a\n{\"half\":"), 0o644); err != nil {
		t.Fatal(err)
	}
	completed := false
	n, err := rewriteFile(path, func(line []byte) ([]byte, bool) {
		if !completed {
			completed = true
			f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.WriteString("1}\n")
			_ = f.Close()
		}
		return []byte("A"), true
	})
	if err != nil || n != 1 {
		t.Fatalf("rewriteFile = %d, %v; want 1, nil", n, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "A\n{\"half\":1}\n" {
		t.Fatalf("got %q; partial record must be completed, not split", got)
	}
}
