package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
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

func (a *appends) fail(err error) {
	a.mu.Lock()
	a.err = err
	a.mu.Unlock()
}

// An append that fails holds the task where its row says why, not
// handed over, and the user sees it as a task that needs them; a change
// of the config file has the relay try again, and the task then hands
// over as any other, and so does a record that handed over before with
// its append still asked for. The laptop's daemon here has no store,
// its own entry no directories: it watches the file all the same.
func TestRelayRememberOnConfigChange(t *testing.T) {
	f := newRelayFixture(t, nil)
	other := filepath.Join(t.TempDir(), "other.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", f.source(), other).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	var mu sync.Mutex
	changed := false
	a := &appends{err: errors.New("config.yaml: yaml: bad")}
	dir := t.TempDir()
	ret := pendingFile{Pending: protocol.Pending{ID: "ret", Host: "vm", EnvironmentID: "henv", Source: "/r/two.git", Repo: "two", Branch: "b", Agent: "argv",
		Sent: true, Taken: true, Done: true, OK: true, Listed: true, Root: "/w/two/b", Prompt: protocol.DeliveryNone, SubmittedAt: time.Now(), UpdatedAt: time.Now()},
		RepoEntry: &protocol.RepoEntry{Source: "/r/two.git", Name: "two"}, Remember: true, ReplacedBy: "henv/worktree//w/two/b", RetiredAt: time.Now()}
	b, _ := json.Marshal(ret)
	if err := os.WriteFile(filepath.Join(dir, FileName("ret")), b, 0o600); err != nil {
		t.Fatal(err)
	}
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: f.remote.dial, Pending: dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond, Timings: testTimings,
		AppendRepo: a.add, WorktreeInterval: 30 * time.Millisecond,
		Reread: func() (ConfigRead, bool, error) {
			mu.Lock()
			defer mu.Unlock()
			c := changed
			changed = false
			return ConfigRead{}, c, nil
		},
	})
	discovered(local)
	go local.Run(f.ctx)
	f.setLocal(local)
	if res := f.request(t, protocol.Message{Type: protocol.TypeAdd, ID: "rem", Relay: "vm", Repo: other, Name: "sent", Branch: "task",
		RepoEntry: &protocol.RepoEntry{Source: other, Name: "sent"}, Remember: true, AgentName: "argv", SubmittedAt: time.Now()}); !res.OK {
		t.Fatalf("accept: %+v", res)
	}
	p := f.awaitRecord(t, "rem", 30*time.Second, func(p pendingFile) bool { return p.RememberError != "" })
	if !p.OK || !p.Remember || p.RememberError != "config.yaml: yaml: bad" {
		t.Fatalf("record %+v", p)
	}
	if state, detail := rows.PendingState(p.Pending, false); state != "done, not added to the config" || detail != "config.yaml: yaml: bad" {
		t.Fatalf("state %q %q", state, detail)
	}
	if r := (rows.Row{Pending: &p.Pending}); !r.NeedsUser() {
		t.Fatal("a failed append does not need the user")
	}
	time.Sleep(300 * time.Millisecond)
	if p, _ := local.relay.get("rem"); p.retired() || p.Listed {
		t.Fatalf("handed over with the append failed: %+v", p)
	}
	n := len(a.calls())
	a.fail(nil)
	mu.Lock()
	changed = true
	mu.Unlock()
	p = f.awaitRecord(t, "rem", 30*time.Second, func(p pendingFile) bool { return p.retired() })
	if p.Remember || p.RememberError != "" {
		t.Fatalf("record %+v", p)
	}
	for deadline := time.Now().Add(10 * time.Second); readPending(t, dir, "ret").Remember; {
		if time.Now().After(deadline) {
			t.Fatal("the retired record's ask was not retried on the change")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := a.calls()[n:]
	if len(got) != 2 || !slices.Contains(got, [2]string{other, "sent"}) || !slices.Contains(got, [2]string{"/r/two.git", "two"}) {
		t.Fatalf("appends after the change %v", got)
	}
}

// The config's first look is made before the relay resumes a record
// asking for its append: a repair of the file after the resumed append
// read it is then a change the watch sees, never the first look it
// passes over.
func TestConfigReadBeforeResume(t *testing.T) {
	f := newRelayFixture(t, nil)
	dir := t.TempDir()
	p := pendingFile{Pending: protocol.Pending{ID: "old", Host: "vm", EnvironmentID: "henv", Source: "/r/new.git", Repo: "new", Branch: "b", Agent: "argv",
		Sent: true, Taken: true, Done: true, OK: true, Root: "/w/new/b", Prompt: protocol.DeliveryNone, SubmittedAt: time.Now(), UpdatedAt: time.Now()},
		RepoEntry: &protocol.RepoEntry{Source: "/r/new.git", Name: "new"}, Remember: true}
	b, _ := json.Marshal(p)
	if err := os.WriteFile(filepath.Join(dir, FileName("old")), b, 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []string
	note := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: f.remote.dial, Pending: dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond, Timings: testTimings,
		AppendRepo: func(string, string) (bool, error) { note("append"); return true, nil },
		Reread:     func() (ConfigRead, bool, error) { note("read"); return ConfigRead{}, false, nil },
	})
	discovered(local)
	go local.Run(f.ctx)
	f.setLocal(local)
	f.awaitRecord(t, "old", 10*time.Second, func(p pendingFile) bool { return !p.Remember })
	mu.Lock()
	defer mu.Unlock()
	if len(events) < 2 || events[0] != "read" {
		t.Fatalf("events %v", events)
	}
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
	// The ask is cleared in memory first, then in the file.
	for deadline := time.Now().Add(10 * time.Second); readPending(t, dir, "rem").Remember; {
		if time.Now().After(deadline) {
			t.Fatalf("the ask is still in the file: %+v", readPending(t, dir, "rem"))
		}
		time.Sleep(20 * time.Millisecond)
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
// an append succeeds, a record that has handed over to its worktree
// since included.
func TestRelayRememberAtStart(t *testing.T) {
	f := newRelayFixture(t, nil)
	dir := t.TempDir()
	write := func(p pendingFile) {
		t.Helper()
		b, _ := json.Marshal(p)
		if err := os.WriteFile(filepath.Join(dir, FileName(p.ID)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(pendingFile{Pending: protocol.Pending{ID: "old", Host: "vm", EnvironmentID: "henv", Source: "/r/new.git", Repo: "new", Branch: "b", Agent: "argv",
		Sent: true, Taken: true, Done: true, OK: true, Root: "/w/new/b", Prompt: protocol.DeliveryNone, SubmittedAt: time.Now(), UpdatedAt: time.Now()},
		RepoEntry: &protocol.RepoEntry{Source: "/r/new.git", Name: "new"}, Remember: true})
	write(pendingFile{Pending: protocol.Pending{ID: "ret", Host: "vm", EnvironmentID: "henv", Source: "/r/two.git", Repo: "two", Branch: "b", Agent: "argv",
		Sent: true, Taken: true, Done: true, OK: true, Listed: true, Root: "/w/two/b", Prompt: protocol.DeliveryNone, SubmittedAt: time.Now(), UpdatedAt: time.Now()},
		RepoEntry: &protocol.RepoEntry{Source: "/r/two.git", Name: "two"}, Remember: true, ReplacedBy: "henv/worktree//w/two/b", RetiredAt: time.Now()})
	failing := &appends{err: errors.New("config.yaml: yaml: bad")}
	rememberDaemon(f, dir, failing)
	for deadline := time.Now().Add(10 * time.Second); len(failing.calls()) < 2; {
		if time.Now().After(deadline) {
			t.Fatalf("appends at start: %v", failing.calls())
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, id := range []string{"old", "ret"} {
		if got := readPending(t, dir, id); !got.Remember {
			t.Fatalf("a failed append cleared the ask: %+v", got)
		}
	}
	a := &appends{}
	rememberDaemon(f, dir, a)
	f.awaitRecord(t, "old", 10*time.Second, func(p pendingFile) bool { return !p.Remember })
	for deadline := time.Now().Add(10 * time.Second); readPending(t, dir, "ret").Remember; {
		if time.Now().After(deadline) {
			t.Fatal("the retired record's ask was not cleared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := a.calls()
	if len(got) != 2 || !slices.Contains(got, [2]string{"/r/new.git", "new"}) || !slices.Contains(got, [2]string{"/r/two.git", "two"}) {
		t.Fatalf("appends %v", got)
	}
}

// lockedLog is a daemon's log as a test reads it while the daemon's
// goroutines write.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Dismissing a task whose append to the config failed drops the append
// for good: the result's detail names the source, its name and why the
// append failed, the log has it once, and neither an append after the
// dismiss nor a change of the config file appends anything. A task that
// handed over with its append still asked for says so too, and so does
// one whose host is gone from the config; a dismiss of a task that asked
// for no append, or whose add failed, says nothing more. Appends and
// dismisses of one record take turns.
func TestDismissDropsAppend(t *testing.T) {
	f := newRelayFixture(t, nil)
	dir := t.TempDir()
	write := func(p pendingFile) {
		t.Helper()
		b, _ := json.Marshal(p)
		if err := os.WriteFile(filepath.Join(dir, FileName(p.ID)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	done := protocol.Pending{Host: "vm", EnvironmentID: "henv", Repo: "new", Branch: "b", Agent: "argv",
		Sent: true, Taken: true, Done: true, OK: true, Root: "/w/new/b", Prompt: protocol.DeliveryNone, SubmittedAt: time.Now(), UpdatedAt: time.Now()}
	held := done
	held.ID, held.Source = "held", "git@x:nrkno/scripts.git"
	write(pendingFile{Pending: held, RepoEntry: &protocol.RepoEntry{Source: held.Source, Name: "nrkno-scripts"}, Remember: true})
	ret := done
	ret.ID, ret.Source, ret.Listed = "ret", "/r/two.git", true
	write(pendingFile{Pending: ret, RepoEntry: &protocol.RepoEntry{Source: "/r/two.git", Name: "two"}, Remember: true,
		ReplacedBy: "henv/worktree//w/two/b", RetiredAt: time.Now()})
	plain := done
	plain.ID, plain.Source, plain.Prompt, plain.Error = "plain", "/r/new.git", protocol.DeliveryNotDelivered, "not ready"
	write(pendingFile{Pending: plain, RepoEntry: &protocol.RepoEntry{Source: "/r/new.git", Name: "new"}})
	// One on a host gone from the config, and an add that failed, which
	// has nothing to append.
	away := done
	away.ID, away.Host, away.Source = "away", "box", "/r/away.git"
	write(pendingFile{Pending: away, RepoEntry: &protocol.RepoEntry{Source: "/r/away.git", Name: "away"}, Remember: true})
	failed := done
	failed.ID, failed.Source, failed.OK, failed.Stage, failed.Error = "failed", "/r/failed.git", false, protocol.StageClone, "failed at clone: no such repository"
	write(pendingFile{Pending: failed, RepoEntry: &protocol.RepoEntry{Source: "/r/failed.git", Name: "failed"}, Remember: true})
	a := &appends{err: errors.New("config.yaml: yaml: bad")}
	var mu sync.Mutex
	changed := false
	logged := &lockedLog{}
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: f.remote.dial, Pending: dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond, Timings: testTimings,
		AppendRepo: a.add, WorktreeInterval: 30 * time.Millisecond, Logger: log.New(logged, "", 0),
		Reread: func() (ConfigRead, bool, error) {
			mu.Lock()
			defer mu.Unlock()
			c := changed
			changed = false
			return ConfigRead{}, c, nil
		},
	})
	discovered(local)
	go local.Run(f.ctx)
	f.setLocal(local)
	for _, id := range []string{"held", "ret", "away"} {
		f.awaitRecord(t, id, 10*time.Second, func(p pendingFile) bool { return p.RememberError != "" })
	}
	// waits runs do under the record's remember lock, held as an append
	// under way holds it: do must not end until the lock is let go.
	waits := func(id string, do func()) {
		t.Helper()
		rl := local.relay.rememberLock(id)
		rl.Lock()
		ended := make(chan struct{})
		go func() { do(); close(ended) }()
		select {
		case <-ended:
			rl.Unlock()
			t.Fatalf("%s: done with the remember lock held", id)
		case <-time.After(100 * time.Millisecond):
		}
		rl.Unlock()
		<-ended
	}
	// An append waits for another under way, or a dismiss.
	n := len(a.calls())
	waits("held", func() { local.remember(f.ctx, "held") })
	if len(a.calls()) != n+1 {
		t.Fatalf("appends after the lock: %v", a.calls()[n:])
	}

	res := local.dismiss("held")
	want := "the append of git@x:nrkno/scripts.git to the config's repos as nrkno-scripts is dropped (config.yaml: yaml: bad); add it to the config by hand, or paste the source again"
	if !res.OK || res.Detail != want {
		t.Fatalf("dismiss: %+v", res)
	}
	line := "relay held: dismissed; the append of git@x:nrkno/scripts.git to the config's repos as nrkno-scripts is dropped (config.yaml: yaml: bad)"
	if res := local.dismiss("held"); res.OK {
		t.Fatalf("a second dismiss: %+v", res)
	}
	if n := strings.Count(logged.String(), line); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, logged.String())
	}
	// Every dismiss waits for an append under way, the handed-over and
	// the host-gone paths' too.
	waits("ret", func() { res = local.dismiss("ret") })
	if !res.OK || res.Detail != "the append of /r/two.git to the config's repos as two is dropped (config.yaml: yaml: bad); add it to the config by hand, or paste the source again" {
		t.Fatalf("dismiss of a handed-over task: %+v", res)
	}
	waits("away", func() { res = local.dismiss("away") })
	if !res.OK || res.Detail != "the append of /r/away.git to the config's repos as away is dropped (config.yaml: yaml: bad); add it to the config by hand, or paste the source again" {
		t.Fatalf("dismiss of a task whose host is gone: %+v", res)
	}
	for _, line := range []string{
		"relay ret: dismissed; the append of /r/two.git to the config's repos as two is dropped (config.yaml: yaml: bad)\n",
		"relay away: dismissed; the append of /r/away.git to the config's repos as away is dropped (config.yaml: yaml: bad)\n",
	} {
		if n := strings.Count(logged.String(), line); n != 1 {
			t.Fatalf("%q logged %d times:\n%s", line, n, logged.String())
		}
	}
	waits("plain", func() { res = local.dismiss("plain") })
	if !res.OK || res.Detail != "" {
		t.Fatalf("dismiss of a task without an append: %+v", res)
	}
	if res := local.dismiss("failed"); !res.OK || res.Detail != "" {
		t.Fatalf("dismiss of a failed add: %+v", res)
	}
	n = len(a.calls())
	// An append that comes after the dismiss finds no record and is not
	// made.
	a.fail(nil)
	if local.remember(f.ctx, "held") || len(a.calls()) != n {
		t.Fatalf("appended after the dismiss: %v", a.calls()[n:])
	}
	mu.Lock()
	changed = true
	mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	if got := a.calls()[n:]; len(got) != 0 {
		t.Fatalf("appended after the dismiss: %v", got)
	}
}

// A task that handed over with its append still asked for keeps asking
// until a dismiss: the sweep leaves it past the retention with its host
// gone, and one dropped with its worktree, by a listing that found the
// worktree removed or by rm, has the dropped append in the log once,
// with what dropped it and why the append failed.
func TestRetiredDropLogsAppend(t *testing.T) {
	f := newRelayFixture(t, nil)
	dir := t.TempDir()
	barrier := &protocol.Listing{Generation: 1, Revision: 5}
	for _, id := range []string{"gone", "rmd", "old"} {
		p := protocol.Pending{ID: id, Host: "vm", EnvironmentID: "henv", Source: "/r/" + id + ".git", Repo: id, Branch: "b", Agent: "argv", Root: "/w/" + id + "/b",
			Sent: true, Taken: true, Done: true, OK: true, Listed: true, Prompt: protocol.DeliveryNone, SubmittedAt: time.Now(), UpdatedAt: time.Now()}
		retiredAt := time.Now()
		if id == "old" {
			retiredAt = retiredAt.Add(-2 * handoffRetention)
		}
		b, _ := json.Marshal(pendingFile{Pending: p, Barrier: barrier, RepoEntry: &protocol.RepoEntry{Source: p.Source, Name: id}, Remember: true,
			ReplacedBy: "henv/worktree/" + p.Root, RetiredAt: retiredAt})
		if err := os.WriteFile(filepath.Join(dir, FileName(id)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := &appends{err: errors.New("config.yaml: yaml: bad")}
	logged := &lockedLog{}
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: f.remote.dial, Pending: dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond, Timings: testTimings,
		AppendRepo: a.add, Logger: log.New(logged, "", 0),
	})
	discovered(local)
	go local.Run(f.ctx)
	f.setLocal(local)
	for _, id := range []string{"gone", "rmd", "old"} {
		f.awaitRecord(t, id, 10*time.Second, func(p pendingFile) bool { return p.RememberError != "" })
	}
	at := protocol.Listing{Generation: 1, Revision: 6}
	local.dropRetiredAt("henv/worktree//w/gone/b", at)
	if res := local.dismissAt("r1", "henv", "/w/rmd/b", &at); !res.OK {
		t.Fatalf("dismiss at the root: %+v", res)
	}
	f.hosts.set()
	local.sweepRelay(time.Now())
	for id, want := range map[string]bool{"gone": false, "rmd": false, "old": true} {
		if _, ok := local.relay.get(id); ok != want {
			t.Errorf("%s kept %v", id, ok)
		}
	}
	for _, line := range []string{
		"relay gone: its worktree is removed; the append of /r/gone.git to the config's repos as gone is dropped (config.yaml: yaml: bad)\n",
		"relay rmd: dismissed by rm of its worktree; the append of /r/rmd.git to the config's repos as rmd is dropped (config.yaml: yaml: bad)\n",
	} {
		if n := strings.Count(logged.String(), line); n != 1 {
			t.Errorf("%q logged %d times:\n%s", line, n, logged.String())
		}
	}
}

// An append made while the pending file cannot be written, a full disk
// say, is on the record at once, and the write's retries do not hold a
// dismiss: the dismiss answers, here with the removal's own failure,
// and says nothing dropped, since the append was made.
func TestDismissNotHeldByAWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only directory")
	}
	f := newRelayFixture(t, nil)
	dir := t.TempDir()
	held := pendingFile{Pending: protocol.Pending{ID: "held", Host: "vm", EnvironmentID: "henv", Source: "/r/new.git", Repo: "new", Branch: "b", Agent: "argv",
		Sent: true, Taken: true, Done: true, OK: true, Root: "/w/new/b", Prompt: protocol.DeliveryNone, SubmittedAt: time.Now(), UpdatedAt: time.Now()},
		RepoEntry: &protocol.RepoEntry{Source: "/r/new.git", Name: "new"}, Remember: true}
	b, _ := json.Marshal(held)
	if err := os.WriteFile(filepath.Join(dir, FileName("held")), b, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &appends{err: errors.New("config.yaml: yaml: bad")}
	var mu sync.Mutex
	changed := false
	local := New(Config{
		EnvironmentID: "lenv", Version: "local", Hosts: f.hosts.get, Dial: f.remote.dial, Pending: dir,
		MergedIdle: 200 * time.Millisecond, ReconnectMin: 20 * time.Millisecond, Timings: testTimings,
		AppendRepo: a.add, WorktreeInterval: 30 * time.Millisecond,
		Reread: func() (ConfigRead, bool, error) {
			mu.Lock()
			defer mu.Unlock()
			c := changed
			changed = false
			return ConfigRead{}, c, nil
		},
	})
	discovered(local)
	go local.Run(f.ctx)
	f.setLocal(local)
	f.awaitRecord(t, "held", 10*time.Second, func(p pendingFile) bool { return p.RememberError != "" })
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	a.fail(nil)
	mu.Lock()
	changed = true
	mu.Unlock()
	f.awaitRecord(t, "held", 10*time.Second, func(p pendingFile) bool { return !p.Remember })
	dismissed := make(chan protocol.Message, 1)
	go func() { dismissed <- local.dismiss("held") }()
	select {
	case res := <-dismissed:
		if res.OK || res.Detail != "" || !strings.Contains(res.Error, "permission denied") {
			t.Fatalf("dismiss: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dismiss waits on the pending file's write")
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
	d := New(Config{EnvironmentID: "env", Store: store, Logger: log.New(&logged, "", 0), Reread: func() (ConfigRead, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		r := reads[0]
		reads = reads[1:]
		return ConfigRead{Listed: worktree.Listed{Repos: r.repos}}, r.changed, r.err
	}})
	label := func() string {
		for _, w := range d.worktreeRecords() {
			if w.Source == other {
				return w.Repo
			}
		}
		return ""
	}
	d.readConfig(ctx)
	d.pollWorktrees(ctx)
	if label() != "sent" || len(store.Repos()) != 1 {
		t.Fatalf("before: %q %v", label(), store.Repos())
	}
	for i, want := range []string{"listed", "listed", "listed", "listed"} {
		d.readConfig(ctx)
		d.pollWorktrees(ctx)
		if label() != want || len(store.Repos()) != 2 {
			t.Fatalf("poll %d: %q %v", i, label(), store.Repos())
		}
	}
	if n := strings.Count(logged.String(), "config: yaml: bad; the daemon keeps the config it had"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, logged.String())
	}
}

// An add takes what the config lists from the daemon's last read of
// the file: a copy rule for every worktree, a listed repository's copy
// and setup, and the agents' commands, edited after the daemon started,
// are used by the next add without a restart, an agent added since
// included.
func TestAddFollowsConfig(t *testing.T) {
	d, ft, store, remote := newAddDaemon(t)
	ctx := context.Background()
	reads := make(chan ConfigRead, 1)
	d.cfg.Reread = func() (ConfigRead, bool, error) {
		select {
		case r := <-reads:
			return r, true, nil
		default:
			return ConfigRead{}, false, nil
		}
	}
	d.readConfig(ctx)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "a1", Repo: "proj", Branch: "one", AgentName: "claude"})
	first, _ := result(t, pc, "a1")
	if !first.OK {
		t.Fatalf("add: %+v", first)
	}
	checkout := store.Dirs.Checkout("proj")
	for name, body := range map[string]string{"host.local": "every worktree", "repo.local": "proj's"} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reads <- ConfigRead{
		Listed: worktree.Listed{
			Repos: worktree.Repos{{Source: remote, Name: "proj", Copy: []string{"repo.local"}, Setup: []string{"echo edited >> log"}}},
			Copy:  []string{"host.local"},
		},
		Agents: map[string][]string{"claude": {"claude", "--edited"}, "gemini": {"gemini"}},
	}
	d.readConfig(ctx)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "a2", Repo: "proj", Branch: "two", AgentName: "claude"})
	second, _ := result(t, pc, "a2")
	if !second.OK {
		t.Fatalf("add: %+v", second)
	}
	for name, want := range map[string]string{"host.local": "every worktree", "repo.local": "proj's", "log": "ran\nedited\n"} {
		if b, err := os.ReadFile(filepath.Join(second.Root, name)); err != nil || string(b) != want {
			t.Errorf("%s after the edit: %q %v", name, b, err)
		}
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "a3", Repo: "proj", Branch: "three", AgentName: "gemini"})
	if third, _ := result(t, pc, "a3"); !third.OK {
		t.Fatalf("add with an agent added since: %+v", third)
	}
	ft.mu.Lock()
	cmds := fmt.Sprint(ft.cmds)
	ft.mu.Unlock()
	if cmds != "[[claude] [claude --edited] [gemini]]" {
		t.Errorf("the agents started: %s", cmds)
	}
	// Every agent gone from the file: none is started.
	reads <- ConfigRead{Listed: store.Listed(), Agents: map[string][]string{}}
	d.readConfig(ctx)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "a4", Repo: "proj", Branch: "four", AgentName: "claude"})
	if res, _ := result(t, pc, "a4"); res.OK || !strings.Contains(res.Error, `unknown agent "claude"`) {
		t.Fatalf("add with an agent removed: %+v", res)
	}
}
