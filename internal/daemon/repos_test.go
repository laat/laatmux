package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/worktree"
)

// A host's repositories are every main checkout under its repos
// directory with an origin, the config's or not, in use or not, by the
// label the host gives it: in the snapshot once the first listing is
// done, none before, and in an upsert of their own when a listing
// changes the set, not when it does not.
func TestHostRepos(t *testing.T) {
	store, remote := newStore(t)
	base := filepath.Dir(store.Dirs.Repos[0])
	proj := filepath.Join(store.Dirs.Repos[0], "proj")
	sh(t, base, "git", "clone", "-q", remote, proj)
	d := New(Config{EnvironmentID: "env", Store: store})
	d.discoveredOnce.Do(func() { close(d.discovered) })
	snapshot := func() protocol.Message {
		t.Helper()
		pc := conn(t, d)
		if err := pc.Write(protocol.Message{Type: protocol.TypeSubscribe}); err != nil {
			t.Fatal(err)
		}
		m, err := pc.Read()
		if err != nil || m.Type != protocol.TypeSnapshot {
			t.Fatalf("snapshot %+v %v", m, err)
		}
		return m
	}
	if snap := snapshot(); snap.Repos != nil {
		t.Fatalf("repos before the first listing: %+v", snap.Repos)
	}
	if !slices.Contains(d.capabilities(), protocol.CapRepos) {
		t.Fatal("a daemon with a store without repos")
	}
	s := subscribed(t, d, false)
	ctx := context.Background()
	d.pollWorktrees(ctx)
	sets := func(ms []protocol.Message) []*protocol.RepoSet {
		var out []*protocol.RepoSet
		for _, m := range ms {
			if m.Repos != nil {
				out = append(out, m.Repos)
			}
		}
		return out
	}
	want := []protocol.Checkout{{Repo: "proj", Source: remote, Root: proj}}
	if got := sets(s.drain()); len(got) != 1 || !slices.Equal(got[0].Checkouts, want) {
		t.Fatalf("after the first listing: %+v", got)
	}
	if snap := snapshot(); snap.Repos == nil || !slices.Equal(snap.Repos.Checkouts, want) {
		t.Fatalf("snapshot: %+v", snap.Repos)
	}
	d.pollWorktrees(ctx)
	if got := sets(s.drain()); len(got) != 0 {
		t.Fatalf("a listing that changed nothing: %+v", got)
	}
	// A clone of another repository the config does not list, in no
	// use, named as git names it.
	other := filepath.Join(store.Dirs.Repos[0], "next.js")
	sh(t, base, "git", "clone", "-q", remote, other)
	sh(t, other, "git", "remote", "set-url", "origin", "git@github.com:vercel/next.js.git")
	d.pollWorktrees(ctx)
	got := sets(s.drain())
	want = append(want[:0:0], protocol.Checkout{Repo: "next_js", Source: "git@github.com:vercel/next.js.git", Root: other}, want[0])
	if len(got) != 1 || !slices.Equal(got[0].Checkouts, want) {
		t.Fatalf("after a clone: %+v", got)
	}
}

// A merging daemon carries each host's repositories on its record: the
// remote host's from its snapshot and its upserts, kept while it is
// down; its own on the local host's record, set from its listing, and
// never as an upsert of its own in the merged stream.
func TestMergedRepos(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMergedFixture(t, ctx, nil)
	main := func(label, src, root string) worktree.Record {
		return worktree.Record{Repo: label, Source: src, Root: root, Main: true}
	}
	set := func(rs ...worktree.Record) []protocol.Checkout {
		var out []protocol.Checkout
		for _, r := range rs {
			out = append(out, protocol.Checkout{Repo: r.Repo, Source: r.Source, Root: r.Root})
		}
		return out
	}
	notes := main("notes", "git@x:o/notes.git", "/r/notes")
	tools := main("tools", "git@x:o/tools.git", "/r/tools")
	mine := main("mine", "git@x:o/mine.git", "/l/mine")
	publishRepos := func(d *Daemon, rs ...worktree.Record) {
		d.mu.Lock()
		d.publishReposLocked(rs)
		d.mu.Unlock()
	}
	publishRepos(f.remote.d, notes)
	publishRepos(f.local, mine)
	has := func(name string, want []protocol.Checkout) func(protocol.Message) bool {
		return hostStatus(name, func(st protocol.HostStatus) bool {
			return st.Repos != nil && slices.Equal(st.Repos.Checkouts, want)
		})
	}
	c, pc, snap := f.subscribe(t, ctx)
	defer c.Close()
	if h, _ := findHost(snap.Hosts, "here"); h.Repos == nil || !slices.Equal(h.Repos.Checkouts, set(mine)) {
		t.Fatalf("the local host's record: %+v", h)
	}
	until(t, c, pc, has("vm", set(notes)))
	publishRepos(f.remote.d, notes, tools)
	until(t, c, pc, has("vm", set(notes, tools)))
	publishRepos(f.local, mine, notes)
	for _, m := range until(t, c, pc, has("here", set(mine, notes))) {
		if m.Repos != nil {
			t.Fatalf("the local repositories' own upsert in the merged stream: %+v", m)
		}
	}
	// Down: the record says so and keeps the set.
	f.remote.mu.Lock()
	f.remote.down = errors.New("ssh: no route")
	f.remote.mu.Unlock()
	f.remote.dropAll()
	until(t, c, pc, hostStatus("vm", func(st protocol.HostStatus) bool {
		return st.Error == "ssh: no route" && st.Repos != nil && slices.Equal(st.Repos.Checkouts, set(notes, tools))
	}))
}
