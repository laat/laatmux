package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/term"
	"github.com/laat/laatmux/internal/view"
)

// knownConfig is a config of this machine and vm that lists proj alone,
// written where LAATMUX_CONFIG points.
func knownConfig(t *testing.T) config.Config {
	t.Helper()
	const text = `hosts:
  - name: mac
    repos: /r
    worktrees: /w
  - name: vm
    ssh: vm
    repos: /r
    worktrees: /w
repos:
  - git@github.com:laat/proj.git
`
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", p)
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// notesSource is a repository vm has a checkout of that the config does
// not list.
const notesSource = "git@github.com:laat/notes.git"

// startKnownDaemon starts a stand-in for the local daemon whose merged
// stream has vm's set of repositories, notes in it, and a worktree of
// notes and one of proj on vm; vm is listed a moment after the
// snapshot. It counts the merged subscriptions.
func startKnownDaemon(t *testing.T) (subs func() int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	caps := []string{protocol.CapStatus, protocol.CapWorktrees, protocol.CapRepos}
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type != protocol.TypeSubscribe || !m.Merged {
			return true
		}
		mu.Lock()
		n++
		mu.Unlock()
		pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Hosts: []protocol.HostStatus{
			{Name: "mac", EnvironmentID: "lenv", Connected: true, Listed: true, Capabilities: caps, Repos: &protocol.RepoSet{}},
			{Name: "vm", SSH: "vm"},
		}})
		time.Sleep(20 * time.Millisecond)
		vm := protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Capabilities: caps}
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 2, HostStatus: &vm})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 3, Worktree: &protocol.Worktree{ID: "venv/worktree//w/notes/fix", EnvironmentID: "venv", Repo: "notes", Source: notesSource, Branch: "fix", Root: "/w/notes/fix"}})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 4, Worktree: &protocol.Worktree{ID: "venv/worktree//w/proj/fix", EnvironmentID: "venv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "fix", Root: "/w/proj/fix"}})
		vm.Listed = true
		vm.Repos = &protocol.RepoSet{Checkouts: []protocol.Checkout{
			{Repo: "proj", Source: "git@github.com:laat/proj.git", Root: "/r/proj"},
			{Repo: "notes", Source: notesSource, Root: "/r/notes"},
		}}
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 5, HostStatus: &vm})
		return true
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// path names a repository a host has discovered by the label the host
// gives it, which the config does not list, and finds its worktree on
// the host; the local daemon's merged stream is read for it, waiting
// for vm to be listed. A repository the config lists is resolved
// without that read, as before. A name nobody knows is refused, saying
// so and listing what is known.
func TestPathDiscovered(t *testing.T) {
	knownConfig(t)
	subs := startKnownDaemon(t)
	path := func(args ...string) (string, error) {
		t.Helper()
		f, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		stdout := os.Stdout
		os.Stdout = f
		err = cmdPath(context.Background(), args)
		os.Stdout = stdout
		f.Close()
		b, _ := os.ReadFile(f.Name())
		return string(b), err
	}
	if out, err := path("proj/fix", "--host", "vm"); err != nil || out != "/w/proj/fix\n" || subs() != 1 {
		t.Fatalf("proj/fix: %q %v, %d subscriptions", out, err, subs())
	}
	if out, err := path("notes/fix", "--host", "vm"); err != nil || out != "/w/notes/fix\n" || subs() != 3 {
		t.Fatalf("notes/fix: %q %v, %d subscriptions", out, err, subs())
	}
	if _, err := path("nope/fix", "--host", "vm"); err == nil || err.Error() != `unknown repository "nope": not checked out on any host and not configured; known: proj, notes` {
		t.Fatalf("nope/fix: %v", err)
	}
}

// The task form's repository picker lists the known set: the config's
// entries, then what the hosts have discovered. A discovered
// repository's line preselects it; its source pasted in any form is
// it, not new; a source no known repository has is new, named apart
// from a host's label. A clone discovered while the form is up is a
// known candidate once the picker opens again, and the submit carries
// it as the host labels it.
func TestFormKnownRepos(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t)
	set := func(cos ...protocol.Checkout) *protocol.RepoSet { return &protocol.RepoSet{Checkouts: cos} }
	notes := protocol.Checkout{Repo: "notes", Source: notesSource, Root: "/r/notes"}
	vm := protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Repos: set(notes)}
	st := merged.New()
	st.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{vm}})
	d := &dash{ctx: context.Background(), cfg: cfg, st: st, reload: func() (config.Config, error) { return cfg, nil }}
	var got command.Add
	d.submit = func(a command.Add) (string, error) { got = a; return "add-1", nil }
	in := rows.Input{
		Hosts:     []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Worktrees: []protocol.Worktree{{ID: "venv/worktree//w/notes/x", EnvironmentID: "venv", Repo: "notes", Source: notesSource, Branch: "x", Root: "/w/notes/x", Session: "notes/x"}},
	}
	m := &view.Model{Width: 100, Height: 40, View: view.ViewTree}
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Render()
	if !m.Select(rows.RepoNode(notesSource)) {
		t.Fatal("no notes line")
	}
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
	form, ok := m.Overlay.(*view.Form)
	if !ok {
		t.Fatalf("no form: %q", m.Message)
	}
	labels := func() string {
		var out []string
		for _, ch := range form.Chips[0].Choices {
			out = append(out, ch.Label)
		}
		return strings.Join(out, ",")
	}
	if labels() != "laatmux,proj,notes" || form.Chips[0].Label() != "notes" {
		t.Fatalf("candidates %s, preselected %q", labels(), form.Chips[0].Label())
	}
	pick(form, "https://github.com/laat/notes")
	if r, isNew := d.add.repo(form, form.Chips[0].Selected); isNew || r.Source != notesSource {
		t.Fatalf("notes pasted: %+v new %v", r, isNew)
	}
	pick(form, "git@github.com:other/notes.git")
	if r, isNew := d.add.repo(form, form.Chips[0].Selected); !isNew || r.Name != "other-notes" || form.Note(form) != "a new repository, which the host clones" {
		t.Fatalf("another notes pasted: %+v new %v, note %q", r, isNew, form.Note(form))
	}
	// pin-scripts cloned on vm since: known as the picker opens.
	const pin = "git@github.com:nrkno/pin-scripts.git"
	vm.Repos = set(notes, protocol.Checkout{Repo: "pins", Source: pin, Root: "/r/pins"})
	st.Apply(protocol.Message{Type: protocol.TypeUpsert, HostStatus: &vm})
	pick(form, pin)
	if r, isNew := d.add.repo(form, form.Chips[0].Selected); isNew || r.Name != "pins" || labels() != "laatmux,proj,notes,pins,other-notes" {
		t.Fatalf("a clone discovered since: %+v new %v, candidates %s", r, isNew, labels())
	}
	form.SetPrompt("Fix it")
	form.Handle(term.Key{Kind: term.KeyNewline})
	if !d.act(m, m.Poll()) {
		t.Fatalf("not submitted: %q %q", form.Error, m.Message)
	}
	if e := got.Request("x").RepoEntry; got.Repo.Name != "pins" || e == nil || e.Source != pin || e.Name != "pins" {
		t.Fatalf("add %+v entry %+v", got, e)
	}
}

