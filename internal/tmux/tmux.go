// Package tmux runs tmux commands against one tmux server.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	return s.RunInput(ctx, nil, a...)
}

// RunInput executes a tmux command with stdin from in, for a command
// that reads its input rather than taking it in its arguments, and
// returns stdout.
func (s Server) RunInput(ctx context.Context, in io.Reader, a ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tmux", s.args(a...)...)
	cmd.Stdin = in
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

// ListPanes returns every pane on the server in one call. A server
// that runs with no sessions, which the managed one does after its last
// session ends, has no panes: tmux answers "no current target" for it,
// and that is an empty listing, not a failure to observe.
func (s Server) ListPanes(ctx context.Context) ([]Pane, error) {
	out, err := s.Run(ctx, "list-panes", "-a", "-F", paneFormat)
	if err != nil {
		var te *Error
		if errorsAs(err, &te) && strings.Contains(te.Msg, "no current target") {
			return nil, nil
		}
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
		// The server stays when its last session ends, so a cold start
		// can make it empty and configure it before any session.
		{"set-option", "-s", "exit-empty", "off"},
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
// pane, in one tmux invocation: new-session and the set-option calls are
// one ;-separated command sequence, which tmux runs to completion once
// submitted, so the session is never observable without its options.
// Returns the pane id and the server's pid, the instance the pane is
// on. If the server is not running it is started first, empty, and
// configured, then the session is made.
func (s Server) NewSession(ctx context.Context, o NewSessionOpts) (made Session, err error) {
	if o.Name == "" {
		return made, fmt.Errorf("tmux: session name required")
	}
	if o.Cwd == "" {
		return made, fmt.Errorf("tmux: cwd required")
	}
	if _, err := os.Stat(o.Cwd); err != nil {
		return made, fmt.Errorf("tmux: cwd: %w", err)
	}
	_, notRunning := s.Run(ctx, "list-sessions")
	if notRunning != nil && s.Managed() {
		// Cold start: the server is started on its own, with no config
		// file and told to stay without sessions, and the session is
		// made in a second invocation. The process that starts a tmux
		// server is the server, and keeps its command line for as long
		// as it runs; a new-session that started it would leave the
		// agent's command, a prompt included, on the process list for
		// the server's lifetime rather than the agent's.
		if _, err := s.Run(ctx, "-f", "/dev/null", "start-server", ";", "set-option", "-s", "exit-empty", "off"); err != nil {
			return made, err
		}
	}
	if s.Managed() {
		// Reconcile before creating the session, so inherited hooks
		// cannot act on it; a cold-started server has the built-in
		// defaults to undo.
		if err := s.EnsureConfigured(ctx); err != nil {
			return made, err
		}
	}
	args := []string{"new-session", "-d", "-s", o.Name, "-c", o.Cwd, "-P", "-F", "#{pane_id} #{pid}"}
	for k, v := range o.Env {
		args = append(args, "-e", k+"="+v)
	}
	if len(o.Cmd) > 0 {
		args = append(args, shellJoin(o.Cmd))
	}
	// The pane target is the session by exact name: a session target with
	// a trailing colon resolves to its current window's active pane, and
	// the session has exactly one.
	target := "=" + o.Name + ":"
	opts := [][2]string{{"@laatmux_managed", "1"}, {"@laatmux_cwd", o.Cwd}}
	if o.Host != "" {
		opts = append(opts, [2]string{"@laatmux_host", o.Host})
	}
	for _, kv := range opts {
		args = append(args, ";", "set-option", "-p", "-t", target, kv[0], kv[1])
	}
	// From here on the session may exist whatever the error: the
	// sequence runs to completion once submitted, and the steps after
	// it act on a session that is there.
	out, err := s.Run(ctx, args...)
	if err != nil {
		return made, &SubmittedError{Err: err}
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return made, &SubmittedError{Err: fmt.Errorf("tmux: new-session printed %q, expected a pane id and a server pid", strings.TrimSpace(string(out)))}
	}
	made.PaneID = fields[0]
	made.ServerPID, _ = strconv.Atoi(fields[1])
	if s.Managed() {
		// The invariant is one session, one window, one pane. Anything
		// else means something outside laatmux acted on the session;
		// refuse it rather than report a topology that jump cannot use.
		if pout, err := s.Run(ctx, "list-panes", "-s", "-t", "="+o.Name, "-F", "#{pane_id}"); err == nil {
			if n := len(strings.Fields(string(pout))); n != 1 {
				_, _ = s.Run(ctx, "kill-session", "-t", "="+o.Name)
				return Session{}, &SubmittedError{Err: fmt.Errorf("tmux: session %q came up with %d panes, expected 1; server config interfered", o.Name, n)}
			}
		}
	}
	return made, nil
}

// Session is what NewSession made: the pane and the server instance,
// by its pid, the pane is on.
type Session struct {
	PaneID    string
	ServerPID int
}

// SubmittedError is a NewSession failure after new-session was
// submitted to the server: the session, and the command in it, may
// exist. An error before that point is plain, and nothing was started.
type SubmittedError struct{ Err error }

func (e *SubmittedError) Error() string { return e.Err.Error() }
func (e *SubmittedError) Unwrap() error { return e.Err }

// Submitted reports whether err is a launch that may have taken.
func Submitted(err error) bool {
	var se *SubmittedError
	return errors.As(err, &se)
}

// Paste types text into a pane as one bracketed paste followed by Enter,
// through a buffer named for the caller: load-buffer reads the text from
// stdin, so it is never on a command line; paste-buffer -p sends it
// bracketed, so an application that asked for bracketed paste takes it
// as one insertion; send-keys Enter submits it; the buffer is deleted
// whether or not the steps before succeeded. The error says how far it
// got: a PasteError whose Step is "load" or "paste" means nothing
// reached the pane, "enter" means the text did and the submit may not
// have.
func (s Server) Paste(ctx context.Context, buffer, paneID, text string) error {
	defer s.Run(ctx, "delete-buffer", "-b", buffer)
	if _, err := s.RunInput(ctx, strings.NewReader(text), "load-buffer", "-b", buffer, "-"); err != nil {
		return &PasteError{Step: "load", Err: err}
	}
	if _, err := s.Run(ctx, "paste-buffer", "-p", "-b", buffer, "-t", paneID); err != nil {
		return &PasteError{Step: "paste", Err: err}
	}
	if _, err := s.Run(ctx, "send-keys", "-t", paneID, "Enter"); err != nil {
		return &PasteError{Step: "enter", Err: err}
	}
	return nil
}

// PasteError is a Paste that failed at Step.
type PasteError struct {
	Step string
	Err  error
}

func (e *PasteError) Error() string { return "paste (" + e.Step + "): " + e.Err.Error() }
func (e *PasteError) Unwrap() error { return e.Err }

// DeleteBuffers deletes every buffer on the server whose name has the
// prefix: what a Paste interrupted between loading and deleting leaves.
// No server is nothing to delete.
func (s Server) DeleteBuffers(ctx context.Context, prefix string) error {
	out, err := s.Run(ctx, "list-buffers", "-F", "#{buffer_name}")
	if err != nil {
		if NoServer(err) {
			return nil
		}
		return err
	}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name == "" || !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, err := s.Run(ctx, "delete-buffer", "-b", name); err != nil {
			return err
		}
	}
	return nil
}

// Redact replaces every occurrence of secret in an error's text, bare
// and as shellJoin quotes it, with placeholder, so a tmux error that
// echoes its command line does not carry a prompt into a result or a
// log. The error's type is lost; the caller has classified it already.
func Redact(err error, secret, placeholder string) error {
	if err == nil || secret == "" {
		return err
	}
	msg := err.Error()
	for _, form := range []string{shellJoin([]string{secret}), secret} {
		msg = strings.ReplaceAll(msg, form, placeholder)
	}
	return errors.New(msg)
}

// KillSession kills the session with exactly this name.
func (s Server) KillSession(ctx context.Context, name string) error {
	_, err := s.Run(ctx, "kill-session", "-t", "="+name)
	return err
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

// EncodeBranch makes a branch safe for a tmux session name, injectively:
// tmux rejects "." and ":" in session names, so "%" becomes "%25", "."
// becomes "%2e" and ":" becomes "%3a"; nothing else changes. Distinct
// branches give distinct names and DecodeBranch is exact.
func EncodeBranch(branch string) string {
	var b strings.Builder
	for i := 0; i < len(branch); i++ {
		switch c := branch[i]; c {
		case '%':
			b.WriteString("%25")
		case '.':
			b.WriteString("%2e")
		case ':':
			b.WriteString("%3a")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// DecodeBranch reverses EncodeBranch. Sequences EncodeBranch never emits
// are left as they are.
func DecodeBranch(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '%' && i+2 < len(name) {
			switch name[i+1 : i+3] {
			case "25":
				b.WriteByte('%')
				i += 2
				continue
			case "2e":
				b.WriteByte('.')
				i += 2
				continue
			case "3a":
				b.WriteByte(':')
				i += 2
				continue
			}
		}
		b.WriteByte(name[i])
	}
	return b.String()
}

// SessionName is the managed session name for a worktree on its host:
// <repo>/<encoded branch>. Labels cannot contain "/", so the first
// component is the label and the rest is the branch, slashes included.
func SessionName(repo, branch string) string { return repo + "/" + EncodeBranch(branch) }
