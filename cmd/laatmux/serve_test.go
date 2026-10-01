package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/worktree"
)

// served is a daemon serving a seeded repository with one worktree:
// what the serve tests start.
type served struct {
	remote string         // the bare repository
	added  worktree.Added // the worktree task, added before the daemon started
	done   chan error     // serve's return
	cancel context.CancelFunc
	conn   *client.Conn // a connection with the hello read
}

// serveFixture seeds a bare repository with one commit, a config with
// one local host named box and the repository as proj, adds a worktree
// task from the store, points the state, config and tmux directories
// under base so the daemon polls a laatmux server that is not there and
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
	dirs := config.Dirs{Repos: filepath.Join(base, "repos"), Worktrees: filepath.Join(base, "worktrees")}
	cfg := fmt.Sprintf("hosts:\n  - name: box\n    repos: %s\n    worktrees: %s\nrepos:\n  - source: %s\n    name: proj\n", dirs.Repos, dirs.Worktrees, remote)
	os.WriteFile(filepath.Join(base, "config.yaml"), []byte(cfg), 0o644)
	// State, config and the tmux socket directory all under base: the
	// daemon polls a laatmux server that is not there and never starts one.
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
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- cmdServe(ctx, []string{"--listen", "tcp:127.0.0.1:0"}) }()
	var c *client.Conn
	for deadline := time.Now().Add(10 * time.Second); ; {
		nc, err := client.DialLocal(ctx, false)
		if err == nil {
			c, err = client.Connect(ctx, client.Host{Name: "box"}, nc, nc, func() { nc.Close() })
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
	return &served{remote: remote, added: added, done: done, cancel: cancel, conn: c}
}

// serve's own shutdown, the context ending as a signal ends it, stops
// the runs the way cancel does and returns once they are gone, whichever
// of its goroutines noticed first.
func TestServeShutdownCancelsRuns(t *testing.T) {
	s := serveFixture(t)
	c, added, cancel, served := s.conn, s.added, s.cancel, s.done
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
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return")
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
// stream read as ls reads it, ls's listing, then the merged state
// filled into a view and both views rendered. The worktree the fixture
// added shows in the listing and the tree, the agent view has no tile
// for it, and the host is connected and listed. No view, merged state
// and daemon were tested together before this.
func TestServeToRender(t *testing.T) {
	s := serveFixture(t)
	ctx := context.Background()
	c, ok := dialMerged(ctx)
	if !ok {
		t.Fatal("the local daemon does not merge")
	}
	defer c.Close()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	m := newMerged()
	m.configure(cfg)
	pending, err := m.readMerged(ctx, c, 20*time.Second, func(m *merged) bool { return len(m.pending()) == 0 })
	if err != nil || len(pending) != 0 {
		t.Fatalf("read merged: %v %v", pending, err)
	}
	// ls's listing: the host line, the repository and its worktree.
	listing := m.render(m.locals())
	for _, want := range []string{"box  connected", "proj", "task (box)"} {
		if !strings.Contains(listing, want) {
			t.Errorf("ls lacks %q:\n%s", want, listing)
		}
	}
	if strings.Contains(listing, "DOWN") || strings.Contains(listing, "not listed") {
		t.Errorf("ls shows a failure:\n%s", listing)
	}
	// The dashboard's fill and both views.
	v := &view.Model{Width: 100, Height: 30, LocalHost: "box", View: view.ViewTree, Layout: view.Compact, Titles: true}
	m.fill(v, "")
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
	m.mu.Lock()
	n := len(m.worktrees)
	m.mu.Unlock()
	if n != 1 || s.added.Root == "" {
		t.Errorf("worktrees in the merged state: %d", n)
	}
}
