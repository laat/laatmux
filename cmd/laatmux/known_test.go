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
	"github.com/laat/laatmux/internal/home"
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
// snapshot. mac and vm each label a repository of their own tools, and
// vm has a worktree of its. It counts the merged subscriptions.
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
			{Name: "mac", EnvironmentID: "lenv", Connected: true, Listed: true, Capabilities: caps,
				Repos: &protocol.RepoSet{Checkouts: []protocol.Checkout{{Repo: "tools", Source: "git@github.com:b/tools.git", Root: "/r/tools"}}}},
			{Name: "vm", SSH: "vm"},
		}})
		time.Sleep(20 * time.Millisecond)
		vm := protocol.HostStatus{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Capabilities: caps}
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 2, HostStatus: &vm})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 3, Worktree: &protocol.Worktree{ID: "venv/worktree//w/notes/fix", EnvironmentID: "venv", Repo: "notes", Source: notesSource, Branch: "fix", Root: "/w/notes/fix"}})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 4, Worktree: &protocol.Worktree{ID: "venv/worktree//w/proj/fix", EnvironmentID: "venv", Repo: "proj", Source: "git@github.com:laat/proj.git", Branch: "fix", Root: "/w/proj/fix"}})
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 5, Worktree: &protocol.Worktree{ID: "venv/worktree//w/tools/fix", EnvironmentID: "venv", Repo: "tools", Source: "git@github.com:a/tools.git", Branch: "fix", Root: "/w/tools/fix"}})
		vm.Listed = true
		vm.Repos = &protocol.RepoSet{Checkouts: []protocol.Checkout{
			{Repo: "proj", Source: "git@github.com:laat/proj.git", Root: "/r/proj"},
			{Repo: "notes", Source: notesSource, Root: "/r/notes"},
			{Repo: "tools", Source: "git@github.com:a/tools.git", Root: "/r/tools"},
		}}
		pc.Write(protocol.Message{Type: protocol.TypeUpsert, Seq: 6, HostStatus: &vm})
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
// without that read, as before. A name two hosts label two sources by
// is the one --host's host labels so, and refused without it. A name
// nobody knows is refused, saying so and listing what is known.
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
	if out, err := path("tools/fix", "--host", "vm"); err != nil || out != "/w/tools/fix\n" {
		t.Fatalf("tools/fix on vm: %q %v", out, err)
	}
	if _, err := path("tools/fix", "--host", "mac"); err == nil || err.Error() != "no worktree for tools/fix on mac" {
		t.Fatalf("tools/fix on mac: %v", err)
	}
	if _, err := path("tools/fix"); err == nil || !strings.HasPrefix(err.Error(), `"tools" is the label of git@github.com:a/tools.git and git@github.com:b/tools.git on different hosts`) {
		t.Fatalf("tools/fix: %v", err)
	}
	if _, err := path("nope/fix", "--host", "vm"); err == nil || err.Error() != `unknown repository "nope": not checked out on any host and not configured; known: proj, notes, tools, tools` {
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
	// box, after vm in the config, labels notes otherwise.
	box := protocol.HostStatus{Name: "box", SSH: "box", EnvironmentID: "benv", Connected: true, Listed: true,
		Repos: set(protocol.Checkout{Repo: "notes-b", Source: notesSource, Root: "/r/notes-b"})}
	st := merged.New()
	st.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{vm, box}})
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
	// notes-b is box's label for notes, whose checkout has that
	// directory there.
	pick(form, "git@github.com:other/notes-b.git")
	if r, isNew := d.add.repo(form, form.Chips[0].Selected); !isNew || r.Name != "other-notes-b" {
		t.Fatalf("a paste named as box labels notes: %+v new %v", r, isNew)
	}
	// pin-scripts cloned on vm since: known as the picker opens.
	const pin = "git@github.com:nrkno/pin-scripts.git"
	vm.Repos = set(notes, protocol.Checkout{Repo: "pins", Source: pin, Root: "/r/pins"})
	st.Apply(protocol.Message{Type: protocol.TypeUpsert, HostStatus: &vm})
	pick(form, pin)
	if r, isNew := d.add.repo(form, form.Chips[0].Selected); isNew || r.Name != "pins" || labels() != "laatmux,proj,notes,pins,other-notes,other-notes-b" {
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
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("waited on vm")
	}
	start := time.Now()
	awaitLocal(context.Background(), merged.New(), 50*time.Millisecond)
	if time.Since(start) > 5*time.Second {
		t.Fatal("waited past the bound")
	}
}

