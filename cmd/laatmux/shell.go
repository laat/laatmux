package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/workspace"
)

// cmdShell opens a shell at the worktree root on the worktree's host, in
// a window of the workspace session it runs from: a local window started
// in the root, or an ssh window. The window is tagged @laatmux_shell; a
// second invocation selects it rather than opening another. Meant to be
// bound in the user's tmux config. The doing is command.Shell, which the
// dashboard's S runs too.
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
	return command.Shell(ctx, cfg, cur)
}
