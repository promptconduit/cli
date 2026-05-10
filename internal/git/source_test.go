package git

import "testing"

func TestDetectSource(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// GitHub
		{"git@github.com:promptconduit/cli.git", "github"},
		{"https://github.com/promptconduit/cli.git", "github"},
		{"https://user:pat@github.com/foo/bar", "github"},
		{"ssh://git@github.com/foo/bar.git", "github"},
		// Enterprise GitHub subdomain
		{"git@code.github.com:foo/bar.git", "github"},
		// GitLab
		{"git@gitlab.com:group/proj.git", "gitlab"},
		{"https://gitlab.com/group/proj", "gitlab"},
		{"git@gitlab.example.gitlab.com:foo/bar.git", "gitlab"},
		// Bitbucket
		{"git@bitbucket.org:team/repo.git", "bitbucket"},
		{"https://bitbucket.org/team/repo", "bitbucket"},
		// Azure DevOps
		{"https://dev.azure.com/org/proj/_git/repo", "azure"},
		{"https://org.visualstudio.com/proj/_git/repo", "azure"},
		// Codeberg / SourceHut
		{"https://codeberg.org/foo/bar", "codeberg"},
		{"git@git.sr.ht:~user/repo", "sourcehut"},
		// Unknown / empty
		{"", ""},
		{"git@self-hosted.example.com:foo/bar.git", ""},
		{"https://internal.corp/foo/bar.git", ""},
		// Case insensitive
		{"git@GitHub.com:foo/bar.git", "github"},
	}

	for _, tc := range cases {
		got := DetectSource(tc.in)
		if got != tc.want {
			t.Errorf("DetectSource(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExtractHost(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"git@github.com:foo/bar.git", "github.com"},
		{"https://github.com/foo/bar", "github.com"},
		{"https://user@github.com:443/foo/bar", "github.com"},
		{"ssh://git@host.example/foo", "host.example"},
		{"", ""},
		{"not-a-url", ""},
	}
	for _, tc := range cases {
		got := extractHost(tc.in)
		if got != tc.want {
			t.Errorf("extractHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
