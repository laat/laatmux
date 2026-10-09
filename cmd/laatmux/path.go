package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
)

// cmdPath prints the worktree root of a branch on its host, from the
// host's records, or the directory of the main checkout that has the
// branch checked out when no worktree is on it.
func cmdPath(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("path", flag.ContinueOnError)
	hostFlag := fs.String("host", "", "host name; default the last used for the repository")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: laatmux path <repo>/<branch> [--host h]")
	}
	target := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return err
	}
	repoLabel, branch, err := splitRepoBranch(target)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	repo, err := lookupRepo(cfg, lazyKnown(ctx, cfg), repoLabel, *hostFlag)
	if err != nil {
		return err
	}
	h, _, err := hostFor(cfg, *hostFlag, repo, false)
	if err != nil {
		return err
	}
	_, snap, err := snapshot(ctx, h.Host, protocol.CapWorktrees)
	if err != nil {
		return err
	}
	w, ok, err := findRecord(snap.Worktrees, repo, branch)
	if err != nil {
		return err
	}
	if !ok {
		return noWorktree(repo, branch, h.Name)
	}
	fmt.Println(w.Root)
	return nil
}
