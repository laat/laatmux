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

// A source no known repository has is named for its last path element,
// else its org and that, else that with a hash, the first no known
// repository has, the config's or a host's label; one known in any
// form, and one that is no forge source, are refused.
func TestNewRepo(t *testing.T) {
	cfg, err := Parse([]byte("repos:\n  - git@github.com:laat/scripts.git\n  - source: https://github.com/laat/other.git\n    name: tools\n"))
	if err != nil {
		t.Fatal(err)
	}
	// A host's label for a checkout the config does not list.
	known := append(cfg.Repos, Repo{Source: "git@github.com:laat/notes.git", Name: "notes"})
	for _, c := range []struct{ src, name string }{
		{"git@github.com:nrkno/pin-scripts.git", "pin-scripts"},
		{"ssh://git@github.com/nrkno/pin-scripts", "pin-scripts"},
		{"https://github.com/nrkno/pin-scripts.git", "pin-scripts"},
		{"git@github.com:nrkno/scripts.git", "nrkno-scripts"},
		// tools is the explicit name of another.
		{"https://github.com/nrkno/tools", "nrkno-tools"},
		// notes is a host's label for another.
		{"git@github.com:nrkno/notes.git", "nrkno-notes"},
		{"https://github.com/vercel/next.js.git", "next_js"},
	} {
		r, err := NewRepo(c.src, known)
		if err != nil || r.Source != c.src || r.Name != c.name || r.Explicit {
			t.Errorf("%s: %+v, %v", c.src, r, err)
		}
	}
	// Both plain names taken: the hash.
	taken := append(known, Repo{Source: "git@github.com:a/x.git", Name: "x"}, Repo{Source: "git@github.com:b/y.git", Name: "o-x"})
	if r, err := NewRepo("git@github.com:o/x.git", taken); err != nil || r.Name != "x-"+sourceHash("git@github.com:o/x.git") {
		t.Errorf("both plain names taken: %+v, %v", r, err)
	}
	for _, src := range []string{"https://github.com/laat/scripts", "git@github.com:laat/other", "https://github.com/laat/notes", "pin-scripts", "/srv/git/proj.git", "alice@box:proj", ""} {
		if r, err := NewRepo(src, known); err == nil {
			t.Errorf("%q taken as %+v", src, r)
		}
	}
}

// The credential of a pasted URL is left out of the repository, so it
// never reaches a pending file or a host: an https URL's user and
// token, an ssh URL's password; the user git of an ssh URL stays.
func TestNewRepoLeavesOutCredential(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"https://laat:ghp_secret@github.com/nrkno/pin-scripts.git", "https://github.com/nrkno/pin-scripts.git"},
		{"https://ghp_secret@github.com/nrkno/pin-scripts", "https://github.com/nrkno/pin-scripts"},
		{"ssh://git:secret@github.com/nrkno/pin-scripts.git", "ssh://git@github.com/nrkno/pin-scripts.git"},
		{"ssh://git@github.com/nrkno/pin-scripts.git", "ssh://git@github.com/nrkno/pin-scripts.git"},
		{"git@github.com:nrkno/pin-scripts.git", "git@github.com:nrkno/pin-scripts.git"},
	} {
		r, err := NewRepo(c.src, nil)
		if err != nil || r.Source != c.want || r.Name != "pin-scripts" {
			t.Errorf("%s: %+v %v", c.src, r, err)
		}
	}
}

const twoHosts = "hosts:\n  - name: mac\n  - name: vm\n    ssh: vm\n"

// A config that is a symlink, into a dotfiles checkout say, stays one:
// the target is written.
func TestSetPausedThroughLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "laatmux.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(twoHosts), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if changed, err := SetPaused(link, "vm", true); err != nil || !changed {
		t.Fatalf("changed %v, %v", changed, err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link is gone: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(target); string(b) != twoHosts+"    paused: true\n" {
		t.Errorf("target:\n%s", b)
	}
	if ents, _ := os.ReadDir(filepath.Dir(target)); len(ents) != 1 {
		t.Errorf("a temporary left behind: %v", ents)
	}
}

