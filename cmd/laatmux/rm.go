package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// cmdRm removes a worktree and its managed session on the host, then the
// local workspace session. The root is sent whenever it is known, from the
// host's record or, when the worktree is already gone, from the key of the
// local session, since a branch alone maps to no root then; that is what
// reaches a managed session whose worktree was removed by hand. The local
// session is found by its source and branch tags, which survive a renamed
// host or label. A session found by name instead is accepted only when it
// carries no identity tags at all, from before they existed: tags that
// name another source or branch mean the name has moved on to another
// workspace, and its root must not be sent with this one's identity.
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
	req := protocol.Message{Type: protocol.TypeRm, ID: commandID("rm"), Force: *force, Root: *root}
	var h config.Host
	what := ""
	if target != "" {
		repoLabel, branch, err := splitRepoBranch(target)
		if err != nil {
			return err
		}
		repo, ok := cfg.RepoByName(repoLabel)
		if !ok {
			return fmt.Errorf("unknown repository %q; configured: %s", repoLabel, repoList(cfg))
		}
		if h, _, err = hostFor(cfg, *hostFlag, repo); err != nil {
			return err
		}
		req.Repo, req.Branch = repo.Source, branch
		what = repo.Name + "/" + branch
		hello, snap, err := snapshot(ctx, h.Host, protocol.CapRm)
		if err != nil {
			return err
		}
		if w, ok := findWorktree(snap.Worktrees, repo.Name, branch); ok {
			req.Root = w.Root
		} else {
			locals, err := workspace.List(ctx)
			if err != nil {
				return err
			}
			l, ok := workspace.FindWorktree(locals, hello.EnvironmentID, repo.Source, branch)
			if !ok {
				l, ok = workspace.ByName(locals, workspace.SessionName(h.Name, repo.Name, branch))
				ok = ok && l.Source == "" && l.Branch == ""
			}
			if ok && l.Workspace() {
				if env, root := workspace.SplitKey(l.Key); env == hello.EnvironmentID {
					req.Root = root
				}
			}
		}
	} else {
		if *hostFlag == "" {
			return errors.New("--root needs --host")
		}
		if h, err = cfg.DefaultHost(*hostFlag, ""); err != nil {
			return err
		}
		what = *root
	}
	hello, res, err := stream(ctx, h.Host, protocol.CapRm, req, printProgress)
	if err != nil {
		return err
	}
	root2 := res.Root
	if root2 == "" {
		root2 = req.Root
	}
	fmt.Printf("removed %s on %s", what, h.Name)
	if root2 != "" {
		fmt.Printf(" (%s)", root2)
	}
	fmt.Println()
	if root2 == "" {
		return nil
	}
	// The local workspace session is the client's to clean up.
	locals, err := workspace.List(ctx)
	if err != nil {
		return err
	}
	key := workspace.Key(hello.EnvironmentID, root2)
	for _, l := range locals {
		if l.Key == key {
			if err := workspace.Kill(ctx, l.Name); err != nil {
				return err
			}
			fmt.Printf("killed local session %s\n", l.Name)
		}
	}
	return nil
}
