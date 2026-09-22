// Package tmux runs tmux commands against one tmux server.
package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Server addresses one tmux server: by -L name or -S path. The zero Server
// adds no selector, so tmux follows the inherited TMUX variable: that is the
// server the calling process is attached to, not the default one. Use
// DefaultServer for the default server; it selects -L default explicitly,
// which overrides TMUX.
type Server struct {
	Name string // -L
	Path string // -S, wins over Name
}

// DefaultServer is the user's default tmux server, selected explicitly so it
// stays the default server no matter which server the daemon or client was
// started from.
var DefaultServer = Server{Name: "default"}

// LaatmuxServer is the dedicated server managed agents live on. It is the
// only server laatmux configures or creates sessions on; every other server
// is watched read-only.
var LaatmuxServer = Server{Name: "laatmux"}

// Parse reads a server spec as config and flags write it: "default" or ""
// is DefaultServer, a value containing "/" is a -S socket path, and anything
// else is a -L name. Parse never returns the zero Server.
func Parse(v string) Server {
	switch {
	case v == "default" || v == "":
		return DefaultServer
	case strings.Contains(v, "/"):
		return Server{Path: v}
	default:
		return Server{Name: v}
	}
}

// Label names the server in agent ids and listings: the -S path or the -L
// name. Parse(s.Label()) == s for any server Parse returns. The zero Server
// is labelled "current", since it is whatever TMUX points at.
func (s Server) Label() string {
	switch {
	case s.Path != "":
		return s.Path
	case s.Name != "":
		return s.Name
	}
	return "current"
}

func (s Server) args(a ...string) []string {
	var pre []string
	switch {
	case s.Path != "":
		pre = []string{"-S", s.Path}
	case s.Name != "":
		pre = []string{"-L", s.Name}
	}
	return append(pre, a...)
}

// Run executes a tmux command and returns stdout.
func (s Server) Run(ctx context.Context, a ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tmux", s.args(a...)...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), &Error{Args: a, Msg: msg}
	}
	return out.Bytes(), nil
}

// Error is a failed tmux command.
type Error struct {
	Args []string
	Msg  string
}

func (e *Error) Error() string { return "tmux " + strings.Join(e.Args, " ") + ": " + e.Msg }

// NoServer reports whether the error means the server is not running. tmux
// says "no server running on <path>" when the socket is missing, and "error
// connecting to <path> (<reason>)" when it exists but cannot be used. Only a
// stale socket counts as absent; "Permission denied" and other reasons are
// failures to observe, not an empty server.
func NoServer(err error) bool {
	var te *Error
	if !errorsAs(err, &te) {
		return false
	}
	switch {
	case strings.Contains(te.Msg, "no server running"):
		return true
	case strings.Contains(te.Msg, "error connecting to"):
		return strings.Contains(te.Msg, "(No such file or directory)") || strings.Contains(te.Msg, "(Connection refused)")
	}
	return false
}

func errorsAs(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Pane is one row of list-panes -a.
type Pane struct {
	Session        string
	WindowIndex    int
	WindowName     string
	ID             string // %N
	TTY            string
	PID            int // first process in the pane
	CurrentCommand string
	CurrentPath    string
	Title          string
	Dead           bool
	WindowActivity int64
	Host           string // @laatmux_host pane option, "" when unset
	Cwd            string // @laatmux_cwd pane option, "" when unset
	Managed        bool   // @laatmux_managed pane option set
	ServerPID      int    // pid of the tmux server; changes when the server restarts
}

// Sep separates fields in list-panes output. tmux 3.5 strips control
// characters from expanded formats, so a control byte fuses the fields on
// Linux while passing through on macOS 3.6. A printable sequence that cannot
// occur in a title, path or session name is used instead.
const Sep = "\u2063\u2063" // two INVISIBLE SEPARATOR code points

var paneFormat = strings.Join([]string{
	"#{session_name}", "#{window_index}", "#{window_name}", "#{pane_id}", "#{pane_tty}",
	"#{pane_pid}", "#{pane_current_command}", "#{pane_current_path}", "#{pane_title}",
	"#{pane_dead}", "#{window_activity}", "#{@laatmux_host}", "#{@laatmux_cwd}", "#{@laatmux_managed}",
	"#{pid}",
}, Sep)

// ListPanes returns every pane on the server in one call.
func (s Server) ListPanes(ctx context.Context) ([]Pane, error) {
	out, err := s.Run(ctx, "list-panes", "-a", "-F", paneFormat)
	if err != nil {
		return nil, err
	}
	var panes []Pane
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, Sep)
		if len(f) < 15 {
			continue
		}
		p := Pane{
			Session: f[0], WindowName: f[2], ID: f[3], TTY: f[4],
			CurrentCommand: f[6], CurrentPath: f[7], Title: f[8],
			Host: f[11], Cwd: f[12],
		}
		p.WindowIndex, _ = strconv.Atoi(f[1])
		p.PID, _ = strconv.Atoi(f[5])
		p.Dead = f[9] == "1"
		p.WindowActivity, _ = strconv.ParseInt(f[10], 10, 64)
		p.Managed = f[13] != ""
		p.ServerPID, _ = strconv.Atoi(f[14])
		panes = append(panes, p)
	}
	return panes, nil
}

