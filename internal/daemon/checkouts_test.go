package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

// stream is a subscriber's connection, its messages collected.
type stream struct{ got chan protocol.Message }

// subscribed opens a connection to d and subscribes, asking for the main
// checkouts' records or not.
func subscribed(t *testing.T, d *Daemon, checkouts bool) *stream {
	t.Helper()
	pc := conn(t, d)
	if err := pc.Write(protocol.Message{Type: protocol.TypeSubscribe, Checkouts: checkouts}); err != nil {
		t.Fatal(err)
	}
	if m, err := pc.Read(); err != nil || m.Type != protocol.TypeSnapshot {
		t.Fatalf("snapshot %+v %v", m, err)
	}
	s := &stream{got: make(chan protocol.Message, 256)}
	go func() {
		for {
			m, err := pc.Read()
			if err != nil {
				return
			}
			s.got <- m
		}
	}()
	return s
}

// drain returns what the subscriber got until the stream is quiet.
func (s *stream) drain() []protocol.Message {
	var out []protocol.Message
	for {
		select {
		case m := <-s.got:
			out = append(out, m)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

// recordAt is the index of the last upsert of the worktree record with
// the id, -1 for none.
func recordAt(ms []protocol.Message, id string) int {
	at := -1
	for i, m := range ms {
		if m.Worktree != nil && m.Worktree.ID == id {
			at = i
		}
	}
	return at
}

// agentAt is the index of the last upsert of the agent in the pane, -1
// for none.
func agentAt(ms []protocol.Message, paneID string) int {
	at := -1
	for i, m := range ms {
		if m.Agent != nil && m.Agent.PaneID == paneID {
			at = i
		}
	}
	return at
}

// removedAt is the index of the remove of the record with the id, -1
// for none.
func removedAt(ms []protocol.Message, id string) int {
	return slices.IndexFunc(ms, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.WorktreeID == id })
}

func sh(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// A main checkout is published while it is in use: its repository in
// the config, from the first listing, with its branch and, as a
// worktree's, its git status; another checkout once an agent in a plain
// session on the default server is in it, its record before the agent
// naming it, and taken back after the agent has left. A managed agent
// in a checkout is not its, a shell there has no pane record, and a
// subscriber that did not ask for the records gets none.
func TestMainCheckoutRecords(t *testing.T) {
	store, remote := newStore(t)
	base := filepath.Dir(store.Dirs.Repos)
	proj := filepath.Join(store.Dirs.Repos, "proj")
	other := filepath.Join(store.Dirs.Repos, "other")
	sh(t, base, "git", "clone", "-q", remote, proj)
	sh(t, base, "git", "clone", "-q", remote, other)
	// Another repository, which the config does not list.
	sh(t, other, "git", "remote", "set-url", "origin", filepath.Join(base, "other.git"))
	sh(t, other, "git", "checkout", "-q", "-b", "feature")
	mkdirs(t, filepath.Join(other, "sub"))
	// A third, not listed either, with a worktree under the worktrees
	// directory.
	linked := filepath.Join(store.Dirs.Repos, "linked")
	sh(t, base, "git", "clone", "-q", remote, linked)
	sh(t, linked, "git", "remote", "set-url", "origin", filepath.Join(base, "linked.git"))
	sh(t, linked, "git", "worktree", "add", "-q", "-b", "x", filepath.Join(store.Dirs.Worktrees, "linked", "x"))
	laatmux, def := &fakeServer{}, &fakeServer{}
	start := time.Now().Add(-time.Hour)
	ids := &ttyProcs{ids: map[string]procs.Identity{
		"/dev/a1": {Agent: "claude", PID: 11, Start: start, Comm: "claude"},
		"/dev/a2": {Agent: "codex", PID: 12, Start: start, Comm: "codex"},
		"/dev/a3": {Agent: "claude", PID: 13, Start: start, Comm: "claude"},
	}}
	d := New(Config{
		EnvironmentID: "env", Store: store,
		Targets: []Target{{Label: "laatmux", Tmux: laatmux, Managed: true}, {Label: "default", Tmux: def}},
		Procs:   ids,
	})
	d.discoveredOnce.Do(func() { close(d.discovered) })
	with, without := subscribed(t, d, true), subscribed(t, d, false)
	ctx := context.Background()
	projID, otherID := "env/checkout/"+proj, "env/checkout/"+other

	d.pollWorktrees(ctx)
	ms := with.drain()
	if i := recordAt(ms, projID); i < 0 {
		t.Fatalf("the configured checkout is not published: %+v", ms)
	} else if w := ms[i].Worktree; !w.Main || w.Branch != "main" || w.Root != proj || w.Repo != "proj" || w.Session != "" {
		t.Fatalf("the configured checkout's record: %+v", w)
	}
	if recordAt(ms, otherID) >= 0 {
		t.Fatalf("a checkout in no use is published: %+v", ms)
	}
	if i := recordAt(ms, "env/checkout/"+linked); i < 0 || ms[i].Worktree.Branch != "main" {
		t.Fatalf("a checkout with a worktree is not published: %+v", ms)
	}
	if ms := without.drain(); slices.ContainsFunc(ms, func(m protocol.Message) bool { return m.Worktree != nil && m.Worktree.Main }) {
		t.Fatalf("a subscriber that did not ask got a main checkout: %+v", ms)
	}

	// Its git status, read as a worktree's is.
	if err := os.WriteFile(filepath.Join(proj, "notes"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Two rounds: two workers, three records due.
	refresh(t, d)
	refresh(t, d)
	ms = with.drain()
	if i := recordAt(ms, projID); i < 0 || ms[i].Worktree.Git == nil || !ms[i].Worktree.Git.Dirty || ms[i].Worktree.Git.Uncommitted != [2]int{2, 0} {
		t.Fatalf("no git status on the checkout's record: %+v", ms)
	}
	without.drain()

	laatmux.set(func() {
		laatmux.panes = []tmux.Pane{{Session: "notes", ID: "%1", TTY: "/dev/a2", Managed: true, Cwd: other, CurrentPath: other}}
	})
	def.set(func() {
		def.panes = []tmux.Pane{
			{Session: "work", ID: "%7", TTY: "/dev/a1", CurrentPath: filepath.Join(other, "sub")},
			{Session: "work", ID: "%8", TTY: "/dev/s1", CurrentPath: proj},
		}
	})
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	ms = with.drain()
	rec, agent := recordAt(ms, otherID), agentAt(ms, "%7")
	if rec < 0 || agent < 0 || rec > agent {
		t.Fatalf("the record of the checkout the agent is in, then the agent: %+v", ms)
	}
	// The managed pane at the root would make notes its home session;
	// a main checkout has none.
	if w := ms[rec].Worktree; !w.Main || w.Branch != "feature" || w.Repo != "other" || w.Session != "" {
		t.Fatalf("the checkout's record: %+v", w)
	}
	if a := ms[agent].Agent; a.WorktreeID != otherID {
		t.Fatalf("the agent in the checkout: %+v", a)
	}
	if i := agentAt(ms, "%1"); i < 0 || ms[i].Agent.WorktreeID != "" {
		t.Fatalf("a managed agent in the checkout: %+v", ms)
	}
	if slices.ContainsFunc(ms, func(m protocol.Message) bool { return m.Pane != nil }) {
		t.Fatalf("a shell in a checkout has a pane record: %+v", ms)
	}
	// A subscriber that did not ask gets the agent as one of no
	// worktree, as before, in upserts and in a snapshot.
	ms = without.drain()
	if slices.ContainsFunc(ms, func(m protocol.Message) bool { return m.Worktree != nil && m.Worktree.Main }) {
		t.Fatalf("a subscriber that did not ask got a main checkout: %+v", ms)
	}
	if i := agentAt(ms, "%7"); i < 0 || ms[i].Agent.WorktreeID != "" {
		t.Fatalf("a subscriber that did not ask got the attribution: %+v", ms)
	}
	s, snap := d.subscribe(nil, false)
	d.unsubscribe(s)
	if i := slices.IndexFunc(snap.Agents, func(a protocol.Agent) bool { return a.PaneID == "%7" }); i < 0 || snap.Agents[i].WorktreeID != "" {
		t.Fatalf("a snapshot that did not ask has the attribution: %+v", snap.Agents)
	}

	// The agent leaves: its upsert, then the record goes; the
	// configured checkout's stays.
	def.set(func() { def.panes[0].CurrentPath = "/" })
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	ms = with.drain()
	gone, agent := removedAt(ms, otherID), agentAt(ms, "%7")
	if agent < 0 || ms[agent].Agent.WorktreeID != "" || gone < agent || ms[gone].RemovedIn != nil {
		t.Fatalf("the agent left, then the record went: %+v", ms)
	}
	d.mu.Lock()
	_, kept := d.worktrees[proj]
	d.mu.Unlock()
	if !kept {
		t.Fatal("the configured checkout's record went with the other's")
	}

	// Back in it, then the agent exits with its shell left in the pane:
	// a gone agent keeps no checkout in use.
	def.set(func() { def.panes[0].CurrentPath = other })
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if ms := with.drain(); recordAt(ms, otherID) < 0 {
		t.Fatalf("back in the checkout: %+v", ms)
	}
	ids.mu.Lock()
	delete(ids.ids, "/dev/a1")
	ids.mu.Unlock()
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	ms = with.drain()
	gone, agent = removedAt(ms, otherID), agentAt(ms, "%7")
	if agent < 0 || ms[agent].Agent.Liveness != protocol.Gone || ms[agent].Agent.WorktreeID != "" || gone < agent {
		t.Fatalf("the agent gone, then the record: %+v", ms)
	}

	// Another agent in it, then the checkout moves out of the repos
	// directory: the agent's attribution goes before the record does.
	def.set(func() {
		def.panes = append(def.panes, tmux.Pane{Session: "more", ID: "%9", TTY: "/dev/a3", CurrentPath: filepath.Join(other, "sub")})
	})
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if ms := with.drain(); recordAt(ms, otherID) < 0 {
		t.Fatalf("a second agent in the checkout: %+v", ms)
	}
	if err := os.Rename(other, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	d.pollWorktrees(ctx)
	ms = with.drain()
	gone, agent = removedAt(ms, otherID), agentAt(ms, "%9")
	if agent < 0 || ms[agent].Agent.WorktreeID != "" || gone < agent {
		t.Fatalf("the checkout moved: the agent, then the record: %+v", ms)
	}
}

// The order around a main checkout's record on the other paths that
// change an agent: an agent seen before the first listing, as at every
// start, is attributed by that listing after the record is published; a
// listing that drops the checkout takes the agent's attribution back
// before the record; a pane that closes is removed before the record.
// A shell, an identified pane with no named agent, puts no checkout in
// use.
func TestMainCheckoutOrdering(t *testing.T) {
	store, remote := newStore(t)
	base := filepath.Dir(store.Dirs.Repos)
	other := filepath.Join(store.Dirs.Repos, "other")
	sh(t, base, "git", "clone", "-q", remote, other)
	sh(t, other, "git", "remote", "set-url", "origin", filepath.Join(base, "other.git"))
	laatmux, def := &fakeServer{}, &fakeServer{}
	start := time.Now().Add(-time.Hour)
	d := New(Config{
		EnvironmentID: "env", Store: store,
		Targets: []Target{{Label: "laatmux", Tmux: laatmux, Managed: true}, {Label: "default", Tmux: def}},
		Procs: &ttyProcs{ids: map[string]procs.Identity{
			"/dev/a1": {Agent: "claude", PID: 11, Start: start, Comm: "claude"},
			"/dev/s1": {PID: 12, Start: start, Comm: "zsh"},
		}},
	})
	d.discoveredOnce.Do(func() { close(d.discovered) })
	with := subscribed(t, d, true)
	ctx := context.Background()
	otherID := "env/checkout/" + other

	def.set(func() {
		def.panes = []tmux.Pane{
			{Session: "work", ID: "%7", TTY: "/dev/a1", CurrentPath: other},
			{Session: "work", ID: "%8", TTY: "/dev/s1", CurrentPath: other},
		}
	})
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	with.drain()
	d.pollWorktrees(ctx)
	ms := with.drain()
	rec, agent := recordAt(ms, otherID), agentAt(ms, "%7")
	if rec < 0 || agent < 0 || rec > agent || ms[agent].Agent.WorktreeID != otherID {
		t.Fatalf("the first listing: the record, then the agent: %+v", ms)
	}
	if i := agentAt(ms, "%8"); i >= 0 && ms[i].Agent.WorktreeID != "" {
		t.Fatalf("a shell attributed to the checkout: %+v", ms[i].Agent)
	}

	// Its origin removed, the listing drops the checkout.
	sh(t, other, "git", "remote", "remove", "origin")
	d.pollWorktrees(ctx)
	ms = with.drain()
	gone, agent := removedAt(ms, otherID), agentAt(ms, "%7")
	if agent < 0 || ms[agent].Agent.WorktreeID != "" || gone < agent {
		t.Fatalf("the listing drops it: the agent, then the record: %+v", ms)
	}

	sh(t, other, "git", "remote", "add", "origin", filepath.Join(base, "other.git"))
	d.pollWorktrees(ctx)
	if ms := with.drain(); recordAt(ms, otherID) < 0 {
		t.Fatalf("listed again: %+v", ms)
	}
	def.set(func() { def.panes = def.panes[1:] })
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	ms = with.drain()
	gone = removedAt(ms, otherID)
	agent = slices.IndexFunc(ms, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.AgentID == "env/default/%7" })
	if agent < 0 || gone < agent {
		t.Fatalf("the pane closes: the agent, then the record: %+v", ms)
	}
	d.mu.Lock()
	_, kept := d.worktrees[other]
	d.mu.Unlock()
	if kept {
		t.Fatal("the record is kept with no agent in the checkout")
	}
}
