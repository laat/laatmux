package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// cmdRm removes a worktree and its managed session on the host, then the
// local workspace session. The root is sent whenever it is known, from the
// host's record or, when the worktree is already gone, from the key of the
// local session, since a branch alone maps to no root then; that is what
// reaches a managed session whose worktree was removed by hand. Inside a
// workspace session with no target named, the target is that workspace,
// from the session's tags, as shell and run default. The doing is
// command.Rm, which the dashboard runs too; this is the flags, the
// lookup of the root and the printing.
func cmdRm(ctx context.Context, args []string) error {
	a, err := parseRmArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	rm := command.Rm{Force: a.force, Root: a.root}
	switch {
	case !a.targetGiven && !a.rootGiven:
		if a.host != "" {
			return errors.New("--host goes with <repo>/<branch> or --root; inside a workspace session the target is the workspace")
		}
		cur, err := workspace.Current(ctx)
		if err != nil {
			return fmt.Errorf("laatmux rm must name <repo>/<branch>, give --root, or run inside a workspace session: %w", err)
		}
		if !cur.Workspace() {
			return fmt.Errorf("%s is not a workspace session; name <repo>/<branch> or give --root", cur.Name)
		}
		h, hello, snap, err := hostForSession(ctx, cfg, cur)
		if err != nil {
			return err
		}
		if rm, err = rmCurrent(cfg, cur, h, hello.EnvironmentID, snap.Worktrees); err != nil {
			return err
		}
		rm.Force = a.force
		fmt.Printf("removing the workspace of this session, %s on %s (%s)\n", rm.Describe(), rm.Host.Name, rm.Root)
	case a.targetGiven:
		repoLabel, branch, err := splitRepoBranch(a.target)
		if err != nil {
			return err
		}
		repo, ok := cfg.RepoByName(repoLabel)
		if !ok {
			return fmt.Errorf("unknown repository %q; configured: %s", repoLabel, repoList(cfg))
		}
		if rm.Host, _, err = hostFor(cfg, a.host, repo); err != nil {
			return err
		}
		rm.Repo, rm.Branch = repo, branch
		hello, snap, err := snapshot(ctx, rm.Host.Host, protocol.CapRm)
		if err != nil {
			return err
		}
		w, ok, err := findWorktree(snap.Worktrees, repo, branch)
		if err != nil {
			return err
		}
		if ok {
			rm.Root = w.Root
			if w.Source != "" {
				// As the host has it, for an older host.
				rm.Repo.Source = w.Source
			}
		} else {
			locals, err := workspace.List(ctx)
			if err != nil {
				return err
			}
			rm.Root = command.RootOf(locals, hello.EnvironmentID, rm.Host, repo, branch)
		}
	default:
		if a.host == "" {
			return errors.New("--root needs --host")
		}
		if rm.Host, err = cfg.DefaultHost(a.host, ""); err != nil {
			return err
		}
	}
	res, err := rm.Run(ctx, printer{})
	// A root in the result means the host's side is done, whatever
	// happened to the local session after; that is said before the
	// error, so a destructive step that succeeded is never hidden.
	if err == nil || res.Root != "" {
		fmt.Printf("removed %s on %s", rm.Describe(), rm.Host.Name)
		if res.Root != "" {
			fmt.Printf(" (%s)", res.Root)
		}
		fmt.Println()
	}
	for _, name := range res.Killed {
		fmt.Printf("killed local session %s\n", name)
	}
	return err
}

// hostForSession is the host a workspace session is on, with its hello
// and snapshot: the one the session's tag names, when the tag names a
// configured host. A tag from before a rename names nothing, and a
// session from before the tag has none, which is not the local host
// though Find would say so for an empty name; then the host is
// whichever configured one answers as the key's environment, which is
// how the dashboard routes a row.
func hostForSession(ctx context.Context, cfg config.Config, cur workspace.Local) (config.Host, protocol.Message, protocol.Message, error) {
	if cur.Host != "" {
		if h, ok := cfg.Find(cur.Host); ok {
			hello, snap, err := snapshot(ctx, h.Host, protocol.CapRm)
			return h, hello, snap, err
		}
	}
	env, _ := workspace.SplitKey(cur.Key)
	h, hello, snap, err := hostByEnvironment(ctx, cfg, env)
	if err != nil {
		if cur.Host == "" {
			return h, hello, snap, fmt.Errorf("workspace session %s carries no host tag, and %w", cur.Name, err)
		}
		return h, hello, snap, fmt.Errorf("workspace session %s is on host %q, which is not configured, and %w", cur.Name, cur.Host, err)
	}
	return h, hello, snap, nil
}

