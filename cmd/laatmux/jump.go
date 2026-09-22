package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// jump switches to the local workspace session for <host>/<repo>/<branch>,
// creating it from the host's record when it is missing. The workspace
// session is the unit: one local session per worktree, its first window
// attached to the managed session on the host. Local and remote are the
// same operation, since managed agents live on the dedicated laatmux
// server, which the user's tmux cannot switch-client into.
//
// A managed session that is no worktree's, one that new made, is reached
// the same way through a local session named <host>/<session>, tagged as
// a plain attachment rather than a workspace.
//
// With --server default the session is one the daemon merely observes on
// this machine's own tmux. It is already in the user's server, so jump is
// a switch-client. An observed session on any other server, or on a remote
// host, is refused: attach is only ever done to sessions laatmux created.
func cmdJump(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("jump", flag.ContinueOnError)
	server := fs.String("server", tmux.LaatmuxServer.Label(), `tmux server the session is on, as ls prints it after the host: "laatmux" or "default"`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: laatmux jump [--server s] <host>/<repo>/<branch> | <host>/<session>")
	}
	target := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return err
	}
	hostName, rest, ok := strings.Cut(target, "/")
	if !ok {
		rest, hostName = hostName, ""
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	h, ok := cfg.Find(hostName)
	if !ok {
		return fmt.Errorf("unknown host %q", hostName)
	}
	how, err := jumpMode(h.Host, tmux.Parse(*server), rest)
	if err != nil {
		return err
	}
	if how == jumpSwitch {
		// A client belongs to one server, so switching only works when the
		// server jump runs in is the default one.
		if !workspace.Server.HasSession(ctx, rest) {
			return fmt.Errorf("%s/%s: no such session on the default tmux server", h.Name, rest)
		}
		if !workspace.Inside(ctx) {
			return fmt.Errorf("%s/%s: is on the default tmux server; run jump from a client of it", h.Name, rest)
		}
		return workspace.Switch(ctx, rest)
	}
	hello, snap, err := snapshot(ctx, h.Host, "")
	if err != nil {
		return err
	}
	spec := workspace.Spec{Host: h.Host}
	if w, ok := matchWorktree(snap.Worktrees, cfg, rest); ok {
		if w.Session == "" {
			// The hint's --repo is resolved against this machine's config,
			// so it names the source as this machine knows it, not by the
			// host's label.
			return fmt.Errorf("%s/%s/%s has no managed session; start one with: laatmux add %s --repo %s --host %s", h.Name, w.Repo, w.Branch, w.Branch, localRepoArg(cfg, w), h.Name)
		}
		// A detached worktree has no <repo>/<branch> form; it is reached
		// by its session name.
		spec.Managed = w.Session
		// The managed session is <repo>/<encoded branch> as it was when
		// add made it; the local name follows it rather than the record's
		// branch, which is empty for a worktree detached since.
		spec.Name = h.Name + "/" + w.Session
		spec.Key = workspace.Key(hello.EnvironmentID, w.Root)
		spec.Branch = w.Branch
		// The source is the identity and comes from the record. A daemon
		// from before records carried it leaves it to this machine's
		// config, by the host's label, and empty when the labels differ;
		// Ensure then keeps whatever the session already knows.
		spec.Source = w.Source
		if spec.Source == "" {
			if r, ok := cfg.RepoByName(w.Repo); ok {
				spec.Source = r.Source
			}
		}
	} else {
		if err := checkSession(ctx, h.Host, rest); err != nil {
			return err
		}
		spec.Managed = rest
		spec.Name = h.Name + "/" + rest
	}
	name, created, err := workspace.Ensure(ctx, spec)
	if err != nil {
		return err
	}
	return focus(ctx, name, created)
}

// localRepoArg is what --repo takes for the record's repository on this
// machine: its label here when the source is known, else the source
// itself, which --repo also accepts, else the host's label from a daemon
// that sends no source.
func localRepoArg(cfg config.Config, w protocol.Worktree) string {
	if w.Source == "" {
		return w.Repo
	}
	if r, ok := cfg.RepoBySource(w.Source); ok {
		return r.Name
	}
	return w.Source
}

// matchWorktree finds the worktree a jump target names after the host,
// in order of precedence: <repo>/<branch> with the repository as this
// machine's label, which is the user's own vocabulary; the same with the
// host's label, for a source this machine has no label for; then the
// managed session's name, which is the host's label with the branch
// encoded. Each pass is a different reading of the target, so the first
// that matches wins whatever order the records arrive in: with branches
// a.b and a%2eb, the target proj/a%2eb is the second branch, not the
// first's session name. The session-name pass does not need a branch: a
// worktree detached in place keeps its root and session, and stays the
// same workspace.
func matchWorktree(ws []protocol.Worktree, cfg config.Config, rest string) (protocol.Worktree, bool) {
	label, branch, _ := strings.Cut(rest, "/")
	if local, ok := cfg.RepoByName(label); ok && branch != "" {
		for _, w := range ws {
			if w.Branch == branch && sameRepo(w, local) {
				return w, true
			}
		}
	}
	if branch != "" {
		for _, w := range ws {
			if w.Branch == branch && w.Repo == label {
				return w, true
			}
		}
	}
	for _, w := range ws {
		if w.Session != "" && w.Session == rest {
			return w, true
		}
	}
	return protocol.Worktree{}, false
}

type jumpKind int

const (
	jumpAttach jumpKind = iota // a local session attached to the managed server
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
