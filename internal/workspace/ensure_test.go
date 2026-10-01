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

// startServers starts both servers without the user's config, whose
// hooks could split a new session, and keeps them up with no session;
// each is killed at the end. A server the previous test killed may
// still be going: a start that reaches it is tried again.
func startServers(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, s := range []tmux.Server{tmux.LaatmuxServer, tmux.DefaultServer} {
		var err error
		for i := 0; i < 50; i++ {
			if _, err = s.Run(ctx, "-f", "/dev/null", "start-server", ";", "set-option", "-s", "exit-empty", "off"); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Run(ctx, "kill-server") })
	}
}

// A workspace session found again for a spec that names another
// managed session has its attach pane moved there: the worktree's
// agent is in another session now. The pane's tag says which.
func TestEnsureRetargetsAttach(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
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
	startServers(t)
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

// A plain attachment named as a worktree's workspace would be, left by
// an older build's jump from the agent's row: a keyed spec to the same
// managed session adopts it, keyed and tagged, its untagged attach pane
// given the target, and found by key after; one to another managed
// session is still a name in use.
func TestEnsureAdoptsAttachment(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	for _, name := range []string{"proj/w", "proj/other"} {
		if _, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", name, "sleep", "600"); err != nil {
			t.Fatal(err)
		}
	}
	host := client.Host{Name: "mac"}
	if _, created, err := Ensure(ctx, Spec{Host: host, Managed: "proj/w", Name: "mac/proj/w"}); err != nil || !created {
		t.Fatalf("the plain attachment: %v %v", created, err)
	}
	// An older build left the attach pane without its target, and the
	// managed session went away and came back since, so the pane is
	// dead.
	if _, err := Server.Run(ctx, "set-option", "-p", "-u", "-t", "=mac/proj/w:", "@laatmux_attach_target"); err != nil {
		t.Fatal(err)
	}
	if _, err := tmux.LaatmuxServer.Run(ctx, "kill-session", "-t", "=proj/w"); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		out, _ := Server.Run(ctx, "list-panes", "-s", "-t", "=mac/proj/w", "-F", "#{pane_dead}")
		if strings.TrimSpace(string(out)) == "1" {
			break
		}
		if i > 200 {
			t.Fatal("attach pane still alive with its session gone")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", "proj/w", "sleep", "600"); err != nil {
		t.Fatal(err)
	}
	keyed := Spec{Host: host, Managed: "proj/w", Name: "mac/proj/w", Key: "env//r/w", Source: "git@github.com:laat/proj.git", Branch: "w"}
	name, created, err := Ensure(ctx, keyed)
	if err != nil || created || name != "mac/proj/w" {
		t.Fatalf("adopt: %q %v %v", name, created, err)
	}
	locals, err := List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := Find(locals, keyed.Key, "")
	if !ok || l.Name != "mac/proj/w" || l.Attach != "" || l.Host != "mac" || l.Source != keyed.Source || l.Branch != "w" {
		t.Fatalf("after adopt: %+v %v", l, ok)
	}
	if _, ok := Find(locals, "", "mac/proj/w"); ok {
		t.Error("still found as a plain attachment")
	}
	// The pane tagged with the target and respawned on it.
	out, err := Server.Run(ctx, "list-panes", "-s", "-t", "=mac/proj/w", "-F", "#{@laatmux_attach_pane} #{@laatmux_attach_target} #{pane_dead} #{pane_start_command}")
	if f := strings.Fields(strings.TrimSpace(string(out))); err != nil || len(f) < 4 || f[0] != "1" || f[1] != "proj/w" || f[2] != "0" || !strings.Contains(strings.TrimSpace(string(out)), "proj/w") {
		t.Fatalf("the attach pane after adopt: %q %v", out, err)
	}
	// Found by key the next time, nothing created.
	if name, created, err := Ensure(ctx, keyed); err != nil || created || name != "mac/proj/w" {
		t.Fatalf("ensure after adopt: %q %v %v", name, created, err)
	}
	// A plain attachment to another managed session stays a name in use.
	if _, created, err := Ensure(ctx, Spec{Host: host, Managed: "proj/other", Name: "mac/proj/other"}); err != nil || !created {
		t.Fatalf("the other attachment: %v %v", created, err)
	}
	_, _, err = Ensure(ctx, Spec{Host: host, Managed: "proj/w2", Name: "mac/proj/other", Key: "env//r/w2", Branch: "w2"})
	if err == nil || !strings.Contains(err.Error(), "name in use") {
		t.Fatalf("another session's attachment adopted: %v", err)
	}
}