// The file a link whose target is not there names, through a second
// link here, is the last link's target, a relative one from the link's
// own directory with the links in it resolved, a .. after a directory
// link taken after the link.
func TestLinkTarget(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink("hop.yaml", link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "dotfiles", "laatmux.yaml"), filepath.Join(dir, "hop.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "real", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real", "nested"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../missing.yaml", filepath.Join(dir, "real", "nested", "config.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("alias/../gone.yaml", filepath.Join(dir, "other.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("alias/../new/config.yaml", filepath.Join(dir, "third.yaml")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, want string }{
		{link, filepath.Join(real, "dotfiles", "laatmux.yaml")},
		// config.yaml in real/nested, reached through the directory
		// link alias, names ../missing.yaml, which is real's.
		{filepath.Join(dir, "alias", "config.yaml"), filepath.Join(real, "real", "missing.yaml")},
		// alias is real/nested, so alias/../gone.yaml is real's.
		{filepath.Join(dir, "other.yaml"), filepath.Join(real, "real", "gone.yaml")},
		// The same with a directory after the .. that is not there.
		{filepath.Join(dir, "third.yaml"), filepath.Join(real, "real", "new", "config.yaml")},
	} {
		if got, err := linkTarget(c.path); err != nil || got != c.want {
			t.Errorf("%s: %s, %v; want %s", c.path, got, err, c.want)
		}
	}
}

// A file of two YAML documents is refused: the config is the first,
// and a rewrite of it would drop the second. An empty one after the
// first, a --- at the end or one of comments alone, is none: the line
// goes before its marker, and the rest of the file stays as it was;
// but a rewrite, of an entry written as a flow mapping, is refused.
func TestSetPausedSecondDocument(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	for _, c := range []struct{ in, want string }{
		{twoHosts + "---\n# mine\nfoo: bar\n", "has 2 YAML documents"},
		// A second document that does not parse, not taken for the end
		// of the file.
		{twoHosts + "---\nfoo: [\n", "document 2"},
		{"hosts:\n  - {name: vm, ssh: vm}\n---\n# only a comment\n", "a document marker after its first document"},
		// A marker before a tab is one too.
		{"hosts:\n  - {name: vm, ssh: vm}\n---\t# tail\n# keep me\n", "a document marker after its first document"},
	} {
		if err := os.WriteFile(p, []byte(c.in), 0o600); err != nil {
			t.Fatal(err)
		}
		if changed, err := SetPaused(p, "vm", true); changed || err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: changed %v, %v", c.in, changed, err)
		}
		if b, _ := os.ReadFile(p); string(b) != c.in {
			t.Errorf("%q: file changed to %q", c.in, b)
		}
	}
	for _, tail := range []string{"---\n", "---\n# only a comment\n", "...\n"} {
		if err := os.WriteFile(p, []byte(twoHosts+tail), 0o600); err != nil {
			t.Fatal(err)
		}
		if changed, err := SetPaused(p, "vm", true); !changed || err != nil {
			t.Fatalf("%q at the end: changed %v, %v", tail, changed, err)
		}
		if b, _ := os.ReadFile(p); string(b) != twoHosts+"    paused: true\n"+tail {
			t.Fatalf("%q at the end: file %q", tail, b)
		}
	}
	// One document with its start marker is one.
	if err := os.WriteFile(p, []byte("---\n"+twoHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := SetPaused(p, "vm", true); !changed || err != nil {
		t.Fatalf("one document: changed %v, %v", changed, err)
	}
}

// manyHosts is a config of this machine and n hosts h0, h1, ...
func manyHosts(n int) string {
	s := "hosts:\n  - name: mac\n"
	for i := range n {
		s += "  - name: h" + string(rune('0'+i)) + "\n    ssh: h" + string(rune('0'+i)) + "\n"
	}
	return s
}

// Edits at once each keep the others': they take turns under the lock,
// so none reads a file another is about to replace.
func TestSetPausedConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	const n = 8
	if err := os.WriteFile(p, []byte(manyHosts(n)), 0o600); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, n)
	for i := range n {
		go func() {
			_, err := SetPaused(p, "h"+string(rune('0'+i)), true)
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
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range cfg.Hosts[1:] {
		if !h.Paused {
			t.Fatalf("%s not paused:\n%s", h.Name, b)
		}
	}
}

// Edits from two processes, a pause in two terminals say, take turns:
// with a pause between each write's check and its rename, in which two
// writers that did not would both pass the check and the second rename
// would drop the first's edit, every edit of both is there.
func TestSetPausedTwoProcesses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(manyHosts(4)), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var cmds []*exec.Cmd
	var outs []*strings.Builder
	for _, hosts := range []string{"h0,h1", "h2,h3"} {
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), "LAATMUX_TEST_PAUSE="+p+"|"+hosts)
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
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range cfg.Hosts[1:] {
		if !h.Paused {
			t.Fatalf("%s not paused:\n%s", h.Name, b)
		}
	}
}

// A file another writer changes while SetPaused edits it, an editor
// that does not take the lock say, is read again and the edit made on
// what it has: both changes are there.
func TestSetPausedRetriesAChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(twoHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	once := false
	testAfterRead = func() {
		if !once {
			once = true
			os.WriteFile(p, []byte(twoHosts+"icons: ascii\n"), 0o600)
		}
	}
	defer func() { testAfterRead = nil }()
	if changed, err := SetPaused(p, "vm", true); !changed || err != nil {
		t.Fatalf("changed %v, %v", changed, err)
	}
	if b, _ := os.ReadFile(p); string(b) != twoHosts+"    paused: true\nicons: ascii\n" {
		t.Fatalf("file %q", b)
	}
}

// A file another writer changed between the read and the rename, an
// editor that does not take the lock say, is not written over: the
// write says so, the file is as the other writer left it, and SetPaused
// makes its edit again on that.
func TestWriteOverChanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(twoHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)
	if err := os.WriteFile(p, []byte(twoHosts+"icons: ascii\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeOver(p, []byte(twoHosts+"    paused: true\n"), before); !errors.Is(err, errChanged) {
		t.Fatalf("write over a changed file: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != twoHosts+"icons: ascii\n" {
		t.Fatalf("file %q", b)
	}
	if ents, _ := os.ReadDir(filepath.Dir(p)); len(ents) != 1 {
		t.Fatalf("a temporary left behind: %v", ents)
	}
	if changed, err := SetPaused(p, "vm", true); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if b, _ := os.ReadFile(p); string(b) != twoHosts+"    paused: true\nicons: ascii\n" {
		t.Fatalf("file %q", b)
	}
}

// Watch answers the config on its first call and after the file
// changed, by a rename over it or an edit in place; not between.
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
	made := filepath.Join(filepath.Dir(p), "made.yaml")
	if err := os.WriteFile(made, []byte("repos:\n  - git@x:o/p.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(made, p); err != nil {
		t.Fatal(err)
	}
	if cfg, changed, err := w.Changed(); err != nil || !changed || len(cfg.Repos) != 1 || cfg.Repos[0].Name != "p" {
		t.Fatalf("after the rename: %v %v %v", cfg.Repos, changed, err)
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
// a paused: false line is made true in place, its comment kept, and a
// paused line taken out leaves its comment. An entry paused through a
// mapping merged into it is resumed with a paused: false line, the
// other entries that merge it still paused, and can be paused and
// resumed again. This machine's entry, a host the
// file does not list, and a file of two documents are refused, the file
// as it was.
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
		{"hosts:\n  - name: vm\n    paused: false  # costs money\n    ssh: vm\n", true, "hosts:\n  - name: vm\n    paused: true # costs money\n    ssh: vm\n"},
		{"hosts:\n  - name: vm\n    paused: true  # costs money\n    ssh: vm\n", false, "hosts:\n  - name: vm\n    # costs money\n    ssh: vm\n"},
		{"hosts:\r\n  - name: vm\r\n    ssh: vm\r\n", true, "hosts:\r\n  - name: vm\r\n    ssh: vm\r\n    paused: true\r\n"},
		{"hosts:\r\n  - name: vm\r\n    ssh: vm", true, "hosts:\r\n  - name: vm\r\n    ssh: vm\r\n    paused: true\r\n"},
		// On the item's first line: written again, the comment kept on
		// the value, or above the entry where the key goes.
		{"hosts:\n  - paused: false  # costs money\n    name: vm\n    ssh: vm\n", true, "paused: true # costs money"},
		{"hosts:\n  - paused: true  # costs money\n    name: vm\n    ssh: vm\n", false, "# costs money\n  - name: vm"},
		// Paused of its own and through a merge: paused: false.
		{"b: &b {paused: true}\nhosts:\n  - <<: *b\n    name: vm\n    ssh: vm\n    paused: true\n", false, "    ssh: vm\n    paused: false\n"},
		// The line the text edit would put in goes inside the block
		// scalars, which the check refuses, so the nodes are written.
		{"hosts:\n  - name: vm\n    ssh: vm\n    bin: |\n      # not a comment\nicons: ascii\n", true, "paused: true"},
		{"hosts:\n  - name: vm\n    ssh: vm\n    bin: |+\n      laatmux\n\n  - name: box\n    ssh: box\n", true, "paused: true"},
	} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(c.in), 0o600); err != nil {
			t.Fatal(err)
		}
		if changed, err := SetPaused(p, "vm", c.paused); err != nil || !changed {
			t.Errorf("%q: changed %v, %v", c.in, changed, err)
			continue
		}
		before, _ := Parse([]byte(c.in))
		b, _ := os.ReadFile(p)
		cfg, err := Parse(b)
		if err != nil || len(cfg.Hosts) != len(before.Hosts) || cfg.Hosts[0].Name != "vm" || cfg.Hosts[0].SSH != "vm" || cfg.Hosts[0].Bin != before.Hosts[0].Bin || cfg.Hosts[0].Paused != c.paused || !strings.Contains(string(b), c.want) {
			t.Errorf("%q: file %q, %+v, %v", c.in, b, cfg.Hosts, err)
		}
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	// Through a merge, resumed, paused and resumed again, the file's
	// layout kept: paused: false is a line of the entry's.
	merged := "base: &b\n  paused: true   # all off\nhosts:\n  - <<: *b\n    name: vm\n    ssh: vm\n  - <<: *b\n    name: box\n    ssh: box\n"
	if err := os.WriteFile(p, []byte(merged), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed := strings.Replace(merged, "    ssh: vm\n", "    ssh: vm\n    paused: false\n", 1)
	for _, step := range []struct {
		paused bool
		want   string
	}{
		{false, resumed},
		{true, strings.Replace(merged, "    ssh: vm\n", "    ssh: vm\n    paused: true\n", 1)},
		{false, resumed},
	} {
		if changed, err := SetPaused(p, "vm", step.paused); err != nil || !changed {
			t.Fatalf("through a merge, paused %v: changed %v, %v", step.paused, changed, err)
		}
		b, _ := os.ReadFile(p)
		if cfg, err := Parse(b); err != nil || string(b) != step.want || cfg.Hosts[0].Paused != step.paused || !cfg.Hosts[1].Paused {
			t.Fatalf("through a merge, paused %v: %+v, %v, file:\n%s", step.paused, cfg.Hosts, err, b)
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
