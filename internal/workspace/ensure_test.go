package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
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

// A user's after-list-sessions and after-list-panes hooks that fail
// after their listings printed: Ensure makes the session, and finds it
// again with its attach pane moved to the spec's managed session, as
// the listing of its panes has them, or left as it is, each time with a
// *tmux.HookError; list-panes' alone once the other hook is gone. A
// step that fails is the error returned, not the hook's: the adoption
// of a plain attachment lists its panes through Run, and fails on the
// hook as before.
func TestEnsureHookFails(t *testing.T) {
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
	hooks := func(args ...string) {
		t.Helper()
		if _, err := Server.Run(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
	// A session for list-sessions to print before its hook fails: on a
	// server with none, it prints nothing, and that fails as any listing.
	hooks("new-session", "-d", "-s", "boot", "sleep 600",
		tmux.Next, "set-hook", "-g", "after-list-sessions", "select-window -t nosuch:9",
		tmux.Next, "set-hook", "-g", "after-list-panes", "select-window -t nosuch:9")
	// The attach pane's target, read by show-options, which no hook
	// follows.
	target := func() string {
		t.Helper()
		out, err := Server.Run(ctx, "show-options", "-p", "-v", "-t", "=mac/w:", "@laatmux_attach_target")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	spec := Spec{Host: peer.Host{Name: "mac"}, Managed: "s1", Name: "mac/w", Key: "env//w", Branch: "w"}
	if name, created, err := Ensure(ctx, spec); name != "mac/w" || !created || !tmux.HookOnly(err) || !strings.HasPrefix(err.Error(), "tmux list-sessions ") {
		t.Fatalf("made with the hooks: %q %v %v, want mac/w made and list-sessions' HookError", name, created, err)
	}
	spec.Managed = "s2"
	if name, created, err := Ensure(ctx, spec); name != "mac/w" || created || !tmux.HookOnly(err) {
		t.Fatalf("found with the hooks: %q %v %v, want mac/w and a HookError", name, created, err)
	}
	if got := target(); got != "s2" {
		t.Fatalf("attach pane on %q after the retarget, want s2", got)
	}
	hooks("set-hook", "-gu", "after-list-sessions")
	spec.Managed = "s1"
	if _, _, err := Ensure(ctx, spec); !tmux.HookOnly(err) || !strings.HasPrefix(err.Error(), "tmux list-panes ") {
		t.Fatalf("found with the list-panes hook alone: %v, want its HookError", err)
	}
	if got := target(); got != "s1" {
		t.Fatalf("attach pane on %q after the second retarget, want s1", got)
	}
	// Its attach pane live on the spec's session: nothing to respawn.
	if _, _, err := Ensure(ctx, spec); !tmux.HookOnly(err) || !strings.HasPrefix(err.Error(), "tmux list-panes ") {
		t.Fatalf("found as it should be with the list-panes hook: %v, want its HookError", err)
	}
	hooks("set-hook", "-g", "after-list-sessions", "select-window -t nosuch:9")
	plain := Spec{Host: peer.Host{Name: "mac"}, Managed: "s2", Name: "mac/s2"}
	if _, created, err := Ensure(ctx, plain); !created || !tmux.HookOnly(err) {
		t.Fatalf("plain attachment: %v %v, want made with a HookError", created, err)
	}
	plain.Key = "env//s2"
	if _, _, err := Ensure(ctx, plain); err == nil || tmux.HookOnly(err) || !strings.HasPrefix(err.Error(), "tmux list-panes -s -t ") {
		t.Fatalf("adoption with the hooks: %v, want adopt's list-panes failure", err)
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

// A workspace whose root, source and branch have tmux.Sep in them, the
// root a newline too, is found by FindWorktree and again by its key,
// and from a pane in it, whose directory is the root, by PaneSession,
// with the pane's directory, and by Current. Split at Sep, the source
// and branch were read from the wrong fields or cut at their first Sep,
// and PaneSession found no session. Each value ends in a $ and comes
// back as written: tmux 3.4 on macOS puts a backslash before a $ that
// comes before a U+2063, which the | a separator starts with keeps
// away from a value, and Query's decoding would undo. Each has a $
// before a letter, which tmux 3.4 prints as \$ everywhere: in the root
// before its newline, where a Query that decoded the line with the
// probe only left it (#288). The user's after-display-message and
// after-list-sessions hooks print a line after the records, which
// PaneSession read into the pane's directory (#282).
func TestEnsureValuesWithSep(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	if _, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", "m1", "sleep", "600"); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "proj", "fix"+tmux.Sep+"x$a\ny$")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	src, branch := "/src/"+tmux.Sep+"$HOME/proj$", "fix"+tmux.Sep+"x$y"+tmux.Sep+"$"
	spec := Spec{Host: peer.Host{Name: "mac"}, Managed: "m1", Name: SessionName("mac", "proj", branch), Key: protocol.SessionKey("env", root), Source: src, Branch: branch}
	if name, created, err := Ensure(ctx, spec); err != nil || !created || name != spec.Name {
		t.Fatalf("ensure: %q %v %v", name, created, err)
	}
	out, err := Server.Run(ctx, "new-window", "-d", "-t", tmux.SessionTarget(spec.Name), "-c", tmux.FormatLiteral(root), "-P", "-F", "#{pane_id}", "sleep 600")
	if err != nil {
		t.Fatal(err)
	}
	pane := strings.TrimSpace(string(out))
	sock, err := Server.Run(ctx, "display-message", "-p", "#{socket_path}")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", strings.TrimSpace(string(sock))+",0,0")
	for _, hook := range []string{"after-display-message", "after-list-sessions"} {
		if _, err := Server.Run(ctx, "set-hook", "-g", hook, "display-message -p hook"); err != nil {
			t.Fatal(err)
		}
	}
	locals, err := List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := FindWorktree(locals, "env", src, branch); !ok || l.Name != spec.Name || l.Key != spec.Key || l.Host != "mac" || len(locals) != 1 {
		t.Errorf("FindWorktree: %+v %v, sessions %+v", l, ok, locals)
	}
	if name, created, err := Ensure(ctx, spec); err != nil || created || name != spec.Name {
		t.Errorf("ensure again: %q %v %v", name, created, err)
	}
	// The pane's path is read from its process, which may not have
	// changed directory yet.
	var l protocol.Session
	var dir string
	for i := 0; i < 200; i++ {
		if l, dir, err = PaneSession(ctx, pane); err == nil && dir == real {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || dir != real || l.Name != spec.Name || l.Key != spec.Key || l.Source != src || l.Branch != branch {
		t.Errorf("PaneSession: %+v %q %v", l, dir, err)
	}
	t.Setenv("TMUX_PANE", pane)
	if cur, err := Current(ctx); err != nil || cur.Name != spec.Name || cur.Key != spec.Key || cur.Source != src || cur.Branch != branch {
		t.Errorf("Current: %+v %v", cur, err)
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

// A workspace session an earlier build keyed with the root as given is
// found by its key, whatever its name, for a root with a tab or a % in
// it, which tmux gives back as written and which is stored encoded now;
// a root with %01 in it is not taken for one with the byte, which gets
// a session of its own, its key stored encoded.
func TestEnsureFindsKeyWrittenAsGiven(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	if _, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", "m1", "sleep", "600"); err != nil {
		t.Fatal(err)
	}
	host := peer.Host{Name: "mac"}
	roots := []string{"/w/proj/tab\tx", "/w/proj/100%", "/w/proj/a%01b"}
	for i, root := range roots {
		name := fmt.Sprintf("old%d", i)
		target := "=" + name + ":"
		if _, err := Server.Run(ctx, "new-session", "-d", "-s", name, placeholder,
			tmux.Next, "set-option", "-t", target, "@laatmux_workspace", "env/"+root,
			tmux.Next, "set-option", "-t", target, "@laatmux_host", "mac"); err != nil {
			t.Fatal(err)
		}
	}
	for i, root := range roots {
		spec := Spec{Host: host, Managed: "m1", Name: fmt.Sprintf("mac/new%d", i), Key: protocol.SessionKey("env", root)}
		if name, created, err := Ensure(ctx, spec); err != nil || created || name != fmt.Sprintf("old%d", i) {
			t.Errorf("%q: %q %v %v", root, name, created, err)
		}
	}
	spec := Spec{Host: host, Managed: "m1", Name: "mac/byte", Key: protocol.SessionKey("env", "/w/proj/a\x01b")}
	if name, created, err := Ensure(ctx, spec); err != nil || !created || name != spec.Name {
		t.Errorf("a\\x01b: %q %v %v", name, created, err)
	}
	if out, err := Server.Run(ctx, "show-options", "-qv", "-t", "=mac/byte:", "@laatmux_workspace"); err != nil || string(out) != "env%/w/proj/a%01b\n" {
		t.Errorf("a\\x01b: stored key %q %v", out, err)
	}
}

// A plain attachment to a session laatmux new made is made under
// <host>/<session>, tagged, found again, and its attach pane reaches
// the managed session, for names with what new takes: a #, which
// new-session expands as a format, so x#{session_id}y was made as
// mac/xy and untagged, and a run of them before a [, which it keeps; a
// space, a ; at the end, a leading = or %, a letter that is not ASCII.
// A managed session from an older laatmux new or made by hand can have
// a character tmux would not store as given, a\\b or a$b say: its
// attachment is refused before new-session, as mac/a\\b was made as
// mac/a\\\\b, untagged, and nothing is made; one that is there
// already, under any name, is reused. Neither a worktree's managed
// session nor its workspace is checked, the branch being SessionName's
// to encode: v$1, which every tmux keeps, gets both.
func TestEnsureAttachmentNames(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	host := peer.Host{Name: "mac"}
	// attached waits for the attach pane to be a client of the managed
	// session, once its tmux has started.
	attached := func(m string) {
		t.Helper()
		var clients string
		for i := 0; i < 250 && clients == ""; i++ {
			out, _ := tmux.LaatmuxServer.Run(ctx, "list-clients", "-t", "="+m, "-F", "#{client_session}")
			clients = strings.TrimSpace(string(out))
			if clients == "" {
				time.Sleep(20 * time.Millisecond)
			}
		}
		if clients != m {
			t.Errorf("%q: clients of the managed session %q", m, clients)
		}
	}
	names := []string{"notes draft", "notes#draft", "x#{session_id}y", "a##[b", "semi;", "=eq", "%pct", "ø-norsk"}
	for _, m := range names {
		if _, err := tmux.LaatmuxServer.NewSession(ctx, tmux.NewSessionOpts{Name: m, Cwd: t.TempDir(), Cmd: []string{"sleep", "600"}}); err != nil {
			t.Errorf("%q: %v", m, err)
			continue
		}
		spec := Spec{Host: host, Managed: m, Name: "mac/" + m}
		if name, created, err := Ensure(ctx, spec); err != nil || !created || name != spec.Name {
			t.Errorf("%q: %q %v %v", m, name, created, err)
			continue
		}
		if name, created, err := Ensure(ctx, spec); err != nil || created || name != spec.Name {
			t.Errorf("%q again: %q %v %v", m, name, created, err)
		}
		attached(m)
	}
	for m, want := range map[string]string{`a\\b`: `has a \`, "a$b": "has a $"} {
		if _, _, err := Ensure(ctx, Spec{Host: host, Managed: m, Name: "mac/" + m}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want a refusal with %q", m, err, want)
		}
	}
	// A managed session with a U+2063 in its name is refused under the
	// name AttachName gives it too, which has it encoded: the attach tag
	// has it as it is, and would not read back.
	sep := "a\u2063\u2063b"
	if _, _, err := Ensure(ctx, Spec{Host: host, Managed: sep, Name: AttachName("mac", sep)}); err == nil || !strings.Contains(err.Error(), "U+2063") {
		t.Errorf("%q: %v, want a refusal", sep, err)
	}
	// So is one whose name starts with a $, which the target =$0 reads
	// as the session id $0, another session's.
	if _, _, err := Ensure(ctx, Spec{Host: host, Managed: "$0", Name: AttachName("mac", "$0")}); err == nil || !strings.Contains(err.Error(), "starts with a $") {
		t.Errorf("$0: %v, want a refusal", err)
	}
	// And one with a :, which tmux 3.7 keeps in a name made by hand:
	// the attach target =c:d: is a window of a session c, so it is
	// refused for a workspace too, whose name is not checked.
	for _, spec := range []Spec{{Host: host, Managed: "c:d", Name: AttachName("mac", "c:d")}, {Host: host, Managed: "c:d", Name: "mac/proj/w", Key: "env//r/w"}} {
		if _, _, err := Ensure(ctx, spec); err == nil || !strings.Contains(err.Error(), "managed session \"c:d\" has a :, at which tmux splits a target") {
			t.Errorf("%+v: %v, want a refusal", spec, err)
		}
	}
	if locals, err := List(ctx); err != nil || len(Records(locals)) != len(names) {
		t.Errorf("after the refusals: %+v %v", locals, err)
	}
	if _, err := Server.Run(ctx, "new-session", "-d", "-s", "legacy", "sleep 600", tmux.Next, "set-option", "-t", "=legacy:", "@laatmux_attach", "mac/a$1"); err != nil {
		t.Fatal(err)
	}
	if name, created, err := Ensure(ctx, Spec{Host: host, Managed: "a$1", Name: "mac/a$1"}); err != nil || created || name != "legacy" {
		t.Fatalf("the attachment there already: %q %v %v", name, created, err)
	}
	keyed := Spec{Host: host, Managed: tmux.SessionName("proj", "v$1"), Name: SessionName("mac", "proj", "v$1"), Key: "env//r/v$1", Branch: "v$1"}
	if _, err := tmux.LaatmuxServer.NewSession(ctx, tmux.NewSessionOpts{Name: keyed.Managed, Cwd: t.TempDir(), Cmd: []string{"sleep", "600"}}); err != nil {
		t.Fatalf("the managed session for v$1: %v", err)
	}
	if name, created, err := Ensure(ctx, keyed); err != nil || !created || name != keyed.Name {
		t.Fatalf("the workspace for v$1: %q %v %v", name, created, err)
	}
	attached(keyed.Managed)
	locals, err := List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(locals) != len(names)+2 {
		t.Errorf("sessions %+v, want %d", locals, len(names)+2)
	}
	for _, m := range names {
		if l, ok := ByName(locals, "mac/"+m); !ok || l.Attach != "mac/"+m || l.Host != "mac" {
			t.Errorf("%q: %+v %v", m, l, ok)
		}
	}
}

// A worktree whose home is a session made by hand as a.b on a host's
// tmux 3.7, which keeps the ., gets its workspace session, mac/a%2eb,
// and the attach pane becomes a client of a.b: the attach target =a.b:
// reaches it, where =a.b looked for pane b of window a and the attach
// exited at once. Kill kills it. A workspace an earlier build made as
// mac/a.b on a local tmux 3.7 is found by its key and kept under its
// name, and Kill kills it too, which =mac/a.b did not find. Neither
// reaches mac/a, which =mac/a.b alone can: tmux reads its mac/a as a
// window, then as a session. A plain attachment a build before the .
// was checked made as mac/a.b is adopted as the workspace under its
// name. A tmux before 3.7 stores the . as _, and the test is skipped
// there.
func TestEnsureDottedManagedSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	out, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", "a.b", "-P", "-F", "#{session_name}", "sleep 600")
	if err != nil {
		t.Fatal(err)
	}
	if m := strings.TrimSpace(string(out)); m != "a.b" {
		t.Skipf("tmux before 3.7 stores a . in a session name as _: %q", m)
	}
	// A local session mac/a gets none of the new session's tags.
	if _, err := Server.Run(ctx, "new-session", "-d", "-s", "mac/a", "sleep 600"); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Host: peer.Host{Name: "mac"}, Managed: "a.b", Name: AttachName("mac", "a.b"), Key: "env//r/a.b", Branch: "a.b"}
	name, created, err := Ensure(ctx, spec)
	if err != nil || !created || name != "mac/a%2eb" {
		t.Fatalf("ensure: %q %v %v", name, created, err)
	}
	if locals, err := List(ctx); err != nil || len(Records(locals)) != 1 || Records(locals)[0].Name != "mac/a%2eb" {
		t.Errorf("laatmux's sessions: %+v %v", locals, err)
	}
	var clients string
	for i := 0; i < 250 && clients == ""; i++ {
		out, _ := tmux.LaatmuxServer.Run(ctx, "list-clients", "-t", "=a.b:", "-F", "#{client_session}")
		clients = strings.TrimSpace(string(out))
		if clients == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if clients != "a.b" {
		t.Errorf("clients of a.b: %q", clients)
	}
	if err := Kill(ctx, name); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if locals, err := List(ctx); err != nil || len(locals) != 1 || locals[0].Name != "mac/a" {
		t.Errorf("after the kill: %+v %v", locals, err)
	}
	if _, err := Server.Run(ctx, "new-session", "-d", "-s", "mac/a.b", "sleep 600", tmux.Next, "set-option", "-t", "=mac/a.b:", "@laatmux_workspace", spec.Key); err != nil {
		t.Fatal(err)
	}
	if name, created, err = Ensure(ctx, spec); err != nil || created || name != "mac/a.b" {
		t.Fatalf("the earlier build's workspace: %q %v %v", name, created, err)
	}
	if err := Kill(ctx, name); err != nil {
		t.Fatalf("kill %s: %v", name, err)
	}
	if locals, err := List(ctx); err != nil || len(locals) != 1 || locals[0].Name != "mac/a" {
		t.Errorf("after the kill of %s: %+v %v", name, locals, err)
	}
	// A plain attachment a build before the . was checked made as
	// mac/a.b, tagged with a.b as listed, is adopted as the workspace
	// under its name, and mac/a%2eb is not made.
	if _, err := Server.Run(ctx, "new-session", "-d", "-s", "mac/a.b", "sleep 600", tmux.Next, "set-option", "-t", "=mac/a.b:", "@laatmux_attach", "mac/a.b"); err != nil {
		t.Fatal(err)
	}
	if name, created, err = Ensure(ctx, spec); err != nil || created || name != "mac/a.b" {
		t.Fatalf("the older build's attachment: %q %v %v", name, created, err)
	}
	locals, err := List(ctx)
	if l, ok := ByName(locals, "mac/a.b"); err != nil || !ok || l.Key != spec.Key || l.Attach != "" || len(locals) != 2 {
		t.Errorf("after the adoption: %+v %v", locals, err)
	}
}

// A worktree whose home a host's tmux 3.7 lists as a.b gets its
// workspace session on any local tmux, under the name AttachName gives
// it, mac/a%2eb, tagged with its key and found by it again: a tmux
// before 3.7 stored mac/a.b as mac/a_b, and the set-option calls in
// new-session's own sequence found no session mac/a.b, which failed
// the jump and left that session untagged. The workspace is made
// whether or not the host session is there.
func TestEnsureDottedHomeOnAnyTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	spec := Spec{Host: peer.Host{Name: "mac"}, Managed: "a.b", Name: AttachName("mac", "a.b"), Key: "env//r/a.b", Branch: "a.b"}
	name, created, err := Ensure(ctx, spec)
	if err != nil || !created || name != "mac/a%2eb" {
		t.Fatalf("ensure: %q %v %v", name, created, err)
	}
	locals, err := List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := Find(locals, spec.Key, ""); !ok || l.Name != name || len(locals) != 1 {
		t.Errorf("by key: %+v %v, of %+v", l, ok, locals)
	}
	if name, created, err := Ensure(ctx, spec); err != nil || created || name != "mac/a%2eb" {
		t.Errorf("ensure again: %q %v %v", name, created, err)
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
// given the target, and found by key after; one an older build named
// after a managed session with a $ in it is adopted by a workspace named
// after that session, but one named otherwise is not, nor by a workspace
// named after a worktree whose agent is in the session; one to another
// managed session is still a name in use.
func TestEnsureAdoptsAttachment(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	for _, name := range []string{"proj/w", "proj/live", `proj/r\x`, "proj/live2", "proj/other"} {
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
	// Its root has a newline, so the key is stored encoded.
	live := Spec{Host: host, Managed: "proj/live", Name: "mac/proj/live", Key: protocol.SessionKey("env", "/r/live\nx"), Branch: "live"}
	if name, created, err := Ensure(ctx, live); err != nil || created || name != "mac/proj/live" {
		t.Fatalf("adopt with a live pane: %q %v %v", name, created, err)
	}
	after, _ := Server.Run(ctx, "list-panes", "-s", "-t", "=mac/proj/live", "-F", "#{pane_pid}")
	if tag, target, dead, _ := pane("mac/proj/live"); tag != "1" || target != "proj/live" || dead != "0" || string(before) != string(after) {
		t.Fatalf("the live pane after adopt: %q %q %q, pid %q then %q", tag, target, dead, before, after)
	}
	// Found by key the next time, nothing created.
	for _, spec := range []Spec{keyed, live} {
		if name, created, err := Ensure(ctx, spec); err != nil || created || name != spec.Name {
			t.Fatalf("ensure %s after adopt: %q %v %v", spec.Name, name, created, err)
		}
	}
	// Its agent moved to a session with a U+2063 in its name: refused
	// before the attach pane is pointed at it, which keeps its target.
	moved := keyed
	// So with one whose name starts with a $, which the target reads as
	// a session id.
	for m, want := range map[string]string{"proj/w\u2063\u2063x": "U+2063", "$0": "starts with a $"} {
		moved.Managed = m
		if _, _, err := Ensure(ctx, moved); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("the moved agent's session %q: %v, want a refusal", m, err)
		}
		if _, target, _, _ := pane("mac/proj/w"); target != "proj/w" {
			t.Fatalf("the attach pane's target after the refusal for %q: %q", m, target)
		}
	}
	// One an older build named after a managed session with a $ in it,
	// as listed, which AttachName now encodes, is adopted under its own
	// name by a workspace named after that session, and nothing is made
	// under the spec's. tmux 3.2 to 3.4 store such a $ escaped, so the
	// older build made no session of that name there.
	legacy := Spec{Host: host, Managed: "proj/fix$HOME", Name: AttachName("mac", "proj/fix$HOME"), Key: "env//r/fix$HOME", Branch: "fix$HOME"}
	if _, err := Server.Run(ctx, "new-session", "-d", "-s", "mac/proj/fix$HOME", "sleep 600", tmux.Next, "set-option", "-t", "=mac/proj/fix$HOME:", "@laatmux_attach", "mac/proj/fix$HOME"); err != nil {
		t.Logf("no session of the older build's name: %v", err)
	} else {
		if name, created, err := Ensure(ctx, legacy); err != nil || created || name != "mac/proj/fix$HOME" {
			t.Fatalf("adopt the older build's attachment: %q %v %v", name, created, err)
		}
		locals, err = List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if l, ok := Find(locals, legacy.Key, ""); !ok || l.Name != "mac/proj/fix$HOME" || l.Attach != "" {
			t.Fatalf("after adopting the older build's attachment: %+v %v", l, ok)
		}
		if l, ok := ByName(locals, legacy.Name); ok {
			t.Fatalf("a session made under the spec's name: %+v", l)
		}
	}
	// A session of the older build's name without its tag, the user's
	// own say, is not adopted: the workspace is made beside it.
	other := Spec{Host: host, Managed: "proj/v$2", Name: AttachName("mac", "proj/v$2"), Key: "env//r/v$2", Branch: "v$2"}
	if _, err := Server.Run(ctx, "new-session", "-d", "-s", "mac/proj/v$2", "sleep 600", tmux.Next, "has-session", "-t", "=mac/proj/v$2:"); err != nil {
		t.Logf("no session of the older build's name: %v", err)
	} else {
		if name, created, err := Ensure(ctx, other); err != nil || !created || name != "mac/proj/v%242" {
			t.Fatalf("the workspace beside the untagged session: %q %v %v", name, created, err)
		}
		locals, err = List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if l, ok := ByName(locals, "mac/proj/v$2"); !ok || l.Key != "" || l.Attach != "" {
			t.Fatalf("the untagged session after: %+v %v", l, ok)
		}
	}
	// One under another name, renamed by hand say, is not adopted by a
	// workspace named after the session, here one AttachName renames: it
	// may be another worktree's to adopt by its name. The workspace is
	// made, the attachment left.
	renamed := `proj/r\\x` // as tmux lists proj/r\x
	if _, created, err := Ensure(ctx, Spec{Host: host, Managed: renamed, Name: "mac/old/r"}); err != nil || !created {
		t.Fatalf("the attachment named otherwise: %v %v", created, err)
	}
	if name, created, err := Ensure(ctx, Spec{Host: host, Managed: renamed, Name: AttachName("mac", renamed), Key: "env//r/r", Branch: "r"}); err != nil || !created || name != "mac/proj/r%5cx" {
		t.Fatalf("the workspace beside it: %q %v %v", name, created, err)
	}
	locals, err = List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := ByName(locals, "mac/old/r"); !ok || l.Attach != "mac/"+renamed || l.Key != "" {
		t.Fatalf("the attachment named otherwise after: %+v %v", l, ok)
	}
	// A workspace named otherwise, after a worktree whose root agent was
	// moved into the session, leaves the plain attachment to it alone,
	// named after the session as it is, and the attachment is still
	// found by a plain spec.
	if _, created, err := Ensure(ctx, Spec{Host: host, Managed: "proj/live2", Name: "mac/proj/live2"}); err != nil || !created {
		t.Fatalf("the attachment to the agent's session: %v %v", created, err)
	}
	if name, created, err := Ensure(ctx, Spec{Host: host, Managed: "proj/live2", Name: "mac/proj/a", Key: "env//r/a", Branch: "a"}); err != nil || !created || name != "mac/proj/a" {
		t.Fatalf("the lost home's workspace: %q %v %v", name, created, err)
	}
	if name, created, err := Ensure(ctx, Spec{Host: host, Managed: "proj/live2", Name: "mac/proj/live2"}); err != nil || created || name != "mac/proj/live2" {
		t.Fatalf("the attachment to the agent's session after: %q %v %v", name, created, err)
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
// with an ESC ] 0 ; x BEL, a tab and a C1 CSI in it, is a name in use,
// and the error names that root quoted, with no control byte in it:
// printed as it is, the tab would break the line and the ESC sequence
// set the terminal's title. The root is the one tmux reads back, which
// 3.4 and 3.5 give with the ESC and BEL escaped but the tab and the CSI
// as they are.
func TestEnsureNameInUsePrintable(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	if _, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", "m1", "sleep", "600"); err != nil {
		t.Fatal(err)
	}
	root := "/w/proj/a\x1b]0;x\x07b\tc\u009b"
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

// A workspace whose root, source and branch have a $ before a letter,
// attached to a managed session whose name has one, reads back as
// given, though tmux 3.4 prints such a $ as \$. A second Ensure finds
// it by its key and finds its attach pane on the managed session, and
// leaves that pane running rather than respawning it; Current and
// PaneSession, which ask the server TMUX names, give its key from
// inside it, and a pane's path. tmux 3.2 to 3.4 store the managed
// session's name m$x as m\$x, and the spec names it as stored, as the
// host's record does. The user's server has after-hooks that print a
// line after the records of list-sessions and list-panes, which the
// readers skip.
func TestEnsureValuesWithDollar(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	startServers(t)
	for _, hook := range []string{"after-list-sessions", "after-list-panes"} {
		if _, err := Server.Run(ctx, "set-hook", "-g", hook, "display-message -p hook"); err != nil {
			t.Fatal(err)
		}
	}
	out, err := tmux.LaatmuxServer.Run(ctx, "new-session", "-d", "-s", "m$x", "-P", "-F", "#{session_id}", "sleep 600")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(out))
	out, err = tmux.LaatmuxServer.Query(ctx, "#{session_name}", "display-message", "-p", "-t", id)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "proj", "fix$foo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{Host: peer.Host{Name: "mac"}, Managed: strings.TrimSpace(string(out)), Name: SessionName("mac", "proj", "fix$foo"),
		Key: "env/" + root, Source: "/src/$HOME/proj", Branch: "fix$foo"}
	name, created, err := Ensure(ctx, spec)
	if err != nil || !created || name != spec.Name {
		t.Fatalf("first ensure: %q %v %v", name, created, err)
	}
	pane := AttachPane(ctx, name)
	pid := func() string {
		t.Helper()
		out, err := Server.Run(ctx, "display-message", "-p", "-t", pane, "#{pane_pid}")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	// Attached, the pane does not die before the second Ensure looks.
	for i := 0; ; i++ {
		if out, _ := tmux.LaatmuxServer.Run(ctx, "list-clients", "-t", id, "-F", "#{client_pid}"); strings.TrimSpace(string(out)) != "" {
			break
		}
		if i > 200 {
			t.Fatalf("the attach pane %q is not attached to %q", pane, spec.Managed)
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := pid()
	if name, created, err := Ensure(ctx, spec); err != nil || created || name != spec.Name {
		t.Fatalf("second ensure: %q %v %v", name, created, err)
	}
	if AttachPane(ctx, name) != pane || pid() != before {
		t.Errorf("the attach pane was respawned: %s pid %s, was %s", AttachPane(ctx, name), pid(), before)
	}
	locals, err := List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := Find(locals, spec.Key, ""); !ok || l.Source != spec.Source || l.Branch != spec.Branch {
		t.Errorf("by key: %+v %v", l, ok)
	}
	out, err = Server.Run(ctx, "new-window", "-d", "-t", "="+name+":", "-c", tmux.FormatLiteral(root), "-P", "-F", "#{pane_id}", "sleep 600")
	if err != nil {
		t.Fatal(err)
	}
	shell := strings.TrimSpace(string(out))
	sock, err := Server.Run(ctx, "display-message", "-p", "#{socket_path}")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", strings.TrimSpace(string(sock))+",1,0")
	t.Setenv("TMUX_PANE", shell)
	if l, err := Current(ctx); err != nil || l.Key != spec.Key {
		t.Errorf("current: %+v %v", l, err)
	}
	// The pane's path is read from its process, which may not have
	// changed directory yet.
	var cwd string
	for i := 0; i < 200 && cwd != real; i++ {
		l, got, err := PaneSession(ctx, shell)
		if err != nil || l.Key != spec.Key {
			t.Fatalf("pane session: %+v %v", l, err)
		}
		cwd = got
		time.Sleep(10 * time.Millisecond)
	}
	if cwd != real {
		t.Errorf("pane path %q, want %q", cwd, real)
	}
}
