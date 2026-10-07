// Package workspace is the laptop side of a workspace: the local tmux
// session that attaches to a managed session on its host.
//
// A workspace is one worktree with one managed agent session and one local
// session. The local session lives in the user's default tmux server and
// is tagged with @laatmux_workspace = <environment_id>/<root>, the
// workspace key, written with the root encoded where tmux would not give
// it back as given (encodeKey), and @laatmux_host = the configured host
// name. The key is what a local session is matched on: neither the host
// name nor the repository label is in it, so the session still matches
// its workspace after either is renamed. The session name,
// <host>/<repo>/<encoded branch>, is for display and for switching by
// name. @laatmux_repo, the repository source, and @laatmux_branch
// identify the worktree when its record is gone from the host: that is
// how rm finds the root of an orphaned workspace. The host, source and
// branch tags are refreshed every time the session is reused, so a
// renamed host or label does not go stale in them.
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
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/source"
	"github.com/laat/laatmux/internal/tmux"
)

// Server is the tmux server local sessions live on: the user's default one.
var Server = tmux.DefaultServer

// SessionName is the local session name for a workspace:
// <host>/<repo>/<encoded branch>. The host is part of the name because the
// same repository and branch may be checked out on two hosts at once.
func SessionName(host, repo, branch string) string {
	return host + "/" + tmux.SessionName(repo, branch)
}

// AttachName is the local session name for a managed session on a host,
// by the session's name as the host's tmux lists it: <host>/<session>,
// the session's part made one tmux stores as given (tmux.EncodeListed).
// A session made with a "\" or a tab in its name is listed escaped, and
// as it is listed would be escaped again in the local name; one made
// with a "." on a host's tmux 3.7 is listed with it, which a local tmux
// before 3.7 would store as "_".
func AttachName(host, session string) string {
	return host + "/" + tmux.EncodeListed(session)
}

// Records is laatmux's sessions among the ones listed, a workspace or a
// plain attachment each, which is what the merging daemon publishes; a
// session with neither tag is not laatmux's and is left out.
func Records(sessions []protocol.Session) []protocol.Session {
	var out []protocol.Session
	for _, s := range sessions {
		if s.Laatmux() {
			out = append(out, s)
		}
	}
	return out
}

var sessionFormat = strings.Join([]string{
	"#{session_name}", "#{@laatmux_workspace}", "#{@laatmux_host}", "#{@laatmux_attach}", "#{@laatmux_settled}",
	"#{@laatmux_repo}", "#{@laatmux_branch}",
}, tmux.Sep)

// List returns every session on the default server. No server running is
// an empty list, and so is no tmux on PATH with nothing at the server's
// socket, which a tmux that ran would call no server: rm on a machine
// without tmux has nothing local to clean up. No tmux with the socket
// there is an error, since a server may be running that cannot be
// reached, during a tmux upgrade say.
func List(ctx context.Context) ([]protocol.Session, error) {
	out, err := Server.Query(ctx, sessionFormat, "list-sessions")
	if err != nil {
		if tmux.NoServer(err) || tmux.NotInstalled(err) && Server.NoSocket() {
			return nil, nil
		}
		return nil, err
	}
	return parseSessions(string(out)), nil
}

func parseSessions(out string) []protocol.Session {
	var locals []protocol.Session
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Split(line, tmux.Sep)
		if len(f) < 7 || f[0] == "" {
			continue
		}
		locals = append(locals, protocol.Session{Name: f[0], Key: DecodeKey(f[1]), Host: f[2], Attach: f[3], Settled: f[4] != "", Source: f[5], Branch: f[6]})
	}
	return locals
}

// encodeKey is the value @laatmux_workspace is set to for a key: the
// key as given, or <environment_id>%<encoded root> for a root that tmux
// or the reading of its listings would not give back as written. tmux
// 3.4 and 3.5 print a control byte, DEL and a byte that is not part of
// a valid UTF-8 sequence in an option value escaped by vis(3), a
// newline ends the line a listing has the key on, and the line is
// split at tmux.Sep, made of U+2063. Each byte of a control character,
// a tab and a C1 one included, of a byte that is not UTF-8, of a U+2063
// and of every "%" in the root is written "%" and two lowercase hex
// digits, as tmux.EncodeBranch writes them. The environment id is hex,
// so the byte after it says which form the value has: a value an
// earlier build wrote is the key as given, whatever its root holds.
func encodeKey(key string) string {
	env, root := protocol.SplitSessionKey(key)
	if enc := encodeRoot(root); enc != root {
		return env + "%" + enc
	}
	return key
}

