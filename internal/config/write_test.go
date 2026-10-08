package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A source new to the list is named as the list would derive it; one
// whose name would rename a listed repository, or that derives no
// label, gets a name of its own, explicit; one listed in any form, and
// one that is no forge source, are refused.
func TestNewRepo(t *testing.T) {
	cfg, err := Parse([]byte("repos:\n  - git@github.com:laat/scripts.git\n  - source: https://github.com/laat/other.git\n    name: tools\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		src, name string
		explicit  bool
	}{
		{"git@github.com:nrkno/pin-scripts.git", "pin-scripts", false},
		{"ssh://git@github.com/nrkno/pin-scripts", "pin-scripts", false},
		{"https://github.com/nrkno/pin-scripts.git", "pin-scripts", false},
		// scripts would rename laat/scripts to laat-scripts.
		{"git@github.com:nrkno/scripts.git", "nrkno-scripts", true},
		// tools is the explicit name of another.
		{"https://github.com/nrkno/tools", "nrkno-tools", true},
		{"https://github.com/vercel/next.js.git", "next_js", true},
	} {
		r, err := cfg.NewRepo(c.src)
		if err != nil || r.Source != c.src || r.Name != c.name || r.Explicit != c.explicit {
			t.Errorf("%s: %+v, %v", c.src, r, err)
		}
	}
	for _, src := range []string{"https://github.com/laat/scripts", "git@github.com:laat/other", "pin-scripts", "/srv/git/proj.git", "alice@box:proj", ""} {
		if r, err := cfg.NewRepo(src); err == nil {
			t.Errorf("%q taken as %+v", src, r)
		}
	}
}

const personal = `# my laatmux
hosts:
  - name: mac   # this machine
    repos: ~/code
    worktrees: ~/worktrees

# the repos I work in
repos:
  - git@github.com:laat/laatmux.git  # mine
  - source: https://github.com/laat/other.git
    name: other
    setup:
      - |
        echo one
  # more to come

sidebar:
  width: "40"   # columns
`

// The entry is a line after the list's last item, in its indentation:
// the rest of the file stays as it was, byte for byte, comments and
// blank lines included. The same repository in another form is not
// added again.
func TestAddRepoKeepsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(personal), 0o640); err != nil {
		t.Fatal(err)
	}
	added, err := AddRepo(p, "git@github.com:nrkno/pin-scripts.git", "pin-scripts")
	if err != nil || !added {
		t.Fatalf("added %v, %v", added, err)
	}
	b, _ := os.ReadFile(p)
	want := strings.Replace(personal, "        echo one\n", "        echo one\n  - git@github.com:nrkno/pin-scripts.git\n", 1)
	if string(b) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", b, want)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	for _, src := range []string{"https://github.com/nrkno/pin-scripts", "ssh://git@github.com/nrkno/pin-scripts.git", "git@github.com:laat/laatmux.git"} {
		if added, err := AddRepo(p, src, "pin-scripts"); err != nil || added {
			t.Errorf("%s: added %v, %v", src, added, err)
		}
	}
	if b2, _ := os.ReadFile(p); string(b2) != want {
		t.Errorf("a listed source changed the file:\n%s", b2)
	}
	cfg, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := cfg.RepoBySource("https://github.com/nrkno/pin-scripts"); !ok || r.Name != "pin-scripts" {
		t.Errorf("listed as %+v %v", r, ok)
	}
}

// The shapes the line goes into, and the ones written from the nodes:
// the result always parses with the entry and the rest unchanged.
func TestAddRepoShapes(t *testing.T) {
	for _, c := range []struct {
		name, in, src, as, want string
	}{
		{"no repos", "hosts:\n  - name: mac\n", "git@x:o/p.git", "p", "hosts:\n  - name: mac\nrepos:\n  - git@x:o/p.git\n"},
		{"no newline at the end", "hosts:\n  - name: mac", "git@x:o/p.git", "p", "hosts:\n  - name: mac\nrepos:\n  - git@x:o/p.git\n"},
		{"empty repos", "repos:   # none yet\nicons: ascii\n", "git@x:o/p.git", "p", "repos:   # none yet\n  - git@x:o/p.git\nicons: ascii\n"},
		{"unindented list", "repos:\n- git@x:o/a.git\n# end\n", "git@x:o/p.git", "p", "repos:\n- git@x:o/a.git\n- git@x:o/p.git\n# end\n"},
		{"empty file", "", "git@x:o/p.git", "p", "repos:\n  - git@x:o/p.git\n"},
		{"comments alone", "# nothing yet\n", "git@x:o/p.git", "p", "# nothing yet\nrepos:\n  - git@x:o/p.git\n"},
		{"explicit name", "repos:\n  - git@x:a/p.git\n", "git@x:o/p.git", "o-p", "repos:\n  - git@x:a/p.git\n  - source: git@x:o/p.git\n    name: o-p\n"},
		{"flow list", "repos: [git@x:o/a.git]\n", "git@x:o/p.git", "p", "repos: ['git@x:o/a.git', 'git@x:o/p.git']\n"},
		{"null", "repos: ~\n", "git@x:o/p.git", "p", "repos:\n  - git@x:o/p.git\n"},
	} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(c.in), 0o600); err != nil {
			t.Fatal(err)
		}
		if added, err := AddRepo(p, c.src, c.as); err != nil || !added {
			t.Errorf("%s: added %v, %v", c.name, added, err)
			continue
		}
		b, _ := os.ReadFile(p)
		if string(b) != c.want {
			t.Errorf("%s:\n%s\nwant:\n%s", c.name, b, c.want)
		}
	}
}

