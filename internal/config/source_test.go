package config

import "testing"

func TestSameSource(t *testing.T) {
	same := []string{
		"git@github.com:laat/laatmux.git",
		"git@github.com:laat/laatmux",
		"github.com:laat/laatmux.git",
		"ssh://git@github.com/laat/laatmux.git",
		"ssh://git@github.com:22/laat/laatmux",
		"git+ssh://git@GitHub.com/laat/laatmux.git",
		"https://github.com/laat/laatmux.git",
		"https://github.com/laat/laatmux/",
		"https://user@github.com/laat/laatmux",
		"http://github.com/laat/laatmux",
		"HTTPS://github.com/laat/laatmux",
	}
	for _, a := range same {
		for _, b := range same {
			if !SameSource(a, b) {
				t.Errorf("%s and %s differ", a, b)
			}
		}
	}
	differ := [][2]string{
		{"git@github.com:laat/laatmux.git", "git@github.com:laat/other.git"},
		{"git@github.com:laat/laatmux.git", "git@gitlab.com:laat/laatmux.git"},
		{"git@github.com:laat/laatmux.git", "git@github.com:Laat/laatmux.git"},
		{"git@github.com:laat/laatmux.git", "/Users/me/laatmux.git"},
		{"/src/laatmux", "/src/laatmux.git"},
		{"/src/laatmux", "file:///src/laatmux"},
		{"git@github.com:laat/laatmux.git", "https://github.com/laat/laatmux.git?x=1"},
		{"host:~/laatmux", "ssh://host/~/laatmux"},
	}
	for _, p := range differ {
		if SameSource(p[0], p[1]) {
			t.Errorf("%s and %s are the same", p[0], p[1])
		}
	}
	for _, s := range []string{"/src/laatmux", "./laatmux", "file:///src/x", "C:/x", "host:~/x", ""} {
		if SourceKey(s) != s {
			t.Errorf("SourceKey(%q) = %q, want it unchanged", s, SourceKey(s))
		}
	}
}
