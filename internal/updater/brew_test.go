package updater

import "testing"

func TestIsUnderCellar(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/opt/homebrew/Cellar/promptconduit/0.7.3/bin/promptconduit", true},
		{"/usr/local/Cellar/promptconduit/0.7.3/bin/promptconduit", true},
		{"/home/linuxbrew/.linuxbrew/Cellar/promptconduit/0.7.3/bin/promptconduit", true},
		{"/usr/local/bin/promptconduit", false},
		{"/Users/me/.local/bin/promptconduit", false},
		{"/opt/CellarX/promptconduit", false}, // not a Cellar path component
		{"", false},
	}
	for _, c := range cases {
		if got := isUnderCellar(c.path); got != c.want {
			t.Errorf("isUnderCellar(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestIsUnderDir(t *testing.T) {
	cases := []struct {
		path, dir string
		want      bool
	}{
		{"/opt/homebrew/bin/promptconduit", "/opt/homebrew", true},
		{"/opt/homebrew/bin/promptconduit", "/opt/homebrew/", true}, // trailing slash tolerated
		{"/opt/homebrew", "/opt/homebrew", true},                    // exact
		{"/opt/homebrew-extra/bin/x", "/opt/homebrew", false},       // prefix-but-not-nested
		{"/usr/local/bin/x", "/opt/homebrew", false},
	}
	for _, c := range cases {
		if got := isUnderDir(c.path, c.dir); got != c.want {
			t.Errorf("isUnderDir(%q, %q) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
}

func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.7.3", "0.7.2", true},
		{"v0.7.3", "0.7.2", true},
		{"0.8.0", "0.7.9", true},
		{"1.0.0", "0.9.9", true},
		{"0.7.2", "0.7.2", false}, // equal is not newer
		{"0.7.1", "0.7.2", false}, // older
		{"garbage", "0.7.2", false},
		{"0.7.3", "garbage", false},
	}
	for _, c := range cases {
		if got := IsNewerVersion(c.a, c.b); got != c.want {
			t.Errorf("IsNewerVersion(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