// A file that does not parse, a name another repository has, and a
// file whose repos is no list are refused, the file as it was.
func TestAddRepoRefuses(t *testing.T) {
	for _, c := range []struct{ name, in, as, want string }{
		{"bad file", "repos: [\n", "p", "config.yaml"},
		{"name taken", "repos:\n  - source: git@x:a/q.git\n    name: p\n", "p", "both get the name p"},
		{"not a list", "repos: git@x:o/a.git\n", "p", "config.yaml"},
	} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(c.in), 0o600); err != nil {
			t.Fatal(err)
		}
		if added, err := AddRepo(p, "git@x:o/p.git", c.as); added || err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: added %v, %v", c.name, added, err)
		}
		if b, _ := os.ReadFile(p); string(b) != c.in {
			t.Errorf("%s: file changed to %q", c.name, b)
		}
	}
}

// A config that is a symlink, into a dotfiles checkout say, stays one:
// the target is written.
func TestAddRepoThroughLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "laatmux.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("repos:\n  - git@x:o/a.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if added, err := AddRepo(link, "git@x:o/p.git", "p"); err != nil || !added {
		t.Fatalf("added %v, %v", added, err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link is gone: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(target); string(b) != "repos:\n  - git@x:o/a.git\n  - git@x:o/p.git\n" {
		t.Errorf("target:\n%s", b)
	}
	if ents, _ := os.ReadDir(filepath.Dir(target)); len(ents) != 1 {
		t.Errorf("a temporary left behind: %v", ents)
	}
}

// Watch answers the config on its first call and after the file
// changed, by an append's rename or an edit in place; not between.
func TestWatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("LAATMUX_CONFIG", p)
	var w Watch
	if cfg, changed, err := w.Changed(); err != nil || !changed || len(cfg.Repos) != 0 {
		t.Fatalf("first call: %v %v %v", cfg.Repos, changed, err)
	}
	if _, changed, _ := w.Changed(); changed {
		t.Fatal("a missing file changed")
	}
	if _, err := AddRepo(p, "git@x:o/p.git", "p"); err != nil {
		t.Fatal(err)
	}
	if cfg, changed, err := w.Changed(); err != nil || !changed || len(cfg.Repos) != 1 || cfg.Repos[0].Name != "p" {
		t.Fatalf("after the append: %v %v %v", cfg.Repos, changed, err)
	}
	if _, changed, _ := w.Changed(); changed {
		t.Fatal("an unchanged file changed")
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("  - git@x:o/q.git\n")
	f.Close()
	if cfg, changed, err := w.Changed(); err != nil || !changed || len(cfg.Repos) != 2 {
		t.Fatalf("after an edit in place: %v %v %v", cfg.Repos, changed, err)
	}
	os.WriteFile(p, []byte("repos: [\n"), 0o600)
	if _, changed, err := w.Changed(); err == nil || changed {
		t.Fatalf("a bad file: %v %v", changed, err)
	}
	if _, changed, err := w.Changed(); err != nil || changed {
		t.Fatalf("the bad file again: %v %v", changed, err)
	}
}

// A line that looks like a comment but is the end of a block scalar
// would take the entry inside the scalar: the check after the edit
// sees the scalar changed, and the file is written from its nodes, the
// scalar whole.
func TestAddRepoChecked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	in := "repos:\n  - source: git@x:o/a.git\n    setup:\n      - |\n        echo\n        # last\nicons: ascii\n"
	if err := os.WriteFile(p, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := AddRepo(p, "git@x:o/p.git", "p"); err != nil || !added {
		t.Fatalf("added %v, %v", added, err)
	}
	b, _ := os.ReadFile(p)
	cfg, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repos) != 2 || len(cfg.Repos[0].Setup) != 1 || cfg.Repos[0].Setup[0] != "echo\n# last\n" || cfg.Repos[1].Source != "git@x:o/p.git" || cfg.Icons != "ascii" {
		t.Fatalf("file:\n%s", b)
	}
}
