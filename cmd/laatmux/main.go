// laatmux: git worktrees and coding agents across hosts, from tmux.
//
// Milestone one: status daemon, sidebar, launcher, jump.
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
	case "jump":
		err = cmdJump(ctx, os.Args[2:])
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
  new       laatmux new <name> [--host h] --cwd <path> [-- <cmd>...]
  ls        list agents across configured hosts
  watch     live list, redraws on change (sidebar)
  jump      laatmux jump [--server s] <host>/<session>   focus or open a pane attached to it
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
