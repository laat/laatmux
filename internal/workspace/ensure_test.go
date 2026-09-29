package workspace

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/tmux"
)

// A workspace session found again for a spec that names another
// managed session has its attach pane moved there: the worktree's
// agent is in another session now. The pane's tag says which.
func TestEnsureRetargetsAttach(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	// Both servers start without the user's config, whose hooks could
	// split a new session, and stay up with no session.
	for _, s := range []tmux.Server{tmux.LaatmuxServer, tmux.DefaultServer} {
		if _, err := s.Run(ctx, "-f", "/dev/null", "start-server", ";", "set-option", "-s", "exit-empty", "off"); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"s1", "s2"} {
		if _, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", name, "sleep", "600"); err != nil {
			t.Fatal(err)
		}
	}
	spec := Spec{Host: client.Host{Name: "mac"}, Managed: "s1", Name: "mac/w", Key: "env//w", Branch: "w"}
	target := func() (string, string) {
		t.Helper()
		out, err := Server.Run(ctx, "list-panes", "-s", "-t", "=mac/w", "-F", "#{@laatmux_attach_target} #{pane_start_command}")
		if err != nil {
			t.Fatal(err)
		}
		f := strings.SplitN(strings.TrimSpace(string(out)), " ", 2)
		return f[0], f[1]
	}
	if _, created, err := Ensure(ctx, spec); err != nil || !created {
		t.Fatalf("first ensure: %v %v", created, err)
	}
	if tag, cmd := target(); tag != "s1" || !strings.Contains(cmd, "s1") {
		t.Fatalf("attach %q %q", tag, cmd)
	}
	spec.Managed = "s2"
	if _, created, err := Ensure(ctx, spec); err != nil || created {
		t.Fatalf("second ensure: %v %v", created, err)
	}
	if tag, cmd := target(); tag != "s2" || !strings.Contains(cmd, "s2") {
		t.Fatalf("attach after retarget %q %q", tag, cmd)
	}
}
