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
	"github.com/laat/laatmux/internal/worktree"
)

// serve's own shutdown, the context ending as a signal ends it, stops
// the runs the way cancel does and returns once they are gone, whichever
// of its goroutines noticed first.
func TestServeShutdownCancelsRuns(t *testing.T) {
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
	t.Setenv("TMUX_TMPDIR", filepath.Join(base, "tmux"))
	os.MkdirAll(filepath.Join(base, "tmux"), 0o700)
	store := worktree.New(dirs, []config.Repo{{Source: remote, Name: "proj"}})
	repo, _ := store.Repo(remote)
	added, err := store.Add(context.Background(), repo, "task", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- cmdServe(ctx, []string{"--listen", "tcp:127.0.0.1:0"}) }()
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
	defer c.Close()
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
