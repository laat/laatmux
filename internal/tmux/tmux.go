// Package tmux runs tmux commands against one tmux server.
package tmux

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

// probe starts what ends every record Query has the server print,
// after a Sep: tmux 3.4 prints it back as `\$_`, every other version
// as given. Query follows it with a random tail drawn for the call, so
// no value holds the whole of it.
const probe = "$_"

// Query runs a command that prints a line in a format for each pane,
// session or client, list-panes or display-message -p say, with -F
// format, and returns its output as the server holds the values. tmux
// 3.4 puts a backslash before a "$" that comes before a letter, "_" or
// "{" in everything a command prints to a client: an option value, a
// pane's path, a session name. Query ends the format with Sep, probe
// and a random tail, and undoes those backslashes in each record whose
// probe the server put one before; a server that prints values as
// given, 3.3 and older or 3.5 and later, prints a "\$" only where the
// value has one. The probe is in the format rather than a command of
// its own, which would run a user's after-display-message hook. tmux
// 3.4 also writes a control byte other than a tab or a newline, DEL and
// a byte that is not part of valid UTF-8 as vis(3) does, \001 say, and
// those are left as printed: it writes a backslash of the value as it
// is, so such an escape cannot be told from the same text in the value.
func (s Server) Query(ctx context.Context, format string, a ...string) ([]byte, error) {
	end := probe + rand.Text()
	out, err := s.Run(ctx, append(slices.Clip(a), "-F", format+Sep+end)...)
	var te *Error
	if errors.As(err, &te) {
		e := *te
		e.Args = append(slices.Clip(a), "-F", format)
		err = &e
	}
	return []byte(unframe(string(out), end)), err
}

