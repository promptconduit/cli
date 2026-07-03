package sessions

import (
	"reflect"
	"testing"
)

func TestParseClaudePIDs(t *testing.T) {
	// Real-ish `ps -axo pid=,comm=` output: several CLI `claude` processes
	// (one of them the probe's own ancestor, which pgrep would drop), the
	// Claude desktop app and its helpers (basename "Claude"/other — must be
	// excluded), plus unrelated processes and blank lines.
	out := `
  501 /sbin/launchd
53134 claude
11351 claude
81491 claude
35241 /Applications/Claude.app/Contents/MacOS/Claude
 1489 /Applications/Claude.app/Contents/Helpers/chrome-native-host
  777 claude-code-something
  920 /usr/local/bin/claude
`
	got := parseClaudePIDs(out)
	want := []string{"53134", "11351", "81491", "920"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseClaudePIDs\n got: %v\nwant: %v", got, want)
	}
}

func TestParseClaudePIDsEmpty(t *testing.T) {
	if got := parseClaudePIDs(""); got != nil {
		t.Fatalf("expected nil for empty input, got %v", got)
	}
}

func TestAdditionalDirs(t *testing.T) {
	root := "/Users/x/GitHub/promptconduit"
	cli := root + "/cli"
	ext := root + "/editor-extension"
	other := "/Users/x/GitHub/other"

	cases := []struct {
		name    string
		launch  string
		touched []string
		want    []string
	}{
		{
			// pc workflow: launched at root, only touched dirs under root →
			// nothing to re-add (resuming from root already covers them).
			name:    "all under launch dir",
			launch:  root,
			touched: []string{cli, root, ext},
			want:    nil,
		},
		{
			// launched narrow (in cli) but reached a sibling repo → re-add it.
			name:    "sibling outside launch dir",
			launch:  cli,
			touched: []string{cli, ext, other},
			want:    []string{ext, other},
		},
		{
			name:    "launch dir itself is skipped",
			launch:  cli,
			touched: []string{cli},
			want:    nil,
		},
		{
			name:    "first-seen order preserved, no dups",
			launch:  cli,
			touched: []string{other, ext, other},
			want:    []string{other, ext},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := additionalDirs(tc.launch, tc.touched)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("additionalDirs(%q, %v)\n got: %v\nwant: %v", tc.launch, tc.touched, got, tc.want)
			}
		})
	}
}
