package config

import "testing"

func TestSameSource(t *testing.T) {
	same := []string{
		"git@github.com:laat/laatmux.git",
		"git@github.com:laat/laatmux",
		"git@github.com:laat/laatmux/",
		"ssh://git@github.com/laat/laatmux.git",
		"ssh://git@github.com:22/laat/laatmux",
		"git+ssh://git@GitHub.com/laat/laatmux.git",
		"https://github.com/laat/laatmux.git",
		"https://github.com/laat/laatmux/",
		"https://user@github.com/laat/laatmux",
		"https://github.com:443/laat/laatmux",
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
		{"git@github.com:laat/laatmux.git", "https://github.com/laat/laatmux.git?x=1"},
		// A user other than git, or none, is a home directory.
		{"alice@box:proj.git", "bob@box:proj.git"},
		{"alice@box:proj.git", "https://box/proj"},
		{"box:proj", "https://box/proj"},
		{"box:/proj", "box:proj"},
		{"ssh://alice@box/proj", "ssh://bob@box/proj"},
		{"ssh://box/proj", "https://box/proj"},
		// An absolute or home path on the git user is the server's own.
		{"git@box:/srv/proj.git", "git@box:srv/proj.git"},
		{"git@box:~/proj.git", "https://box/~/proj"},
		// Ports other than the default are other servers.
		{"ssh://git@host:2222/o/r.git", "ssh://git@host:2223/o/r.git"},
		{"ssh://git@host:2222/o/r.git", "https://host/o/r"},
		{"https://host:8443/o/r", "https://host/o/r"},
		{"http://host:8080/o/r", "http://host/o/r"},
		{"/src/laatmux", "/src/laatmux.git"},
		{"/src/laatmux", "file:///src/laatmux"},
		// A source that spells another's key is not that source.
		{"forge:example.com/o/r", "https://example.com/o/r"},
		{SourceKey("https://example.com/o/r"), "https://example.com/o/r"},
	}
	for _, p := range differ {
		if SameSource(p[0], p[1]) {
			t.Errorf("%s and %s are the same", p[0], p[1])
		}
	}
	// Other sources are compared exactly.
	for _, s := range []string{"/src/laatmux", "./laatmux", "file:///src/x", "C:/x", "host:~/x", "alice@box:proj", "ssh://git@h:2222/o/r"} {
		if !SameSource(s, s) || SameSource(s, s+"x") || SameSource(s, s+".git") {
			t.Errorf("%q is not compared exactly", s)
		}
	}
}