// The form opened on a worktree preselects its repository by source: a
// configured repository's name that a host also gives a checkout of
// another source, a fork say, does not take the form to the fork.
func TestFormPreselectsBySource(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := dashConfig(t) // laat/laatmux and laat/proj
	const fork = "git@github.com:other/proj.git"
	st := merged.New()
	st.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true,
		Repos: &protocol.RepoSet{Checkouts: []protocol.Checkout{{Repo: "proj", Source: fork, Root: "/r2/proj"}}}}}})
	d := &dash{ctx: context.Background(), cfg: cfg, st: st}
	for _, src := range []string{"git@github.com:laat/proj.git", fork} {
		w := protocol.Worktree{ID: "venv/worktree//w/" + src, EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "x", Root: "/w/" + src}
		in := rows.Input{
			Hosts:     []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
			Worktrees: []protocol.Worktree{w},
		}
		m := &view.Model{Width: 100, Height: 40, View: view.ViewTree}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		m.Render()
		if !m.Select(w.ID) {
			t.Fatal("no worktree line")
		}
		d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'a'}})
		form, ok := m.Overlay.(*view.Form)
		if !ok {
			t.Fatalf("no form: %q", m.Message)
		}
		if r, _ := d.add.repo(form, form.Chips[0].Selected); r.Source != src {
			t.Errorf("a worktree of %s preselected %s", src, r.Source)
		}
		form.Handle(term.Key{Kind: term.KeyEsc})
		d.act(m, m.Poll())
	}
	// A name alone: the config's entry before a host's label for another
	// source.
	f := &addForm{hosts: cfg.Hosts[:2], agents: cfg.AgentNames(), known: st.Known}
	if form := buildForm(cfg, f, home.Last{}, "proj", "", "", nil); form.Chips[0].Choices[form.Chips[0].Selected].Detail != "git@github.com:laat/proj.git" {
		t.Errorf("proj by name preselected %+v", form.Chips[0])
	}
}