// unframe is what a Query's command printed, as the server holds the
// values, each record's Sep and end dropped. A record ends in Sep, the
// end Query drew, which is probe and a random tail, and the newline
// tmux ends a line with; no value holds the end, so a record whose
// value has a newline, or Sep and probe, is read whole. Each record
// tells by its own probe whether the server escaped it: one whose
// probe is printed as `\$_` has its backslashes undone, as a whole,
// then its Sep and end dropped. It is decoded with them: tmux on macOS
// takes the first byte of Sep for a letter, so a last value that ends
// in "$" has a backslash only the Sep after it accounts for. What comes
// after the last record, a line a user's after-hook printed, is left
// as printed.
func unframe(out, end string) string {
	parts := strings.Split(out, end+"\n")
	var b strings.Builder
	for i, r := range parts {
		if i == len(parts)-1 {
			b.WriteString(r)
			break
		}
		if strings.HasSuffix(r, Sep+`\`) {
			r = unescapeDollar(r + end)
		} else {
			r += end
		}
		b.WriteString(strings.TrimSuffix(r, Sep+end))
		b.WriteByte('\n')
	}
	return b.String()
}

// unescapeDollar drops the backslash tmux 3.4 puts before a "$" that
// comes before a byte dollarLetter takes. tmux puts it there whatever
// comes before the "$" and writes a backslash of the value as it is, so
// the backslash right before such a "$" is always its own: `\\$a` is
// the value `\$a`, and `\$1` is the value `\$1`, since tmux puts none
// before a "$" that comes before a digit. A "\$" before an escaped
// byte is left as printed: on macOS tmux puts its backslash before a
// "$" that comes before a lone 0xe9, which it prints as \351, and that
// cannot be told from the value `\$\351`.
func unescapeDollar(s string) string {
	if !strings.Contains(s, `\$`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+2 < len(s) && s[i+1] == '$' && dollarLetter(s[i+2]) {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// dollarLetter reports whether tmux 3.4 puts a backslash before a "$"
// that comes before the byte b: an ASCII letter, "_" or "{", or a byte
// its C library's isalpha takes for a letter in the UTF-8 locale tmux
// runs in. On macOS that is a Latin-1 letter too, which includes the
// first byte of every multibyte UTF-8 character but those from U+05C0
// to U+05FF; glibc and musl take no byte above 0x7f.
func dollarLetter(b byte) bool {
	switch {
	case b == '_' || b == '{' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z':
		return true
	case b >= utf8.RuneSelf:
		return runtime.GOOS == "darwin" && unicode.IsLetter(rune(b))
	}
	return false
}

// Error is a failed tmux command.
type Error struct {
	Args []string
	Msg  string
	err  error // the run's own, for NotInstalled
}

// Error names the command as the caller gave it, each Next written as
// the ";" it stands for, every other argument and tmux's message as
// Printable shows them: a root goes into the arguments as it is, and
// tmux's message can repeat a target that has it.
func (e *Error) Error() string {
	a := make([]string, len(e.Args))
	for i, v := range e.Args {
		if v == Next {
			v = ";"
		}
		a[i] = Printable(v)
	}
	return "tmux " + strings.Join(a, " ") + ": " + Printable(e.Msg)
}

// Printable is s as an error or a line laatmux prints shows it: as it
// is, or quoted as strconv.Quote quotes it when it has a control
// character, C0, DEL or C1, or a byte that is not UTF-8. A worktree's
// directory name can have any of them; printed raw, a tab or a newline
// breaks the line and an ESC starts an escape sequence the terminal
// acts on.
func Printable(s string) string {
	if !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return strconv.Quote(s)
	}
	return s
}

// PrintableLines is s with each of its lines as Printable shows it and
// the newlines between them kept: a message of several lines, git's
// with its hints, reads as it did, and a line with a control character
// is quoted on its own. Printable of the whole would make it one line
// with \n in it. A newline inside a path the message repeats is a line
// break like any other.
func PrintableLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = Printable(l)
	}
	return strings.Join(lines, "\n")
}

// PrintablePath is err with its path as Printable shows it when err is
// a bare *fs.PathError, as os's functions return, which names the path
// as it is, or with both paths so when it is a bare *os.LinkError, as a
// rename returns; err otherwise: one that wraps either is left as it
// is, since rebuilding it would drop what wraps it. A cause that is
// itself one, as os.Root's MkdirAll nests a failed stat in its error,
// is rebuilt the same way. The rebuilt error keeps the op and the
// cause, so errors.Is still finds fs.ErrNotExist and the like.
func PrintablePath(err error) error {
	switch e := err.(type) {
	case *fs.PathError:
		return &fs.PathError{Op: e.Op, Path: Printable(e.Path), Err: PrintablePath(e.Err)}
	case *os.LinkError:
		return &os.LinkError{Op: e.Op, Old: Printable(e.Old), New: Printable(e.New), Err: PrintablePath(e.Err)}
	}
	return err
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

// Sep separates the fields of a line tmux prints for a listing. tmux 3.5
// strips control characters from expanded formats, so a control byte
// fuses the fields on Linux while passing through on macOS 3.6. A
// printable sequence is used instead. A directory, a branch, a pane's
// title and a window's name can have it, so a listing with such a
// value in it is read through Fields; one of ids, flags and tags
// laatmux writes without it is split at Sep.
const Sep = "\u2063\u2063" // two INVISIBLE SEPARATOR code points

// Fields is a -F format that prints a line of values for each pane,
// session or client, and reads the values back whatever they hold. Each
// value is followed by a separator NewFields drew at random, so a value
// with Sep in it, or a newline, which a directory can have, is read
// back whole and does not shift the values after it: no value holds
// the separator unless it was made knowing it, and one is drawn for
// each listing. tmux prints a newline in a value as it is, so the
// newline that ends a line is the one after its last separator. A
// value escaped by tmux's s/// modifier would not do: the regexec of
// macOS and of musl refuses a value with a byte that is not UTF-8, and
// tmux then prints the value as it is.
type Fields struct {
	n      int
	sep    string
	format string
}

// NewFields is the format of the values of vars, each a format of its
// own, #{pane_id} say, with a separator drawn for it: a | and random
// base32, letters and digits. Every tmux prints those as given; none
// is a %, which display-message expands as strftime does. The | comes
// first since what follows a value is what tmux 3.4 looks at to put a
// backslash before a $ the value ends in: a letter, an _ or a {, or on
// macOS the first byte of a character such as U+2063, of which Sep is
// made.
func NewFields(vars ...string) Fields {
	sep := "|" + rand.Text()
	return Fields{n: len(vars), sep: sep, format: strings.Join(vars, sep) + sep}
}

// Records runs a command that prints a line for each pane, session or
// client, list-panes or display-message -p say, through Query with the
// format of f, and returns what Parse reads from its output: the
// values as the server holds them. A failed command is named with Sep
// where each separator was: the daemon logs a failed listing at every
// poll, and the line then reads the same each time.
func (s Server) Records(ctx context.Context, f Fields, a ...string) ([][]string, error) {
	out, err := s.Query(ctx, f.format, a...)
	var te *Error
	if errors.As(err, &te) {
		named := *te
		named.Args = make([]string, len(te.Args))
		for i, v := range te.Args {
			named.Args[i] = strings.ReplaceAll(v, f.sep, Sep)
		}
		err = &named
	}
	return f.Parse(out), err
}

// Parse is what a command printed in the format: a record of the
// values of each line, in the order of the vars. What a user's
// after-list-panes or after-display-message hook prints comes after
// the last separator, and is not read.
func (f Fields) Parse(out []byte) [][]string {
	if f.n == 0 {
		return nil
	}
	parts := strings.Split(string(out), f.sep)
	var recs [][]string
	for len(parts) > f.n {
		recs = append(recs, parts[:f.n:f.n])
		parts = parts[f.n:]
		parts[0] = strings.TrimPrefix(parts[0], "\n")
	}
	return recs
}

var paneVars = []string{
	"#{session_name}", "#{window_index}", "#{window_name}", "#{pane_id}", "#{pane_tty}",
	"#{pane_pid}", "#{pane_current_command}", "#{pane_current_path}", "#{pane_title}",
	"#{pane_dead}", "#{window_activity}", "#{@laatmux_host}", "#{@laatmux_cwd}", "#{@laatmux_managed}",
	"#{pid}", "#{pane_in_mode}", "#{@laatmux_sidebar}", "#{@laatmux_attach_pane}",
}

// ListPanes returns every pane on the server in one call. A server
// that runs with no sessions, which the managed one does after its last
// session ends, has no panes: tmux answers "no current target" for it,
// and that is an empty listing, not a failure to observe. A pane's
// directory and its @laatmux_cwd can have a newline or Sep in them, and
// its title and its window's name Sep, so the panes are read through
// Fields, as the server holds them.
func (s Server) ListPanes(ctx context.Context) ([]Pane, error) {
	recs, err := s.Records(ctx, NewFields(paneVars...), "list-panes", "-a")
	if err != nil {
		var te *Error
		if errors.As(err, &te) && strings.Contains(te.Msg, "no current target") {
			return nil, nil
		}
		return nil, err
	}
	var panes []Pane
	for _, f := range recs {
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
		p.InMode = f[15] == "1"
		p.Own = f[16] != "" || f[17] != ""
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
	// Each session is named by its id, which reaches it whatever its
	// name: a hand-made session can have a name no target reaches
	// (CheckTarget), c:d say on tmux 3.7, which =c:d: takes for window
	// d: of session c.
	if out, err := s.Query(ctx, "#{session_id}", "list-sessions"); err == nil {
		for _, id := range strings.Fields(string(out)) {
			for _, opt := range []string{"prefix", "prefix2", "status", "mouse"} {
				_, _ = s.Run(ctx, "set-option", "-u", "-t", id, opt)
			}
		}
	}
	return s.ensureLocale(ctx)
}

// CheckSessionName refuses a session name that tmux would not store as
// given, or could not find by its name once made, and says which
// character is the reason. tmux rewrites a name after expanding it as a
// format, so no escape gets these through:
//   - a . or a : is stored as _ by tmux before 3.7, and no target
//     reaches a session with a : (CheckTarget); a \ is stored doubled;
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
		case widthless(r):
			why = fmt.Sprintf("the character %U, which tmux 3.3 stores escaped", r)
		case r == '.':
			why = "a ., which tmux before 3.7 stores as _"
		case r == ':':
			why = "a :, which tmux before 3.7 stores as _ and a target splits at"
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

// widthless is a character tmux 3.3 built without utf8proc stores
// escaped, as its C library has no width for it, and which is known
// from here: a line or paragraph separator, U+2028 and U+2029, and a
// noncharacter, U+FDD0 to U+FDEF and the last two code points of every
// plane. CheckSessionName refuses one, and EncodeBranch and
// EncodeListed encode it.
func widthless(r rune) bool {
	return r == 0x2028 || r == 0x2029 || r >= 0xfdd0 && r <= 0xfdef || r&0xfffe == 0xfffe
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
		return made, fmt.Errorf("tmux: cwd: %w", PrintablePath(err))
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
	target := SessionTarget(o.Name)
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
				_, _ = s.Run(ctx, "kill-session", "-t", target)
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
// log. A prompt with a newline makes the argument it is in one that
// Printable quotes, and each form is in it escaped: strconv.Quote
// escapes rune by rune, so the form there is its own quoting without
// the quotes, and that is replaced as well. Every occurrence of every
// form, overlapping ones too, is found in the text as it is, and each
// run of text they cover becomes one placeholder: a placeholder is
// never searched, where a prompt that is a part of it, p say, would be
// found, and forms that overlap leave nothing of either, as a bare
// prompt that starts with the command's name does before its own
// shell-quoted form. The error's type is lost; the caller has
// classified it already.
func Redact(err error, secret, placeholder string) error {
	if err == nil || secret == "" {
		return err
	}
	msg := err.Error()
	hide := make([]bool, len(msg))
	for _, form := range []string{shellJoin([]string{secret}), secret} {
		q := strconv.Quote(form)
		cover(hide, msg, form)
		cover(hide, msg, q[1:len(q)-1])
	}
	var b strings.Builder
	for i := 0; i < len(msg); i++ {
		switch {
		case !hide[i]:
			b.WriteByte(msg[i])
		case i == 0 || !hide[i-1]:
			b.WriteString(placeholder)
		}
	}
	return errors.New(b.String())
}

// cover sets hide for every byte of msg in an occurrence of f, which
// is not empty, overlapping occurrences too. It is Knuth-Morris-Pratt:
// one pass over msg, each byte compared a bounded number of times
// amortized, so a prompt that repeats itself, a run of backslashes
// that Printable doubles say, costs no more than one that does not,
// where a search from each byte after a match would compare the whole
// prompt again at every one.
func cover(hide []bool, msg, f string) {
	// fail[i] is the length of the longest proper prefix of f[:i+1]
	// that is also a suffix of it.
	fail := make([]int, len(f))
	for i, k := 1, 0; i < len(f); i++ {
		for k > 0 && f[i] != f[k] {
			k = fail[k-1]
		}
		if f[i] == f[k] {
			k++
		}
		fail[i] = k
	}
	end := 0 // hide is set up to here
	for i, k := 0, 0; i < len(msg); i++ {
		for k > 0 && msg[i] != f[k] {
			k = fail[k-1]
		}
		if msg[i] == f[k] {
			k++
		}
		if k == len(f) {
			for j := max(i+1-len(f), end); j <= i; j++ {
				hide[j] = true
			}
			end = i + 1
			k = fail[k-1]
		}
	}
}

// SessionTarget is the target of any command for the session with
// exactly this name, and a session gone is an error rather than one its
// name is a prefix of. The = asks for the session by its exact name and
// the : ends the session's part of the target. Without the colon tmux
// reads a . in the name as the separator of a window and a pane: tmux
// 3.7 keeps a . in a session name, and a session named a.b is reached
// by =a.b: and not by =a.b, which looks for pane b of window a. A bare
// name may be taken as a pane or window of the current session before
// it is taken as a session, and then for a session whose name starts
// with it; the current session is the one with the pane TMUX_PANE
// names on that server, else the most recently active. =name without
// the colon still falls back to a session prefix where a window is
// wanted, set-option refuses it, and switch-client looks it up as a
// pane when the name has a %, as an encoded branch does. A name no
// session has but a client does, its tty without /dev/ say, is that
// client's session even so. A name CheckTarget refuses is reached by
// no target by name.
func SessionTarget(name string) string { return "=" + name + ":" }

// CheckTarget refuses a session name that SessionTarget does not reach,
// and says why: no name, since =: is the current session; one with a :,
// which tmux 3.7 keeps in a session name, since a target's session part
// ends at its first :, so =c:d: is a window of session c, its window
// named d:x say; and one that starts with a $, which =$0: reads as the
// session id $0, another session's.
func CheckTarget(name string) error {
	switch {
	case name == "":
		return errors.New("session name required")
	case strings.Contains(name, ":"):
		return fmt.Errorf("session %q has a :, at which tmux splits a target, so no target reaches it by name", name)
	case strings.HasPrefix(name, "$"):
		return fmt.Errorf("session %q starts with a $, which a target reads as a session id", name)
	}
	return nil
}

// KillSession kills the session with exactly this name; one no target
// reaches is refused, not another session killed.
func (s Server) KillSession(ctx context.Context, name string) error {
	if err := CheckTarget(name); err != nil {
		return err
	}
	_, err := s.Run(ctx, "kill-session", "-t", SessionTarget(name))
	return err
}

// HasSession reports whether a session with exactly this name exists on
// the server; one no target reaches is not found.
func (s Server) HasSession(ctx context.Context, name string) bool {
	if CheckTarget(name) != nil {
		return false
	}
	_, err := s.Run(ctx, "has-session", "-t", SessionTarget(name))
	return err == nil
}

// AttachArgsBare is the argv after "tmux" to attach a terminal to a session
// on this server, for use locally or after ssh -t, its arguments escaped
// as args escapes them.
func (s Server) AttachArgsBare(session string) []string {
	return s.args("attach-session", "-t", SessionTarget(session))
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
		// whose value is expanded, so a==ls is a=/bin/ls. Under zsh's
		// extended_glob ^ is glob negation: ^x is every file but x, and
		// a^b every file that starts with a and is not ab; when none
		// matches, zsh aborts the line. The Bourne shell read ^ as a
		// pipe.
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`!*?[]{}()<>|&;#~=^") {
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
// tmux before 3.7 does not keep "." or ":" in a session name, no target
// reaches a session with a ":" (CheckTarget), tmux stores a "\" in one
// doubled and a control byte, DEL or a byte that is not part of a valid
// UTF-8 sequence escaped by vis(3), tmux 3.3 a C1 control character
// too, tmux 3.2 to 3.4 store a "$" before a letter, "_" or "{" as "\$",
// new-session expands a "#" in the name as a format, an argument that
// ends in ";" is a command separator, and laatmux splits tmux's listings
// at Sep, so each of those bytes, and "%" itself, becomes "%" and its
// two lowercase hex digits: "%25", "%23", "%24", "%2e", "%3a", "%3b",
// "%5c", a tab "%09", DEL "%7f", a lone 0xff "%ff". A C1 control
// character and a U+2063 are encoded byte by byte, U+0085 as "%c2%85";
// so are a line or paragraph separator and a noncharacter (widthless),
// which a tmux 3.3 built without utf8proc stores escaped, U+2028 as
// "%e2%80%a8". Every "$" is encoded, whatever follows it: what tmux
// takes for a letter there is its C library's isalpha, which differs by
// platform and locale, and a name then does not depend on the tmux it
// is made on. Nothing else changes, any other multibyte UTF-8 character
// included; a tmux 3.3 built without utf8proc also escapes a code point
// its C library's tables do not have, unassigned or a recent emoji say,
// which is not encoded: Go's tables are not the host's, and a name
// would change with the Unicode version laatmux is built with. git
// takes no "\", C0 control byte or DEL in a branch, but a detached
// worktree's session is named by its directory, encoded the same way.
// Distinct branches give distinct names and the encoding is exact.
func EncodeBranch(branch string) string {
	return encodeBytes(branch, func(i int) bool {
		c := branch[i]
		return c < 0x20 || c >= 0x7f || strings.IndexByte(`$%#.:;\`, c) >= 0
	})
}

// encodeBytes writes each byte of s that escape picks by its index as
// "%" and its two lowercase hex digits, and the rest as they are. A
// valid multibyte UTF-8 character is kept whole, as tmux keeps it, but
// for a C1 control character, a U+2063 and a character widthless
// takes; a byte that is not part of a character kept is offered to
// escape alone.
func encodeBytes(s string, escape func(i int) bool) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			if r, n := utf8.DecodeRuneInString(s[i:]); (r != utf8.RuneError || n > 1) && !unicode.IsControl(r) && !strings.ContainsRune(Sep, r) && !widthless(r) {
				b.WriteString(s[i : i+n])
				i += n
				continue
			}
		}
		if escape(i) {
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

// EncodeListed makes a session name as tmux lists it a part of another
// session's name that tmux stores as given, for a local session named
// after a session on a host. tmux stores a session name through vis(3),
// as visName writes it, and lists it as stored: a session made as a\b
// is listed as a\\b, and one with a tab as tab\tx. Passed to
// new-session again as part of a name, such a name is escaped again,
// and the name stored is not the one computed. So the name is decoded
// first, when it is one tmux could have stored, that is when encoding
// what it decodes to gives it back; a name that is not, a\q say, is
// taken as it is. Then each byte EncodeBranch encodes becomes "%" and
// its two lowercase hex digits, as EncodeBranch writes them, but for
// "%", "#", ";", "." and ":": a "\", a control character, DEL, a byte
// that is not part of a valid UTF-8 sequence, a "$", a U+2063, a line
// or paragraph separator and a noncharacter. So a\\b becomes a%5cb and
// tab\tx becomes tab%09x, as EncodeBranch writes a\b and the tab, and a
// name with none of these is kept as it is. A "%" is kept since the
// names listed are mostly managed sessions', encoded already, whose
// workspace sessions are named after them as they are; a "#" since
// new-session is given the name as FormatLiteral writes it; a ";" since
// args writes a last one "\;". tmux before 3.7 lists no "." or ":",
// storing them as "_"; tmux 3.7 lists them as given. Both are kept for
// CheckSessionName to refuse a plain attachment to such a session: no
// target reaches one with a ":" (CheckTarget), and a local tmux before
// 3.7 would store a "." in the local name as "_". Two sessions on a
// host, a\b and a%5cb, can so get one local name; the second's jump is
// then refused as a name in use, since the local session is found by
// its attach tag, which is exact. tmux 3.2 to 3.4 store a "$" before a
// letter with a "\" before it, so c$xd is listed as c\$xd, which does
// not decode: tmux 3.4 prints it with one more "\", as c\\$xd, which
// Query undoes. Either becomes c%5c%24xd, a name kept as given.
func EncodeListed(name string) string {
	if d := unvisName(name); visName(d) == name {
		name = d
	}
	return encodeBytes(name, func(i int) bool {
		c := name[i]
		return c < 0x20 || c >= 0x7f || c == '\\' || c == '$'
	})
}

// The control bytes vis(3) writes as a letter after a "\", under
// VIS_CSTYLE, and the letters.
const (
	cstyleBytes   = "\a\b\t\n\v\f\r"
	cstyleLetters = "abtnvfr"
)

// visName is the name tmux 3.5 and later store for a session named s,
// as session_check_name writes it through vis(3) with VIS_OCTAL,
// VIS_CSTYLE, VIS_TAB and VIS_NL: a "\" doubled; a bell, backspace,
// tab, newline, vertical tab, form feed and carriage return as "\" and
// a letter; any other control byte, DEL and a byte that is not part of
// a valid UTF-8 sequence as "\" and three octal digits; every other
// byte, and a valid multibyte UTF-8 character, as it is. The ":" and
// "." tmux before 3.7 turns into "_" first are not its business.
func visName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			if r, n := utf8.DecodeRuneInString(s[i:]); r != utf8.RuneError || n > 1 {
				b.WriteString(s[i : i+n])
				i += n
				continue
			}
		}
		switch j := strings.IndexByte(cstyleBytes, c); {
		case c == '\\':
			b.WriteString(`\\`)
		case j >= 0:
			b.WriteByte('\\')
			b.WriteByte(cstyleLetters[j])
		case c < 0x20 || c >= 0x7f:
			b.WriteByte('\\')
			b.WriteByte('0' + c>>6)
			b.WriteByte('0' + c>>3&7)
			b.WriteByte('0' + c&7)
		default:
			b.WriteByte(c)
		}
		i++
	}
	return b.String()
}

// unvisName reads a name as visName writes it. A "\" that starts none
// of the escapes visName writes is read as itself, and visName then
// does not give back the name read.
func unvisName(s string) string {
	octal := func(c byte) bool { return '0' <= c && c <= '7' }
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if rest := s[i+1:]; c == '\\' && rest != "" {
			switch j := strings.IndexByte(cstyleLetters, rest[0]); {
			case rest[0] == '\\':
				i++
			case j >= 0:
				c = cstyleBytes[j]
				i++
			case len(rest) >= 3 && '0' <= rest[0] && rest[0] <= '3' && octal(rest[1]) && octal(rest[2]):
				c = (rest[0]-'0')<<6 | (rest[1]-'0')<<3 | (rest[2] - '0')
				i += 3
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}