// DecodeKey is the key a @laatmux_workspace value stands for: the root
// decoded where encodeKey encoded it, else the value as it is.
func DecodeKey(v string) string {
	i := strings.IndexAny(v, "/%")
	if i < 0 || v[i] == '/' {
		return v
	}
	return protocol.SessionKey(v[:i], decodeRoot(v[i+1:]))
}

// encodeRoot is a root as encodeKey writes it after the %.
func encodeRoot(root string) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < len(root); {
		r, n := utf8.DecodeRuneInString(root[i:])
		if r == utf8.RuneError && n == 1 || unicode.IsControl(r) || r == '\u2063' || r == '%' {
			for _, c := range []byte(root[i : i+n]) {
				b.WriteByte('%')
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			}
		} else {
			b.WriteString(root[i : i+n])
		}
		i += n
	}
	return b.String()
}

// decodeRoot reverses encodeRoot: "%" and two hex digits is the byte
// they spell, and anything else is kept.
func decodeRoot(enc string) string {
	var b strings.Builder
	for i := 0; i < len(enc); i++ {
		if enc[i] == '%' && i+2 < len(enc) {
			if v, err := strconv.ParseUint(enc[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(enc[i])
	}
	return b.String()
}

// Current is the session the calling process runs in: the pane's session
// when TMUX_PANE is set, as it is for a process in a pane, else the
// session TMUX names, which is what a run-shell job from a key binding
// gets. Not inside tmux is an error saying so.
func Current(ctx context.Context) (protocol.Session, error) {
	if os.Getenv("TMUX") == "" {
		return protocol.Session{}, errors.New("not inside tmux")
	}
	// The lookup is on the server TMUX names, which may not be the
	// default one; the zero server follows TMUX.
	args := []string{"display-message", "-p"}
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		args = append(args, "-t", pane)
	}
	out, err := (tmux.Server{}).Query(ctx, sessionFormat, args...)
	if err != nil {
		return protocol.Session{}, err
	}
	locals := parseSessions(string(out))
	if len(locals) != 1 {
		return protocol.Session{}, errors.New("cannot find the current tmux session")
	}
	return locals[0], nil
}

// PaneSession is the session a pane is in, with its tags, and the pane's
// current directory. The lookup is on the server TMUX names, as Current's
// is, since a pane id is per server; split runs from a binding on the
// user's server.
func PaneSession(ctx context.Context, paneID string) (protocol.Session, string, error) {
	out, err := (tmux.Server{}).Query(ctx, sessionFormat+tmux.Sep+"#{pane_current_path}", "display-message", "-p", "-t", paneID)
	if err != nil {
		return protocol.Session{}, "", err
	}
	line := strings.TrimRight(string(out), "\n")
	f := strings.Split(line, tmux.Sep)
	if len(f) != 8 {
		return protocol.Session{}, "", errors.New("cannot find the session of pane " + paneID)
	}
	locals := parseSessions(strings.Join(f[:7], tmux.Sep) + "\n")
	if len(locals) != 1 {
		return protocol.Session{}, "", errors.New("cannot find the session of pane " + paneID)
	}
	return locals[0], f[7], nil
}

// FindWorktree returns the workspace session for a branch of a repository
// on the host with the environment id, by its tags.
func FindWorktree(locals []protocol.Session, environmentID, src, branch string) (protocol.Session, bool) {
	for _, l := range locals {
		if env, _ := protocol.SplitSessionKey(l.Key); l.Workspace() && env == environmentID && source.Same(l.Source, src) && l.Branch == branch && src != "" {
			return l, true
		}
	}
	return protocol.Session{}, false
}

// Find returns the session with the key, or the plain attachment with the
// tag, whichever is given.
func Find(locals []protocol.Session, key, attach string) (protocol.Session, bool) {
	for _, l := range locals {
		if key != "" && l.Key == key {
			return l, true
		}
		if attach != "" && l.Attach == attach {
			return l, true
		}
	}
	return protocol.Session{}, false
}

// ByName returns the session with exactly this name.
func ByName(locals []protocol.Session, name string) (protocol.Session, bool) {
	for _, l := range locals {
		if l.Name == name {
			return l, true
		}
	}
	return protocol.Session{}, false
}

// Spec describes the local session for a managed session on a host.
type Spec struct {
	Host    peer.Host // how the attach command reaches the host
	Managed string    // managed session name on the host
	Name    string    // local session name
	Key     string    // workspace key; "" for a plain attachment
	Source  string    // repository source, when known
	Branch  string
}

// Ensure finds the local session for the spec, or creates it: one window
// for the attach command, with the session tagged in the same tmux
// command sequence so it is never observable untagged, then the attach
// pane tagged by id and started. An existing session is found by its key,
// or by its attach tag for a plain attachment, whatever its name; its
// host, source and branch tags are refreshed, since it may predate a
// rename, and a dead attach pane in it is respawned. A session with the
// intended name that is not it is a name in use, but for a plain
// attachment to the managed session a keyed spec names, which it
// adopts as the workspace: an older build's jump from the agent's row
// made such a session, named as the workspace would be, before the
// worktree had its home session back. A workspace named after its
// managed session also adopts one under the name an older build gave
// it. A plain attachment to be made under a name tmux would not store
// as given is refused. The name of the session, existing or new, and
// whether it was created are returned.
func Ensure(ctx context.Context, s Spec) (name string, created bool, err error) {
	// The attach tags and the attach command have the managed session's
	// name, so one that cannot be in them is refused before any is
	// written, a reuse's or an adoption's too: a U+2063, since the tags
	// are read back split at Sep, and a name the attach target does not
	// reach (tmux.CheckTarget), a $ at the start or a :, which a host's
	// tmux 3.7 keeps in a session made by hand. AttachName encodes a
	// U+2063 and a $ in the local name, which CheckSessionName refused
	// for them before.
	if strings.ContainsAny(s.Managed, tmux.Sep) {
		return "", false, fmt.Errorf("managed session %q has the character U+2063, which laatmux separates the fields of tmux's listings with", s.Managed)
	}
	if err := tmux.CheckTarget(s.Managed); err != nil {
		return "", false, fmt.Errorf("managed %w", err)
	}
	locals, err := List(ctx)
	if err != nil {
		return "", false, err
	}
	attach := s.Host.Name + "/" + s.Managed
	find := attach
	if s.Key != "" {
		find = ""
	}
	if l, ok := Find(locals, s.Key, find); ok {
		if _, err := Server.Run(ctx, tagArgs(l.Name, s)...); err != nil {
			return "", false, err
		}
		return l.Name, false, ensureAttach(ctx, l.Name, s)
	}
	if l, ok := ByName(locals, s.Name); ok {
		// A name, a root or a tag is printed as Printable shows it: a
		// root is a directory's name, which can have a control byte.
		switch {
		case l.Workspace():
			_, root := protocol.SplitSessionKey(l.Key)
			return "", false, fmt.Errorf("local session %s is the workspace for %s on %s; name in use", tmux.Printable(s.Name), tmux.Printable(root), tmux.Printable(l.Host))
		case l.Attach == attach && s.Key != "":
			return l.Name, false, adopt(ctx, l.Name, s)
		case l.Attach != "":
			return "", false, fmt.Errorf("local session %s is attached to %s; name in use", tmux.Printable(s.Name), tmux.Printable(l.Attach))
		default:
			return "", false, fmt.Errorf("local session %s exists and is not laatmux's; name in use", tmux.Printable(s.Name))
		}
	}
	// A workspace named after its managed session, as worktreeSpec names
	// one after the worktree's home, adopts the plain attachment an
	// older build named after the session as listed, a $ in it kept,
	// which AttachName encodes now: the name its tag has. One named
	// otherwise, after a worktree whose root agent is in a session that
	// is not its home, leaves a plain attachment to that session alone.
	if s.Key != "" && s.Name != attach && s.Name == AttachName(s.Host.Name, s.Managed) {
		if l, ok := ByName(locals, attach); ok && l.Attach == attach {
			return l.Name, false, adopt(ctx, l.Name, s)
		}
	}
	// A plain attachment's name has the managed session's in it, which
	// one made by an older laatmux new, or by hand, can have a character
	// in that tmux would not store as given: the session made would have
	// another name, and the tags in its own sequence would find no
	// session, leaving it untagged. AttachName encodes every character
	// CheckSessionName refuses but a ":", which tmux 3.7 lists as given
	// and which is refused above. A workspace's name is not checked: it
	// has a worktree's session name in it, which SessionName encoded from
	// the branch, new took for a session started at the root, or
	// AttachName encoded as listed, a "." from a host's tmux 3.7 too.
	// new-session expands the name as a format, and a name new took can
	// have a # in it.
	if s.Key == "" {
		if err := tmux.CheckSessionName(s.Name); err != nil {
			return "", false, err
		}
	}
	args := []string{"new-session", "-d", "-s", tmux.FormatLiteral(s.Name), "-n", "agent", "-P", "-F", "#{pane_id}", placeholder}
	if s.Key != "" {
		args = append(args, tmux.Next, "set-option", "-t", tmux.SessionTarget(s.Name), "@laatmux_workspace", encodeKey(s.Key))
	} else {
		args = append(args, tmux.Next, "set-option", "-t", tmux.SessionTarget(s.Name), "@laatmux_attach", attach)
	}
	args = append(args, tmux.Next)
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

// adopt makes a plain attachment the workspace a keyed spec names: its
// attach pane's target tagged first where an older build left it
// untagged, as a session made by Ensure carries it, which is safe while
// the session is still plain since the pane attaches to the spec's
// managed session; then the key set and the attach tag unset in one
// sequence, so the session is never observable as both or neither,
// with the identity tags. An adopt cut short before the flip is met as
// it was again and redone whole; one cut short after it is a workspace
// with its pane tagged, as Ensure then finds it by key. Last, the
// attach pane is respawned or made as for a reuse.
func adopt(ctx context.Context, name string, s Spec) error {
	out, err := Server.Run(ctx, "list-panes", "-s", "-t", tmux.SessionTarget(name), "-F", strings.Join([]string{"#{pane_id}", "#{@laatmux_attach_pane}", "#{@laatmux_attach_target}"}, tmux.Sep))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, tmux.Sep)
		if len(f) == 3 && f[1] != "" && f[2] == "" {
			if _, err := Server.Run(ctx, "set-option", "-p", "-t", f[0], "@laatmux_attach_target", s.Managed); err != nil {
				return err
			}
		}
	}
	args := []string{"set-option", "-t", tmux.SessionTarget(name), "@laatmux_workspace", encodeKey(s.Key),
		tmux.Next, "set-option", "-u", "-t", tmux.SessionTarget(name), "@laatmux_attach", tmux.Next}
	if _, err := Server.Run(ctx, append(args, tagArgs(name, s)...)...); err != nil {
		return err
	}
	return ensureAttach(ctx, name, s)
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
		tmux.Next, "set-option", "-p", "-t", paneID, "@laatmux_attach_pane", "1",
		tmux.Next, "set-option", "-p", "-t", paneID, "@laatmux_attach_target", s.Managed,
		tmux.Next, "respawn-pane", "-k", "-t", paneID, AttachCommand(s.Host, s.Managed))
	return err
}

