package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
	"github.com/laat/laatmux/internal/worktree"
)

// cmdAdd creates a worktree for the branch on a host, starts an agent in
// it and opens the local workspace session. The repository comes from
// --repo, else from the current directory; host and agent from their
// flags, else the last used for the repository, else the config's
// defaults. With -p the agent is started with the prompt, and the
// branch may be left out: a name is proposed from the prompt and the
// host makes it unique. Progress prints one line per step, and the
// prompt's delivery state is the last line. The doing is command.Add,
// which the dashboard runs too; this is the flags and the printing.
func cmdAdd(ctx context.Context, args []string) error {
	a, err := parseAddArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	repo, err := resolveRepo(ctx, cfg, a.repo)
	if err != nil {
		return err
	}
	h, lr, err := hostFor(cfg, a.host, repo)
	if err != nil {
		return err
	}
	agentName := ""
	if len(a.cmd) == 0 {
		if agentName, _, err = cfg.DefaultAgent(a.agent, lr.Agent); err != nil {
			return err
		}
	}
	branch, prompt := a.branch, a.prompt
	add := command.Add{Host: h, Repo: repo, Copy: cfg.Copy, Branch: branch, Agent: agentName, Cmd: a.cmd, Prompt: prompt, Generated: a.generated}
	fmt.Println(add.Describe())
	if a.detach {
		// The daemon has the task once an id comes back, whatever the
		// local bookkeeping after; a retry would submit another.
		id, err := add.Submit(ctx)
		switch {
		case err == nil:
			fmt.Printf("accepted %s; the daemon runs it, laatmux tasks shows it\n", id)
		case id != "":
			fmt.Printf("submitted %s; laatmux tasks says whether the daemon holds it\n", id)
		}
		return err
	}
	res, err := add.Run(ctx, printer{})
	// A host result that succeeded means the worktree and its agent
	// exist there, whatever happened to last.json or the local session
	// after; that is said before the error, so it is never hidden. A
	// result that failed may still name a root, a removed one say, and
	// that is not ready.
	if res.Done {
		fmt.Printf("%s/%s ready: %s, session %s\n", repo.Name, res.Branch, res.Root, res.Managed)
	}
	// The delivery state is printed as soon as the host has said it,
	// before any local failure, and the exit is 0 whatever it says when
	// the rest went through: the worktree and the agent are there, and
	// the state is what the user reads to decide about a resubmit.
	if prompt != "" && res.Prompt != "" {
		if res.Reason != "" {
			fmt.Printf("prompt %s: %s\n", res.Prompt, res.Reason)
		} else {
			fmt.Printf("prompt %s\n", res.Prompt)
		}
	}
	if err != nil {
		return err
	}
	return focus(ctx, res.Session, res.Created)
}

// addArgs is the add command line: the flags, the branch, given or
// proposed from the prompt, and the command override after --.
type addArgs struct {
	repo, host, agent string
	prompt            string
	branch            string
	generated         bool
	detach            bool
	cmd               []string
}

// parseAddArgs reads the command line. The command override after --
// is taken off first, so it is never read as the optional branch:
// `add -p x -- claude` runs claude under a proposed name. Flags come
// before or after the branch.
func parseAddArgs(args []string) (addArgs, error) {
	var a addArgs
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.StringVar(&a.repo, "repo", "", "repository name or source; default from the current directory")
	fs.StringVar(&a.host, "host", "", "host name; default the last used for the repository")
	fs.StringVar(&a.agent, "agent", "", "agent to start; default the last used for the repository")
	fs.StringVar(&a.prompt, "p", "", "prompt the agent is started with")
	fs.StringVar(&a.prompt, "prompt", "", "alias of -p")
	fs.BoolVar(&a.detach, "detach", false, "hand the add to the local daemon and return; laatmux tasks shows it")
	if i := slices.Index(args, "--"); i >= 0 {
		args, a.cmd = args[:i], args[i+1:]
	}
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	usage := errors.New("usage: laatmux add [<branch>] [-p <prompt>] [--detach] [--repo r] [--host h] [--agent a] [-- <cmd>...]")
	switch {
	case fs.NArg() >= 1 && fs.Arg(0) != "":
		a.branch = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return a, err
		}
		if fs.NArg() > 0 {
			return a, usage
		}
	case a.prompt != "":
		// No branch: a proposal from the prompt, allocated on the host.
		a.branch, a.generated = worktree.ProposeBranch(a.prompt), true
		if a.branch == "" {
			return a, errors.New("no branch name can be made from the prompt; give one")
		}
	default:
		return a, usage
	}
	return a, nil
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
