package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// Shell opens a shell at the worktree root on its host, in a window of
// the local workspace session: a local window started in the root, or
// an ssh window. The window is tagged @laatmux_shell; when the session
// has one already it is selected rather than opened again. The session
// must be a workspace session; h is its host, resolved by the caller,
// and a paused one is refused.
func Shell(ctx context.Context, h config.Host, l protocol.Session) error {
	if !l.Workspace() {
		return fmt.Errorf("%s is not a workspace session", l.Name)
	}
	if h.Paused && !h.Local() {
		return &peer.PausedError{Name: h.Name}
	}
	_, root := protocol.SplitSessionKey(l.Key)
	target := tmux.SessionTarget(l.Name)
	out, err := workspace.Server.Run(ctx, "list-windows", "-t", target, "-F", "#{window_id}"+tmux.Sep+"#{@laatmux_shell}")
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
	cmd := []string{"new-window", "-t", target, "-n", "shell"}
	if h.Local() {
		// new-window expands -c as a format, and the root has the
		// branch in it: a directory that is not there after expansion
		// starts the shell in $HOME, with no error. The ssh command is
		// not expanded.
		cmd = append(cmd, "-c", tmux.FormatLiteral(root))
	} else {
		cmd = append(cmd, workspace.ShellCommand(h.Host, root))
	}
	cmd = append(cmd, tmux.Next, "set-option", "-w", "-t", target, "@laatmux_shell", "1")
	_, err = workspace.Server.Run(ctx, cmd...)
	return err
}
