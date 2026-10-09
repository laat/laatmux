package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// A link whose target is not there, through a second link here, has
// the target made, its directory too, and stays a link: a file in its
// place would leave the target, in a dotfiles checkout say, without the
// entry.
func TestAddRepoDanglingLink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink("hop.yaml", link); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "dotfiles", "laatmux.yaml")
	if err := os.Symlink(target, filepath.Join(dir, "hop.yaml")); err != nil {
		t.Fatal(err)
	}
	if added, err := AddRepo(link, "git@x:o/p.git", "p"); !added || err != nil {
		t.Fatalf("added %v, %v", added, err)
	}
	for _, l := range []string{link, filepath.Join(dir, "hop.yaml")} {
		if fi, err := os.Lstat(l); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the link %s is gone: %v %v", l, fi, err)
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "repos:\n  - git@x:o/p.git\n" {
		t.Fatalf("target:\n%s", b)
	}
	// A relative target is from the link's own directory with the links
	// in it resolved: config.yaml in real/nested, reached through the
	// directory link alias, names ../missing.yaml, which is real's.
	if err := os.MkdirAll(filepath.Join(dir, "real", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real", "nested"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../missing.yaml", filepath.Join(dir, "real", "nested", "config.yaml")); err != nil {
		t.Fatal(err)
	}
	if added, err := AddRepo(filepath.Join(dir, "alias", "config.yaml"), "git@x:o/p.git", "p"); !added || err != nil {
		t.Fatalf("through the directory link: added %v, %v", added, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "real", "missing.yaml")); err != nil {
		t.Fatalf("the target is not real's: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("written beside the directory link")
	}
	// A .. after a directory link in the target is taken after the
	// link: other.yaml names alias/../gone.yaml, and alias is
	// real/nested, so the target is real/gone.yaml.
	if err := os.Symlink("alias/../gone.yaml", filepath.Join(dir, "other.yaml")); err != nil {
		t.Fatal(err)
	}
	if added, err := AddRepo(filepath.Join(dir, "other.yaml"), "git@x:o/p.git", "p"); !added || err != nil {
		t.Fatalf("a .. after a link: added %v, %v", added, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "real", "gone.yaml")); err != nil {
		t.Fatalf("the target is not real/gone.yaml: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gone.yaml")); err == nil {
		t.Fatal("the .. taken before the link")
	}
	// The same with a directory after the .. that is not there yet: it
	// is made under real, not beside the link.
	if err := os.Symlink("alias/../new/config.yaml", filepath.Join(dir, "third.yaml")); err != nil {
		t.Fatal(err)
	}
	if added, err := AddRepo(filepath.Join(dir, "third.yaml"), "git@x:o/p.git", "p"); !added || err != nil {
		t.Fatalf("a missing directory after a link: added %v, %v", added, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "real", "new", "config.yaml")); err != nil {
		t.Fatalf("the target is not real/new/config.yaml: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "new")); err == nil {
		t.Fatal("the missing directory made beside the link")
	}
}

// A file of two YAML documents is refused, as it was: the config is the
// first, and a rewrite of it would drop the second.
func TestAddRepoSecondDocument(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	in := "repos: [git@x:o/a.git]\n---\n# mine\nfoo: bar\n"
	if err := os.WriteFile(p, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := AddRepo(p, "git@x:o/p.git", "p"); added || err == nil || !strings.Contains(err.Error(), "has 2 YAML documents") {
		t.Fatalf("added %v, %v", added, err)
	}
	if b, _ := os.ReadFile(p); string(b) != in {
		t.Fatalf("file changed to %q", b)
	}
	// A second document that does not parse is refused too, not taken
	// for the end of the file.
	bad := "repos: [git@x:o/a.git]\n---\nfoo: [\n"
	if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := AddRepo(p, "git@x:o/p.git", "p"); added || err == nil || !strings.Contains(err.Error(), "document 2") {
		t.Fatalf("a bad second document: added %v, %v", added, err)
	}
	if b, _ := os.ReadFile(p); string(b) != bad {
		t.Fatalf("file changed to %q", b)
	}
	// An empty document after the first, a --- at the end or one of
	// comments alone, is none: the line goes before its marker, and the
	// rest of the file stays as it was.
	for _, tail := range []string{"---\n", "---\n# only a comment\n", "...\n"} {
		if err := os.WriteFile(p, []byte("repos:\n  - git@x:o/a.git\n"+tail), 0o600); err != nil {
			t.Fatal(err)
		}
		if added, err := AddRepo(p, "git@x:o/p.git", "p"); !added || err != nil {
			t.Fatalf("%q at the end: added %v, %v", tail, added, err)
		}
		if b, _ := os.ReadFile(p); string(b) != "repos:\n  - git@x:o/a.git\n  - git@x:o/p.git\n"+tail {
			t.Fatalf("%q at the end: file %q", tail, b)
		}
	}
	// A file without repos and an empty document after its first has
	// the list made at the first's end.
	if err := os.WriteFile(p, []byte("icons: ascii\n---\n# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := AddRepo(p, "git@x:o/p.git", "p"); !added || err != nil {
		t.Fatalf("no repos and an empty document: added %v, %v", added, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "icons: ascii\nrepos:\n  - git@x:o/p.git\n---\n# mine\n" {
		t.Fatalf("no repos and an empty document: file %q", b)
	}
	// One that needs the rewrite, a flow list, is refused: the rewrite
	// is of the first document, and would drop the marker after it.
	flow := "repos: [git@x:o/a.git]\n---\n# only a comment\n"
	if err := os.WriteFile(p, []byte(flow), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := AddRepo(p, "git@x:o/p.git", "p"); added || err == nil || !strings.Contains(err.Error(), "a document marker after its first document") {
		t.Fatalf("a flow list and an empty document: added %v, %v", added, err)
	}
	if b, _ := os.ReadFile(p); string(b) != flow {
		t.Fatalf("file changed to %q", b)
	}
	// A marker before a tab is one too.
	tabbed := "repos: [git@x:o/a.git]\n---\t# tail\n# keep me\n"
	if err := os.WriteFile(p, []byte(tabbed), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := AddRepo(p, "git@x:o/p.git", "p"); added || err == nil || !strings.Contains(err.Error(), "a document marker after its first document") {
		t.Fatalf("a marker before a tab: added %v, %v", added, err)
	}
	if b, _ := os.ReadFile(p); string(b) != tabbed {
		t.Fatalf("file changed to %q", b)
	}
	// One document with its start marker is one.
	if err := os.WriteFile(p, []byte("---\nrepos:\n  - git@x:o/a.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := AddRepo(p, "git@x:o/p.git", "p"); !added || err != nil {
		t.Fatalf("one document: added %v, %v", added, err)
	}
}

// The credential of a pasted URL is left out of the entry, so it never
// reaches the config: an https URL's user and token, an ssh URL's
// password; the user git of an ssh URL stays.
func TestNewRepoLeavesOutCredential(t *testing.T) {
	var cfg Config
	for _, c := range []struct{ src, want string }{
		{"https://laat:ghp_secret@github.com/nrkno/pin-scripts.git", "https://github.com/nrkno/pin-scripts.git"},
		{"https://ghp_secret@github.com/nrkno/pin-scripts", "https://github.com/nrkno/pin-scripts"},
		{"ssh://git:secret@github.com/nrkno/pin-scripts.git", "ssh://git@github.com/nrkno/pin-scripts.git"},
		{"ssh://git@github.com/nrkno/pin-scripts.git", "ssh://git@github.com/nrkno/pin-scripts.git"},
		{"git@github.com:nrkno/pin-scripts.git", "git@github.com:nrkno/pin-scripts.git"},
	} {
		r, err := cfg.NewRepo(c.src)
		if err != nil || r.Source != c.want || r.Name != "pin-scripts" {
			t.Errorf("%s: %+v %v", c.src, r, err)
		}
	}
}

// Appends at once, the relay's for several tasks or the relay's and a
// foreground add's, each keep the others': they take turns under the
// lock, so none reads a file another is about to replace.
func TestAddRepoConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("repos:\n  - git@x:o/a.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const n = 8
	errs := make(chan error, n)
	for i := range n {
		go func() {
			name := "r" + string(rune('0'+i))
			_, err := AddRepo(p, "git@x:o/"+name+".git", name)
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	cfg, err := Parse(b)
	if err != nil || len(cfg.Repos) != n+1 {
		t.Fatalf("%d repos, %v:\n%s", len(cfg.Repos), err, b)
	}
}

// Appends from two processes, the daemon's relay and add in the
// foreground say, take turns: with a pause between each write's check
// and its rename, in which two writers that did not would both pass the
// check and the second rename would drop the first's entry, every
// entry of both is there.
func TestAddRepoTwoProcesses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("repos:\n  - git@x:o/a.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var cmds []*exec.Cmd
	var outs []*strings.Builder
	for _, srcs := range []string{"git@x:o/b1.git,git@x:o/b2.git", "git@x:o/c1.git,git@x:o/c2.git"} {
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), "LAATMUX_TEST_APPEND="+p+"|"+srcs)
		out := &strings.Builder{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds, outs = append(cmds, cmd), append(outs, out)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child %d: %v\n%s", i, err, outs[i])
		}
	}
	b, _ := os.ReadFile(p)
	cfg, err := Parse(b)
	if err != nil || len(cfg.Repos) != 5 {
		t.Fatalf("%d repos, %v:\n%s", len(cfg.Repos), err, b)
	}
}

// A file another writer changes while AddRepo edits it, an editor that
// does not take the lock say, is read again and the edit made on what
// it has: both entries are there.
func TestAddRepoRetriesAChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("repos:\n  - git@x:o/a.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	once := false
	testAfterRead = func() {
		if !once {
			once = true
			os.WriteFile(p, []byte("repos:\n  - git@x:o/a.git\n  - git@x:o/hand.git\n"), 0o600)
		}
	}
	defer func() { testAfterRead = nil }()
	if added, err := AddRepo(p, "git@x:o/p.git", "p"); !added || err != nil {
		t.Fatalf("added %v, %v", added, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "repos:\n  - git@x:o/a.git\n  - git@x:o/hand.git\n  - git@x:o/p.git\n" {
		t.Fatalf("file %q", b)
	}
}

// A file another writer changed between the read and the rename, an
// editor that does not take the lock say, is not written over: the
// write says so, the file is as the other writer left it, and AddRepo
// makes its edit again on that.
func TestWriteOverChanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("repos:\n  - git@x:o/a.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)
	if err := os.WriteFile(p, []byte("repos:\n  - git@x:o/a.git\n  - git@x:o/b.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeOver(p, []byte("repos:\n  - git@x:o/a.git\n  - git@x:o/p.git\n"), before); !errors.Is(err, errChanged) {
		t.Fatalf("write over a changed file: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "repos:\n  - git@x:o/a.git\n  - git@x:o/b.git\n" {
		t.Fatalf("file %q", b)
	}
	if ents, _ := os.ReadDir(filepath.Dir(p)); len(ents) != 1 {
		t.Fatalf("a temporary left behind: %v", ents)
	}
	if added, err := AddRepo(p, "git@x:o/p.git", "p"); err != nil || !added {
		t.Fatal(added, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "repos:\n  - git@x:o/a.git\n  - git@x:o/b.git\n  - git@x:o/p.git\n" {
		t.Fatalf("file %q", b)
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
	// Another file renamed over it, of the same size and modification
	// time: another file all the same.
	fi, _ := os.Stat(p)
	b, _ := os.ReadFile(p)
	other := filepath.Join(filepath.Dir(p), "other.yaml")
	if err := os.WriteFile(other, []byte(strings.Replace(string(b), "o/p.git", "o/r.git", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, p); err != nil {
		t.Fatal(err)
	}
	if cfg, changed, err := w.Changed(); err != nil || !changed || len(cfg.Repos) != 1 || cfg.Repos[0].Name != "r" {
		t.Fatalf("after a rename of the same size and time: %v %v %v", cfg.Repos, changed, err)
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
	// Truncated in place by a writer that has not written yet: no
	// change, on every look until it has.
	os.WriteFile(p, nil, 0o600)
	for i := 0; i < 2; i++ {
		if _, changed, err := w.Changed(); err != nil || changed {
			t.Fatalf("an empty file, look %d: %v %v", i, changed, err)
		}
	}
	if _, err := LoadSettled(); err != ErrWriting {
		t.Fatalf("LoadSettled of the file being written: %v", err)
	}
	os.WriteFile(p, []byte("repos: [git@x:o/s.git]\n"), 0o600)
	if cfg, changed, err := w.Changed(); err != nil || !changed || len(cfg.Repos) != 1 || cfg.Repos[0].Name != "s" {
		t.Fatalf("written after the truncation: %v %v %v", cfg.Repos, changed, err)
	}
	// Emptied, and left so, untouched, past the settling time: the
	// default config, for the watch and LoadSettled alike.
	os.WriteFile(p, nil, 0o600)
	if _, changed, _ := w.Changed(); changed {
		t.Fatal("an empty file taken at once")
	}
	time.Sleep(settling + 100*time.Millisecond)
	if cfg, changed, err := w.Changed(); err != nil || !changed || len(cfg.Repos) != 0 || len(cfg.Hosts) != 1 {
		t.Fatalf("an emptied file: %v %v %v", cfg.Repos, changed, err)
	}
	if cfg, err := LoadSettled(); err != nil || len(cfg.Hosts) != 1 {
		t.Fatalf("LoadSettled of an emptied file: %v %v", cfg.Hosts, err)
	}
	// An empty file dated ahead of the clock is no write under way.
	ahead := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, ahead, ahead); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadSettled(); err != nil || len(cfg.Hosts) != 1 {
		t.Fatalf("LoadSettled of an empty file dated ahead: %v %v", cfg.Hosts, err)
	}
	// An empty file at the first look is the default config, as Load
	// reads it.
	var fresh Watch
	os.WriteFile(p, nil, 0o600)
	if cfg, changed, err := fresh.Changed(); err != nil || !changed || len(cfg.Hosts) != 1 {
		t.Fatalf("an empty file first: %v %v %v", cfg.Hosts, changed, err)
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

const hostsFile = `# where I work
hosts:
  - name: mac   # this machine
    repos: ~/code
    worktrees: ~/worktrees
  - name: vm
    ssh: vm     # the coder box
    repos: [~/src, ~/src/work]
    worktrees: ~/wt

    # the old one
  - name: box
    ssh: box
icons: ascii   # plain
`

// Pausing puts paused: true after the entry's last line with content,
// in the indentation of its keys, and resuming takes the line out: the
// rest of the file stays as it was, byte for byte, comments and blank
// lines included, the last entry's as the others'. A host that is so
// already is not written.
func TestSetPausedKeepsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(hostsFile), 0o640); err != nil {
		t.Fatal(err)
	}
	step := func(host string, paused bool, want string) {
		t.Helper()
		changed, err := SetPaused(p, host, paused)
		if err != nil || !changed {
			t.Fatalf("%s %v: changed %v, %v", host, paused, changed, err)
		}
		b, _ := os.ReadFile(p)
		if string(b) != want {
			t.Fatalf("%s %v: file:\n%s\nwant:\n%s", host, paused, b, want)
		}
		if changed, err := SetPaused(p, host, paused); err != nil || changed {
			t.Fatalf("%s %v again: changed %v, %v", host, paused, changed, err)
		}
	}
	vm := strings.Replace(hostsFile, "    worktrees: ~/wt\n", "    worktrees: ~/wt\n    paused: true\n", 1)
	both := strings.Replace(vm, "    ssh: box\n", "    ssh: box\n    paused: true\n", 1)
	step("vm", true, vm)
	step("box", true, both)
	b, _ := os.ReadFile(p)
	cfg, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hosts[0].Paused || !cfg.Hosts[1].Paused || !cfg.Hosts[2].Paused || cfg.Icons != "ascii" {
		t.Fatalf("parsed %+v", cfg.Hosts)
	}
	step("vm", false, strings.Replace(hostsFile, "    ssh: box\n", "    ssh: box\n    paused: true\n", 1))
	step("box", false, hostsFile)
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v, %v", fi.Mode(), err)
	}
}

// An entry the line cannot be put in or taken out of, a flow mapping or
// one with paused on its first line, is written again from its nodes;
// a paused: false line is made true in place. This machine's entry, a
// host the file does not list, and a file of two documents are refused,
// the file as it was.
func TestSetPausedShapes(t *testing.T) {
	for _, c := range []struct {
		in     string
		paused bool
		want   string // what the result has
	}{
		{"hosts:\n  - {name: vm, ssh: vm}\n", true, "paused: true"},
		{"hosts:\n  - {name: vm, ssh: vm, paused: true}\n", false, "ssh: vm"},
		{"hosts:\n  - paused: true\n    name: vm\n    ssh: vm\n", false, "ssh: vm"},
		{"hosts:\n  - name: vm\n    paused: false\n    ssh: vm\n", true, "hosts:\n  - name: vm\n    paused: true\n    ssh: vm\n"},
		{"hosts:\n  - name: vm\n    ssh: vm\nnote: x\n", true, "hosts:\n  - name: vm\n    ssh: vm\n    paused: true\nnote: x\n"},
	} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(c.in), 0o600); err != nil {
			t.Fatal(err)
		}
		if changed, err := SetPaused(p, "vm", c.paused); err != nil || !changed {
			t.Errorf("%q: changed %v, %v", c.in, changed, err)
			continue
		}
		b, _ := os.ReadFile(p)
		cfg, err := Parse(b)
		if err != nil || len(cfg.Hosts) != 1 || cfg.Hosts[0].Name != "vm" || cfg.Hosts[0].SSH != "vm" || cfg.Hosts[0].Paused != c.paused || !strings.Contains(string(b), c.want) {
			t.Errorf("%q: file %q, %+v, %v", c.in, b, cfg.Hosts, err)
		}
	}
	for _, c := range []struct{ in, host, want string }{
		{"hosts:\n  - name: mac\n  - name: vm\n    ssh: vm\n", "mac", "mac is this machine"},
		{"hosts:\n  - name: vm\n    ssh: vm\n", "box", `unknown host "box"; configured: vm`},
		{"hosts:\n  - name: vm\n    ssh: vm\n---\nicons: ascii\n", "vm", "has 2 YAML documents"},
		{"", "vm", "unknown host"},
	} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(c.in), 0o600); err != nil {
			t.Fatal(err)
		}
		changed, err := SetPaused(p, c.host, true)
		if b, _ := os.ReadFile(p); changed || err == nil || !strings.Contains(err.Error(), c.want) || string(b) != c.in {
			t.Errorf("%q %s: changed %v, %v, file %q", c.in, c.host, changed, err, b)
		}
	}
}
