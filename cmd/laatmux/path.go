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
// host's records.
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
	repo, ok := cfg.RepoByName(repoLabel)
	if !ok {
		return fmt.Errorf("unknown repository %q; configured: %s", repoLabel, repoList(cfg))
	}
	h, _, err := hostFor(cfg, *hostFlag, repo)
	if err != nil {
		return err
	}
	_, snap, err := snapshot(ctx, h.Host, protocol.CapWorktrees)
	if err != nil {
		return err
	}
	w, ok, err := findWorktree(snap.Worktrees, repo, branch)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no worktree for %s/%s on %s", repo.Name, branch, h.Name)
	}
	fmt.Println(w.Root)
	return nil
}
