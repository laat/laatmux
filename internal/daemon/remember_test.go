package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/worktree"
)

// appends records the relay's appends to the config, answering err.
type appends struct {
	mu  sync.Mutex
	got [][2]string
	err error
}

func (a *appends) add(src, name string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, [2]string{src, name})
	return a.err == nil, a.err
}

func (a *appends) calls() [][2]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][2]string(nil), a.got...)
}

// rememberDaemon is a laptop daemon of the fixture's on its own pending
// directory, appending to the config through a.
func rememberDaemon(f *relayFixture, dir string, a *appends) *Daemon {
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: f.remote.dial, Pending: dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond, Timings: testTimings,
		AppendRepo: a.add,
	})
	discovered(local)
	go local.Run(f.ctx)
	f.setLocal(local)
	return local
}

// A relayed add with remember has its repository entry appended to the
// config once the host's add has succeeded, and the ask cleared; an add
// the host refuses appends nothing. A daemon without AppendRepo has no
// remember capability and refuses such an add, and so does one with
// it for an add without an entry.
func TestRelayRemember(t *testing.T) {
	f := newRelayFixture(t, nil)
	other := filepath.Join(t.TempDir(), "other.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", f.source(), other).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	entry := &protocol.RepoEntry{Source: other, Name: "sent"}
	add := protocol.Message{Type: protocol.TypeAdd, ID: "rem", Relay: "vm", Repo: other, Name: "sent", Branch: "task",
		RepoEntry: entry, Remember: true, AgentName: "argv", SubmittedAt: time.Now()}
	if protocol.Has(f.local.capabilities(), protocol.CapRemember) {
		t.Fatal("remember without AppendRepo")
	}
	if res := f.request(t, add); res.OK || res.Error != "this daemon cannot add a repository to the config" {
		t.Fatalf("without AppendRepo: %+v", res)
	}

	a := &appends{}
	dir := t.TempDir()
	local := rememberDaemon(f, dir, a)
	if !protocol.Has(local.capabilities(), protocol.CapRemember) {
		t.Fatalf("capabilities %v", local.capabilities())
	}
	noEntry := add
	noEntry.ID, noEntry.RepoEntry, noEntry.Repo = "bare", nil, f.source()
	if res := f.request(t, noEntry); res.OK || res.Error != "remember needs the add's repository entry" {
		t.Fatalf("without an entry: %+v", res)
	}
	if res := f.request(t, add); !res.OK {
		t.Fatalf("accept: %+v", res)
	}
	if p := readPending(t, dir, "rem"); !p.Remember {
		t.Fatalf("file after accept: %+v", p)
	}
	p := f.awaitRecord(t, "rem", 30*time.Second, func(p pendingFile) bool { return p.Done && !p.Remember })
	if !p.OK {
		t.Fatalf("record %+v", p)
	}
	if got := a.calls(); len(got) != 1 || got[0] != [2]string{other, "sent"} {
		t.Fatalf("appends %v", got)
	}
	if p := readPending(t, dir, "rem"); p.Remember {
		t.Fatalf("the ask is still in the file: %+v", p)
	}

	// The host cannot clone it: nothing is appended.
	missing := filepath.Join(t.TempDir(), "missing.git")
	bad := add
	bad.ID, bad.Repo, bad.Name, bad.RepoEntry = "bad", missing, "missing", &protocol.RepoEntry{Source: missing, Name: "missing"}
	if res := f.request(t, bad); !res.OK {
		t.Fatalf("accept: %+v", res)
	}
	p = f.awaitRecord(t, "bad", 30*time.Second, func(p pendingFile) bool { return p.Done })
	if p.OK || !strings.Contains(p.Error, "failed at clone") {
		t.Fatalf("record %+v", p)
	}
	time.Sleep(100 * time.Millisecond)
	if got := a.calls(); len(got) != 1 {
		t.Fatalf("a refused add appended: %v", got)
	}
}

// A successful add whose append the last daemon did not get to, or
// could not make, is appended at start: the ask is in the file until
// an append succeeds.
func TestRelayRememberAtStart(t *testing.T) {
	f := newRelayFixture(t, nil)
	dir := t.TempDir()
	p := pendingFile{Pending: protocol.Pending{ID: "old", Host: "vm", EnvironmentID: "henv", Source: "/r/new.git", Repo: "new", Branch: "b", Agent: "argv",
		Sent: true, Taken: true, Done: true, OK: true, Root: "/w/new/b", Prompt: protocol.DeliveryNone, SubmittedAt: time.Now(), UpdatedAt: time.Now()},
		RepoEntry: &protocol.RepoEntry{Source: "/r/new.git", Name: "new"}, Remember: true}
	b, _ := json.Marshal(p)
	if err := os.WriteFile(filepath.Join(dir, FileName("old")), b, 0o600); err != nil {
		t.Fatal(err)
	}
	failing := &appends{err: errors.New("config.yaml: yaml: bad")}
	rememberDaemon(f, dir, failing)
	for deadline := time.Now().Add(10 * time.Second); len(failing.calls()) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("no append at start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := readPending(t, dir, "old"); !got.Remember {
		t.Fatalf("a failed append cleared the ask: %+v", got)
	}
	a := &appends{}
	rememberDaemon(f, dir, a)
	f.awaitRecord(t, "old", 10*time.Second, func(p pendingFile) bool { return !p.Remember })
	if got := a.calls(); len(got) != 1 || got[0] != [2]string{"/r/new.git", "new"} {
		t.Fatalf("appends %v", got)
	}
}

// The store follows the config's repositories: a poll that finds the
// file changed gives the store the new list, and the listing labels a
// checkout by it; an unchanged file keeps the list, and one that does
// not read keeps it too, logged once.
func TestPollReadsRepos(t *testing.T) {
	store, remote := newStore(t)
	ctx := context.Background()
	other := filepath.Join(t.TempDir(), "other.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", remote, other).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	// A worktree of a repository the config does not list, as an add
	// with an entry makes one: labelled by its checkout's directory.
	if _, err := store.Add(ctx, worktree.Repo{Source: other, Name: "sent"}, "task", nil); err != nil {
		t.Fatal(err)
	}
	type read struct {
		repos   []worktree.Repo
		changed bool
		err     error
	}
	var mu sync.Mutex
	reads := []read{
		{nil, false, nil},
		{[]worktree.Repo{{Source: remote, Name: "proj"}, {Source: other, Name: "listed"}}, true, nil},
		{nil, false, nil},
		{nil, false, errors.New("yaml: bad")},
		{nil, false, errors.New("yaml: bad")},
	}
	var logged strings.Builder
	d := New(Config{EnvironmentID: "env", Store: store, Logger: log.New(&logged, "", 0), Repos: func() ([]worktree.Repo, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		r := reads[0]
		reads = reads[1:]
		return r.repos, r.changed, r.err
	}})
	label := func() string {
		for _, w := range d.worktreeRecords() {
			if w.Source == other {
				return w.Repo
			}
		}
		return ""
	}
	d.pollWorktrees(ctx)
	if label() != "sent" || len(store.Repos()) != 1 {
		t.Fatalf("before: %q %v", label(), store.Repos())
	}
	for i, want := range []string{"listed", "listed", "listed", "listed"} {
		d.pollWorktrees(ctx)
		if label() != want || len(store.Repos()) != 2 {
			t.Fatalf("poll %d: %q %v", i, label(), store.Repos())
		}
	}
	if n := strings.Count(logged.String(), "worktrees: config: yaml: bad; the repositories stay as they were"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, logged.String())
	}
}
