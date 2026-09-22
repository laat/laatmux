package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/laat/laatmux/internal/workspace"
)

// cmdSettle sets @laatmux_settled on a local workspace session, the one
// it runs from or the named one, and cmdUnsettle clears it. ls and the
// sidebar list settled workspaces in a collapsed section. Nothing else
// changes: the worktree, the managed session and the agent stay.
func cmdSettle(ctx context.Context, args []string) error {
	return setSettled(ctx, "settle", args, true)
}
func cmdUnsettle(ctx context.Context, args []string) error {
	return setSettled(ctx, "unsettle", args, false)
}

func setSettled(ctx context.Context, verb string, args []string, settled bool) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: laatmux %s [<host>/<repo>/<branch>]", verb)
	}
	var l workspace.Local
	if len(args) == 1 {
		locals, err := workspace.List(ctx)
		if err != nil {
			return err
		}
		var ok bool
		if l, ok = workspace.ByName(locals, args[0]); !ok {
			return fmt.Errorf("no local session %s", args[0])
		}
	} else {
		var err error
		if l, err = workspace.Current(ctx); err != nil {
			return fmt.Errorf("laatmux %s must run inside a workspace session, or name one: %w", verb, err)
		}
	}
	if !l.Workspace() {
		return errors.New(l.Name + " is not a workspace session")
	}
	return workspace.SetSettled(ctx, l.Name, settled)
}