// compose waits for the merged stream's snapshot and this machine's
// host listed in it, whose checkouts the directory's repository is
// preselected from, not for another host; and no longer than its bound.
func TestAwaitLocal(t *testing.T) {
	st := merged.New()
	done := make(chan struct{})
	go func() {
		awaitLocal(context.Background(), st, time.Minute)
		close(done)
	}()
	waits := func() bool {
		select {
		case <-done:
			return false
		case <-time.After(50 * time.Millisecond):
			return true
		}
	}
	if !waits() {
		t.Fatal("returned before the snapshot")
	}
	here := protocol.HostStatus{Name: "mac", EnvironmentID: "lenv", Connected: true}
	st.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{here, {Name: "vm", SSH: "vm"}}})
	if !waits() {
		t.Fatal("returned before the local host was listed")
	}
	here.Listed = true
	st.Apply(protocol.Message{Type: protocol.TypeUpsert, HostStatus: &here})
	if waits() {
		t.Fatal("waited on vm")
	}
	start := time.Now()
	awaitLocal(context.Background(), merged.New(), 50*time.Millisecond)
	if time.Since(start) > 5*time.Second {
		t.Fatal("waited past the bound")
	}
}

// readKnown takes the hosts' sets from the local daemon's merged stream;
// a local daemon without the merged stream leaves the config's entries
// alone, and says so.
func TestReadKnown(t *testing.T) {
	cfg := knownConfig(t)
	startKnownDaemon(t)
	k := readKnown(context.Background(), cfg)
	if !k.Discovered || len(k.Repos) != 2 || k.Repos[0].Name != "proj" || !k.Repos[0].Configured || k.Repos[1].Name != "notes" || len(k.Repos[1].Found) != 1 || k.Repos[1].Found[0].Root != "/r/notes" {
		t.Fatalf("known %+v", k)
	}
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapWorktrees}, nil)
	if k := readKnown(context.Background(), cfg); k.Discovered || len(k.Repos) != 1 {
		t.Fatalf("without the merged stream: %+v", k)
	}
}

// A directory whose origin is a source a host has discovered resolves
// to it, where it was refused as not configured; one whose origin no
// host has and the config does not list is refused, saying so. A
// directory of a listed repository does not read the known set.
func TestResolveRepoDiscovered(t *testing.T) {
	cfg := knownConfig(t)
	st := merged.New()
	st.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true,
		Repos: &protocol.RepoSet{Checkouts: []protocol.Checkout{{Repo: "notes", Source: notesSource, Root: "/r/notes"}}}}}})
	reads := 0
	known := func() merged.Known { reads++; return st.Known(cfg) }
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("remote", "add", "origin", "https://github.com/laat/proj")
	t.Chdir(dir)
	ctx := context.Background()
	if r, err := resolveRepo(ctx, cfg, known, ""); err != nil || r.Name != "proj" || reads != 0 {
		t.Fatalf("a listed origin: %+v %v, %d reads", r, err, reads)
	}
	git("remote", "set-url", "origin", "https://github.com/laat/notes")
	if r, err := resolveRepo(ctx, cfg, known, ""); err != nil || r.Name != "notes" || r.Source != notesSource {
		t.Fatalf("a discovered origin: %+v %v", r, err)
	}
	git("remote", "set-url", "origin", "git@github.com:laat/other.git")
	_, err := resolveRepo(ctx, cfg, known, "")
	if err == nil || !strings.HasSuffix(err.Error(), "has origin git@github.com:laat/other.git, which is not checked out on any host and not configured; use --repo (known: proj, notes)") {
		t.Fatalf("an unknown origin: %v", err)
	}
	// --repo by a host's label and by a discovered source.
	for _, flag := range []string{"notes", "https://github.com/laat/notes.git"} {
		if r, err := resolveRepo(ctx, cfg, known, flag); err != nil || r.Source != notesSource {
			t.Errorf("--repo %s: %+v %v", flag, r, err)
		}
	}
}
