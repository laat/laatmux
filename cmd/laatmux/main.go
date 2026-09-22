// laatmux: git worktrees and coding agents across hosts, from tmux.
//
// Milestone one: status daemon, sidebar, launcher, jump.
// Milestone two: worktrees and workspaces: add, rm, path, shell, settle.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/protocol"
)

var version = "0.0.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	case "rm":
		err = cmdRm(ctx, os.Args[2:])
	case "path":
		err = cmdPath(ctx, os.Args[2:])
	case "jump":
		err = cmdJump(ctx, os.Args[2:])
	case "shell":
		err = cmdShell(ctx, os.Args[2:])
	case "settle":
		err = cmdSettle(ctx, os.Args[2:])
	case "unsettle":
		err = cmdUnsettle(ctx, os.Args[2:])
	case "hosts":
		err = cmdHosts(ctx, os.Args[2:])
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
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "laatmux:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: laatmux <command> [flags]

  serve     run the per-host daemon (polls the configured tmux servers, serves status)
  bridge    connect stdio to the local daemon (what ssh runs on a remote host)
  add       laatmux add <branch> [--repo r] [--host h] [--agent a] [-- <cmd>...]
            worktree and agent on the host, then the local workspace session
  rm        laatmux rm <repo>/<branch> [--host h] [--force]     remove the worktree, its sessions
            laatmux rm --root <path> --host h [--force]          a detached worktree
  path      laatmux path <repo>/<branch> [--host h]             the worktree root on the host
  ls        workspaces and agents across configured hosts
  watch     live list, redraws on change (sidebar)
  jump      laatmux jump <host>/<repo>/<branch>   switch to the workspace session, creating it
            laatmux jump [--server default] <host>/<session>   a session that is no worktree's
  shell     a shell at the worktree root, in the workspace session this runs from
  settle    laatmux settle [<host>/<repo>/<branch>]     collapse the workspace in ls
  unsettle  laatmux unsettle [<host>/<repo>/<branch>]
  new       laatmux new <name> [--host h] --cwd <path> [-- <cmd>...]   managed session, no worktree
  hosts     reachability and daemon version per host
  repos     known repositories and where each lands on each host
  explain   laatmux explain <pane-id>       show detection inputs and decision
  version

config: ~/.config/laatmux/config.yaml   state: $LAATMUX_HOME or ~/.local/state/laatmux
`)
}

func tmuxServerFlag(fs *flag.FlagSet) *string {
	return fs.String("tmux-socket", "laatmux", `tmux server: a -L name, "default" for the default server, or a -S path`)
}