// jump takes a name of the known set's for a repository the host labels
// otherwise, last: box, first in the config, labels notes so, and vm,
// where the worktree is, labels it notes-vm. A target another reading
// takes, vm's own label, is taken without the known set, and a name
// the known set has for no repository is refused as no session.
func TestJumpKnownName(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	preflights := filepath.Join(t.TempDir(), "ssh")
	fakeSSH(t, `echo x >> '`+preflights+`'; printf "can't find session: x\n" >&2; exit 1`)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("hosts:\n  - name: mac\n  - name: box\n    ssh: box\n  - name: vm\n    ssh: vm\n    repos: /r\n    worktrees: /w\nagents:\n  claude: {cmd: [claude]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", cfgPath)
	w := protocol.Worktree{ID: "venv/worktree//w/notes-vm/fix", EnvironmentID: "venv", Repo: "notes-vm", Source: notesSource, Branch: "fix", Root: "/w/notes-vm/fix"}
	var mu sync.Mutex
	subs := 0
	caps := []string{protocol.CapStatus, protocol.CapWorktrees, protocol.CapRepos}
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type == protocol.TypeSubscribe && m.Merged {
			mu.Lock()
			subs++
			mu.Unlock()
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Hosts: []protocol.HostStatus{
				{Name: "mac", EnvironmentID: "lenv", Connected: true, Listed: true, Capabilities: caps, Repos: &protocol.RepoSet{}},
				{Name: "box", SSH: "box", EnvironmentID: "benv", Connected: true, Listed: true, Capabilities: caps,
					Repos: &protocol.RepoSet{Checkouts: []protocol.Checkout{{Repo: "notes", Source: notesSource, Root: "/r/notes"}}}},
				{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Capabilities: caps,
					Repos: &protocol.RepoSet{Checkouts: []protocol.Checkout{{Repo: "notes-vm", Source: notesSource, Root: "/r/notes-vm"}}}},
			}, Worktrees: []protocol.Worktree{w}})
		}
		return true
	})
	// The worktree has no session and vm's daemon no new: the refusal
	// is the add line, which says the worktree was found.
	refusal := "vm/notes-vm/fix has no managed session; laatmux add fix --repo " + notesSource + " --host vm makes one"
	// The preflight for the session runs once a target names no
	// worktree, and before the known set is read.
	for _, c := range []struct {
		target, want     string
		subs, preflights int
	}{
		{"vm/notes-vm/fix", refusal, 1, 0},
		{"vm/notes/fix", refusal, 3, 1},
		{"vm/nope/fix", "vm/nope/fix: no such session on the laatmux tmux server", 5, 2},
	} {
		err := cmdJump(context.Background(), []string{c.target})
		mu.Lock()
		n := subs
		mu.Unlock()
		b, _ := os.ReadFile(preflights)
		if err == nil || !strings.HasPrefix(err.Error(), c.want) || n != c.subs || strings.Count(string(b), "x\n") != c.preflights {
			t.Errorf("jump %s: %v, %d subscriptions, preflights %q; want %q, %d, %d", c.target, err, n, b, c.want, c.subs, c.preflights)
		}
	}
	// A preflight that could not be made says so: the session may be
	// there, and the known set is not read.
	fakeSSH(t, `printf "ssh: connect to host vm port 22: Connection refused\n" >&2; exit 255`)
	err := cmdJump(context.Background(), []string{"vm/notes/fix"})
	mu.Lock()
	n := subs
	mu.Unlock()
	if err == nil || err.Error() != "vm: ssh failed: ssh: connect to host vm port 22: Connection refused" || n != 6 {
		t.Errorf("jump vm/notes/fix, the preflight refused: %v, %d subscriptions", err, n)
	}
}

// readKnown takes the hosts' sets from the local daemon's merged stream;
// a local daemon without the merged stream leaves the config's entries
// alone, and says so.
func TestReadKnown(t *testing.T) {
	cfg := knownConfig(t)
	startKnownDaemon(t)
	k := readKnown(context.Background(), cfg)
	if !k.Discovered || len(k.Repos) != 4 || k.Repos[0].Name != "proj" || !k.Repos[0].Configured || k.Repos[1].Name != "notes" || len(k.Repos[1].Found) != 1 || k.Repos[1].Found[0].Root != "/r/notes" {
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
	if r, err := resolveRepo(ctx, cfg, known, "", ""); err != nil || r.Name != "proj" || reads != 0 {
		t.Fatalf("a listed origin: %+v %v, %d reads", r, err, reads)
	}
	git("remote", "set-url", "origin", "https://github.com/laat/notes")
	if r, err := resolveRepo(ctx, cfg, known, "", ""); err != nil || r.Name != "notes" || r.Source != notesSource {
		t.Fatalf("a discovered origin: %+v %v", r, err)
	}
	git("remote", "set-url", "origin", "git@github.com:laat/other.git")
	_, err := resolveRepo(ctx, cfg, known, "", "")
	if err == nil || !strings.HasSuffix(err.Error(), "has origin git@github.com:laat/other.git, which is not checked out on any host and not configured; use --repo (known: proj, notes)") {
		t.Fatalf("an unknown origin: %v", err)
	}
	// --repo by a host's label and by a discovered source.
	for _, flag := range []string{"notes", "https://github.com/laat/notes.git"} {
		if r, err := resolveRepo(ctx, cfg, known, flag, ""); err != nil || r.Source != notesSource {
			t.Errorf("--repo %s: %+v %v", flag, r, err)
		}
	}
}
