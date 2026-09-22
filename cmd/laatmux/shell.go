package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// cmdShell opens a shell at the worktree root on the worktree's host, in
// a window of the workspace session it runs from: a local window started
// in the root, or an ssh window. The window is tagged @laatmux_shell; a
// second invocation selects it rather than opening another. Meant to be
// bound in the user's tmux config.
func cmdShell(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: laatmux shell")
	}
	cur, err := workspace.Current(ctx)
	if err != nil {
		return fmt.Errorf("laatmux shell must run inside a workspace session: %w", err)
	}
	if !cur.Workspace() {
		return fmt.Errorf("%s is not a workspace session; laatmux shell runs inside one", cur.Name)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	h, ok := cfg.Find(cur.Host)
	if !ok {
		return fmt.Errorf("workspace session %s is on host %q, which is not configured", cur.Name, cur.Host)
	}
	_, root := workspace.SplitKey(cur.Key)
	out, err := workspace.Server.Run(ctx, "list-windows", "-t", "="+cur.Name, "-F", "#{window_id}"+tmux.Sep+"#{@laatmux_shell}")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Split(line, tmux.Sep); len(f) == 2 && f[1] != "" {
			_, err := workspace.Server.Run(ctx, "select-window", "-t", f[0])
			return err
		}
	}
	// new-window makes the new window current, so the option set in the
	// same sequence lands on it and the window is never seen untagged.
	sessionTarget := "=" + cur.Name + ":"
	cmd := []string{"new-window", "-t", sessionTarget, "-n", "shell"}
	if h.Local() {
		cmd = append(cmd, "-c", root)
	} else {
		cmd = append(cmd, workspace.ShellCommand(h.Host, root))
	}
	cmd = append(cmd, ";", "set-option", "-w", "-t", sessionTarget, "@laatmux_shell", "1")
	_, err = workspace.Server.Run(ctx, cmd...)
	return err
}
