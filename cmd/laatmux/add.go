package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// cmdAdd creates a worktree for the branch on a host, starts an agent in
// it and opens the local workspace session. The repository comes from
// --repo, else from the current directory; host and agent from their
// flags, else the last used for the repository, else the config's
// defaults. Progress prints one line per step. The command id is chosen
// here and kept across reconnects, so a dropped bridge resumes the same
// add rather than starting another.
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
	fmt.Printf("add %s/%s on %s", repo.Name, branch, h.Name)
	if agentName != "" {
		fmt.Printf(" with %s", agentName)
	}
	fmt.Println()
	req := protocol.Message{Type: protocol.TypeAdd, ID: commandID("add"), Repo: repo.Source, Branch: branch, AgentName: agentName, Cmd: cmd}
	hello, res, err := stream(ctx, h.Host, protocol.CapAdd, req, printProgress)
	if err != nil {
		if res.Stage != "" {
			return fmt.Errorf("add failed at %s: %s", res.Stage, res.Error)
		}
		return err
	}
	fmt.Printf("%s/%s ready: %s, session %s\n", repo.Name, branch, res.Root, res.Session)
	if err := home.UpdateLast(func(l *home.Last) {
		cur := l.Get(repo.Source)
		cur.Host = h.Name
		if agentName != "" {
			cur.Agent = agentName
		}
		l.Set(repo.Source, cur)
	}); err != nil {
		return err
	}
	name, created, err := workspace.Ensure(ctx, workspace.Spec{
		Host: h.Host, Managed: res.Session,
		Name: workspace.SessionName(h.Name, repo.Name, branch),
		Key:  workspace.Key(hello.EnvironmentID, res.Root),
	})
	if err != nil {
		return err
	}
	return focus(ctx, name, created)
}

// commandID is a client-chosen id for one command invocation: unique
// across processes and time, and reused for every reconnect within it.
func commandID(kind string) string {
	return fmt.Sprintf("%s-%d-%d", kind, os.Getpid(), time.Now().UnixNano())
}

// printProgress writes one line per step: the stage, its state and the
// detail; a setup command's output is indented under it.
func printProgress(m protocol.Message) {
	switch m.State {
	case protocol.StateOutput:
		fmt.Printf("%-9s   | %s\n", "", m.Detail)
	case protocol.StateStart:
		fmt.Printf("%-9s %-5s %s\n", m.Stage, "", m.Detail)
	default:
		fmt.Printf("%-9s %-5s %s\n", m.Stage, m.State, m.Detail)
	}
}

// focus switches the calling client to the local session, or, outside
// the default tmux server, says how to attach.
func focus(ctx context.Context, name string, created bool) error {
	if workspace.Inside(ctx) {
		return workspace.Switch(ctx, name)
	}
	verb := "session"
	if created {
		verb = "created session"
	}
	fmt.Printf("%s %s; attach with: %s\n", verb, name, workspace.AttachHint(name))
	return nil
}
