// Package tmux runs tmux commands against one tmux server.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
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

// ParseServers turns the specs a config's tmux_servers or a --tmux-servers
// flag lists into servers, rejecting duplicates. An empty list is the
// managed laatmux server alone.
func ParseServers(specs []string) ([]Server, error) {
	if len(specs) == 0 {
		return []Server{LaatmuxServer}, nil
	}
	seen := map[string]bool{}
	out := make([]Server, 0, len(specs))
	for _, v := range specs {
		s := Parse(v)
		if seen[s.Label()] {
			return nil, fmt.Errorf("tmux_servers: %s listed twice", s.Label())
		}
		seen[s.Label()] = true
		out = append(out, s)
	}
	return out, nil
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

// Next separates two commands of one tmux invocation: callers pass it
// between them, and args writes it as the bare ";" tmux takes for a
// command separator. A ";" a caller passes is a value, a branch named
// ";" say, and is written "\;", which tmux reads as that ";"; so a
// separator cannot be passed as ";". Next has a NUL byte in it, which
// no argument of a command can have, so it is never a value.
const Next = "\x00;"

// args is the argv after "tmux": the server's selector, then the
// command. tmux takes an argument that ends in ";" as the argument
// before it followed by a command separator, so a root or an option
// value that ends in one would be cut there and split the sequence, and
// it takes a "\;" at the end as a literal ";". Next is written ";";
// every other argument that ends in ";", a bare ";" included, gets a
// backslash before that last ";", and one that ends in "\;" becomes
// "\\;", which tmux reads back as "\;". The selector is read by tmux's
// option parser, which takes it as it is; so is a global flag a caller
// puts before the command, -f /dev/null, which never ends in ";".
func (s Server) args(a ...string) []string {
	var pre []string
	switch {
	case s.Path != "":
		pre = []string{"-S", s.Path}
	case s.Name != "":
		pre = []string{"-L", s.Name}
	}
	out := append(make([]string, 0, len(pre)+len(a)), pre...)
	for _, v := range a {
		switch {
		case v == Next:
			v = ";"
		case strings.HasSuffix(v, ";"):
			v = v[:len(v)-1] + `\;`
		}
		out = append(out, v)
	}
	return out
}

// Run executes a tmux command and returns stdout.
func (s Server) Run(ctx context.Context, a ...string) ([]byte, error) {
	return s.RunInput(ctx, nil, a...)
}

// RunInput executes a tmux command with stdin from in, for a command
// that reads its input rather than taking it in its arguments, and
// returns stdout.
func (s Server) RunInput(ctx context.Context, in io.Reader, a ...string) ([]byte, error) {
	// -u: tmux writes every non-ASCII character of what it prints, an
	// expanded format or an error, as _ to a client that is not UTF-8,
	// and Sep, which takes no column, as nothing; a client is UTF-8
	// only by -u, a UTF-8 locale or an inherited TMUX. A daemon started
	// without LANG, or a command run over ssh, can have neither, and
	// would list no pane at all and read a session name, root or tag
	// with a non-ASCII byte in it as another.
	cmd := exec.CommandContext(ctx, "tmux", append([]string{"-u"}, s.args(a...)...)...)
	cmd.Stdin = in
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), &Error{Args: a, Msg: msg, err: err}
	}
	return out.Bytes(), nil
}

// Error is a failed tmux command.
type Error struct {
	Args []string
	Msg  string
	err  error // the run's own, for NotInstalled
}

// Error names the command as the caller gave it, each Next written as
// the ";" it stands for.
func (e *Error) Error() string {
	a := make([]string, len(e.Args))
	for i, v := range e.Args {
		if v == Next {
			v = ";"
		}
		a[i] = v
	}
	return "tmux " + strings.Join(a, " ") + ": " + e.Msg
}

