package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/workspace"
)

// isolatedDefault points the default tmux server at a socket directory
// of the test's own, starts it with no config at all, not the user's
// nor a system-wide one that might source it, and kills it at the end.
// HOME and the state directory are the test's too. The directory is
// under /tmp: a unix socket's path is short on macOS.
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
		_, _ = workspace.Server.Run(context.Background(), "kill-server")
		os.RemoveAll(dir)
	})
	// The server starts here, with -f /dev/null; every later command
	// reaches it through workspace.Server's socket and reads no config.
	if out, err := exec.Command("tmux", "-L", "default", "-f", "/dev/null", "new-session", "-d", "-s", "boot", "sleep 1000").CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
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
	if _, err := scopeSidebar(ctx, true); err != nil {
		t.Fatal(err)
	}
	if sessions, _ := sidebarSessions(ctx); len(sessions) != 1 || sessions[0] != target {
		t.Errorf("the option after two scoped ons: %v", sessions)
	}
	boot := target
	if out := run("list-panes", "-a", "-F", "#{"+sidebarTag+"}"); strings.Contains(out, "1") {
		t.Error("the tagged pane in the other session stayed")
	}
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
