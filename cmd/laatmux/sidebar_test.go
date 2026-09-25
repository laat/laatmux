package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/workspace"
)

// isolatedDefault points the default tmux server at a socket directory
// of the test's own, with HOME there too so no user's tmux config, its
// plugins and hooks, is read, and kills that server at the end. The
// directory is under /tmp: a unix socket's path is short on macOS.
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
	t.Setenv("TMUX", "")
	t.Cleanup(func() {
		_, _ = workspace.Server.Run(context.Background(), "kill-server")
		os.RemoveAll(dir)
	})
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
}

// The resize hook is among the hooks on sets and off unsets.
func TestSidebarHooksFit(t *testing.T) {
	for _, h := range sidebarHooks {
		if h.hook == "window-resized[9105]" && h.cmd == "sidebar fit '#{window_id}'" {
			return
		}
	}
	t.Fatal("no window-resized hook")
}
