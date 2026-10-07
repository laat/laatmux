package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/workspace"
)

// isolatedDefault points the default tmux server at a socket directory
// of the test's own, starts it with no config at all, not the user's
// nor a system-wide one that might source it, and kills it at the end.
// HOME and the state directory are the test's too. The directory is
// under /tmp: a unix socket's path is short on macOS. Before the kill,
// the test fails on anything selfStarts finds on the server.
func isolatedDefault(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir, err := os.MkdirTemp("/tmp", "lmxt")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("LAATMUX_HOME", dir)
	t.Setenv("TMUX", "")
	t.Cleanup(func() {
		for _, l := range selfStarts(context.Background()) {
			t.Errorf("%s %s", selfStartTest, l)
		}
		_, _ = workspace.Server.Run(context.Background(), "kill-server")
		os.RemoveAll(dir)
	})
	// The server starts here, with -f /dev/null; every later command
	// reaches it through workspace.Server's socket and reads no config.
	if out, err := exec.Command("tmux", "-L", "default", "-f", "/dev/null", "new-session", "-d", "-s", "boot", "sleep 1000").CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
	}
}

// selfStarts is what the default tmux server can still start as this
// test binary: the lines that name it in the panes' start commands, a
// pane running or kept dead by remain-on-exit, in the global session
// and window hooks, where the sidebar sets its own, and in the key
// bindings of every table. TestMain's guard ends such a start, and its
// marker fails the run, but only a start that gets that far: one in a
// pane or a hook's job dies unrecorded when its server is killed first.
// These lines are there before the kill, and a hook or a key that never
// fired is a start waiting to happen. No tmux at all, or no server,
// starts nothing: the first listing that finds no server ends the look.
// Any other error is a line, since the server was not seen. -N on each:
// list-keys would start a server where none runs, and with it read the
// config under HOME, the user's in TestMain.
func selfStarts(ctx context.Context) []string {
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return []string{err.Error()}
	}
	var found []string
	for _, args := range [][]string{
		{"list-panes", "-a", "-F", "#{pane_start_command}"},
		{"show-hooks", "-g"},
		{"show-hooks", "-gw"},
		{"list-keys"},
	} {
		out, err := workspace.Server.Run(ctx, append([]string{"-N"}, args...)...)
		if tmux.NoServer(err) {
			return found
		}
		if err != nil {
			found = append(found, err.Error())
			continue
		}
		for _, l := range strings.Split(string(out), "\n") {
			if namesPath(l, self) {
				found = append(found, l)
			}
		}
	}
	return found
}

// namesPath reports whether a line tmux printed names path. Both are
// compared bare of what the quoting adds: tmux prints a value in double
// quotes with a \ before a ", \ or $, ShellJoin closes its single quote
// before a ' and escapes it with a \, and the sidebar's hooks and keys
// have the path as a format literal, each # doubled but before a [. So
// a path that differs from it only in those, such as a##b for a#b, is
// taken for it, and a path with a control character, which tmux prints
// as an escape, is not found.
func namesPath(l, path string) bool {
	return strings.Contains(bare(l), bare(path))
}

