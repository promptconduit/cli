package eventlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tailReference is the previous whole-file implementation.
func tailReference(data []byte, n int) string {
	if n <= 0 {
		return string(data)
	}
	return lastLines(data, n)
}

func TestTailFileMatchesWholeFileReference(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", tailChunk+17)
	contents := map[string]string{
		"empty":            "",
		"one line":         "a\n",
		"no trailing nl":   "a\nb\nc",
		"only newlines":    "\n\n\n",
		"blank lines":      "a\n\nb\n\n",
		"long last line":   "a\nb\n" + long + "\n",
		"long first line":  long + "\nb\nc\n",
		"many short lines": strings.Repeat("line\n", 50000),
		"chunk boundary":   strings.Repeat("y", tailChunk-1) + "\n" + strings.Repeat("z", tailChunk) + "\nlast\n",
	}
	for name, c := range contents {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, n := range []int{-1, 0, 1, 2, 3, 20, 100000} {
			got, err := tailFile(p, n)
			if err != nil {
				t.Fatalf("%s n=%d: %v", name, n, err)
			}
			if want := tailReference([]byte(c), n); got != want {
				t.Errorf("%s n=%d: got %d bytes, want %d bytes", name, n, len(got), len(want))
			}
		}
	}
	if got, err := tailFile(filepath.Join(dir, "missing"), 5); got != "" || err != nil {
		t.Fatalf("missing file = %q, %v", got, err)
	}
}

func TestCountCapturedStreams(t *testing.T) {
	withTempDir(t)
	if _, ok := CountCaptured(); ok {
		t.Fatal("missing file must report !ok")
	}
	for _, tc := range []struct {
		content string
		want    int
	}{
		{"", 0},
		{"a\n", 1},
		{"a\nb", 2},
		{strings.Repeat("e\n", 300000), 300000}, // spans several read chunks
		{strings.Repeat("e\n", 1000) + "tail", 1001},
	} {
		if err := os.WriteFile(EventsJSONLPath(), []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, ok := CountCaptured(); !ok || got != tc.want {
			t.Errorf("content %s: got %d ok=%v, want %d", fmt.Sprintf("%.10q", tc.content), got, ok, tc.want)
		}
	}
}
