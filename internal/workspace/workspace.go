// Package workspace is the laptop side of a workspace: the local tmux
// session that attaches to a managed session on its host.
//
// A workspace is one worktree with one managed agent session and one local
// session. The local session lives in the user's default tmux server and
// is tagged with @laatmux_workspace = <environment_id>/<root>, the
// workspace key, and @laatmux_host = the configured host name. The key is
// what a local session is matched on: neither the host name nor the
// repository label is in it, so the session still matches its workspace
// after either is renamed. The session name, <host>/<repo>/<encoded
// branch>, is for display and for switching by name. @laatmux_repo, the
// repository source, and @laatmux_branch identify the worktree when its
// record is gone from the host: that is how rm finds the root of a stale
// workspace. The host, source and branch tags are refreshed every time
// the session is reused, so a renamed host or label does not go stale in
// them.
//
// A local session may instead carry @laatmux_attach = <host>/<session>:
// an attachment to a managed session that is not a worktree's, made by
// jump for a session new created. It is not a workspace.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/tmux"
)

// Server is the tmux server local sessions live on: the user's default one.
var Server = tmux.DefaultServer

// Key is the workspace key: <environment_id>/<root>. The environment id
// is hex, so the key parses from the left.
func Key(environmentID, root string) string { return environmentID + "/" + root }

// SplitKey returns the environment id and root of a key.
func SplitKey(key string) (environmentID, root string) {
	environmentID, root, _ = strings.Cut(key, "/")
	return environmentID, root
}

// SessionName is the local session name for a workspace:
// <host>/<repo>/<encoded branch>. The host is part of the name because the
// same repository and branch may be checked out on two hosts at once.
func SessionName(host, repo, branch string) string {
	return host + "/" + tmux.SessionName(repo, branch)
}

// Local is one session in the default tmux server, with the laatmux tags
// it carries. A session with neither Key nor Attach is not laatmux's.
type Local struct {
	Name    string
	Key     string // @laatmux_workspace
	Host    string // @laatmux_host
	Source  string // @laatmux_repo, the repository source; "" when unknown
	Branch  string // @laatmux_branch
	Attach  string // @laatmux_attach, for a plain attachment
	Settled bool   // @laatmux_settled
}

// Workspace reports whether the session is a workspace session.
func (l Local) Workspace() bool { return l.Key != "" }

var sessionFormat = strings.Join([]string{
	"#{session_name}", "#{@laatmux_workspace}", "#{@laatmux_host}", "#{@laatmux_attach}", "#{@laatmux_settled}",
	"#{@laatmux_repo}", "#{@laatmux_branch}",
}, tmux.Sep)

// List returns every session on the default server. No server running is
// an empty list.
func List(ctx context.Context) ([]Local, error) {
	out, err := Server.Run(ctx, "list-sessions", "-F", sessionFormat)
	if err != nil {
		if tmux.NoServer(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseSessions(string(out)), nil
}

func parseSessions(out string) []Local {
	var locals []Local
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Split(line, tmux.Sep)
		if len(f) < 7 || f[0] == "" {
			continue
		}
		locals = append(locals, Local{Name: f[0], Key: f[1], Host: f[2], Attach: f[3], Settled: f[4] != "", Source: f[5], Branch: f[6]})
	}
	return locals
}

// Current is the session the calling process runs in: the pane's session
// when TMUX_PANE is set, as it is for a process in a pane, else the
// session TMUX names, which is what a run-shell job from a key binding
// gets. Not inside tmux is an error saying so.
func Current(ctx context.Context) (Local, error) {
	if os.Getenv("TMUX") == "" {
		return Local{}, errors.New("not inside tmux")
	}
	// The lookup is on the server TMUX names, which may not be the
	// default one; the zero server follows TMUX.
	args := []string{"display-message", "-p"}
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		args = append(args, "-t", pane)
	}
	out, err := (tmux.Server{}).Run(ctx, append(args, sessionFormat)...)
	if err != nil {
		return Local{}, err
	}
	locals := parseSessions(string(out))
	if len(locals) != 1 {
		return Local{}, errors.New("cannot find the current tmux session")
	}
	return locals[0], nil
}

// FindWorktree returns the workspace session for a branch of a repository
// on the host with the environment id, by its tags.
func FindWorktree(locals []Local, environmentID, source, branch string) (Local, bool) {
	for _, l := range locals {
		if env, _ := SplitKey(l.Key); l.Workspace() && env == environmentID && l.Source == source && l.Branch == branch && source != "" {
			return l, true
		}
	}
	return Local{}, false
}