// NoServer reports whether the error means the server is not running. tmux
// says "no server running on <path>" when the socket is missing, and "error
// connecting to <path> (<reason>)" when it exists but cannot be used. Only a
// stale socket counts as absent; "Permission denied" and other reasons are
// failures to observe, not an empty server.
func NoServer(err error) bool {
	var te *Error
	if !errors.As(err, &te) {
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

// NotInstalled reports whether the error means there was no tmux to run:
// no tmux binary on PATH. tmux never ran, so there is no message of its
// own; the run's error says so.
func NotInstalled(err error) bool {
	var te *Error
	return errors.As(err, &te) && errors.Is(te.err, exec.ErrNotFound)
}

// NoSocket reports whether nothing is at the socket a tmux run would
// connect to for the server, so no server can be running on it: what
// tmux says "no server running" for. It is how a caller with no tmux to
// run tells a machine without a server from one whose server it cannot
// reach. Anything there, a stale socket or a link included, a path that
// cannot be checked, or one that cannot be told, is not NoSocket.
func (s Server) NoSocket() bool {
	path, ok := s.socket()
	if !ok {
		return false
	}
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// socket is the path a stock tmux connects to for the server: -S as
// given; with no selector, the socket TMUX names; else the -L name, or
// default, in tmux-<uid> under TMUX_TMPDIR with its links resolved, as
// tmux resolves it, or under /tmp when that is unset or empty. A
// TMUX_TMPDIR that does not resolve cannot be told, so ok is false:
// tmux 3.2 to 3.7 fall back to /tmp then, 3.1 and upstream's master fail.
func (s Server) socket() (path string, ok bool) {
	if s.Path != "" {
		return s.Path, true
	}
	name := s.Name
	if name == "" {
		if v, _, _ := strings.Cut(os.Getenv("TMUX"), ","); v != "" {
			return v, true
		}
		name = "default"
	}
	dir := "/tmp"
	if v := os.Getenv("TMUX_TMPDIR"); v != "" {
		var err error
		if dir, err = filepath.EvalSymlinks(v); err != nil {
			return "", false
		}
	}
	return filepath.Join(dir, "tmux-"+strconv.Itoa(os.Getuid()), name), true
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
	// InMode is a pane in a tmux mode, copy-mode or a chooser: keys sent
	// to it reach the mode, not the program, while a capture still
	// shows the program's screen.
	InMode bool
	// Own is a pane laatmux made for itself, a sidebar pane or a
	// workspace session's attach pane, by its @laatmux_sidebar or
	// @laatmux_attach_pane tag: not a pane of the user's.
	Own bool
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
	"#{pid}", "#{pane_in_mode}", "#{@laatmux_sidebar}", "#{@laatmux_attach_pane}",
}, Sep)

// ListPanes returns every pane on the server in one call. A server
// that runs with no sessions, which the managed one does after its last
// session ends, has no panes: tmux answers "no current target" for it,
// and that is an empty listing, not a failure to observe.
func (s Server) ListPanes(ctx context.Context) ([]Pane, error) {
	out, err := s.Run(ctx, "list-panes", "-a", "-F", paneFormat)
	if err != nil {
		var te *Error
		if errors.As(err, &te) && strings.Contains(te.Msg, "no current target") {
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
		p.InMode = len(f) > 15 && f[15] == "1"
		p.Own = len(f) > 17 && (f[16] != "" || f[17] != "")
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
// allows `tmux -L laatmux new` as manual setup. The global environment's
// locale is set last, by ensureLocale. Idempotent. Never call this on a
// server the user owns; it refuses any server but LaatmuxServer.
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
		// tmux's own list, which has no locale variable: a hand-started
		// server's could copy the daemon's LC_ALL=C into a session, or
		// remove the LANG ensureLocale set, since new-session takes the
		// listed variables from the client, the daemon, set or not.
		{"set-option", "-gu", "update-environment"},
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
				// The session by exact name: a bare 0, the name the
				// first session of a hand-started server gets, is
				// pane 0 of the current session.
				_, _ = s.Run(ctx, "set-option", "-u", "-t", "="+sess+":", opt)
			}
		}
	}
	return s.ensureLocale(ctx)
}

// CheckSessionName refuses a session name that tmux would not store as
// given, or could not find by its name once made, and says which
// character is the reason. tmux rewrites a name after expanding it as a
// format, so no escape gets these through:
//   - a . or a : is stored as _, and a \ doubled;
//   - a byte that is not UTF-8 and a control character are stored
//     escaped, a C1 one by tmux 3.3 (U+0085 as \302\205);
//   - tmux 3.3 built without utf8proc stores escaped a character its C
//     library has no width for: a line or paragraph separator, a
//     noncharacter, and a code point the library's tables do not have,
//     unassigned or a recent emoji on an older glibc; that last one
//     cannot be told from here, since Go's tables are not the host's;
//   - a $ before a letter, _ or { is stored as \$ by tmux 3.2 to 3.4,
//     whatever the host's isalpha takes for a letter, and every version
//     reads the target =$name as the session id $name, so a $ is
//     refused wherever it is.
//
// tmux keeps a U+2063, but Sep is made of it, and a name with one would
// split or shift the fields laatmux reads the name in. A # is not
// refused: new-session is given the name as FormatLiteral writes it. A
// ; is kept, at the end too, since args writes that one \;. A name the
// user chose is the name laatmux reports and targets, so it is refused
// rather than changed; a branch is made safe by EncodeBranch instead.
func CheckSessionName(name string) error {
	if name == "" {
		return errors.New("session name required")
	}
	for i := 0; i < len(name); {
		r, size := utf8.DecodeRuneInString(name[i:])
		var why string
		switch {
		case r == utf8.RuneError && size == 1:
			why = fmt.Sprintf("the byte 0x%02x, which is not UTF-8 and which tmux stores escaped", name[i])
		case unicode.IsControl(r):
			why = fmt.Sprintf("the control character %U, which tmux stores escaped", r)
		case unicode.In(r, unicode.Zl, unicode.Zp) || r >= 0xfdd0 && r <= 0xfdef || r&0xfffe == 0xfffe:
			why = fmt.Sprintf("the character %U, which tmux 3.3 stores escaped", r)
		case r == '.' || r == ':':
			why = fmt.Sprintf("a %c, which tmux stores as _", r)
		case r == '\\':
			why = `a \, which tmux stores doubled`
		case r == '$':
			why = `a $, which tmux before 3.5 stores as \$ before a letter, and which a target reads as a session id when the name starts with it`
		case strings.ContainsRune(Sep, r):
			why = fmt.Sprintf("the character %U, which laatmux separates the fields of tmux's listings with", r)
		}
		if why != "" {
			return fmt.Errorf("session name %q has %s", name, why)
		}
		i += size
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
// configured, then the session is made. The name is not checked here:
// a worktree's comes from SessionName, and a name new was given has
// passed CheckSessionName in the daemon.
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
		if _, err := s.Run(ctx, "-f", "/dev/null", "start-server", Next, "set-option", "-s", "exit-empty", "off"); err != nil {
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
	// new-session expands -c as a format, and a root has the branch in
	// it. A directory that is not there after expansion would start the
	// pane in $HOME, with no error. It expands -s too, and a name new
	// was given may have a # in it.
	args := []string{"new-session", "-d", "-s", FormatLiteral(o.Name), "-c", FormatLiteral(o.Cwd), "-P", "-F", "#{pane_id} #{pid}"}
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
		args = append(args, Next, "set-option", "-p", "-t", target, kv[0], kv[1])
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
		// The session is named exactly here as well: =name alone is
		// taken as a window of the current session first, and for a
		// caller whose TMUX_PANE names a pane on this server the
		// current session is that pane's.
		if pout, err := s.Run(ctx, "list-panes", "-s", "-t", target, "-F", "#{pane_id}"); err == nil {
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

// SelectPane makes a pane and its window the server's current ones, so
// a client attached to its session shows it.
func (s Server) SelectPane(ctx context.Context, paneID string) error {
	if _, err := s.Run(ctx, "select-window", "-t", paneID); err != nil {
		return err
	}
	_, err := s.Run(ctx, "select-pane", "-t", paneID)
	return err
}

// SendKeys presses tmux key names in the pane, Down or Enter say.
func (s Server) SendKeys(ctx context.Context, paneID string, keys ...string) error {
	_, err := s.Run(ctx, append([]string{"send-keys", "-t", paneID}, keys...)...)
	return err
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
	defer func() {
		// The deletion has its own bounded context: a ctx cancelled
		// after the load, by the daemon shutting down, must not leave
		// the text on the server.
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Run(dctx, "delete-buffer", "-b", buffer)
	}()
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
// on this server, for use locally or after ssh -t, its arguments escaped
// as args escapes them.
func (s Server) AttachArgsBare(session string) []string {
	return s.args("attach-session", "-t", "="+session)
}

// ArgsBare prepends this server's -L/-S selection to a tmux command, and
// escapes its arguments as args does: the tmux that runs the line reads
// them the same way.
func (s Server) ArgsBare(a ...string) []string { return s.args(a...) }

// shellJoin quotes argv for tmux's shell-command argument.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		// ~ and = are zsh's tilde and equals expansion: an unquoted "=lcl"
		// makes zsh look up a command named lcl and abort the line. Under
		// zsh's magic_equal_subst any word with an = is an assignment
		// whose value is expanded, so a==ls is a=/bin/ls.
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`!*?[]{}()<>|&;#~=") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}

// ShellJoin is the words as one shell line, each quoted when it needs
// to be; the sidebar's hooks, keys and pane, the workspace's attach
// commands, the ssh commands, the daemon's new-session line, the jump
// hint's add line and the remote binary's word are built with it.
func ShellJoin(argv []string) string { return shellJoin(argv) }

// FormatLiteral is s as a tmux format that expands to s: a # is
// written ##, which expands to #, but a run of #s before a [ is left as
// it is, since tmux keeps such a run, as the start of a style, and
// would keep ## there too. tmux expands the -c directory of
// new-session, new-window and split-window as a format, and a worktree
// root has the branch in it; it expands a run-shell command too, where
// the sidebar's hooks and keys put laatmux's path.
func FormatLiteral(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '#' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '#' {
			j++
		}
		b.WriteString(s[i:j])
		if j == len(s) || s[j] != '[' {
			b.WriteString(s[i:j])
		}
		i = j
	}
	return b.String()
}

// EncodeBranch makes a branch safe for a tmux session name, injectively:
// tmux does not keep "." or ":" in a session name, stores a "\" in one
// doubled and a control byte, DEL or a byte that is not part of a valid
// UTF-8 sequence escaped by vis(3), tmux 3.3 a C1 control character
// too, tmux 3.2 to 3.4 store a "$" before a letter, "_" or "{" as "\$",
// new-session expands a "#" in the name as a format, an argument that
// ends in ";" is a command separator, and laatmux splits tmux's listings
// at Sep, so each of those bytes, and "%" itself, becomes "%" and its
// two lowercase hex digits: "%25", "%23", "%24", "%2e", "%3a", "%3b",
// "%5c", a tab "%09", DEL "%7f", a lone 0xff "%ff". A C1 control
// character and a U+2063 are encoded byte by byte, U+0085 as "%c2%85".
// Every "$" is encoded, whatever follows it: what tmux takes for a
// letter there is its C library's isalpha, which differs by platform
// and locale, and a name then does not depend on the tmux it is made
// on. Nothing else changes, any other multibyte UTF-8 character
// included; a tmux 3.3 built without utf8proc also escapes a character
// its C library has no width for, one newer than its Unicode tables, a
// recent emoji say, which cannot be told from here. git takes no "\",
// C0 control byte or DEL in a branch, but a detached worktree's session
// is named by its directory, encoded the same way. Distinct branches
// give distinct names and the encoding is exact.
func EncodeBranch(branch string) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < len(branch); {
		c := branch[i]
		if c >= utf8.RuneSelf {
			if r, n := utf8.DecodeRuneInString(branch[i:]); (r != utf8.RuneError || n > 1) && !unicode.IsControl(r) && !strings.ContainsRune(Sep, r) {
				b.WriteString(branch[i : i+n])
				i += n
				continue
			}
		}
		if c < 0x20 || c >= 0x7f || strings.IndexByte(`$%#.:;\`, c) >= 0 {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		} else {
			b.WriteByte(c)
		}
		i++
	}
	return b.String()
}

// SessionName is the managed session name for a worktree on its host:
// <repo>/<encoded branch>. Labels cannot contain "/", so the first
// component is the label and the rest is the branch, slashes included.
func SessionName(repo, branch string) string { return repo + "/" + EncodeBranch(branch) }
