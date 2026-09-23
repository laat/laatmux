package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// cmdSplit splits a pane: in a workspace session the new pane is a shell
// at the worktree root on the worktree's host, a local shell started
// there or ssh with cd; anywhere else it is the plain split, in the
// pane's current directory, so a binding of it loses nothing. The pane
// is looked up, and split, on the server TMUX names. Meant to replace
// the user's split bindings, which pass the pane id since a run-shell
// job has no TMUX_PANE:
//
//	bind | run-shell "laatmux split -h '#{pane_id}'"
//	bind - run-shell "laatmux split -v '#{pane_id}'"
//
// Split panes are not tagged. The session carries the tags, so a split
// of a split, or of the shell window, resolves the same way. The new
// pane lands at the root, not the split pane's directory: the attach
// pane's directory is wherever attach was started, and a remote pane's
// is the laptop directory ssh ran from, so neither means anything.
func cmdSplit(ctx context.Context, args []string) error {
	dir := ""
	paneID := os.Getenv("TMUX_PANE")
	for _, a := range args {
		switch {
		case a == "-h" || a == "-v":
			if dir != "" {
				return errors.New("usage: laatmux split [-h|-v] [<pane-id>]")
			}
			dir = a
		case len(a) > 1 && a[0] == '%':
			paneID = a
		default:
			return errors.New("usage: laatmux split [-h|-v] [<pane-id>]")
		}
	}
	if os.Getenv("TMUX") == "" {
		return errors.New("laatmux split must run inside tmux")
	}
	if paneID == "" {
		return errors.New("no pane: pass the pane id, as a run-shell binding does with '#{pane_id}'")
	}
	l, cwd, err := workspace.PaneSession(ctx, paneID)
	if err != nil {
		return err
	}
	var h config.Host
	if l.Workspace() {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		var ok bool
		if h, ok = cfg.Find(l.Host); !ok {
			return fmt.Errorf("workspace session %s is on host %q, which is not configured", l.Name, l.Host)
		}
	}
	// The split runs on the server the lookup used, the one TMUX names:
	// a pane id is per server, and a binding pressed in another server
	// must not split the default server's pane of the same id.
	_, err = (tmux.Server{}).Run(ctx, splitArgs(dir, paneID, cwd, l, h)...)
	return err
}

// splitArgs is the split-window command for a pane: the plain split in
// the pane's directory when its session is not a workspace, else a pane
// at the worktree root on the host.
func splitArgs(dir, paneID, cwd string, l workspace.Local, h config.Host) []string {
	args := []string{"split-window"}
	if dir != "" {
		args = append(args, dir)
	}
	args = append(args, "-t", paneID)
	if !l.Workspace() {
		return append(args, "-c", cwd)
	}
	_, root := workspace.SplitKey(l.Key)
	if h.Local() {
		return append(args, "-c", root)
	}
	return append(args, workspace.ShellCommand(h.Host, root))
}