// Find returns the session with the key, or the plain attachment with the
// tag, whichever is given.
func Find(locals []Local, key, attach string) (Local, bool) {
	for _, l := range locals {
		if key != "" && l.Key == key {
			return l, true
		}
		if attach != "" && l.Attach == attach {
			return l, true
		}
	}
	return Local{}, false
}

// ByName returns the session with exactly this name.
func ByName(locals []Local, name string) (Local, bool) {
	for _, l := range locals {
		if l.Name == name {
			return l, true
		}
	}
	return Local{}, false
}

// Spec describes the local session for a managed session on a host.
type Spec struct {
	Host    client.Host // how the attach command reaches the host
	Managed string      // managed session name on the host
	Name    string      // local session name
	Key     string      // workspace key; "" for a plain attachment
	Source  string      // repository source, when known
	Branch  string
}

// Ensure finds the local session for the spec, or creates it: one window
// for the attach command, with the session tagged in the same tmux
// command sequence so it is never observable untagged, then the attach
// pane tagged by id and started. An existing session is found by its key,
// or by its attach tag for a plain attachment, whatever its name; its
// host, source and branch tags are refreshed, since it may predate a
// rename, and a dead attach pane in it is respawned. A session with the
// intended name that is not it is a name in use. The name of the session,
// existing or new, and whether it was created are returned.
func Ensure(ctx context.Context, s Spec) (name string, created bool, err error) {
	locals, err := List(ctx)
	if err != nil {
		return "", false, err
	}
	attach := ""
	if s.Key == "" {
		attach = s.Host.Name + "/" + s.Managed
	}
	if l, ok := Find(locals, s.Key, attach); ok {
		if _, err := Server.Run(ctx, tagArgs(l.Name, s)...); err != nil {
			return "", false, err
		}
		return l.Name, false, ensureAttach(ctx, l.Name, s)
	}
	if l, ok := ByName(locals, s.Name); ok {
		switch {
		case l.Workspace():
			_, root := SplitKey(l.Key)
			return "", false, fmt.Errorf("local session %s is the workspace for %s on %s; name in use", s.Name, root, l.Host)
		case l.Attach != "":
			return "", false, fmt.Errorf("local session %s is attached to %s; name in use", s.Name, l.Attach)
		default:
			return "", false, fmt.Errorf("local session %s exists and is not laatmux's; name in use", s.Name)
		}
	}
	args := []string{"new-session", "-d", "-s", s.Name, "-n", "agent", "-P", "-F", "#{pane_id}", placeholder}
	if s.Key != "" {
		args = append(args, ";", "set-option", "-t", s.Name, "@laatmux_workspace", s.Key)
	} else {
		args = append(args, ";", "set-option", "-t", s.Name, "@laatmux_attach", attach)
	}
	args = append(args, ";")
	args = append(args, tagArgs(s.Name, s)...)
	out, err := Server.Run(ctx, args...)
	if err != nil {
		return "", false, err
	}
	// The attach pane is tagged by the id new-session printed, not as the
	// session's active pane: the user's hooks may have split the window
	// by now. It stays when ssh or the managed session goes, so the rest
	// of the workspace survives and jump can respawn it.
	if err := startAttach(ctx, strings.TrimSpace(string(out)), s); err != nil {
		return "", false, err
	}
	return s.Name, true, nil
}

// placeholder is what a new attach pane runs until it is tagged: a
// command that does not exit, so the pane cannot die before remain-on-exit
// is set on it. The attach command, which may exit at once when ssh fails
// or the managed session is gone, replaces it in the same sequence as the
// tags; a pane that then dies stays for jump to respawn.
const placeholder = "sleep 2147483647"

// startAttach tags the pane and replaces its placeholder with the attach
// command, in one tmux command sequence.
func startAttach(ctx context.Context, paneID string, s Spec) error {
	_, err := Server.Run(ctx, "set-option", "-p", "-t", paneID, "remain-on-exit", "on",
		";", "set-option", "-p", "-t", paneID, "@laatmux_attach_pane", "1",
		";", "respawn-pane", "-k", "-t", paneID, AttachCommand(s.Host, s.Managed))
	return err
}

// tagArgs is the tmux command sequence that sets the routing and identity
// tags on a session: the host, and for a workspace the source and branch.
// A source or branch the spec does not know is not written, so a reuse
// that could not resolve one keeps what the session already carries.
func tagArgs(name string, s Spec) []string {
	args := []string{"set-option", "-t", name, "@laatmux_host", s.Host.Name}
	if s.Key != "" && s.Source != "" {
		args = append(args, ";", "set-option", "-t", name, "@laatmux_repo", s.Source)
	}
	if s.Key != "" && s.Branch != "" {
		args = append(args, ";", "set-option", "-t", name, "@laatmux_branch", s.Branch)
	}
	return args
}