// SetHost tags the session with the host's name, for one whose tag
// names no configured host: the session is on the server TMUX names,
// where Current and PaneSession found it, and named exactly.
func SetHost(ctx context.Context, name, host string) error {
	_, err := (tmux.Server{}).Run(ctx, "set-option", "-t", tmux.SessionTarget(name), "@laatmux_host", host)
	return err
}

// tagArgs is the tmux command sequence that sets the routing and identity
// tags on a session: the host, and for a workspace the source and branch.
// A source or branch the spec does not know is not written, so a reuse
// that could not resolve one keeps what the session already carries.
func tagArgs(name string, s Spec) []string {
	target := tmux.SessionTarget(name)
	args := []string{"set-option", "-t", target, "@laatmux_host", s.Host.Name}
	if s.Key != "" && s.Source != "" {
		args = append(args, tmux.Next, "set-option", "-t", target, "@laatmux_repo", s.Source)
	}
	if s.Key != "" && s.Branch != "" {
		args = append(args, tmux.Next, "set-option", "-t", target, "@laatmux_branch", s.Branch)
	}
	return args
}

// ensureAttach makes sure the session has a live attach pane on the
// managed session the spec names: a dead one is respawned in place, as
// is a live one attached to another managed session, the worktree's
// agent having moved to another since, and a session with no tagged
// attach pane at all, closed by hand or left by a crash before the tag,
// gets a new attach window. A pane from before the target was tagged
// is left as it is. Other panes in the session are the user's and are
// left alone.
func ensureAttach(ctx context.Context, name string, s Spec) error {
	out, err := Server.Query(ctx, strings.Join([]string{"#{pane_id}", "#{pane_dead}", "#{@laatmux_attach_pane}", "#{@laatmux_attach_target}"}, tmux.Sep), "list-panes", "-s", "-t", tmux.SessionTarget(name))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, tmux.Sep)
		if len(f) != 4 || f[2] == "" {
			continue
		}
		if f[1] == "1" || (f[3] != "" && f[3] != s.Managed) {
			_, err := Server.Run(ctx, "set-option", "-p", "-t", f[0], "@laatmux_attach_target", s.Managed,
				tmux.Next, "respawn-pane", "-k", "-t", f[0], AttachCommand(s.Host, s.Managed))
			return err
		}
		return nil
	}
	out, err = Server.Run(ctx, "new-window", "-t", tmux.SessionTarget(name), "-n", "agent", "-P", "-F", "#{pane_id}", placeholder)
	if err != nil {
		return err
	}
	return startAttach(ctx, strings.TrimSpace(string(out)), s)
}

