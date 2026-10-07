package workspace

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/laat/laatmux/internal/peer"
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
			if _, err = s.Run(ctx, "-f", "/dev/null", "start-server", tmux.Next, "set-option", "-s", "exit-empty", "off"); err == nil {
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
	spec := Spec{Host: peer.Host{Name: "mac"}, Managed: "s1", Name: "mac/w", Key: "env//w", Branch: "w"}
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
	spec := Spec{Host: peer.Host{Name: "mac"}, Managed: "m1", Name: "mac/w", Key: "env//w", Branch: "w"}
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

// Every write names its session exactly. A session gone, whose name is
// a prefix of another's, is an error that touches nothing: its settled
// tag, attach pane and adoption do not land on the other. A session
// that is there is the one written even when the current session has a
// window named after it, which a bare target would take first: the most
// recently active session for a command run outside any pane, and the
// pane's for one run from a pane, whatever was made after it.
func TestExactSessionTargets(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	for _, name := range []string{"m1", "m2"} {
		if _, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", name, "sleep", "600"); err != nil {
			t.Fatal(err)
		}
	}
	host := peer.Host{Name: "mac"}
	if _, created, err := Ensure(ctx, Spec{Host: host, Managed: "m1", Name: "mac/proj/z-2", Key: "env//r/z-2", Branch: "z-2"}); err != nil || !created {
		t.Fatalf("the workspace: %v %v", created, err)
	}
	if _, created, err := Ensure(ctx, Spec{Host: host, Managed: "m2", Name: "mac/proj/y"}); err != nil || !created {
		t.Fatalf("the plain attachment: %v %v", created, err)
	}
	show := func(args ...string) string {
		t.Helper()
		out, err := Server.Run(ctx, append([]string{"show-options", "-qv"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	opt := func(name, option string) string {
		t.Helper()
		return show("-t", "="+name+":", option)
	}
	pane := AttachPane(ctx, "mac/proj/z-2")
	if pane == "" {
		t.Fatal("no attach pane")
	}
	paneTarget := func() string {
		t.Helper()
		return show("-p", "-t", pane, "@laatmux_attach_target")
	}
	// mac/proj/z is gone.
	if err := SetSettled(ctx, "mac/proj/z", true); err == nil || opt("mac/proj/z-2", "@laatmux_settled") != "" {
		t.Fatalf("a gone session settled: %v, the other's tag %q", err, opt("mac/proj/z-2", "@laatmux_settled"))
	}
	if _, err := Server.Run(ctx, "set-option", "-t", "=mac/proj/z-2:", "@laatmux_settled", "1"); err != nil {
		t.Fatal(err)
	}
	if err := SetSettled(ctx, "mac/proj/z", false); err == nil || opt("mac/proj/z-2", "@laatmux_settled") != "1" {
		t.Fatalf("a gone session unsettled: %v, the other's tag %q", err, opt("mac/proj/z-2", "@laatmux_settled"))
	}
	if got := AttachPane(ctx, "mac/proj/z"); got != "" {
		t.Errorf("a gone session's attach pane %q", got)
	}
	if err := ensureAttach(ctx, "mac/proj/z", Spec{Host: host, Managed: "m2"}); err == nil || paneTarget() != "m1" {
		t.Fatalf("a gone session's attach ensured: %v, the other's pane on %q", err, paneTarget())
	}
	if _, err := Server.Run(ctx, "set-option", "-p", "-u", "-t", "=mac/proj/z-2:", "@laatmux_attach_target"); err != nil {
		t.Fatal(err)
	}
	if err := adopt(ctx, "mac/proj/z", Spec{Host: host, Managed: "m2", Key: "env//r/z", Branch: "z"}); err == nil || paneTarget() != "" || opt("mac/proj/z-2", "@laatmux_workspace") != "env//r/z-2" {
		t.Fatalf("a gone session adopted: %v, the other's pane on %q, key %q", err, paneTarget(), opt("mac/proj/z-2", "@laatmux_workspace"))
	}
	// notes, made last, is the current session, and has a window named
	// after both.
	for _, args := range [][]string{
		{"new-session", "-d", "-s", "notes", "-n", "mac/proj/z-2-scratch", "sleep 600"},
		{"new-window", "-d", "-t", "=notes:", "-n", "mac/proj/y-scratch", "sleep 600"},
	} {
		if _, err := Server.Run(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := SetSettled(ctx, "mac/proj/z-2", false); err != nil || opt("mac/proj/z-2", "@laatmux_settled") != "" {
		t.Fatalf("unsettle: %v, tag %q", err, opt("mac/proj/z-2", "@laatmux_settled"))
	}
	if err := SetSettled(ctx, "mac/proj/z-2", true); err != nil || opt("mac/proj/z-2", "@laatmux_settled") != "1" || opt("notes", "@laatmux_settled") != "" {
		t.Fatalf("settle: %v, tags %q and notes %q", err, opt("mac/proj/z-2", "@laatmux_settled"), opt("notes", "@laatmux_settled"))
	}
	// A reuse retags the host, and the adoption keys the session.
	if _, created, err := Ensure(ctx, Spec{Host: peer.Host{Name: "vm"}, Managed: "m1", Name: "mac/proj/z-2", Key: "env//r/z-2", Branch: "z-2"}); err != nil || created {
		t.Fatalf("the reuse: %v %v", created, err)
	}
	if opt("mac/proj/z-2", "@laatmux_host") != "vm" || opt("notes", "@laatmux_host") != "" {
		t.Fatalf("the reuse tagged host %q, notes %q", opt("mac/proj/z-2", "@laatmux_host"), opt("notes", "@laatmux_host"))
	}
	if name, created, err := Ensure(ctx, Spec{Host: host, Managed: "m2", Name: "mac/proj/y", Key: "env//r/y", Branch: "y"}); err != nil || created || name != "mac/proj/y" {
		t.Fatalf("the adoption: %q %v %v", name, created, err)
	}
	if opt("mac/proj/y", "@laatmux_workspace") != "env//r/y" || opt("mac/proj/y", "@laatmux_attach") != "" || opt("notes", "@laatmux_workspace") != "" {
		t.Fatalf("the adoption keyed %q, attach %q, notes %q", opt("mac/proj/y", "@laatmux_workspace"), opt("mac/proj/y", "@laatmux_attach"), opt("notes", "@laatmux_workspace"))
	}
	// From a pane of notes, as the sidebar runs, notes stays current
	// while the sessions made from it are newer; it has windows named
	// after them. The new sessions are tagged, not notes.
	out, err := Server.Run(ctx, "new-window", "-d", "-t", "=notes:", "-n", "mac/proj/k-scratch", "-P", "-F", "#{pane_id}", "sleep 600")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Server.Run(ctx, "new-window", "-d", "-t", "=notes:", "-n", "mac/proj/k", "sleep 600"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_PANE", strings.TrimSpace(string(out)))
	keyed := Spec{Host: host, Managed: "m1", Name: "mac/proj/k", Key: "env//r/k", Branch: "k"}
	if _, created, err := Ensure(ctx, keyed); err != nil || !created {
		t.Fatalf("the workspace from a pane: %v %v", created, err)
	}
	if _, created, err := Ensure(ctx, Spec{Host: host, Managed: "m1", Name: "mac/proj/k-s"}); err != nil || !created {
		t.Fatalf("the plain attachment from a pane: %v %v", created, err)
	}
	if opt("mac/proj/k", "@laatmux_workspace") != "env//r/k" || opt("mac/proj/k-s", "@laatmux_attach") != "mac/m1" || opt("notes", "@laatmux_workspace") != "" || opt("notes", "@laatmux_attach") != "" {
		t.Fatalf("made from a pane: key %q, attach %q, notes %q %q", opt("mac/proj/k", "@laatmux_workspace"), opt("mac/proj/k-s", "@laatmux_attach"), opt("notes", "@laatmux_workspace"), opt("notes", "@laatmux_attach"))
	}
	// Its attach window closed, the workspace gets a new one, in it.
	if _, err := Server.Run(ctx, "new-window", "-d", "-t", "=mac/proj/k:", "sleep 600"); err != nil {
		t.Fatal(err)
	}
	if _, err := Server.Run(ctx, "kill-pane", "-t", AttachPane(ctx, "mac/proj/k")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Ensure(ctx, keyed); err != nil || AttachPane(ctx, "mac/proj/k") == "" {
		t.Fatalf("the attach window remade: %v, pane %q", err, AttachPane(ctx, "mac/proj/k"))
	}
}

// A workspace for a branch with a # or a ; is made under the name
// SessionName computed, tagged, with its attach pane, and reused the
// next time: new-session expands a # in the name as a format, and an
// argument that ends in ; splits the sequence, so the tags would target
// a session that is not there, or nothing would run.
func TestEnsureEncodedNames(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	if _, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", "m1", "sleep", "600"); err != nil {
		t.Fatal(err)
	}
	host := peer.Host{Name: "mac"}
	keys := map[string]string{"fix#12": "env//r/fix-12", "x#{session_id}": "env//r/x", "semi;": "env//r/semi"}
	for branch, key := range keys {
		spec := Spec{Host: host, Managed: "m1", Name: SessionName("mac", "proj", branch), Key: key}
		name, created, err := Ensure(ctx, spec)
		if err != nil || !created || name != spec.Name {
			t.Fatalf("%s: %q %v %v", branch, name, created, err)
		}
		if AttachPane(ctx, name) == "" {
			t.Errorf("%s: no attach pane in %s", branch, name)
		}
		if name, created, err := Ensure(ctx, spec); err != nil || created || name != spec.Name {
			t.Fatalf("%s again: %q %v %v", branch, name, created, err)
		}
	}
	locals, err := List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(locals) != len(keys) {
		t.Fatalf("sessions %+v", locals)
	}
	for branch, key := range keys {
		l, ok := ByName(locals, SessionName("mac", "proj", branch))
		if !ok || l.Key != key || l.Host != "mac" {
			t.Errorf("%s: %+v %v", branch, l, ok)
		}
	}
}

// Switching names the session exactly. A name with a %, as an encoded
// branch has, is one switch-client looks up as a pane, where =name alone
// is no name at all; a gone one does not switch to a session it is a
// prefix of.
func TestSwitchExactName(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	for _, name := range []string{"home", "vm/proj/release-1%2e2", "vm/proj/v1%2e3"} {
		if _, err := Server.Run(ctx, "new-session", "-d", "-s", name, "sleep 600"); err != nil {
			t.Fatal(err)
		}
	}
	// A control-mode client stands in for the user's terminal.
	cmd := exec.Command("tmux", "-L", "default", "-C", "attach-session", "-t", "=home")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait() })
	var client string
	for i := 0; i < 100 && client == ""; i++ {
		out, _ := Server.Run(ctx, "list-clients", "-F", "#{client_name}")
		client = strings.TrimSpace(string(out))
		time.Sleep(20 * time.Millisecond)
	}
	if client == "" {
		t.Fatal("no client")
	}
	session := func() string {
		t.Helper()
		out, err := Server.Run(ctx, "list-clients", "-F", "#{client_session}")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if err := SwitchClient(ctx, client, "vm/proj/release-1%2e2"); err != nil || session() != "vm/proj/release-1%2e2" {
		t.Fatalf("switch the client: %v, on %q", err, session())
	}
	if err := Switch(ctx, "vm/proj/v1%2e3"); err != nil || session() != "vm/proj/v1%2e3" {
		t.Fatalf("switch: %v, on %q", err, session())
	}
	if err := SwitchClient(ctx, client, "vm/proj/release-1"); err == nil || session() != "vm/proj/v1%2e3" {
		t.Fatalf("the client switched to a gone session: %v, on %q", err, session())
	}
	if err := Switch(ctx, "vm/proj/v1"); err == nil || session() != "vm/proj/v1%2e3" {
		t.Fatalf("switched to a gone session: %v, on %q", err, session())
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
	for _, name := range []string{"proj/w", "proj/live", "proj/other"} {
		if _, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", name, "sleep", "600"); err != nil {
			t.Fatal(err)
		}
	}
	host := peer.Host{Name: "mac"}
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
	pane := func(session string) (tag, target, dead, cmd string) {
		t.Helper()
		out, err := Server.Run(ctx, "list-panes", "-s", "-t", "="+session, "-F", strings.Join([]string{"#{@laatmux_attach_pane}", "#{@laatmux_attach_target}", "#{pane_dead}", "#{pane_start_command}"}, tmux.Sep))
		f := strings.SplitN(strings.TrimSpace(string(out)), tmux.Sep, 4)
		if err != nil || len(f) != 4 {
			t.Fatalf("the attach pane of %s: %q %v", session, out, err)
		}
		return f[0], f[1], f[2], f[3]
	}
	// tmux quotes the start command it prints.
	if tag, target, dead, cmd := pane("mac/proj/w"); tag != "1" || target != "proj/w" || dead != "0" || strings.Trim(cmd, "\"") != AttachCommand(host, "proj/w") {
		t.Fatalf("the attach pane after adopt: %q %q %q %q", tag, target, dead, cmd)
	}
	// A live pane without its target: tagged by the adoption itself,
	// not respawned.
	if _, created, err := Ensure(ctx, Spec{Host: host, Managed: "proj/live", Name: "mac/proj/live"}); err != nil || !created {
		t.Fatalf("the live attachment: %v %v", created, err)
	}
	if _, err := Server.Run(ctx, "set-option", "-p", "-u", "-t", "=mac/proj/live:", "@laatmux_attach_target"); err != nil {
		t.Fatal(err)
	}
	before, _ := Server.Run(ctx, "list-panes", "-s", "-t", "=mac/proj/live", "-F", "#{pane_pid}")
	if name, created, err := Ensure(ctx, Spec{Host: host, Managed: "proj/live", Name: "mac/proj/live", Key: "env//r/live", Branch: "live"}); err != nil || created || name != "mac/proj/live" {
		t.Fatalf("adopt with a live pane: %q %v %v", name, created, err)
	}
	after, _ := Server.Run(ctx, "list-panes", "-s", "-t", "=mac/proj/live", "-F", "#{pane_pid}")
	if tag, target, dead, _ := pane("mac/proj/live"); tag != "1" || target != "proj/live" || dead != "0" || string(before) != string(after) {
		t.Fatalf("the live pane after adopt: %q %q %q, pid %q then %q", tag, target, dead, before, after)
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

// A worktree whose root and branch end in ; gets a workspace keyed and
// tagged with them whole, found by key the next time and by its branch:
// tmux took the ; at the end of each as a separator, so the key and the
// branch were set without it, and the second Ensure, not finding the
// key, refused the session as another root's workspace. A branch that
// is ; alone, which git takes, was passed as a bare ;, the separator
// callers passed, so the branch tag had no value: tmux refused it, and
// every Ensure of the worktree failed.
func TestEnsureKeyEndsInSemicolon(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	if _, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", "m1", "sleep", "600"); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []Spec{
		{Host: peer.Host{Name: "mac"}, Managed: "m1", Name: "mac/proj/semi", Key: "env//w/proj/semi;", Source: "/src/proj;", Branch: "semi;"},
		{Host: peer.Host{Name: "mac"}, Managed: "m1", Name: SessionName("mac", "proj", ";"), Key: "env//w/proj/;", Source: "/src/proj", Branch: ";"},
	} {
		if name, created, err := Ensure(ctx, spec); err != nil || !created || name != spec.Name {
			t.Fatalf("%s: first ensure: %q %v %v", spec.Branch, name, created, err)
		}
		if name, created, err := Ensure(ctx, spec); err != nil || created || name != spec.Name {
			t.Fatalf("%s: second ensure: %q %v %v", spec.Branch, name, created, err)
		}
		locals, err := List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		l, ok := Find(locals, spec.Key, "")
		if !ok || l.Name != spec.Name || l.Host != "mac" || l.Source != spec.Source || l.Branch != spec.Branch {
			t.Fatalf("%s: by key: %+v %v", spec.Branch, l, ok)
		}
		if l, ok := FindWorktree(locals, "env", spec.Source, spec.Branch); !ok || l.Name != spec.Name {
			t.Fatalf("%s: by branch: %+v %v", spec.Branch, l, ok)
		}
	}
}

// A spec whose name is held by the workspace of another root, a root
// with an ESC ] 0 ; x BEL and a tab in it, is a name in use, and the
// error names that root quoted, with no control byte in it: printed as
// it is, the tab would break the line and the ESC sequence set the
// terminal's title. The root is the one tmux reads back, which 3.4
// gives with the ESC and BEL escaped but the tab as it is.
func TestEnsureNameInUsePrintable(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	if _, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", "m1", "sleep", "600"); err != nil {
		t.Fatal(err)
	}
	root := "/w/proj/a\x1b]0;x\x07b\tc"
	held := Spec{Host: peer.Host{Name: "mac"}, Managed: "m1", Name: "mac/proj/x", Key: "env/" + root}
	if _, created, err := Ensure(ctx, held); err != nil || !created {
		t.Fatalf("the workspace of %q: %v %v", root, created, err)
	}
	locals, err := List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := ByName(locals, held.Name)
	if !ok || !l.Workspace() {
		t.Fatalf("no workspace %s: %+v", held.Name, locals)
	}
	back := strings.TrimPrefix(l.Key, "env/")
	_, _, err = Ensure(ctx, Spec{Host: held.Host, Managed: "m1", Name: held.Name, Key: "env//w/proj/other"})
	if err == nil || !strings.Contains(err.Error(), "is the workspace for "+strconv.Quote(back)+" on mac; name in use") || strings.ContainsFunc(err.Error(), unicode.IsControl) {
		t.Fatalf("another root's workspace, read back as %q: %v", back, err)
	}
}
