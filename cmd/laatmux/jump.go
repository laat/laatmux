package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/tmux"
)

// jump focuses the local pane attached to <host>/<session>, opening one in
// the session jump was run from if none exists. Local and remote are the same
// operation: managed agents live on the dedicated laatmux server, which the
// user's tmux cannot switch-client into, so both attach through a pane.
//
// With --server default the session is one the daemon merely observes on
// this machine's own tmux. It is already in the user's server, so jump is a
// switch-client. An observed session on any other server, or on a remote
// host, is refused: attach is only ever done to sessions laatmux created.
func cmdJump(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("jump", flag.ContinueOnError)
	server := fs.String("server", tmux.LaatmuxServer.Label(), `tmux server the session is on, as ls prints it after the host: "laatmux" or "default"`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: laatmux jump [--server s] <host>/<session>")
	}
	target := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return err
	}
	hostName, session, ok := strings.Cut(target, "/")
	if !ok {
		session, hostName = hostName, ""
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	h, ok := cfg.Find(hostName)
	if !ok {
		return fmt.Errorf("unknown host %q", hostName)
	}
	here := os.Getenv("TMUX_PANE")
	if os.Getenv("TMUX") == "" || here == "" {
		return errors.New("jump must run inside the local tmux")
	}
	local := tmux.Server{}
	how, err := jumpMode(h.Host, tmux.Parse(*server), session)
	if err != nil {
		return err
	}
	if how == jumpSwitch {
		// A client belongs to one server, so switching only works when the
		// server jump runs in is the default one. Compare socket paths as
		// tmux reports them rather than trusting the inherited TMUX value.
		def := tmux.DefaultServer
		if !def.HasSession(ctx, session) {
			return fmt.Errorf("%s/%s: no such session on the default tmux server", h.Name, session)
		}
		same, err := sameServer(ctx, local, def)
		if err != nil {
			return err
		}
		if !same {
			return fmt.Errorf("%s/%s: is on the default tmux server, but jump was run from another server (%s); run it from a client of the default server", h.Name, session, os.Getenv("TMUX"))
		}
		_, err = def.Run(ctx, "switch-client", "-t", "="+session)
		return err
	}
	// The session jump runs in is where a new attach window goes. Using the
	// pane rather than the current client keeps jump deterministic when run
	// from a script or a sidebar.
	out, err := local.Run(ctx, "display-message", "-p", "-t", here, "#{session_name}")
	if err != nil {
		return err
	}
	hereSession := strings.TrimSpace(string(out))
	tag := h.Name + "/" + session

	// Reuse an existing attachment, alive or dead.
	out, err = local.Run(ctx, "list-panes", "-a", "-F", strings.Join([]string{"#{@laatmux_attach}", "#{session_name}", "#{window_id}", "#{pane_id}", "#{pane_dead}"}, tmux.Sep))
	if err != nil {
		return err
	}
	attachCmd := attachCommand(h.Host, session)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, tmux.Sep)
		if len(f) != 5 || f[0] != tag {
			continue
		}
		if f[4] == "1" {
			// remain-on-exit kept a dead attachment; bring it back in place.
			if err := checkSession(ctx, h.Host, session); err != nil {
				return err
			}
			if _, err := local.Run(ctx, "respawn-pane", "-k", "-t", f[3], attachCmd); err != nil {
				return err
			}
		}
		if f[1] != hereSession {
			if _, err := local.Run(ctx, "switch-client", "-t", f[1]); err != nil {
				return err
			}
		}
		if _, err := local.Run(ctx, "select-window", "-t", f[2]); err != nil {
			return err
		}
		_, err := local.Run(ctx, "select-pane", "-t", f[3])
		return err
	}

	if err := checkSession(ctx, h.Host, session); err != nil {
		return err
	}
	out, err = local.Run(ctx, "new-window", "-t", hereSession+":", "-n", session, "-P", "-F", "#{pane_id}", attachCmd)
	if err != nil {
		return err
	}
	paneID := strings.TrimSpace(string(out))
	for _, kv := range [][2]string{{"@laatmux_attach", tag}, {"@laatmux_host", h.Name}} {
		if _, err := local.Run(ctx, "set-option", "-p", "-t", paneID, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

type jumpKind int

const (
	jumpAttach jumpKind = iota // open or focus an attach pane onto the managed server
	jumpSwitch                 // switch-client within this machine's default server
)

// jumpMode decides how a session is reached, or that it is not: the managed
// server anywhere is attached; the default server on this machine is
// switched to; everything else is observed only.
func jumpMode(h client.Host, srv tmux.Server, session string) (jumpKind, error) {
	switch {
	case srv.Managed():
		return jumpAttach, nil
	case srv == tmux.DefaultServer && h.Local():
		return jumpSwitch, nil
	case srv == tmux.DefaultServer:
		return 0, fmt.Errorf("%s/%s: on %s's default tmux server, which laatmux only observes; attach is limited to managed sessions", h.Name, session, h.Name)
	default:
		return 0, fmt.Errorf("%s/%s: tmux server %s is not managed by laatmux; attach is limited to managed sessions", h.Name, session, srv.Label())
	}
}

// sameServer reports whether two selectors reach the same tmux server.
func sameServer(ctx context.Context, a, b tmux.Server) (bool, error) {
	pa, err := a.Run(ctx, "display-message", "-p", "#{socket_path}")
	if err != nil {
		return false, err
	}
	pb, err := b.Run(ctx, "display-message", "-p", "#{socket_path}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(pa)) == strings.TrimSpace(string(pb)), nil
}

// attachCommand is the shell command an attach pane runs. TMUX is unset so
// the inner tmux does not refuse to nest; -u tells it the terminal is UTF-8.
func attachCommand(h client.Host, session string) string {
	attach := append([]string{"tmux", "-u"}, tmux.LaatmuxServer.AttachArgsBare(session)...)
	if h.Local() {
		return tmux.ShellJoin(append([]string{"env", "-u", "TMUX"}, attach...))
	}
	return tmux.ShellJoin([]string{"ssh", "-t",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
		h.SSH, tmux.ShellJoin(attach)})
}

// checkSession fails early when the target session does not exist, so jump
// reports it instead of opening a window that exits at once. A transport
// failure is reported as such, with ssh's own diagnostics, and the check is
// bounded so a stalled connection cannot block jump before the attach
// window's own keepalive protection applies.
func checkSession(ctx context.Context, h client.Host, session string) error {
	if h.Local() {
		if !tmux.LaatmuxServer.HasSession(ctx, session) {
			return fmt.Errorf("%s/%s: no such session on the laatmux tmux server", h.Name, session)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=2",
		h.SSH,
		tmux.ShellJoin(append([]string{"tmux"}, tmux.LaatmuxServer.ArgsBare("has-session", "-t", "="+session)...)))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	// After the deadline kills ssh, do not wait on its stderr pipe for
	// anything it may have left behind.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return classifyPreflight(h.Name, session, err, ctx.Err(), strings.TrimSpace(stderr.String()))
}

const preflightTimeout = 15 * time.Second

// classifyPreflight turns the preflight's outcome into a message that says
// which of three things happened: the session is absent (tmux exited 1), the
// transport failed (ssh exits 255, or anything else), or the check timed out.
func classifyPreflight(host, session string, runErr, ctxErr error, stderr string) error {
	if runErr == nil {
		return nil
	}
	if ctxErr != nil {
		return fmt.Errorf("%s: ssh preflight timed out after %s", host, preflightTimeout)
	}
	var exit *exec.ExitError
	if errors.As(runErr, &exit) && exit.ExitCode() == 1 {
		// tmux has-session: exit 1 means no such session. Its message
		// ("can't find session") is redundant; a missing server says
		// "no server running", which is the same thing for jump.
		return fmt.Errorf("%s/%s: no such session on the laatmux tmux server", host, session)
	}
	if stderr == "" {
		stderr = runErr.Error()
	}
	return fmt.Errorf("%s: ssh failed: %s", host, stderr)
}
