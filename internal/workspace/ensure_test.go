package workspace

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

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
		t.Cleanup(func() { s.Run(ctx, "kill-server") })
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
	// A dead pane on another target comes back on the spec's.
	if _, err := tmux.LaatmuxServer.Run(ctx, "kill-session", "-t", "=s2"); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		out, _ := Server.Run(ctx, "list-panes", "-s", "-t", "=mac/w", "-F", "#{pane_dead}")
		if strings.TrimSpace(string(out)) == "1" {
			break
		}
		if i > 200 {
			t.Fatal("attach pane still alive with its session gone")
		}
		time.Sleep(10 * time.Millisecond)
	}
	spec.Managed = "s1"
	if _, _, err := Ensure(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if tag, cmd := target(); tag != "s1" || !strings.Contains(cmd, "s1") {
		t.Fatalf("dead pane respawned on %q %q", tag, cmd)
	}
	// A pane from before the tag is left where it is.
	out, err := Server.Run(ctx, "list-panes", "-s", "-t", "=mac/w", "-F", "#{pane_id}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Server.Run(ctx, "set-option", "-p", "-t", strings.TrimSpace(string(out)), "-u", "@laatmux_attach_target"); err != nil {
		t.Fatal(err)
	}
	spec.Managed = "s3"
	if _, _, err := Ensure(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, cmd := target(); !strings.Contains(cmd, "s1") {
		t.Fatalf("untagged pane moved: %q", cmd)
	}
}

// The two tmux steps of a pane's jump: the host's select makes a pane
// in another window of the managed session current, and the workspace
// session's attach pane is found and made current again after the user
// left the session on a shell window.
func TestPaneJumpSteps(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	for _, s := range []tmux.Server{tmux.LaatmuxServer, tmux.DefaultServer} {
		if _, err := s.Run(ctx, "-f", "/dev/null", "start-server", ";", "set-option", "-s", "exit-empty", "off"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Run(ctx, "kill-server") })
	}
	managed := tmux.LaatmuxServer
	if _, err := managed.Run(ctx, "new-session", "-d", "-s", "m1", "sleep", "600"); err != nil {
		t.Fatal(err)
	}
	if _, err := managed.Run(ctx, "new-window", "-d", "-t", "=m1", "sleep", "600"); err != nil {
		t.Fatal(err)
	}
	panes := func(s tmux.Server, session string) map[string]string {
		t.Helper()
		out, err := s.Run(ctx, "list-panes", "-s", "-t", "="+session, "-F", "#{pane_id} #{window_active}#{pane_active}")
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f := strings.Fields(l)
			m[f[0]] = f[1]
		}
		return m
	}
	var back string
	for id, active := range panes(managed, "m1") {
		if active != "11" {
			back = id
		}
	}
	if back == "" {
		t.Fatal("no pane in the background window")
	}
	if err := managed.SelectPane(ctx, back); err != nil {
		t.Fatal(err)
	}
	if got := panes(managed, "m1")[back]; got != "11" {
		t.Fatalf("the background pane after select: active %q", got)
	}
	if err := managed.SelectPane(ctx, "%999"); err == nil {
		t.Error("a pane gone selected without error")
	}
	// The workspace session, its attach pane, and a shell window the
	// user opened and left current.
	spec := Spec{Host: client.Host{Name: "mac"}, Managed: "m1", Name: "mac/w", Key: "env//w", Branch: "w"}
	if _, created, err := Ensure(ctx, spec); err != nil || !created {
		t.Fatalf("ensure: %v %v", created, err)
	}
	attach := AttachPane(ctx, "mac/w")
	if attach == "" {
		t.Fatal("no attach pane")
	}
	if _, err := Server.Run(ctx, "new-window", "-t", "=mac/w", "sleep", "600"); err != nil {
		t.Fatal(err)
	}
	if got := panes(Server, "mac/w")[attach]; got == "11" {
		t.Fatal("the shell window did not take the focus")
	}
	if err := Server.SelectPane(ctx, attach); err != nil {
		t.Fatal(err)
	}
	if got := panes(Server, "mac/w")[attach]; got != "11" {
		t.Fatalf("the attach pane after select: active %q", got)
	}
	if AttachPane(ctx, "no/such") != "" {
		t.Error("an attach pane for a session that is not there")
	}
}
