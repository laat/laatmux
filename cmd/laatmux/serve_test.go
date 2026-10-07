package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/gittest"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/worktree"
)

// served is a daemon serving a seeded repository with one worktree:
// what the serve tests start.
type served struct {
	added  worktree.Added // the worktree task, added before the daemon started
	done   chan error     // serve's return, read once by wait
	cancel context.CancelFunc
	conn   *client.Conn // a connection with the hello read
	once   sync.Once
	err    error
}

// wait ends serve and returns what it returned, once; the fixture's
// cleanup calls it before the environment and the directories go, since
// serve's own exit removes its runtime file by the environment as it is
// then, and a later fixture in the same process shares the pid.
func (s *served) wait(t *testing.T) error {
	t.Helper()
	s.once.Do(func() {
		s.cancel()
		select {
		case s.err = <-s.done:
		case <-time.After(15 * time.Second):
			s.err = fmt.Errorf("serve did not return")
		}
	})
	return s.err
}

// serveFixture seeds a bare repository with one commit, a config with
// one local host named box and the repository as proj, adds a worktree
// task from the store, points the state and config directories under
// base and the tmux socket directory at a short path of its own under
// /tmp, so the daemon polls a laatmux server that is not there and
// never starts one, then starts cmdServe on a loopback port and waits
// for its hello.
func serveFixture(t *testing.T) *served {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	if runtime.GOOS == "darwin" {
		if real, err := filepath.EvalSymlinks(base); err == nil {
			base = real
		}
	}
	remote := seedRemote(t, base)
	dirs := config.Dirs{Repos: filepath.Join(base, "repos"), Worktrees: filepath.Join(base, "worktrees")}
	cfg := fmt.Sprintf("hosts:\n  - name: box\n    repos: %s\n    worktrees: %s\nrepos:\n  - source: %s\n    name: proj\n", dirs.Repos, dirs.Worktrees, remote)
	os.WriteFile(filepath.Join(base, "config.yaml"), []byte(cfg), 0o644)
	// State and config under base; the tmux socket directory below is
	// one of this fixture's own too, so the daemon polls a laatmux
	// server that is not there and never starts one.
	t.Setenv("LAATMUX_HOME", filepath.Join(base, "home"))
	t.Setenv("LAATMUX_CONFIG", filepath.Join(base, "config.yaml"))
	// A short socket directory: a unix socket path has about a hundred
	// bytes, and under a macOS TempDir tmux's "File name too long" is
	// not the absence the daemon's first poll waits out.
	tmuxDir, err := os.MkdirTemp("/tmp", "lmxs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmuxDir) })
	t.Setenv("TMUX_TMPDIR", tmuxDir)
	store := worktree.New(dirs, []config.Repo{{Source: remote, Name: "proj"}})
	repo, _ := store.Repo(remote)
	added, err := store.Add(context.Background(), repo, "task", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmdServe(ctx, []string{"--listen", "tcp:127.0.0.1:0"}) }()
	s := &served{added: added, done: done, cancel: cancel}
	// Registered after the Setenvs, so it runs before they are undone;
	// a serve that does not return would outlive them.
	t.Cleanup(func() {
		if err := s.wait(t); err != nil {
			t.Errorf("serve at the end: %v", err)
		}
	})
	var c *client.Conn
	for deadline := time.Now().Add(10 * time.Second); ; {
		nc, err := client.DialLocal(ctx, false)
		if err == nil {
			c, err = client.Connect(ctx, peer.Host{Name: "box"}, nc, nc, func() { nc.Close() })
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not come up: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() { c.Close() })
	s.conn = c
	return s
}

// seedRemote makes a bare repository under base with one commit on
// main, pushed from a seed repository beside it, and returns its path.
func seedRemote(t *testing.T, base string) string {
	t.Helper()
	sh := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	remote, seed := filepath.Join(base, "remote.git"), filepath.Join(base, "seed")
	sh(base, "git", "init", "-q", "--bare", "--initial-branch=main", remote)
	sh(base, "git", "init", "-q", "--initial-branch=main", seed)
	sh(seed, "git", "config", "user.email", "t@example.com")
	sh(seed, "git", "config", "user.name", "t")
	os.WriteFile(filepath.Join(seed, "README"), []byte("x\n"), 0o644)
	sh(seed, "git", "add", ".")
	sh(seed, "git", "commit", "-q", "-m", "init")
	sh(seed, "git", "push", "-q", remote, "main")
	return remote
}

// The fixture's seed commit succeeds in an environment that names
// config that fails every commit, with an identity from its
// repository's config alone, and leaves alone the repository and index
// the environment names as well.
func TestGitIsolated(t *testing.T) {
	gittest.CheckIsolated(t, func(t *testing.T) string { return seedRemote(t, t.TempDir()) })
}

// serve's own shutdown, the context ending as a signal ends it, stops
// the runs the way cancel does and returns once they are gone, whichever
// of its goroutines noticed first.
func TestServeShutdownCancelsRuns(t *testing.T) {
	s := serveFixture(t)
	c, added := s.conn, s.added
	if !protocol.Has(c.Hello.Capabilities, protocol.CapRun) {
		t.Fatalf("caps %v", c.Hello.Capabilities)
	}
	token := fmt.Sprintf("laatmux-serve-test-%d", os.Getpid())
	c.Write(protocol.Message{Type: protocol.TypeRun, ID: "r1", Root: added.Root, Cmd: []string{"sh", "-c", "echo up; sleep 30", token}})
	for {
		m, err := c.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.State == protocol.StateOutput && m.Detail == "up" {
			break
		}
	}
	if err := s.wait(t); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if out, _ := exec.Command("pgrep", "-f", token).Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("run survived the shutdown: pids %s", out)
	}
	for {
		m, err := c.Read()
		if err != nil {
			t.Fatalf("no result before the connection closed: %v", err)
		}
		if m.Type == protocol.TypeResult && m.ID == "r1" {
			if m.OK || m.Error != protocol.ErrCancelled {
				t.Fatalf("result %+v", m)
			}
			break
		}
	}
}

// The whole client path against a real daemon: serve, the merged
// stream read as ls reads it against a daemon still on its first poll,
// so the snapshot comes before the host is listed and the wait is
// real, ls's listing, then the merged state filled into a view and
// both views rendered. The worktree the fixture added shows in the
// listing and the tree, the agent view has no tile for it, and the
// host is connected and listed. No view, merged state and daemon were
// tested together before this.
func TestServeToRender(t *testing.T) {
	real, err := exec.LookPath("tmux")
	if err != nil {
		// Without tmux the daemon's first poll never completes, so the
		// local host is never listed and ls waits its timeout out.
		t.Skip("tmux not installed")
	}
	// A tmux whose list-panes, the daemon's poll, waits for a gate the
	// test opens once it has seen the snapshot with the host unlisted:
	// the daemon is still on its first poll when the client subscribes,
	// as it is for ls against a daemon it just started, and the host is
	// listed by an upsert after the snapshot. The other commands, the
	// sessions listing the snapshot needs among them, run at once.
	bin := t.TempDir()
	gate := filepath.Join(bin, "gate")
	t.Setenv("LAATMUX_TEST_TMUX", real)
	t.Setenv("LAATMUX_TEST_GATE", gate)
	// The command is matched anywhere in argv, whatever flags precede
	// it; a shim whose test binary died, by a panic before the gate
	// opened, exits rather than loop as an orphan.
	shim := "#!/bin/sh\ncase \" $* \" in *\" list-panes \"*) while [ ! -e \"$LAATMUX_TEST_GATE\" ]; do kill -0 $PPID 2>/dev/null || exit 1; sleep 0.02; done;; esac\nexec \"$LAATMUX_TEST_TMUX\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	open := func() { os.WriteFile(gate, nil, 0o644) }
	t.Cleanup(open)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := serveFixture(t)
	ctx := context.Background()
	// Dialled without the auto-start: a daemon gone by now would have
	// the dial run the test binary as serve.
	nc, err := client.DialLocal(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.Connect(ctx, peer.Host{Name: "local"}, nc, nc, func() { nc.Close() })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapMerged) {
		t.Fatal("the local daemon does not merge")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	m := merged.New()
	m.Configure(cfg)
	unlisted := 0
	waiting, err := m.Read(ctx, c, 20*time.Second, func(waiting []string) bool {
		if len(waiting) == 0 {
			return true
		}
		// The snapshot, with the host still unlisted: let the poll
		// through.
		unlisted++
		open()
		return false
	})
	if err != nil || len(waiting) != 0 {
		t.Fatalf("read merged: %v %v", waiting, err)
	}
	if unlisted == 0 {
		t.Error("the host was listed in the snapshot already: the wait was not exercised")
	}
	// ls's listing: the host line, the repository and its worktree.
	listing := render(m.Status(""))
	for _, want := range []string{"box  connected", "proj", "task (box)"} {
		if !strings.Contains(listing, want) {
			t.Errorf("ls lacks %q:\n%s", want, listing)
		}
	}
	for _, bad := range []string{"DOWN", "not listed", "snapshot pending"} {
		if strings.Contains(listing, bad) {
			t.Errorf("ls shows %q:\n%s", bad, listing)
		}
	}
	// The dashboard's fill and both views.
	v := &view.Model{Width: 100, Height: 30, LocalHost: "box", View: view.ViewTree, Layout: view.Compact, Titles: true}
	fill(v, m.Status(""))
	if v.Loading || len(v.Header) != 0 {
		t.Fatalf("after fill: loading %v header %v", v.Loading, v.Header)
	}
	tree := view.Text(v.Render())
	if !strings.Contains(tree, "proj") || !strings.Contains(tree, "task (box)") {
		t.Errorf("the tree view:\n%s", tree)
	}
	// A worktree with no agent is a tree line, not a tile.
	v.View = view.ViewAgents
	if agents := view.Text(v.Render()); !strings.HasPrefix(agents, "No agents running") {
		t.Errorf("the agent view:\n%s", agents)
	}
	// The worktree the store added is the one the stream carries.
	var roots, branches []string
	for _, w := range m.Status("").Input.Worktrees {
		roots, branches = append(roots, w.Root), append(branches, w.Branch)
	}
	if len(roots) != 1 || roots[0] != s.added.Root || branches[0] != "task" {
		t.Errorf("worktrees in the merged state: %v %v, want %s task", roots, branches, s.added.Root)
	}
}

// serve with a pending directory that cannot be made, a file in its
// place: it ends with the error before announcing itself, so the
// runtime file is never written and no client is served a hello
// without the relay. A runtime file naming another live process stands
// in for the check: serve removes only a file naming its own pid, so
// the file survives only if serve never wrote one.
func TestServeNeedsPendingDirectory(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "config.yaml"), []byte("hosts:\n  - name: box\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "pending"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_HOME", home)
	t.Setenv("LAATMUX_CONFIG", filepath.Join(base, "config.yaml"))
	t.Setenv("TMUX_TMPDIR", base)
	other := fmt.Sprintf(`{"address":"tcp:127.0.0.1:1","pid":%d,"version":"other"}`, os.Getppid())
	if err := os.WriteFile(filepath.Join(home, "runtime.json"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := cmdServe(ctx, []string{"--listen", "tcp:127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "pending:") || ctx.Err() != nil {
		t.Fatalf("serve without a pending directory: %v (context %v)", err, ctx.Err())
	}
	if b, err := os.ReadFile(filepath.Join(home, "runtime.json")); err != nil || string(b) != other {
		t.Errorf("the runtime file touched by a daemon that did not run: %q %v", b, err)
	}
}

// serve has no --tmux-socket, the older spelling of --tmux-servers;
// explain's flag of that name is not serve's. The state and config are
// the test's own and the context is done, so a serve that took the flag
// would return at once rather than serve the user's state directory.
func TestServeHasNoTmuxSocket(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LAATMUX_HOME", filepath.Join(base, "home"))
	t.Setenv("LAATMUX_CONFIG", filepath.Join(base, "config.yaml"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := cmdServe(ctx, []string{"--listen", "tcp:127.0.0.1:0", "--tmux-socket", "laatmux"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -tmux-socket") {
		t.Fatalf("serve --tmux-socket: %v", err)
	}
}
