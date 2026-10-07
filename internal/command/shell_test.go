package command

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// A shell in a workspace session whose name has a ., which tmux 3.7
// keeps, opens one shell window and selects it the second time: the
// session's windows are listed by =mac/a.b:, where =mac/a.b looked for
// pane b of window mac/a and the shell failed. Such a session is the
// workspace of a worktree whose home a.b was made by hand on a host. A
// tmux before 3.7 stores the . as _, and the test is skipped there.
func TestShellDottedSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	s := workspace.Server
	t.Cleanup(func() { s.Run(context.Background(), "kill-server") })
	// No config, and a window made without a command runs a sleep
	// rather than the user's shell.
	out, err := s.Run(ctx, "-f", "/dev/null", "new-session", "-d", "-s", "mac/a.b", "-P", "-F", "#{session_name}", "sleep 600",
		tmux.Next, "set-option", "-g", "default-command", "sleep 600")
	if err != nil {
		t.Fatal(err)
	}
	if name := strings.TrimSpace(string(out)); name != "mac/a.b" {
		t.Skipf("tmux before 3.7 stores a . in a session name as _: %q", name)
	}
	l := protocol.Session{Name: "mac/a.b", Key: protocol.SessionKey("env", t.TempDir()), Host: "mac"}
	h := config.Host{Host: peer.Host{Name: "mac"}}
	for i := 0; i < 2; i++ {
		if err := Shell(ctx, h, l); err != nil {
			t.Fatalf("shell %d: %v", i+1, err)
		}
	}
	out, err = s.Run(ctx, "list-windows", "-t", "=mac/a.b:", "-F", "#{@laatmux_shell}")
	if err != nil || strings.Count(string(out), "\n") != 2 || strings.Count(string(out), "1\n") != 1 {
		t.Fatalf("windows' shell tags %q %v, want the session's and one shell window", out, err)
	}
}
