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
	cfg := config.Config{Sidebar: config.Sidebar{Width: 35}}
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
	cfg := config.Config{Sidebar: config.Sidebar{Width: 35}}
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
	cfg := config.Config{Sidebar: config.Sidebar{Width: 35}}
	for _, c := range []struct{ window, want int }{{200, 35}, {70, 35}, {50, 25}, {36, 18}, {1, 1}, {0, 35}} {
		if got := sidebarWidth(cfg, c.window); got != c.want {
			t.Errorf("window %d: %d, want %d", c.window, got, c.want)
		}
	}
}