// Capture returns the pane's visible screen, oldest line first, as
// capture-pane -p prints it. Only the visible screen: -S with a negative
// number would start in the scrollback, and a dismissed dialog there must
// not override what is on screen now. Trailing blank lines are dropped;
// n caps the number of lines kept from the bottom.
func (s Server) Capture(ctx context.Context, paneID string, n int) ([]string, error) {
	out, err := s.Run(ctx, "capture-pane", "-p", "-t", paneID)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// Managed is true for the server laatmux owns and may configure. A named
// server that is not LaatmuxServer belongs to the user, like the default
// server, and is never configured.
func (s Server) Managed() bool { return s == LaatmuxServer }

// EnsureConfigured applies the managed-server configuration. A cold start
// uses -f /dev/null so the user's config never loads, but that only skips
// config files: the built-in defaults (prefix C-b, status on, default
// bindings) remain, so everything is set explicitly here as well. It is also
// applied when adopting a server that was started by hand, since the plan
// allows `tmux -L laatmux new` as manual setup. Idempotent. Never call this
// on a server the user owns; it refuses any server but LaatmuxServer.
func (s Server) EnsureConfigured(ctx context.Context) error {
	if !s.Managed() {
		return fmt.Errorf("tmux: refusing to configure unmanaged server %s", s.Label())
	}
	cmds := [][]string{
		{"set-option", "-g", "prefix", "None"},
		{"set-option", "-g", "prefix2", "None"},
		{"set-option", "-g", "status", "off"},
		{"set-option", "-g", "mouse", "off"},
		{"set-option", "-g", "set-titles", "off"},
		{"set-option", "-g", "allow-rename", "off"},
		{"set-option", "-g", "history-limit", "50000"},
		{"set-option", "-g", "escape-time", "0"},
		{"set-option", "-g", "focus-events", "on"},
		{"set-option", "-g", "default-terminal", "tmux-256color"},
		{"set-option", "-g", "remain-on-exit", "off"},
		// The most recent client sizes the window, so a second attachment
		// from a smaller terminal does not shrink the first.
		{"set-option", "-g", "window-size", "latest"},
		// -q: a table already emptied by a previous reconciliation no longer
		// exists on tmux 3.5, and that is not an error here.
		{"unbind-key", "-q", "-a", "-T", "root"},
		{"unbind-key", "-q", "-a", "-T", "prefix"},
	}
	for _, c := range cmds {
		if _, err := s.Run(ctx, c...); err != nil {
			return err
		}
	}
	// A hand-started server may carry global hooks from the user's config,
	// such as a split on new-session. Remove every global hook that has a
	// value; tmux 3.5 lists all hook names, set or not.
	if out, err := s.Run(ctx, "show-hooks", "-g"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name, _, ok := strings.Cut(line, " ")
			if !ok || name == "" {
				continue
			}
			if idx := strings.IndexByte(name, '['); idx > 0 {
				name = name[:idx]
			}
			_, _ = s.Run(ctx, "set-hook", "-gu", name)
		}
	}
	// Session-level overrides of the isolation options shadow the globals.
	if out, err := s.Run(ctx, "list-sessions", "-F", "#{session_name}"); err == nil {
		for _, sess := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if sess == "" {
				continue
			}
			for _, opt := range []string{"prefix", "prefix2", "status", "mouse"} {
				// set-option does not accept the =name exact-match form.
				_, _ = s.Run(ctx, "set-option", "-u", "-t", sess, opt)
			}
		}
	}
	return nil
}