// ensureAttach makes sure the session has a live attach pane: a dead one
// is respawned in place, and a session with no tagged attach pane at all,
// closed by hand or left by a crash before the tag, gets a new attach
// window. Other panes in the session are the user's and are left alone.
func ensureAttach(ctx context.Context, name string, s Spec) error {
	out, err := Server.Run(ctx, "list-panes", "-s", "-t", "="+name, "-F", strings.Join([]string{"#{pane_id}", "#{pane_dead}", "#{@laatmux_attach_pane}"}, tmux.Sep))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, tmux.Sep)
		if len(f) != 3 || f[2] == "" {
			continue
		}
		if f[1] == "1" {
			_, err := Server.Run(ctx, "respawn-pane", "-k", "-t", f[0], AttachCommand(s.Host, s.Managed))
			return err
		}
		return nil
	}
	out, err = Server.Run(ctx, "new-window", "-t", "="+name+":", "-n", "agent", "-P", "-F", "#{pane_id}", placeholder)
	if err != nil {
		return err
	}
	return startAttach(ctx, strings.TrimSpace(string(out)), s)
}

// AttachCommand is the shell command the attach window runs. TMUX is unset
// so the inner tmux does not refuse to nest; -u tells it the terminal is
// UTF-8.
func AttachCommand(h client.Host, session string) string {
	attach := append([]string{"tmux", "-u"}, tmux.LaatmuxServer.AttachArgsBare(session)...)
	if h.Local() {
		return tmux.ShellJoin(append([]string{"env", "-u", "TMUX"}, attach...))
	}
	return tmux.ShellJoin([]string{"ssh", "-t",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
		h.SSH, tmux.ShellJoin(attach)})
}

// ShellCommand is the shell command a shell window runs for a worktree
// root on its host: for a remote host, ssh with cd and the single-quoted
// root, then a literal exec of the login shell so the root passes through
// as one argument whatever it contains and $SHELL expands on the remote
// side. A local window needs no command; it is started in the root.
func ShellCommand(h client.Host, root string) string {
	remote := tmux.ShellJoin([]string{"cd", root}) + ` && exec "$SHELL" -l`
	return tmux.ShellJoin([]string{"ssh", "-t", h.SSH, remote})
}

// Inside reports whether the calling process is inside the default tmux
// server, so switch-client can reach a session on it. A process inside
// another server is outside for this purpose. Socket paths are compared
// as tmux reports them rather than trusting the inherited TMUX value.
func Inside(ctx context.Context) bool {
	if os.Getenv("TMUX") == "" {
		return false
	}
	here, err := (tmux.Server{}).Run(ctx, "display-message", "-p", "#{socket_path}")
	if err != nil {
		return false
	}
	def, err := Server.Run(ctx, "display-message", "-p", "#{socket_path}")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(here)) == strings.TrimSpace(string(def))
}

// Switch makes the session current for the calling client.
func Switch(ctx context.Context, name string) error {
	_, err := Server.Run(ctx, "switch-client", "-t", "="+name)
	return err
}

// AttachHint is what to run to attach to the session from outside tmux.
// The default server is selected explicitly, so the command does not
// follow an inherited TMUX that names another server.
func AttachHint(name string) string {
	return tmux.ShellJoin(append([]string{"tmux"}, Server.AttachArgsBare(name)...))
}

// Kill kills the session, switching the calling client away first when it
// is the current one, so the client is not left without a session.
func Kill(ctx context.Context, name string) error {
	if cur, err := Current(ctx); err == nil && cur.Name == name && Inside(ctx) {
		// switch-client -l picks the last session; -n the next. Either
		// fails when this is the only session, and kill-session then
		// detaches the client, which is what tmux does anyway.
		if _, err := Server.Run(ctx, "switch-client", "-l"); err != nil {
			_, _ = Server.Run(ctx, "switch-client", "-n")
		}
	}
	_, err := Server.Run(ctx, "kill-session", "-t", "="+name)
	return err
}

// SetSettled sets or clears @laatmux_settled on the session.
func SetSettled(ctx context.Context, name string, settled bool) error {
	var err error
	if settled {
		_, err = Server.Run(ctx, "set-option", "-t", name, "@laatmux_settled", "1")
	} else {
		_, err = Server.Run(ctx, "set-option", "-u", "-t", name, "@laatmux_settled")
	}
	return err
}
