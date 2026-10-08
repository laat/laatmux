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
	"github.com/laat/laatmux/internal/worktree"
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
	base := filepath.Dir(store.Dirs.Repos[0])
	proj := filepath.Join(store.Dirs.Repos[0], "proj")
	other := filepath.Join(store.Dirs.Repos[0], "other")
	sh(t, base, "git", "clone", "-q", remote, proj)
	sh(t, base, "git", "clone", "-q", remote, other)
	// Another repository, which the config does not list.
	sh(t, other, "git", "remote", "set-url", "origin", filepath.Join(base, "other.git"))
	sh(t, other, "git", "checkout", "-q", "-b", "feature")
	mkdirs(t, filepath.Join(other, "sub"))
	// A third, not listed either, with a worktree under the worktrees
	// directory.
	linked := filepath.Join(store.Dirs.Repos[0], "linked")
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
	// Configured alone is not use: a config that lists every
	// repository would publish a line for each.
	if recordAt(ms, projID) >= 0 {
		t.Fatalf("a configured checkout with no worktree and no agent is published: %+v", ms)
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
	if err := os.WriteFile(filepath.Join(linked, "notes"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Two rounds: two workers, three records due.
	refresh(t, d)
	refresh(t, d)
	ms = with.drain()
	if i := recordAt(ms, "env/checkout/"+linked); i < 0 || ms[i].Worktree.Git == nil || !ms[i].Worktree.Git.Dirty || ms[i].Worktree.Git.Uncommitted != [2]int{2, 0} {
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
	// checkout with a worktree stays.
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
	_, kept := d.worktrees[linked]
	d.mu.Unlock()
	if !kept {
		t.Fatal("the record of the checkout with a worktree went with the other's")
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

// A root whose record changes kind, a worktree made where a main
// checkout was with the repos directory under the worktrees one, or the
// other way round: the new record goes out, then the agent in the root
// takes it, then the old id's remove, a worktree's with its listing
// stamp; the git object is not carried across. A shell's pane record
// naming the old id goes before it too.
func TestRecordChangesKind(t *testing.T) {
	d := New(Config{EnvironmentID: "env"})
	s := &subscriber{ch: make(chan protocol.Message, 64), checkouts: true}
	drain := func() []protocol.Message {
		var out []protocol.Message
		for {
			select {
			case m := <-s.ch:
				out = append(out, m)
			default:
				return out
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.subs[s] = struct{}{}
	d.listed = true
	const dir = "/w/repos/proj"
	main := worktree.Record{Repo: "proj", Branch: "main", Root: dir, Main: true, Configured: true}
	wt := worktree.Record{Repo: "proj", Branch: "main", Root: dir}
	checkoutID, worktreeID := "env/checkout/"+dir, "env/worktree/"+dir
	// A live agent on the default server in the root.
	key := "default/%1"
	d.panes[key] = &paneState{target: &target{Target: Target{Label: "default"}}, observed: true, path: dir + "/src", pane: tmux.Pane{ID: "%1"}}
	d.agents[key] = protocol.Agent{ID: "env/" + key, EnvironmentID: "env", Server: "default", PaneID: "%1", Agent: "claude", Liveness: protocol.Alive}
	// list publishes a listing and attributes the agent again, as
	// pollWorktrees does.
	list := func(recs, mains []worktree.Record) []protocol.Message {
		d.lastList, d.lastMains = recs, mains
		now := time.Now()
		d.publishWorktreesLocked(now)
		var paths []string
		var checkouts []root
		for _, r := range recs {
			paths = append(paths, r.Root)
		}
		for _, r := range mains {
			checkouts = append(checkouts, root{root: r.Root})
		}
		d.setRootsLocked(resolveRoots(paths, checkouts), now)
		return drain()
	}
	if ms := list(nil, []worktree.Record{main}); recordAt(ms, checkoutID) < 0 || agentAt(ms, "%1") < 0 || d.agents[key].WorktreeID != checkoutID {
		t.Fatalf("the main checkout with its agent: %+v", ms)
	}
	w := d.worktrees[dir]
	w.Git = &protocol.GitStatus{Base: "origin/main", Dirty: true}
	d.worktrees[dir] = w
	for _, c := range []struct {
		name         string
		recs, mains  []worktree.Record
		newID, oldID string
		stamped      bool
	}{
		{"to a worktree", []worktree.Record{wt}, nil, worktreeID, checkoutID, false},
		{"to a main checkout", nil, []worktree.Record{main}, checkoutID, worktreeID, true},
	} {
		ms := list(c.recs, c.mains)
		rec, agent, gone := recordAt(ms, c.newID), agentAt(ms, "%1"), removedAt(ms, c.oldID)
		if rec < 0 || agent < rec || gone < agent || ms[agent].Agent.WorktreeID != c.newID || (ms[gone].RemovedIn != nil) != c.stamped {
			t.Fatalf("%s: the new record, the agent, then the old id's remove: %+v", c.name, ms)
		}
		if ms[rec].Worktree.Git != nil {
			t.Fatalf("%s: the git object carried across: %+v", c.name, ms[rec].Worktree.Git)
		}
	}
	// A shell alone in the root, its agent gone: its pane record,
	// the worktree's, goes before the worktree's record does.
	delete(d.agents, key)
	delete(d.panes, key)
	shell := "default/%2"
	d.panes[shell] = &paneState{target: &target{Target: Target{Label: "default"}}, observed: true, bare: true, path: dir, pane: tmux.Pane{ID: "%2"}}
	if ms := list([]worktree.Record{wt}, nil); !slices.ContainsFunc(ms, func(m protocol.Message) bool { return m.Pane != nil && m.Pane.WorktreeID == worktreeID }) {
		t.Fatalf("the shell's pane record in the worktree: %+v", ms)
	}
	ms := list(nil, []worktree.Record{main})
	pane := slices.IndexFunc(ms, func(m protocol.Message) bool {
		return m.Type == protocol.TypeRemove && m.PaneRecordID == "env/pane/"+shell
	})
	if gone := removedAt(ms, worktreeID); pane < 0 || gone < pane {
		t.Fatalf("to a main checkout with a shell: its pane record's remove, then the worktree's: %+v", ms)
	}
}

// A checkout whose HEAD cannot be read, configured or not, has no record
// and fails no listing: the worktrees' changes still go out.
func TestMainCheckoutUnread(t *testing.T) {
	store, remote := newStore(t)
	base := filepath.Dir(store.Dirs.Repos[0])
	proj := filepath.Join(store.Dirs.Repos[0], "proj")
	sh(t, base, "git", "clone", "-q", remote, proj)
	// A listing first, which reads the origin as a running daemon has
	// it: git finds no repository whose HEAD it cannot open.
	d := New(Config{EnvironmentID: "env", Store: store})
	d.pollWorktrees(context.Background())
	head := filepath.Join(proj, ".git", "HEAD")
	if err := os.Chmod(head, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(head, 0o644) })
	if _, err := os.ReadFile(head); err == nil {
		t.Skip("HEAD readable without permission (root)")
	}
	d.pollWorktrees(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	if !slices.ContainsFunc(d.lastMains, func(r worktree.Record) bool { return r.Root == proj && r.Unread }) {
		t.Fatalf("the checkout not listed unread: %+v", d.lastMains)
	}
	if _, ok := d.worktrees[proj]; ok || d.listErr != "" || !d.listed {
		t.Fatalf("records %+v, listing error %q, listed %v", d.worktrees, d.listErr, d.listed)
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
	base := filepath.Dir(store.Dirs.Repos[0])
	other := filepath.Join(store.Dirs.Repos[0], "other")
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
