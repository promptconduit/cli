package extension

import "testing"

func TestDecideReconcile(t *testing.T) {
	cases := []struct {
		name      string
		available bool
		bundled   string
		installed string
		want      reconcileAction
	}{
		{"no editor CLI", false, "0.4.0", "0.3.0", actionSkipNoCLI},
		{"not installed", true, "0.4.0", "", actionSkipNotInstalled},
		{"older installed -> update", true, "0.4.0", "0.3.0", actionUpdate},
		{"equal -> skip", true, "0.4.0", "0.4.0", actionSkipUpToDate},
		{"installed newer -> skip", true, "0.4.0", "0.5.0", actionSkipUpToDate},
		{"unparseable installed -> skip (conservative)", true, "0.4.0", "weird", actionSkipUpToDate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideReconcile(c.available, c.bundled, c.installed); got != c.want {
				t.Errorf("decideReconcile(%v, %q, %q) = %d, want %d",
					c.available, c.bundled, c.installed, got, c.want)
			}
		})
	}
}

func TestParseInstalledVersion(t *testing.T) {
	const id = "promptconduit.promptconduit-cost"
	cases := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "present among others",
			output: "ms-python.python@2024.1.0\npromptconduit.promptconduit-cost@0.4.0\nesbenp.prettier-vscode@10.1.0\n",
			want:   "0.4.0",
		},
		{
			name:   "only entry",
			output: "promptconduit.promptconduit-cost@0.3.0\n",
			want:   "0.3.0",
		},
		{
			name:   "with surrounding whitespace",
			output: "  promptconduit.promptconduit-cost@1.0.0  \n",
			want:   "1.0.0",
		},
		{
			name:   "not installed",
			output: "ms-python.python@2024.1.0\nesbenp.prettier-vscode@10.1.0\n",
			want:   "",
		},
		{
			name:   "id without version (no --show-versions) is ignored",
			output: "promptconduit.promptconduit-cost\n",
			want:   "",
		},
		{
			name:   "empty",
			output: "",
			want:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseInstalledVersion(c.output, id); got != c.want {
				t.Errorf("parseInstalledVersion() = %q, want %q", got, c.want)
			}
		})
	}
}
