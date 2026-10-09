// laatmux: git worktrees and coding agents across hosts, from tmux.
// The commands, and the sidebar's tmux side, over the internal
// packages; docs/architecture.md says what runs where.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

var version = "0.0.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// SIGHUP too: a view in raw mode whose terminal goes away must run
	// its deferred restore, and a client that loses its terminal
	// mid-command stops like one that was interrupted.
	// The signals stay caught until the command returns: a stop's
	// SIGTERM to a serve already shutting down must not end it before
	// its runs are stopped (stop.go).
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(ctx, os.Args[2:])
	case "bridge":
		err = client.Bridge(ctx, os.Stdin, os.Stdout)
	case "new":
		err = cmdNew(ctx, os.Args[2:])
	case "ls":
		err = cmdLs(ctx, os.Args[2:])
	case "watch":
		err = cmdWatch(ctx, os.Args[2:])
	case "add":
		err = cmdAdd(ctx, os.Args[2:])
	case "tasks":
		err = cmdTasks(ctx, os.Args[2:])
	case "rm":
		err = cmdRm(ctx, os.Args[2:])
	case "prune":
		err = cmdPrune(ctx, os.Args[2:])
	case "path":
		err = cmdPath(ctx, os.Args[2:])
	case "jump":
		err = cmdJump(ctx, os.Args[2:])
	case "shell":
		err = cmdShell(ctx, os.Args[2:])
	case "split":
		err = cmdSplit(ctx, os.Args[2:])
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "sidebar":
		err = cmdSidebar(ctx, os.Args[2:])
	case "dashboard":
		err = cmdDashboard(ctx, os.Args[2:])
	case "compose":
		err = cmdCompose(ctx, os.Args[2:])
	case "settle":
		err = cmdSettle(ctx, os.Args[2:])
	case "unsettle":
		err = cmdUnsettle(ctx, os.Args[2:])
	case "hosts":
		err = cmdHosts(ctx, os.Args[2:])
	case "upgrade":
		err = cmdUpgrade(ctx, os.Args[2:])
	case "stop":
		err = cmdStop(ctx, os.Args[2:])
	case "repos":
		err = cmdRepos(ctx, os.Args[2:])
	case "explain":
		err = cmdExplain(ctx, os.Args[2:])
	case "version", "--version":
		fmt.Println("laatmux", version, "protocol", protocol.Version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if code, exit := report(os.Stderr, err); exit {
		os.Exit(code)
	}
}

// report prints a command's error to w and says the status to exit
// with; exit is false for no error and for a cancel. This is where the
// "laatmux: " goes: the packages' errors do not start with it, so the
// line has it once.
func report(w io.Writer, err error) (code int, exit bool) {
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(w, "laatmux:", ee.msg)
		}
		return ee.code, true
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(w, "laatmux:", err)
		return 1, true
	}
	return 0, false
}

// warnings is where warnHook prints.
var warnings io.Writer = os.Stderr

// warnHook takes the error of a tmux listing a command reads, through
// workspace say: a *tmux.HookError, a user's hook that failed after the
// listing printed every record, is printed as a note, and nil returned,
// so the command goes on with the records. Any other error is returned
// as it is. A view has no line to print it on, and its callers take the
// error with tmux.HookOnly instead; so does a sidebar binding, which says
// nothing of any failure.
func warnHook(err error) error {
	if !tmux.HookOnly(err) {
		return err
	}
	fmt.Fprintln(warnings, "laatmux:", err)
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: laatmux <command> [flags]

  serve     run the per-host daemon (polls the configured tmux servers, serves status)
  bridge    connect stdio to the local daemon (what ssh runs on a remote host)
  add       laatmux add [<branch>] [-p <prompt>] [--repo r] [--host h] [--agent a] [-- <cmd>...]
            worktree and agent on the host, then the local workspace session;
            with -p the agent gets the prompt and the branch may be left out;
            --detach hands it to the local daemon and returns
  tasks     laatmux tasks [show|dismiss|prompt <id>]   the background adds and their state
  rm        laatmux rm <repo>/<branch> [--host h] [--force]     remove the worktree, its sessions
            laatmux rm --root <path> --host h [--force]          a detached worktree
            laatmux rm [--force]           inside a workspace session: that workspace
  prune     laatmux prune [--host h] [--repo r] [-n|--dry-run] [--branches] [--yes]
            remove the worktrees with no session that are clean and have no commits
            beyond the default branch or a PR merged into it; -n prints the plan alone
  path      laatmux path <repo>/<branch> [--host h]             the worktree root on the host
  ls        workspaces and agents across configured hosts
  watch     live list, redraws on change, for a plain terminal
  sidebar   laatmux sidebar [toggle|on|off]   a list pane on the left of every window
            laatmux sidebar pane | attach <window> | fit <window> | reap | seen   what the pane and the hooks run
  dashboard the list in a popup for display-popup -E: Enter jumps and closes it;
            a opens the task form, x/X removes, z settles, S opens a shell
  compose   the task form alone, for display-popup -E -d '#{pane_current_path}'
  jump      laatmux jump <host>/<repo>/<branch>   switch to the workspace session, creating it
            laatmux jump [--server default] <host>/<session>   a session that is no worktree's
  shell     a shell at the worktree root, in the workspace session this runs from
  split     laatmux split [-h|-v] [<pane-id>]   split the pane; in a workspace session the new
            pane is a shell at the worktree root on its host, elsewhere the plain split
  run       laatmux run [<repo>/<branch>] [--host h] -- <cmd>...
            run a command in the worktree root on its host, output streamed back, exit status kept
  settle    laatmux settle [<host>/<repo>/<branch>]     collapse the workspace in ls
  unsettle  laatmux unsettle [<host>/<repo>/<branch>]
  new       laatmux new <name> [--host h] --cwd <path> [-- <cmd>...]   managed session, no worktree
  hosts     reachability and daemon version per host; marks daemons that differ from this build
  upgrade   laatmux upgrade <host>... [--src dir] [--bin file]   build for the host, install, restart its daemon
  stop      end this machine's daemon cleanly; the next command starts one again
  repos     known repositories and where each lands on each host
  explain   laatmux explain <pane-id>       show detection inputs and decision
  version

config: ~/.config/laatmux/config.yaml   state: $LAATMUX_HOME or ~/.local/state/laatmux
`)
}

func tmuxServerFlag(fs *flag.FlagSet) *string {
	return fs.String("tmux-socket", "laatmux", `tmux server: a -L name, "default" for the default server, or a -S path`)
}
