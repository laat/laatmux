package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// cmdAdd creates a worktree for the branch on a host, starts an agent in
// it and opens the local workspace session. The repository comes from
// --repo, else from the current directory; host and agent from their
// flags, else the last used for the repository, else the config's
// defaults. Progress prints one line per step. The doing is
// command.Add, which the dashboard runs too; this is the flags and the
// printing.
func cmdAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	repoFlag := fs.String("repo", "", "repository name or source; default from the current directory")
	hostFlag := fs.String("host", "", "host name; default the last used for the repository")
	agentFlag := fs.String("agent", "", "agent to start; default the last used for the repository")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: laatmux add <branch> [--repo r] [--host h] [--agent a] [-- <cmd>...]")
	}
	branch := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return err
	}
	cmd := fs.Args()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	repo, err := resolveRepo(ctx, cfg, *repoFlag)
	if err != nil {
		return err
	}
	h, lr, err := hostFor(cfg, *hostFlag, repo)
	if err != nil {
		return err
	}
	agentName := ""
	if len(cmd) == 0 {
		if agentName, _, err = cfg.DefaultAgent(*agentFlag, lr.Agent); err != nil {
			return err
		}
	}
	add := command.Add{Host: h, Repo: repo, Branch: branch, Agent: agentName, Cmd: cmd}
	fmt.Println(add.Describe())
	res, err := add.Run(ctx, printer{})
	if err != nil {
		return err
	}
	fmt.Printf("%s/%s ready: %s, session %s\n", repo.Name, branch, res.Root, res.Managed)
	return focus(ctx, res.Session, res.Created)
}

// printer is the CLI's Reporter: a line per step on stdout, transport
// notes on stderr.
type printer struct{}

func (printer) Progress(m protocol.Message) { fmt.Println(command.ProgressLine(m)) }
func (printer) Note(s string)               { fmt.Fprintln(os.Stderr, "laatmux:", s) }

// focus switches the calling client to the local session, or, outside
// the default tmux server, says how to attach. Inside another tmux server
// the hint cannot be run as is, since tmux refuses to nest, and says so.
func focus(ctx context.Context, name string, created bool) error {
	if workspace.Inside(ctx) {
		return workspace.Switch(ctx, name)
	}
	verb := "session"
	if created {
		verb = "created session"
	}
	if os.Getenv("TMUX") != "" {
		fmt.Printf("%s %s is on the default tmux server; detach from this one, then: %s\n", verb, name, workspace.AttachHint(name))
		return nil
	}
	fmt.Printf("%s %s; attach with: %s\n", verb, name, workspace.AttachHint(name))
	return nil
}
