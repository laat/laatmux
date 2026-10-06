package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// A split in a plain session is the plain split in the pane's directory;
// in a workspace session it lands at the worktree root, started there
// for a local host and through ssh with cd for a remote one. The
// direction flag is passed through and the default left to tmux. A #
// in a directory, which tmux takes as a format, is written ##; the ssh
// command is not a format and keeps it as it is.
func TestSplitArgs(t *testing.T) {
	plain := protocol.Session{Name: "notes"}
	ws := protocol.Session{Name: "vm/proj/x", Key: "env//home/u/wt/proj/x", Host: "vm"}
	hashed := protocol.Session{Name: "vm/proj/x%23{pid}", Key: "env//w/proj/x#{pid}", Host: "vm"}
	local := config.Host{Host: peer.Host{Name: "mac"}}
	remote := config.Host{Host: peer.Host{Name: "vm", SSH: "vm"}}
	cases := []struct {
		name string
		dir  string
		l    protocol.Session
		h    config.Host
		want string
	}{
		{"plain", "-h", plain, config.Host{}, "split-window -h -t %3 -c /cur"},
		{"plain default direction", "", plain, config.Host{}, "split-window -t %3 -c /cur"},
		{"local workspace", "-v", ws, local, "split-window -v -t %3 -c /home/u/wt/proj/x"},
		{"remote workspace", "-h", ws, remote, "split-window -h -t %3 " + workspace.ShellCommand(remote.Host, "/home/u/wt/proj/x")},
		{"local workspace with a #", "", hashed, local, "split-window -t %3 -c /w/proj/x##{pid}"},
		{"remote workspace with a #", "", hashed, remote, "split-window -t %3 " + workspace.ShellCommand(remote.Host, "/w/proj/x#{pid}")},
	}
	for _, c := range cases {
		if got := strings.Join(splitArgs(c.dir, "%3", "/cur", c.l, c.h), " "); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
	if got := strings.Join(splitArgs("", "%3", "/c#H", plain, config.Host{}), " "); got != "split-window -t %3 -c /c##H" {
		t.Errorf("plain with a #: %s", got)
	}
}

// A split and a shell window start in their directory when it has a #
// in it, as a branch's root has in the default layout and a pane's path
// can: tmux expands the -c of split-window and new-window as a format,
// and a directory that is not there after expansion starts the pane in
// $HOME, with no error. The plain split goes by the pane's path as tmux
// reports it. The worktrees directory, which is the user's, has a #[
// in it, which tmux keeps as it is. A remote host's split and shell
// window run ssh with the root as it is: tmux does not expand a pane's
// command.
func TestSplitAndShellRootWithHash(t *testing.T) {
	// The remote host's ssh, first on the test's PATH, which tmux gives
	// a pane from the client that made it, logs the command for the
	// other side and stays up. Its alias does not resolve, should the
	// real ssh ever run.
	bin := t.TempDir()
	sshLog := filepath.Join(bin, "ssh.log")
	script := "#!/bin/sh\nfor a; do last=$a; done\nprintf '%s\\n' \"$last\" >> '" + sshLog + "'\nexec sleep 1000\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	isolatedDefault(t)
	ctx := context.Background()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("hosts:\n  - name: mac\n  - name: vm\n    ssh: laatmux-test.invalid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", cfgPath)
	run := func(args ...string) string {
		t.Helper()
		out, err := workspace.Server.Run(ctx, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	// A pane started without a command runs sleep, not the user's
	// shell, whose startup could change directory.
	run("set-option", "-g", "default-shell", "/bin/sh", ";", "set-option", "-g", "default-command", "exec sleep 1000")
	t.Setenv("TMUX", run("display-message", "-p", "#{socket_path}")+",0,0")
	panes := func(session string) map[string]bool {
		t.Helper()
		seen := map[string]bool{}
		for _, p := range strings.Fields(run("list-panes", "-s", "-t", "="+session+":", "-F", "#{pane_id}")) {
			seen[p] = true
		}
		return seen
	}
	// in is the directory of the one pane of the session not in before,
	// once it is want: tmux reads it from the pane's process, which may
	// not have changed directory yet.
	in := func(session string, before map[string]bool, want string) string {
		t.Helper()
		var added []string
		for p := range panes(session) {
			if !before[p] {
				added = append(added, p)
			}
		}
		if len(added) != 1 {
			t.Fatalf("%s: new panes %v", session, added)
		}
		var got string
		for i := 0; i < 200 && got != want; i++ {
			got = run("display-message", "-p", "-t", added[0], "#{pane_current_path}")
			time.Sleep(10 * time.Millisecond)
		}
		return got
	}
	// sshed is the n-th command ssh was given, once it has been.
	sshed := func(n int) string {
		t.Helper()
		var lines []string
		for i := 0; i < 200 && len(lines) < n; i++ {
			b, _ := os.ReadFile(sshLog)
			lines = strings.Split(string(b), "\n")
			lines = lines[:len(lines)-1]
			time.Sleep(10 * time.Millisecond)
		}
		if len(lines) < n {
			t.Fatalf("ssh ran %d times, want %d", len(lines), n)
		}
		return lines[n-1]
	}
	for i, branch := range []string{"fix#12", "x#{session_id}", "y##"} {
		root := filepath.Join(t.TempDir(), "#[scratch]", "proj", branch)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		want, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		// A plain session's pane in the root, which its command, not a
		// format, changes to.
		plain := fmt.Sprintf("plain%d", i)
		pane := run("new-session", "-d", "-s", plain, "-P", "-F", "#{pane_id}", "sh", "-c", `cd "$1" && exec sleep 1000`, "sh", root)
		if got := in(plain, nil, want); got != want {
			t.Fatalf("%s: the plain pane is in %q, want %q", branch, got, want)
		}
		before := panes(plain)
		if err := cmdSplit(ctx, []string{"-h", pane}); err != nil {
			t.Fatalf("%s: plain split: %v", branch, err)
		}
		if got := in(plain, before, want); got != want {
			t.Errorf("%s: the plain split is in %q, want %q", branch, got, want)
		}
		// A workspace session of the local host's, its pane anywhere.
		ws := fmt.Sprintf("ws%d", i)
		pane = run("new-session", "-d", "-s", ws, "-P", "-F", "#{pane_id}", "sleep 1000")
		run("set-option", "-t", "="+ws+":", "@laatmux_workspace", protocol.SessionKey("menv", root), ";", "set-option", "-t", "="+ws+":", "@laatmux_host", "mac")
		before = panes(ws)
		if err := cmdSplit(ctx, []string{"-v", pane}); err != nil {
			t.Fatalf("%s: workspace split: %v", branch, err)
		}
		if got := in(ws, before, want); got != want {
			t.Errorf("%s: the workspace split is in %q, want %q", branch, got, want)
		}
		before = panes(ws)
		t.Setenv("TMUX_PANE", pane)
		if err := cmdShell(ctx, nil); err != nil {
			t.Fatalf("%s: shell: %v", branch, err)
		}
		if got := in(ws, before, want); got != want {
			t.Errorf("%s: the shell window is in %q, want %q", branch, got, want)
		}
		// A workspace session of the remote host's.
		remote := tmux.ShellJoin([]string{"cd", root}) + ` && exec "$SHELL" -l`
		ws = fmt.Sprintf("vm%d", i)
		pane = run("new-session", "-d", "-s", ws, "-P", "-F", "#{pane_id}", "sleep 1000")
		run("set-option", "-t", "="+ws+":", "@laatmux_workspace", protocol.SessionKey("venv", root), ";", "set-option", "-t", "="+ws+":", "@laatmux_host", "vm")
		if err := cmdSplit(ctx, []string{"-h", pane}); err != nil {
			t.Fatalf("%s: remote split: %v", branch, err)
		}
		if got := sshed(2*i + 1); got != remote {
			t.Errorf("%s: the remote split ran ssh with %q, want %q", branch, got, remote)
		}
		t.Setenv("TMUX_PANE", pane)
		if err := cmdShell(ctx, nil); err != nil {
			t.Fatalf("%s: remote shell: %v", branch, err)
		}
		if got := sshed(2*i + 2); got != remote {
			t.Errorf("%s: the remote shell window ran ssh with %q, want %q", branch, got, remote)
		}
	}
}

// The command after "--" is taken verbatim, flags and target before it.
func TestSplitDashes(t *testing.T) {
	before, cmd, ok := splitDashes([]string{"proj/x", "--host", "vm", "--", "sh", "-c", "echo -- hi"})
	if !ok || strings.Join(before, " ") != "proj/x --host vm" || strings.Join(cmd, " ") != "sh -c echo -- hi" {
		t.Fatalf("%v %v %v", before, cmd, ok)
	}
	if _, _, ok := splitDashes([]string{"proj/x"}); ok {
		t.Fatal("no dashes taken as ok")
	}
	if before, cmd, ok := splitDashes([]string{"--", "make"}); !ok || len(before) != 0 || len(cmd) != 1 {
		t.Fatalf("%v %v %v", before, cmd, ok)
	}
}