// hostByEnvironment finds the configured host whose daemon answers as
// the environment, with its hello and snapshot: each host is asked in
// config order, through the merged stream where the local daemon has
// one, so a host row already there costs nothing, else by dialling.
// A host that cannot be reached is passed over; none answering is an
// error naming the environment.
func hostByEnvironment(ctx context.Context, cfg config.Config, env string) (config.Host, protocol.Message, protocol.Message, error) {
	var errs []string
	for _, h := range cfg.Hosts {
		hello, snap, err := snapshot(ctx, h.Host, protocol.CapRm)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if hello.EnvironmentID == env {
			return h, hello, snap, nil
		}
	}
	msg := fmt.Sprintf("no configured host answers as environment %s", env)
	if len(errs) > 0 {
		msg += " (" + strings.Join(errs, "; ") + ")"
	}
	return config.Host{}, protocol.Message{}, protocol.Message{}, errors.New(msg)
}

// rmArgs is rm's command line. A target or a root given as the empty
// string is given, not omitted: a script with an empty variable must
// get the usage, not the removal of the workspace it runs in.
type rmArgs struct {
	target, root, host string
	targetGiven        bool
	rootGiven          bool
	force              bool
}

var rmUsage = errors.New("usage: laatmux rm <repo>/<branch> [--host h] [--force]\n       laatmux rm --root <path> --host h [--force]\n       laatmux rm [--force]           inside a workspace session: that workspace")

// parseRmArgs reads the flags, before and after the target.
func parseRmArgs(args []string) (rmArgs, error) {
	var a rmArgs
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	fs.StringVar(&a.host, "host", "", "host name; default the last used for the repository")
	fs.StringVar(&a.root, "root", "", "worktree root on the host, for a detached worktree")
	fs.BoolVar(&a.force, "force", false, "remove a dirty or locked worktree")
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	if fs.NArg() > 0 {
		a.target, a.targetGiven = fs.Arg(0), true
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return a, err
		}
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "root" {
			a.rootGiven = true
		}
	})
	if fs.NArg() > 0 || (a.targetGiven && a.rootGiven) || (a.targetGiven && a.target == "") || (a.rootGiven && a.root == "") {
		return a, rmUsage
	}
	return a, nil
}

// rmCurrent is the rm for the workspace session the command runs in,
// resolved as the dashboard resolves a row: the host must answer as the
// environment the session's key names, since a host entry given to
// another machine would otherwise remove that machine's worktree at
// the same path; the worktree record for the key's root, when the host
// has one, gives the repository and branch, which the session's tags
// can misname after a switch or a detach in the worktree; else the tags
// give them when this machine's config knows the source, else the root
// alone as --root does.
func rmCurrent(cfg config.Config, cur workspace.Local, h config.Host, environmentID string, worktrees []protocol.Worktree) (command.Rm, error) {
	if !cur.Workspace() {
		return command.Rm{}, fmt.Errorf("%s is not a workspace session; name <repo>/<branch> or give --root", cur.Name)
	}
	env, root := workspace.SplitKey(cur.Key)
	if root == "" {
		return command.Rm{}, fmt.Errorf("workspace session %s has no root in its key", cur.Name)
	}
	if environmentID != "" && env != environmentID {
		return command.Rm{}, fmt.Errorf("workspace session %s is on environment %s, but host %s answers as %s; the host entry may have moved to another machine", cur.Name, env, h.Name, environmentID)
	}
	rm := command.Rm{Host: h, Root: root, Environment: env}
	for _, w := range worktrees {
		if w.Root != root || w.EnvironmentID != env {
			continue
		}
		if repo, ok := recordRepo(cfg, w.Source); ok {
			rm.Repo, rm.Branch = repo, w.Branch
		} else if repo, ok := cfg.RepoByName(w.Repo); ok && w.Source == "" {
			rm.Repo, rm.Branch = repo, w.Branch
		}
		return rm, nil
	}
	if repo, ok := recordRepo(cfg, cur.Source); ok && cur.Branch != "" {
		rm.Repo, rm.Branch = repo, cur.Branch
	}
	return rm, nil
}
