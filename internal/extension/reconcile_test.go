package extension

import "testing"

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
