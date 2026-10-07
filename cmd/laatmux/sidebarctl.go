package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/workspace"
)

// Each sidebar pane listens on a unix socket of its own under the state
// directory, named by the tmux server's pid and the pane id, since pane
// ids restart after a server restart; it writes the path to the pane
// option @laatmux_sidebar_socket, and the CLI reads the path from the
// tagged pane rather than building it. `laatmux sidebar next | prev |
// jump N | view V | scope S` is one line to the socket, handled by the
// pane as a navigation event. A window with no sidebar pane, or a socket
// that refuses the connection, makes the command exit quietly, since a
// binding's error flashes in the status line.

// socketTag is the pane option naming a sidebar pane's socket.
const socketTag = "@laatmux_sidebar_socket"

// socketDir is where the sockets live.
func socketDir() string { return filepath.Join(home.Dir(), "sidebar") }

// socketPath is the socket of the pane on the server with the pid.
func socketPath(serverPID int, paneID string) string {
	return filepath.Join(socketDir(), strconv.Itoa(serverPID)+"-"+strings.TrimPrefix(paneID, "%")+".sock")
}

// serverPID is the default server's pid.
func serverPID(ctx context.Context) (int, error) {
	out, err := workspace.Server.Run(ctx, "display-message", "-p", "#{pid}")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// listenPane makes the pane's socket, unlinking a leftover of its name,
// tags the pane with the path, and serves commands into cmds until ctx
// ends; then it removes the socket. A pane outside tmux, a test say,
// listens on nothing.
func listenPane(ctx context.Context, cmds chan<- func(*view.Model) view.Action, act func(view.Command) func(*view.Model) view.Action) (func(), error) {
	paneID := os.Getenv("TMUX_PANE")
	if paneID == "" {
		return func() {}, nil
	}
	pid, err := serverPID(ctx)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(socketDir(), 0o700); err != nil {
		return nil, err
	}
	path := socketPath(pid, paneID)
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if _, err := workspace.Server.Run(ctx, "set-option", "-p", "-t", paneID, socketTag, path); err != nil {
		ln.Close()
		os.Remove(path)
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil && line == "" {
					return
				}
				cmd, err := view.ParseCommand(strings.TrimSpace(line))
				if err != nil {
					fmt.Fprintf(c, "error: %v\n", err)
					return
				}
				select {
				case cmds <- act(cmd):
					fmt.Fprintln(c, "ok")
				case <-ctx.Done():
				}
			}()
		}
	}()
	return func() {
		ln.Close()
		os.Remove(path)
	}, nil
}

// sidebarControl is the CLI side: `laatmux sidebar next|prev|jump N|
// view V|scope S [-t window] [-c client] [--all]`. It acts on the
// sidebar pane in the window named, the current one by default, or on
// every pane with --all, which view and scope take. For view and scope
// the new default goes to sidebar.json once, whether or not a pane
// answered.
func sidebarControl(ctx context.Context, name string, args []string) error {
	window, client, all := "", "", false
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-t" && i+1 < len(args):
			i++
			window = args[i]
		case a == "-c" && i+1 < len(args):
			i++
			client = args[i]
		case a == "--all":
			all = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("sidebar %s: unknown flag %s", name, a)
		default:
			rest = append(rest, a)
		}
	}
	line := strings.Join(append([]string{name}, rest...), " ")
	cmd, err := view.ParseCommand(line)
	if err != nil {
		return fmt.Errorf("sidebar %s: %v", name, err)
	}
	if all && cmd.Name != "view" && cmd.Name != "scope" {
		return fmt.Errorf("sidebar %s: --all is for view and scope", name)
	}
	if client != "" && cmd.Name != "jump" {
		return fmt.Errorf("sidebar %s: -c is for jump", name)
	}
	if cmd.Name == "jump" && client == "" && workspace.Inside(ctx) {
		// The client the command ran from, when it is the default
		// server's: a shell nested on another server, the laatmux one,
		// would name a client the pane's switch-client cannot find.
		if out, err := (tmux.Server{}).Run(ctx, "display-message", "-p", "#{client_name}"); err == nil {
			client = strings.TrimSpace(string(out))
		}
	}
	if client != "" {
		line += " client=" + client
	}
	switch cmd.Name {
	case "view", "scope":
		if err := home.UpdateSidebar(time.Now(), func(s *home.Sidebar) {
			if cmd.Name == "view" {
				s.View = cmd.Arg
			} else {
				s.Scope = cmd.Arg
			}
		}); err != nil {
			return err
		}
	}
	if !all && window == "" {
		if os.Getenv("TMUX") != "" && !workspace.Inside(ctx) {
			// A shell nested on another server, the laatmux one: the
			// default server would take its pane id for one of its
			// own, or fall back to its latest session, and move a
			// sidebar the user is not looking at.
			return nil
		}
		out, err := workspace.Server.Run(ctx, "display-message", "-p", "#{window_id}")
		if err != nil {
			return nil // no server: nothing to control
		}
		window = strings.TrimSpace(string(out))
	}
	sockets, err := sidebarSockets(ctx, window, all)
	if err != nil {
		return nil
	}
	for _, s := range sockets {
		_ = send(s, line)
	}
	return nil
}

// sidebarSockets is the socket paths of the sidebar panes: the window's,
// or every one with all. A path is under the state directory, which can
// have tmux.Sep or a newline in it, so the panes are read through
// tmux.Fields.
func sidebarSockets(ctx context.Context, window string, all bool) ([]string, error) {
	args := []string{"list-panes"}
	if all {
		args = append(args, "-a")
	} else {
		args = append(args, "-t", window)
	}
	recs, err := workspace.Server.Records(ctx, tmux.NewFields("#{"+sidebarTag+"}", "#{"+socketTag+"}"), args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, f := range recs {
		if f[0] != "" && f[1] != "" {
			paths = append(paths, f[1])
		}
	}
	return paths, nil
}

// send writes one command line to a pane's socket and waits for its
// answer, briefly.
func send(path, line string) error {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintln(c, line); err != nil {
		return err
	}
	answer, _ := bufio.NewReader(c).ReadString('\n')
	if strings.HasPrefix(answer, "error:") {
		return errors.New(strings.TrimSpace(answer))
	}
	return nil
}

// reapSockets removes a socket whose server pid is the default server's
// and whose pane is gone, or one that refuses a connection; a socket
// of another server, which it cannot list, goes only when it refuses.
func reapSockets(ctx context.Context) {
	entries, err := os.ReadDir(socketDir())
	if err != nil {
		return
	}
	pid, err := serverPID(ctx)
	if err != nil {
		pid = -1
	}
	live, listed := map[string]bool{}, false
	if pid > 0 {
		if out, err := workspace.Server.Run(ctx, "list-panes", "-a", "-F", "#{pane_id}"); err == nil {
			listed = true
			for _, p := range strings.Fields(string(out)) {
				live[strings.TrimPrefix(p, "%")] = true
			}
		}
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sock") {
			continue
		}
		path := filepath.Join(socketDir(), name)
		parts := strings.SplitN(strings.TrimSuffix(name, ".sock"), "-", 2)
		// A pane gone, by a listing that succeeded: a failed one says
		// nothing, and the dial below decides.
		if len(parts) == 2 && listed && parts[0] == strconv.Itoa(pid) && !live[parts[1]] {
			os.Remove(path)
			continue
		}
		c, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err != nil {
			// A refused connection: no pane listens.
			os.Remove(path)
			continue
		}
		c.Close()
	}
}