// bare is s without a \, ' or ", and with each run of # as one #.
func bare(s string) string {
	return hashRun.ReplaceAllLiteralString(strings.NewReplacer(`\`, "", `'`, "", `"`, "").Replace(s), "#")
}

var hashRun = regexp.MustCompile(`#+`)

// selfStartTest is what a test says, failed by isolatedDefault for a
// line selfStarts found; selfStartRun is what the run says, failed by
// TestMain for one on the run's own default server.
const (
	selfStartTest = "the test's tmux server can start this test binary:"
	selfStartRun  = "the run fails: its default tmux server can start this test binary:"
)

// A test whose server can still start this binary fails, whether or
// not the start reaches TestMain's guard before the server is killed:
// for a pane's start command, the sidebar's, in neither the current
// window nor the current session, and once the guard has ended the
// start, in a pane remain-on-exit keeps; for the sidebar's hooks,
// session and window; and for its jump keys and a key of the prefix
// table. The hooks and the keys never fire. A run whose own default
// server, one started without isolatedDefault, has the jump keys fails
// after its tests pass. Hooks and keys that name another binary of the
// same name, or a path with a # where this one has none, pass. The
// binary linked under a directory with a #, a quote, a \ and a $ in its
// name is found as its pane and its keys print it. Each in a run of its
// own, so the failure is that run's.
func TestSelfStarts(t *testing.T) {
	ctx := context.Background()
	if mode := os.Getenv("LAATMUX_TEST_SELF_START"); mode != "" {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "pane":
			isolatedDefault(t)
			must(workspace.Server.Run(ctx, "set-option", "-gw", "remain-on-exit", "on"))
			w := strings.TrimSpace(string(must(workspace.Server.Run(ctx, "new-window", "-d", "-t", "boot:", "-P", "-F", "#{window_id}", "sleep 1000"))))
			id := strings.TrimSpace(string(must(workspace.Server.Run(ctx, "split-window", "-d", "-t", w, "-P", "-F", "#{pane_id}", tmux.ShellJoin([]string{self, "sidebar", "pane"})))))
			dead := ""
			for i := 0; i < 500 && dead != "1"; i++ {
				time.Sleep(20 * time.Millisecond)
				dead = strings.TrimSpace(string(must(workspace.Server.Run(ctx, "display", "-p", "-t", id, "#{pane_dead}"))))
			}
			if dead != "1" {
				t.Fatal("the pane did not end in 10s")
			}
			// A later session is tmux's current one: the pane is in
			// neither the current window nor the current session.
			must(workspace.Server.Run(ctx, "new-session", "-d", "-s", "later", "sleep 1000"))
		case "hooks":
			isolatedDefault(t)
			if err := setSidebarHooks(ctx, self); err != nil {
				t.Fatal(err)
			}
		case "keys":
			isolatedDefault(t)
			if err := bindJumpKeys(ctx, self); err != nil {
				t.Fatal(err)
			}
			must(workspace.Server.Run(ctx, "bind-key", "-T", "prefix", "F12", jumpKeyCmd(self, 1)))
		case "run":
			// Under the run's TMUX_TMPDIR, which TestMain set.
			if out, err := exec.Command("tmux", "-L", "default", "-f", "/dev/null", "new-session", "-d", "-s", "boot", "sleep 1000").CombinedOutput(); err != nil {
				t.Fatalf("start tmux: %v: %s", err, out)
			}
			if err := bindJumpKeys(ctx, self); err != nil {
				t.Fatal(err)
			}
		case "other":
			isolatedDefault(t)
			if err := setSidebarHooks(ctx, filepath.Join(t.TempDir(), filepath.Base(self))); err != nil {
				t.Fatal(err)
			}
			// A # where this binary's path has none makes another path.
			if err := bindJumpKeys(ctx, filepath.Join(filepath.Dir(self)+"#", filepath.Base(self))); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("mode %q", mode)
		}
		return
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// This binary under a directory whose name has what the shell's and
	// tmux's quoting, and a format literal, write otherwise: a hard link,
	// or a copy from another file system.
	odd := filepath.Join(t.TempDir(), `a#b##c#[d'e"f\g$h`, filepath.Base(self))
	if err := os.Mkdir(filepath.Dir(odd), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(self, odd); err != nil {
		b, err := os.ReadFile(self)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(odd, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// As its os.Executable names it on Linux, which reads /proc/self/exe:
	// a temporary directory under a symlink resolved.
	if odd, err = filepath.EvalSymlinks(odd); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mode, exe string
		// Each a line of the failure, with the binary's path in it; none
		// for a run that passes.
		want []string
	}{
		{"pane", self, []string{"sidebar pane"}},
		{"hooks", self, []string{"after-new-window[9101]", "window-resized[9105]"}},
		{"keys", self, []string{"-T root ", "-T prefix ", "M-1 ", "M-9 ", "F12 "}},
		{"run", self, []string{"M-1 ", "M-9 "}},
		{"other", self, nil},
		{"pane", odd, []string{"sidebar pane"}},
		{"keys", odd, []string{"-T root ", "-T prefix "}},
	} {
		run := exec.Command(c.exe, "-test.run=^TestSelfStarts$")
		run.Env = append(os.Environ(), "LAATMUX_TEST_SELF_START="+c.mode)
		b, err := run.CombinedOutput()
		out := string(b)
		if c.want == nil {
			if err != nil || !strings.Contains(out, "PASS\n") {
				t.Errorf("%s %s: %v\n%s", c.mode, c.exe, err, out)
			}
			continue
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Errorf("%s %s: %v\n%s", c.mode, c.exe, err, out)
			continue
		}
		// The test fails; with "run", the test passes and the run fails
		// after it.
		msg, test := selfStartTest, "--- FAIL: TestSelfStarts"
		if c.mode == "run" {
			msg, test = selfStartRun, "PASS\n"
		}
		if !strings.Contains(out, test) {
			t.Errorf("%s %s: no %q\n%s", c.mode, c.exe, test, out)
		}
		for _, w := range c.want {
			found := false
			for _, l := range strings.Split(out, "\n") {
				if i := strings.Index(l, msg); i >= 0 && namesPath(l[i:], c.exe) && strings.Contains(l[i:], w) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s %s: no line %q with the binary and %q\n%s", c.mode, c.exe, msg, w, out)
			}
		}
	}
}

// selfStarts starts no server where none runs, so reads no config, and
// finds nothing there; a server it cannot see, behind a socket
// directory tmux refuses as unsafe, is a line for each listing.
func TestSelfStartsUnseen(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	ctx := context.Background()
	dir, err := os.MkdirTemp("/tmp", "lmxt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("TMUX_TMPDIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	read := filepath.Join(dir, "read")
	if err := os.WriteFile(filepath.Join(dir, ".tmux.conf"), []byte("run-shell "+tmux.ShellJoin([]string{"touch " + read})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = workspace.Server.Run(context.Background(), "kill-server") })
	if found := selfStarts(ctx); len(found) != 0 {
		t.Errorf("with no server: %q", found)
	}
	if _, err := os.Stat(read); err == nil {
		t.Error("a server started, and read the config")
	}
	unsafe := filepath.Join(dir, "unsafe")
	sock := filepath.Join(unsafe, fmt.Sprintf("tmux-%d", os.Getuid()))
	if err := os.MkdirAll(sock, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sock, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", unsafe)
	found := selfStarts(ctx)
	if len(found) != 4 {
		t.Errorf("behind an unsafe directory: %q, want a line for each listing", found)
	}
	for _, l := range found {
		if !strings.HasPrefix(l, "tmux -N ") {
			t.Errorf("behind an unsafe directory: %q, not a tmux error", l)
		}
	}
}

// A sidebar split off a detached session's 80 columns grows with the
// window when a client switches to it; fit puts it back to the width.
// A window without a sidebar, and a sidebar already at the width, are
// left alone.
func TestSidebarFit(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		out, err := workspace.Server.Run(ctx, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	window := run("new-session", "-d", "-s", "s", "-x", "80", "-y", "24", "-P", "-F", "#{window_id}", "sleep 1000")
	id := run("split-window", "-d", "-h", "-b", "-f", "-l", "35", "-t", window, "-P", "-F", "#{pane_id}", "sleep 1000")
	run("set-option", "-p", "-t", id, sidebarTag, "1")
	other := run("new-window", "-d", "-t", "s:", "-P", "-F", "#{window_id}", "sleep 1000")
	run("resize-window", "-t", window, "-x", "172", "-y", "40")
	width := func() string { return run("display", "-p", "-t", id, "#{pane_width}") }
	if w := width(); w == "35" {
		t.Fatalf("the window grew and the sidebar did not: %s", w)
	}
	cfg := config.Config{Sidebar: config.Sidebar{Width: "35"}}
	if err := sidebarFit(ctx, cfg, window); err != nil {
		t.Fatal(err)
	}
	if w := width(); w != "35" {
		t.Fatalf("after fit: %s", w)
	}
	if err := sidebarFit(ctx, cfg, window); err != nil {
		t.Fatal("fit at the width:", err)
	}
	if err := sidebarFit(ctx, cfg, other); err != nil {
		t.Fatal("fit without a sidebar:", err)
	}
	// Zoomed on the main pane, the window resized: the sidebar is put
	// back and the window stays zoomed on the same pane.
	main := run("display", "-p", "-t", window+".1", "#{pane_id}")
	if main == id {
		main = run("display", "-p", "-t", window+".0", "#{pane_id}")
	}
	run("select-pane", "-t", main)
	run("resize-pane", "-Z", "-t", main)
	run("resize-window", "-t", window, "-x", "100", "-y", "40")
	run("resize-window", "-t", window, "-x", "200", "-y", "40")
	if err := sidebarFit(ctx, cfg, window); err != nil {
		t.Fatal(err)
	}
	if z := run("display", "-p", "-t", window, "#{window_zoomed_flag}"); z != "1" {
		t.Fatal("fit unzoomed the window")
	}
	if a := run("display", "-p", "-t", window, "#{pane_id}"); a != main {
		t.Fatalf("zoomed on %s, want %s", a, main)
	}
	run("resize-pane", "-Z", "-t", main)
	if w := width(); w != "35" {
		t.Fatalf("after unzoom: %s", w)
	}
	// A strip on top: fit sets its height, full width.
	top := run("new-window", "-d", "-t", "s:", "-P", "-F", "#{window_id}", "sleep 1000")
	tcfg := config.Config{Sidebar: config.Sidebar{Position: "top"}}
	strip := run(append(sidebarSplit(tcfg, 0), "-t", top, "-P", "-F", "#{pane_id}", "sleep 1000")...)
	run("set-option", "-p", "-t", strip, sidebarTag, "1")
	run("resize-window", "-t", top, "-x", "172", "-y", "50")
	run("resize-pane", "-t", strip, "-y", "10")
	if err := sidebarFit(ctx, tcfg, top); err != nil {
		t.Fatal(err)
	}
	if h, w := run("display", "-p", "-t", strip, "#{pane_height}"), run("display", "-p", "-t", strip, "#{pane_width}"); h != "3" || w != "172" {
		t.Fatalf("a strip after fit: %s lines, %s wide", h, w)
	}
}

// A narrow window gives the sidebar half; a border dragged by hand is
// put back at the next resize; a dead sidebar pane is left alone.
func TestSidebarFitBounds(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		out, err := workspace.Server.Run(ctx, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	cfg := config.Config{Sidebar: config.Sidebar{Width: "35"}}
	window := run("new-session", "-d", "-s", "n", "-x", "120", "-y", "30", "-P", "-F", "#{window_id}", "sleep 1000")
	id := run("split-window", "-d", "-h", "-b", "-f", "-l", "35", "-t", window, "-P", "-F", "#{pane_id}", "sleep 1000")
	run("set-option", "-p", "-t", id, sidebarTag, "1")
	width := func() string { return run("display", "-p", "-t", id, "#{pane_width}") }
	fit := func() {
		t.Helper()
		if err := sidebarFit(ctx, cfg, window); err != nil {
			t.Fatal(err)
		}
	}
	run("resize-window", "-t", window, "-x", "50", "-y", "30")
	fit()
	if w := width(); w != "25" {
		t.Fatalf("narrow: %s, want half of 50", w)
	}
	run("resize-window", "-t", window, "-x", "160", "-y", "30")
	fit()
	if w := width(); w != "35" {
		t.Fatalf("wide again: %s", w)
	}
	// Dragged, then any resize: the configured width again.
	run("resize-pane", "-t", id, "-x", "50")
	run("resize-window", "-t", window, "-x", "160", "-y", "20")
	fit()
	if w := width(); w != "35" {
		t.Fatalf("after a resize: %s", w)
	}
	// A dead sidebar pane is not resized.
	run("set-option", "-p", "-t", id, "remain-on-exit", "on")
	run("respawn-pane", "-k", "-t", id, "true")
	for i := 0; i < 50 && run("display", "-p", "-t", id, "#{pane_dead}") != "1"; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	run("resize-pane", "-t", id, "-x", "40")
	run("resize-window", "-t", window, "-x", "180", "-y", "20")
	was := width()
	fit()
	if w := width(); w != was {
		t.Fatalf("a dead sidebar was resized: %s, was %s", w, was)
	}
}

// The hooks on the server run the binary as the table says, with the
// resized window's id expanded; off removes them. The binary here is a
// script that records its arguments, never the test binary.
func TestSidebarHooksRun(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	dir := t.TempDir()
	logf := filepath.Join(dir, "args")
	exe := filepath.Join(dir, "fake")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho \"$@\" >> "+logf+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := setSidebarHooks(ctx, exe); err != nil {
		t.Fatal(err)
	}
	out, err := workspace.Server.Run(ctx, "show-hooks", "-gw", "window-resized")
	if err != nil || !strings.Contains(string(out), "window-resized[9105]") {
		t.Fatalf("hook not set: %s %v", out, err)
	}
	window := strings.TrimSpace(string(must(workspace.Server.Run(ctx, "display", "-p", "-t", "boot", "#{window_id}"))))
	must(workspace.Server.Run(ctx, "resize-window", "-t", window, "-x", "150", "-y", "30"))
	var got string
	for i := 0; i < 100; i++ {
		b, _ := os.ReadFile(logf)
		if got = string(b); strings.Contains(got, "sidebar fit "+window) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(got, "sidebar fit "+window) {
		t.Fatalf("the resize hook ran %q, want sidebar fit %s", got, window)
	}
	for _, h := range sidebarHooks {
		name := strings.SplitN(h.hook, "[", 2)[0]
		must(workspace.Server.Run(ctx, "set-hook", "-gu", h.hook))
		out, _ := workspace.Server.Run(ctx, "show-hooks", "-gw", name)
		out2, _ := workspace.Server.Run(ctx, "show-hooks", "-g", name)
		if strings.Contains(string(out)+string(out2), h.hook) {
			t.Fatalf("%s left after unset", h.hook)
		}
	}
	// With the sidebar on for one session, the hooks stay global, so a
	// user's own global hook of the same name runs on; attach reads
	// the sessions option and adds a pane only in the session named,
	// and off unsets the option.
	os.Remove(logf)
	sid := strings.TrimSpace(string(must(workspace.Server.Run(ctx, "display", "-p", "-t", "boot", "#{session_id}"))))
	must(workspace.Server.Run(ctx, "set-hook", "-g", "after-new-window[0]", "run-shell -b \"echo user >> "+logf+"\""))
	must(workspace.Server.Run(ctx, "set-option", "-s", sessionsTag, sid))
	if err := setSidebarHooks(ctx, exe); err != nil {
		t.Fatal(err)
	}
	if on, err := sidebarHooksSet(ctx); err != nil || !on {
		t.Fatalf("hooks not seen as on: %v %v", on, err)
	}
	must(workspace.Server.Run(ctx, "new-session", "-d", "-s", "other", "sleep 1000"))
	otherWin := strings.TrimSpace(string(must(workspace.Server.Run(ctx, "new-window", "-d", "-t", "other:", "-P", "-F", "#{window_id}", "sleep 1000"))))
	bootWin := strings.TrimSpace(string(must(workspace.Server.Run(ctx, "new-window", "-d", "-t", "boot:", "-P", "-F", "#{window_id}", "sleep 1000"))))
	for i := 0; i < 100; i++ {
		b, _ := os.ReadFile(logf)
		if got = string(b); strings.Contains(got, "sidebar attach "+bootWin+" "+sid) && strings.Count(got, "user") >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(logf)
	if got = string(b); !strings.Contains(got, "sidebar attach "+bootWin+" "+sid) || !strings.Contains(got, "sidebar attach "+otherWin) || strings.Count(got, "user") < 2 {
		t.Fatalf("hooks ran %q: want attach for both windows with the session and the user's hook twice", got)
	}
	// attach itself: a window in a session not named gets no pane.
	otherSid := strings.TrimSpace(string(must(workspace.Server.Run(ctx, "display", "-p", "-t", "other", "#{session_id}"))))
	if sessions, err := sidebarSessions(ctx); err != nil || len(sessions) != 1 || sessions[0] != sid {
		t.Fatalf("sessions option: %v %v", sessions, err)
	}
	if err := sidebarAttach(ctx, otherWin, otherSid); err != nil {
		t.Fatal(err)
	}
	if out := strings.TrimSpace(string(must(workspace.Server.Run(ctx, "list-panes", "-t", otherWin, "-F", "#{"+sidebarTag+"}")))); strings.Contains(out, "1") {
		t.Fatal("a pane added in a session not named")
	}
	must(workspace.Server.Run(ctx, "set-option", "-su", sessionsTag))
	must(workspace.Server.Run(ctx, "set-hook", "-gu", "after-new-window[0]"))
	for _, h := range sidebarHooks {
		must(workspace.Server.Run(ctx, "set-hook", "-gu", h.hook))
	}
}

// run-shell expands its command as a format, so a hook or a jump key
// with laatmux's path in it as it is runs another path when the path
// has a #{ or a ## in it, as a worktree's branch can. The binary here
// is a script under such a directory, with a #[ that tmux keeps as it
// is, that records its arguments: the resize hook and a key pressed
// through a client both run it, with the window and the client still
// expanded.
func TestSidebarExeFormat(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(string(must(workspace.Server.Run(ctx, args...))))
	}
	logf := filepath.Join(t.TempDir(), "args")
	dir := filepath.Join(t.TempDir(), "x#{session_id}y##z#[w")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "laatmux")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho \"$@\" >> "+tmux.ShellJoin([]string{logf})+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wait := func(want string) {
		t.Helper()
		var got string
		for i := 0; i < 100; i++ {
			b, _ := os.ReadFile(logf)
			if got = string(b); strings.Contains(got, want) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("ran %q, want %q", got, want)
	}
	if err := setSidebarHooks(ctx, exe); err != nil {
		t.Fatal(err)
	}
	window := run("display", "-p", "-t", "boot", "#{window_id}")
	run("resize-window", "-t", window, "-x", "150", "-y", "30")
	wait("sidebar fit " + window + "\n")
	// send-keys -K, which looks the key up as if the client typed it,
	// needs tmux 3.4.
	if out, err := exec.Command("tmux", "-V").Output(); err == nil {
		if v := strings.TrimPrefix(strings.TrimSpace(string(out)), "tmux "); v < "3.4" {
			t.Skipf("tmux %s has no send-keys -K", v)
		}
	}
	if err := bindJumpKeys(ctx, exe); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("tmux", "-L", "default", "-C", "attach", "-t", "boot")
	in, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	c.Stdout = io.Discard
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); _ = c.Process.Kill(); _ = c.Wait() })
	client := ""
	for i := 0; i < 50 && client == ""; i++ {
		time.Sleep(100 * time.Millisecond)
		client = run("list-clients", "-F", "#{client_name}")
	}
	if client == "" {
		t.Fatal("no client attached")
	}
	run("send-keys", "-K", "-c", client, "M-3")
	wait("sidebar jump 3 -t " + window + " -c " + client + "\n")
}

// on --session names the session in the option, twice once, and kills
// the tagged panes elsewhere; a plain on clears the option; off unsets
// the option, the hooks and the jump keys.
func TestSidebarScopeAndOff(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(string(must(workspace.Server.Run(ctx, args...))))
	}
	run("new-session", "-d", "-s", "other", "sleep 1000")
	t.Setenv("TMUX", "")
	// The session display-message names with no client attached is
	// tmux's choice: the other session is whichever it did not.
	target, err := scopeSidebar(ctx, true)
	if err != nil || target == "" {
		t.Fatalf("scope: %q %v", target, err)
	}
	elsewhere := "boot"
	if run("display", "-p", "-t", "boot", "#{session_id}") == target {
		elsewhere = "other"
	}
	otherPane := run("split-window", "-d", "-h", "-t", elsewhere+":", "-P", "-F", "#{pane_id}", "sleep 1000")
	run("set-option", "-p", "-t", otherPane, sidebarTag, "1")
	ownPane := run("split-window", "-d", "-h", "-t", target+":", "-P", "-F", "#{pane_id}", "sleep 1000")
	run("set-option", "-p", "-t", ownPane, sidebarTag, "1")
	if _, err := scopeSidebar(ctx, true); err != nil {
		t.Fatal(err)
	}
	if sessions, _ := sidebarSessions(ctx); len(sessions) != 1 || sessions[0] != target {
		t.Errorf("the option after two scoped ons: %v", sessions)
	}
	boot := target
	// The scoped session's own pane stays, the other session's goes.
	if out := run("list-panes", "-a", "-F", "#{pane_id}"+tmux.Sep+"#{"+sidebarTag+"}"); strings.Contains(out, otherPane+tmux.Sep+"1") || !strings.Contains(out, ownPane+tmux.Sep+"1") {
		t.Errorf("the tagged panes after a scoped on:\n%s", out)
	}
	run("kill-pane", "-t", ownPane)
	if _, err := scopeSidebar(ctx, false); err != nil {
		t.Fatal(err)
	}
	if sessions, _ := sidebarSessions(ctx); len(sessions) != 0 {
		t.Errorf("the option after a plain on: %v", sessions)
	}
	if err := setSidebarHooks(ctx, "/usr/local/bin/laatmux"); err != nil {
		t.Fatal(err)
	}
	if err := bindJumpKeys(ctx, "/usr/local/bin/laatmux"); err != nil {
		t.Fatal(err)
	}
	run("set-option", "-s", sessionsTag, boot)
	if err := sidebarOff(ctx); err != nil {
		t.Fatal(err)
	}
	if on, _ := sidebarHooksSet(ctx); on {
		t.Error("hooks after off")
	}
	if k := run("list-keys", "-T", "root"); strings.Contains(k, "sidebar jump") {
		t.Error("jump keys after off")
	}
	if sessions, _ := sidebarSessions(ctx); len(sessions) != 0 {
		t.Errorf("the option after off: %v", sessions)
	}
}

// The jump keys: on binds M-1..M-9 in the root table to a jump with
// the window and client, off unbinds them and leaves a user's M-0 and
// a user's M-5 bound to something else alone.
func TestJumpKeys(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	must(workspace.Server.Run(ctx, "bind-key", "-n", "M-0", "display-message", "sidebar jump mine"))
	if err := bindJumpKeys(ctx, "/usr/local/bin/laatmux"); err != nil {
		t.Fatal(err)
	}
	keys := func() string { return string(must(workspace.Server.Run(ctx, "list-keys", "-T", "root"))) }
	if k := keys(); !strings.Contains(k, "M-1") || !strings.Contains(k, "M-9") || !strings.Contains(k, "sidebar jump 3 -t") || !strings.Contains(k, "#{client_name}") {
		t.Fatalf("bound: %s", k)
	}
	must(workspace.Server.Run(ctx, "bind-key", "-n", "M-5", "display-message", "mine"))
	unbindJumpKeys(ctx)
	k := keys()
	if strings.Contains(k, "sidebar jump 1 -t") || strings.Contains(k, "sidebar jump 9 -t") || !strings.Contains(k, "sidebar jump mine") || !strings.Contains(k, "M-5") {
		t.Fatalf("after unbind: %s", k)
	}
}

// A jump's client: switchTo with a client in the context switches
// that client alone, whichever of two attached it is.
func TestSwitchClient(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(string(must(workspace.Server.Run(ctx, args...))))
	}
	run("new-session", "-d", "-s", "other", "sleep 1000")
	sock := run("display", "-p", "#{socket_path}")
	for i := 0; i < 2; i++ {
		c := exec.Command("tmux", "-L", "default", "-C", "attach", "-t", "boot")
		in, err := c.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		c.Stdout = io.Discard
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { in.Close(); _ = c.Process.Kill(); _ = c.Wait() })
	}
	var clients []string
	for i := 0; i < 50 && len(clients) != 2; i++ {
		time.Sleep(100 * time.Millisecond)
		clients = strings.Fields(run("list-clients", "-F", "#{client_name}"))
	}
	if len(clients) != 2 {
		t.Fatalf("clients: %v", clients)
	}
	t.Setenv("TMUX", sock+",1,0")
	for _, target := range clients {
		run("switch-client", "-c", clients[0], "-t", "=boot")
		run("switch-client", "-c", clients[1], "-t", "=boot")
		if err := switchTo(withClient(ctx, target), "other"); err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(run("list-clients", "-F", "#{client_name} #{client_session}"), "\n") {
			f := strings.Fields(l)
			want := "boot"
			if len(f) == 2 && f[0] == target {
				want = "other"
			}
			if len(f) != 2 || f[1] != want {
				t.Errorf("switch of %s: %q", target, l)
			}
		}
	}
}

// A shell nested on another tmux server: no current window or session
// on the default server, so a command without -t does nothing and on
// --session refuses, rather than take the other server's pane id for
// one of the default server's own.
func TestNestedShell(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(string(must(workspace.Server.Run(ctx, args...))))
	}
	if out, err := exec.Command("tmux", "-L", "other", "-f", "/dev/null", "new-session", "-d", "-s", "o", "sleep 1000").CombinedOutput(); err != nil {
		t.Fatalf("start the other server: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", "other", "kill-server").Run() })
	sock, err := tmux.Server{Name: "other"}.Run(ctx, "display", "-p", "#{socket_path}")
	if err != nil {
		t.Fatal(err)
	}
	pane := run("split-window", "-d", "-h", "-t", "boot:", "-P", "-F", "#{pane_id}", "sleep 1000")
	run("set-option", "-p", "-t", pane, sidebarTag, "1")
	t.Setenv("TMUX_PANE", pane)
	got := make(chan view.Command, 8)
	cmds := make(chan func(*view.Model) view.Action, 8)
	stop, err := listenPane(ctx, cmds, func(c view.Command) func(*view.Model) view.Action {
		got <- c
		return func(m *view.Model) view.Action { return view.Action{} }
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	t.Setenv("TMUX", strings.TrimSpace(string(sock))+",1,0")
	if workspace.Inside(ctx) {
		t.Fatal("inside the default server with TMUX naming the other")
	}
	if err := sidebarControl(ctx, "next", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-got:
		t.Errorf("a command from a nested shell reached the default server's pane: %+v", c)
	case <-time.After(300 * time.Millisecond):
	}
	if err := sidebarControl(ctx, "next", []string{"-t", "boot:"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
		<-cmds
	case <-time.After(5 * time.Second):
		t.Error("-t from a nested shell did not reach the pane")
	}
	if _, err := scopeSidebar(ctx, true); err == nil {
		t.Error("on --session from a nested shell went ahead")
	}
	if sessions, _ := sidebarSessions(ctx); len(sessions) != 0 {
		t.Errorf("the option set from a nested shell: %v", sessions)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// The width rule split and fit share: the configured width, half a
// narrow window, the configured width for a window not known.
func TestSidebarWidth(t *testing.T) {
	cfg := config.Config{Sidebar: config.Sidebar{Width: "35"}}
	// The split: left at the width, or top at the height.
	if got := strings.Join(sidebarSplit(cfg, 200), " "); got != "split-window -d -h -b -f -l 35" {
		t.Errorf("the left split: %s", got)
	}
	if got := strings.Join(sidebarSplit(cfg, 50), " "); got != "split-window -d -h -b -f -l 25" {
		t.Errorf("the left split in a narrow window: %s", got)
	}
	if got := strings.Join(sidebarSplit(config.Config{Sidebar: config.Sidebar{Position: "top", Height: 4}}, 200), " "); got != "split-window -d -v -b -f -l 4" {
		t.Errorf("the top split: %s", got)
	}
	// The jump keys' labels: in a pane that listens, not the dashboard.
	cfg.Sidebar.JumpKeys = true
	if !jumpKeysShown(cfg, viewOptions{listen: true}) || jumpKeysShown(cfg, viewOptions{actions: true}) || jumpKeysShown(config.Config{}, viewOptions{listen: true}) {
		t.Error("the jump keys shown in the wrong host")
	}
	for _, c := range []struct{ window, want int }{{200, 35}, {70, 35}, {50, 25}, {36, 18}, {1, 1}, {0, 35}} {
		if got := sidebarWidth(cfg, c.window); got != c.want {
			t.Errorf("window %d: %d, want %d", c.window, got, c.want)
		}
	}
	// A percentage of the window, and the default's clamp.
	cfg.Sidebar.Width = "20%"
	for _, c := range []struct{ window, want int }{{200, 40}, {50, 10}, {0, 35}} {
		if got := sidebarWidth(cfg, c.window); got != c.want {
			t.Errorf("20%% of %d: %d, want %d", c.window, got, c.want)
		}
	}
	cfg.Sidebar.Width = ""
	for _, c := range []struct{ window, want int }{{200, 25}, {300, 30}, {600, 50}, {40, 20}, {0, 35}} {
		if got := sidebarWidth(cfg, c.window); got != c.want {
			t.Errorf("unset at %d: %d, want %d", c.window, got, c.want)
		}
	}
}