// AttachCommand is the shell command the attach window runs. TMUX is unset
// so the inner tmux does not refuse to nest; -u tells it the terminal is
// UTF-8.
func AttachCommand(h peer.Host, session string) string {
	attach := append([]string{"tmux", "-u"}, tmux.LaatmuxServer.AttachArgsBare(session)...)
	if h.Local() {
		return tmux.ShellJoin(append([]string{"env", "-u", "TMUX"}, attach...))
	}
	return tmux.ShellJoin(client.SSH(h.SSH, client.SSHOptions{TTY: true, KeepAlive: 15 * time.Second, KeepAliveCount: 3}, tmux.ShellJoin(attach)))
}

// ShellCommand is the shell command a shell window runs for a worktree
// root on its host: for a remote host, ssh with cd and the single-quoted
// root, then a literal exec of the login shell so the root passes through
// as one argument whatever it contains and $SHELL expands on the remote
// side. A local window needs no command; it is started in the root.
func ShellCommand(h peer.Host, root string) string {
	remote := tmux.ShellJoin([]string{"cd", root}) + ` && exec "$SHELL" -l`
	return tmux.ShellJoin(client.SSH(h.SSH, client.SSHOptions{TTY: true}, remote))
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
	_, err := Server.Run(ctx, "switch-client", "-t", tmux.SessionTarget(name))
	return err
}

