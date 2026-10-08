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
	laatmux, def := &fakeServer{}, &fakeServer{}
	start := time.Now().Add(-time.Hour)
	d := New(Config{
		EnvironmentID: "env", Store: store,
		Targets: []Target{{Label: "laatmux", Tmux: laatmux, Managed: true}, {Label: "default", Tmux: def}},
		Procs: &ttyProcs{ids: map[string]procs.Identity{
			"/dev/a1": {Agent: "claude", PID: 11, Start: start, Comm: "claude"},
			"/dev/a2": {Agent: "codex", PID: 12, Start: start, Comm: "codex"},
		}},
	})
	d.discoveredOnce.Do(func() { close(d.discovered) })
	with, without := subscribed(t, d, true), subscribed(t, d, false)
	ctx := context.Background()
	projID, otherID := "env/worktree/"+proj, "env/worktree/"+other

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
	if ms := without.drain(); slices.ContainsFunc(ms, func(m protocol.Message) bool { return m.Worktree != nil }) {
		t.Fatalf("a subscriber that did not ask got a record: %+v", ms)
	}

	// Its git status, read as a worktree's is.
	if err := os.WriteFile(filepath.Join(proj, "notes"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if w := ms[rec].Worktree; !w.Main || w.Branch != "feature" || w.Repo != "other" {
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
	ms = without.drain()
	if slices.ContainsFunc(ms, func(m protocol.Message) bool { return m.Worktree != nil }) || agentAt(ms, "%7") < 0 {
		t.Fatalf("a subscriber that did not ask: %+v", ms)
	}

	// The agent leaves: its upsert, then the record goes; the
	// configured checkout's stays.
	def.set(func() { def.panes[0].CurrentPath = "/" })
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	ms = with.drain()
	gone := slices.IndexFunc(ms, func(m protocol.Message) bool { return m.Type == protocol.TypeRemove && m.WorktreeID != "" })
	agent = agentAt(ms, "%7")
	if agent < 0 || ms[agent].Agent.WorktreeID != "" || gone < agent || ms[gone].WorktreeID != otherID || ms[gone].RemovedIn != nil {
		t.Fatalf("the agent left, then the record went: %+v", ms)
	}
	d.mu.Lock()
	_, kept := d.worktrees[proj]
	d.mu.Unlock()
	if !kept {
		t.Fatal("the configured checkout's record went with the other's")
	}
}
