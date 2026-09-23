package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// cmdRm removes a worktree and its managed session on the host, then the
// local workspace session. The root is sent whenever it is known, from the
// host's record or, when the worktree is already gone, from the key of the
// local session, since a branch alone maps to no root then; that is what
// reaches a managed session whose worktree was removed by hand. The doing
// is command.Rm, which the dashboard runs too; this is the flags, the
// lookup of the root and the printing.
func cmdRm(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	hostFlag := fs.String("host", "", "host name; default the last used for the repository")
	root := fs.String("root", "", "worktree root on the host, for a detached worktree")
	force := fs.Bool("force", false, "remove a dirty or locked worktree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	usage := errors.New("usage: laatmux rm <repo>/<branch> [--host h] [--force]\n       laatmux rm --root <path> --host h [--force]")
	target := ""
	if fs.NArg() > 0 {
		target = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if (target == "") == (*root == "") || fs.NArg() > 0 {
		return usage
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	rm := command.Rm{Force: *force, Root: *root}
	if target != "" {
		repoLabel, branch, err := splitRepoBranch(target)
		if err != nil {
			return err
		}
		repo, ok := cfg.RepoByName(repoLabel)
		if !ok {
			return fmt.Errorf("unknown repository %q; configured: %s", repoLabel, repoList(cfg))
		}
		if rm.Host, _, err = hostFor(cfg, *hostFlag, repo); err != nil {
			return err
		}
		rm.Repo, rm.Branch = repo, branch
		hello, snap, err := snapshot(ctx, rm.Host.Host, protocol.CapRm)
		if err != nil {
			return err
		}
		if w, ok := findWorktree(snap.Worktrees, repo, branch); ok {
			rm.Root = w.Root
		} else {
			locals, err := workspace.List(ctx)
			if err != nil {
				return err
			}
			rm.Root = command.RootOf(locals, hello.EnvironmentID, rm.Host, repo, branch)
		}
	} else {
		if *hostFlag == "" {
			return errors.New("--root needs --host")
		}
		if rm.Host, err = cfg.DefaultHost(*hostFlag, ""); err != nil {
			return err
		}
	}
	res, err := rm.Run(ctx, printer{})
	if err != nil {
		return err
	}
	fmt.Printf("removed %s on %s", rm.Describe(), rm.Host.Name)
	if res.Root != "" {
		fmt.Printf(" (%s)", res.Root)
	}
	fmt.Println()
	for _, name := range res.Killed {
		fmt.Printf("killed local session %s\n", name)
	}
	return nil
}
