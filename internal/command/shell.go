package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// Shell opens a shell at the worktree root on its host, in a window of
// the local workspace session: a local window started in the root, or
// an ssh window. The window is tagged @laatmux_shell; when the session
// has one already it is selected rather than opened again. The session
// must be a workspace session, and its host is looked up in cfg.
func Shell(ctx context.Context, cfg config.Config, l workspace.Local) error {
	if !l.Workspace() {
		return fmt.Errorf("%s is not a workspace session", l.Name)
	}
	h, ok := cfg.Find(l.Host)
	if !ok {
		return fmt.Errorf("workspace session %s is on host %q, which is not configured", l.Name, l.Host)
	}
	_, root := workspace.SplitKey(l.Key)
	out, err := workspace.Server.Run(ctx, "list-windows", "-t", "="+l.Name, "-F", "#{window_id}"+tmux.Sep+"#{@laatmux_shell}")
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
	sessionTarget := "=" + l.Name + ":"
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
