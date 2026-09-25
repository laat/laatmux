package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// cmdRun runs a command in a worktree root on the worktree's host and
// streams its output back, stdout lines to stdout and stderr lines to
// stderr, then exits with the process's status. Inside a workspace
// session the target defaults to that workspace, as shell's does;
// elsewhere it must be named, and is resolved as path resolves it. The
// command is an argv array run directly, no shell; "-- sh -c '...'" is
// how to get one. It runs with the daemon's environment, its stdin at
// /dev/null and no tty. Ctrl-C sends a cancel and waits for the result;
// a second Ctrl-C gives up on the connection, and says that the cancel
// may not have reached the daemon, since a reconnect may have been in
// progress. The doing is command.Run; this is the flags, the target and
// the printing.
func cmdRun(ctx context.Context, args []string) error {
	before, cmd, ok := splitDashes(args)
	usage := errors.New("usage: laatmux run [<repo>/<branch>] [--host h] -- <cmd>...")
	if !ok || len(cmd) == 0 {
		return usage
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	hostFlag := fs.String("host", "", "host name; default the last used for the repository")
	if err := fs.Parse(before); err != nil {
		return err
	}
	target := ""
	if fs.NArg() > 0 {
		target = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if fs.NArg() > 0 {
		return usage
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	run := command.Run{Cmd: cmd}
	if target != "" {
		repoLabel, branch, err := splitRepoBranch(target)
		if err != nil {
			return err
		}
		repo, ok := cfg.RepoByName(repoLabel)
		if !ok {
			return fmt.Errorf("unknown repository %q; configured: %s", repoLabel, repoList(cfg))
		}
		if run.Host, _, err = hostFor(cfg, *hostFlag, repo); err != nil {
			return err
		}
		_, snap, err := snapshot(ctx, run.Host.Host, protocol.CapWorktrees)
		if err != nil {
			return err
		}
		w, ok := findWorktree(snap.Worktrees, repo, branch)
		if !ok {
			return fmt.Errorf("no worktree for %s/%s on %s", repo.Name, branch, run.Host.Name)
		}
		if w.Source != "" {
			// As the host has it, for an older host.
			repo.Source = w.Source
		}
		run.Repo, run.Branch, run.Root = repo, branch, w.Root
	} else {
		if *hostFlag != "" {
			return errors.New("--host goes with <repo>/<branch>; inside a workspace session the target is the workspace")
		}
		cur, err := workspace.Current(ctx)
		if err != nil {
			return fmt.Errorf("laatmux run must name <repo>/<branch> or run inside a workspace session: %w", err)
		}
		if !cur.Workspace() {
			return fmt.Errorf("%s is not a workspace session; name <repo>/<branch>", cur.Name)
		}
		h, ok := cfg.Find(cur.Host)
		if !ok {
			return fmt.Errorf("workspace session %s is on host %q, which is not configured", cur.Name, cur.Host)
		}
		run.Host = h
		_, run.Root = workspace.SplitKey(cur.Key)
		if repo, ok := recordRepo(cfg, cur.Source); ok {
			run.Repo, run.Branch = repo, cur.Branch
		}
	}
	// The stream runs under its own context: the first signal is a
	// cancel request the daemon answers with a result, not the end of
	// the connection. The context from main is already cancelled by
	// then, so it is used only up to here.
	rctx, stop := context.WithCancel(context.Background())
	defer stop()
	cancel := make(chan struct{})
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "laatmux: cancelling; once more to give up waiting")
		close(cancel)
		<-sig
		stop()
	}()
	run.Cancel = cancel
	res, err := run.Run(rctx, runPrinter{})
	switch {
	case err == nil:
		if res.Exit != 0 {
			return &exitError{code: res.Exit}
		}
		return nil
	case errors.Is(err, command.ErrOutcomeUnknown):
		return &exitError{code: 255, msg: err.Error()}
	case command.Cancelled(err):
		return &exitError{code: 130, msg: "cancelled"}
	case rctx.Err() != nil:
		// The cancel went on the connection that was open when the
		// first signal came; during a reconnect there was none, so it
		// may not have reached the daemon.
		return &exitError{code: 130, msg: fmt.Sprintf("gave up waiting; the cancel may not have reached %s and the run may still be going there", run.Host.Name)}
	}
	return err
}

// splitDashes separates the arguments before "--" from the command after
// it. Not ok when there is no "--".
func splitDashes(args []string) (before, cmd []string, ok bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

// runPrinter is run's Reporter: output lines to the stream they came
// from, a gap and transport notes to stderr, the start to nothing.
type runPrinter struct{}

func (runPrinter) Progress(m protocol.Message) {
	switch m.State {
	case protocol.StateOutput:
		if m.FD == 2 {
			fmt.Fprintln(os.Stderr, m.Detail)
		} else {
			fmt.Println(m.Detail)
		}
	case protocol.StateGap:
		fmt.Fprintln(os.Stderr, "laatmux: "+m.Detail)
	}
}

func (runPrinter) Note(s string) { fmt.Fprintln(os.Stderr, "laatmux:", s) }

// exitError is a command's outcome that sets the exit status: a run
// exits with its process's status, 255 when the outcome is unknown, 130
// when cancelled. The message, when there is one, is printed as an
// error.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }
