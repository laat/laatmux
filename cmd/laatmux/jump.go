package main

import (
	"context"
	"errors"
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
func cmdJump(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: laatmux jump <host>/<session>")
	}
	hostName, session, ok := strings.Cut(args[0], "/")
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
	attachCmd := attachCommand(h, session)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, tmux.Sep)
		if len(f) != 5 || f[0] != tag {
			continue
		}
		if f[4] == "1" {
			// remain-on-exit kept a dead attachment; bring it back in place.
			if err := checkSession(ctx, h, session); err != nil {
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

	if err := checkSession(ctx, h, session); err != nil {
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