// NewSessionOpts describes a managed session.
type NewSessionOpts struct {
	Name string
	Cwd  string
	Cmd  []string // empty: the user's shell
	Host string   // recorded in @laatmux_host
	Env  map[string]string
}

// NewSession creates a detached one-window one-pane session and tags its
// pane. Returns the pane id. If the server is not running it is started by
// new-session itself and then configured.
func (s Server) NewSession(ctx context.Context, o NewSessionOpts) (paneID string, err error) {
	if o.Name == "" {
		return "", fmt.Errorf("tmux: session name required")
	}
	if o.Cwd == "" {
		return "", fmt.Errorf("tmux: cwd required")
	}
	if _, err := os.Stat(o.Cwd); err != nil {
		return "", fmt.Errorf("tmux: cwd: %w", err)
	}
	_, notRunning := s.Run(ctx, "list-sessions")
	args := []string{}
	if notRunning != nil && s.Managed() {
		// Cold start: new-session starts the server. Skip config files.
		args = append(args, "-f", "/dev/null")
	} else if s.Managed() {
		// Adopting a running server: reconcile before creating the session,
		// so inherited hooks cannot act on it.
		if err := s.EnsureConfigured(ctx); err != nil {
			return "", err
		}
	}
	args = append(args, "new-session", "-d", "-s", o.Name, "-c", o.Cwd, "-P", "-F", "#{pane_id}")
	for k, v := range o.Env {
		args = append(args, "-e", k+"="+v)
	}
	if len(o.Cmd) > 0 {
		args = append(args, shellJoin(o.Cmd))
	}
	out, err := s.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	paneID = strings.TrimSpace(string(out))
	if s.Managed() {
		if notRunning != nil {
			if err := s.EnsureConfigured(ctx); err != nil {
				return paneID, err
			}
		}
		// The invariant is one session, one window, one pane. Anything
		// else means something outside laatmux acted on the session;
		// refuse it rather than report a topology that jump cannot use.
		if pout, err := s.Run(ctx, "list-panes", "-s", "-t", "="+o.Name, "-F", "#{pane_id}"); err == nil {
			if n := len(strings.Fields(string(pout))); n != 1 {
				_, _ = s.Run(ctx, "kill-session", "-t", "="+o.Name)
				return "", fmt.Errorf("tmux: session %q came up with %d panes, expected 1; server config interfered", o.Name, n)
			}
		}
	}
	opts := [][]string{
		{"set-option", "-p", "-t", paneID, "@laatmux_managed", "1"},
		{"set-option", "-p", "-t", paneID, "@laatmux_cwd", o.Cwd},
	}
	if o.Host != "" {
		opts = append(opts, []string{"set-option", "-p", "-t", paneID, "@laatmux_host", o.Host})
	}
	for _, c := range opts {
		if _, err := s.Run(ctx, c...); err != nil {
			return paneID, err
		}
	}
	return paneID, nil
}

// HasSession reports whether a session exists on the server.
func (s Server) HasSession(ctx context.Context, name string) bool {
	_, err := s.Run(ctx, "has-session", "-t", "="+name)
	return err == nil
}

// AttachArgsBare is the argv after "tmux" to attach a terminal to a session
// on this server, for use locally or after ssh -t.
func (s Server) AttachArgsBare(session string) []string {
	return s.args("attach-session", "-t", "="+session)
}

// ArgsBare prepends this server's -L/-S selection to a tmux command.
func (s Server) ArgsBare(a ...string) []string { return s.args(a...) }

// shellJoin quotes argv for tmux's shell-command argument.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		// Leading = and ~ are zsh equals and tilde expansion: an unquoted
		// "=lcl" makes zsh look up a command named lcl and abort the line.
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`!*?[]{}()<>|&;#~") || a[0] == '=' {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}

// ShellJoin is exported for the client, which builds ssh commands.
func ShellJoin(argv []string) string { return shellJoin(argv) }