// SwitchClient makes the session current for the named client.
func SwitchClient(ctx context.Context, client, name string) error {
	_, err := Server.Run(ctx, "switch-client", "-c", client, "-t", tmux.SessionTarget(name))
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
	_, err := Server.Run(ctx, "kill-session", "-t", tmux.SessionTarget(name))
	return err
}

// SetSettled sets or clears @laatmux_settled on the session with exactly
// this name; a session gone is an error, not another session tagged.
func SetSettled(ctx context.Context, name string, settled bool) error {
	var err error
	if settled {
		_, err = Server.Run(ctx, "set-option", "-t", tmux.SessionTarget(name), "@laatmux_settled", "1")
	} else {
		_, err = Server.Run(ctx, "set-option", "-u", "-t", tmux.SessionTarget(name), "@laatmux_settled")
	}
	return err
}

// AttachPane is the id of a local session's live attach pane, "" when
// it has none: the pane a jump to an agent's pane selects, so the
// session shows the attach whatever window the user left it on.
func AttachPane(ctx context.Context, name string) string {
	out, err := Server.Run(ctx, "list-panes", "-s", "-t", tmux.SessionTarget(name), "-F", strings.Join([]string{"#{pane_id}", "#{pane_dead}", "#{@laatmux_attach_pane}"}, tmux.Sep))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, tmux.Sep)
		if len(f) == 3 && f[2] != "" && f[1] != "1" {
			return f[0]
		}
	}
	return ""
}
