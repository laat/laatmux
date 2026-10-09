package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/source"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
	"github.com/laat/laatmux/internal/worktree"
)

// jump switches to the local workspace session for <host>/<repo>/<branch>,
// creating it from the host's record when it is missing. The workspace
// session is the unit: one local session per worktree, its first window
// attached to the managed session on the host. Local and remote are the
// same operation, since managed agents live on the dedicated laatmux
// server, which the user's tmux cannot switch-client into.
//
// The <repo> is read as this machine's name for the repository, the
// host's label, or as part of the managed session's name
// (matchWorktree); last, when no reading names a worktree and the
// target is no managed session, as a name of the known set's for a
// repository the host labels otherwise (matchKnown), as path reads it.
//
// A worktree with no managed session and no agent gets one first, with
// the user's shell at its root, from the host's daemon (newHome); where
// the jump makes none, the refusal says how add makes one (addHint).
//
// A managed session that is no worktree's, one that new made, is reached
// the same way through a local session named <host>/<session>, tagged as
// a plain attachment rather than a workspace.
//
// A repository's main checkout, <host>/<repo>/<branch> for the branch it
// has checked out, gets no session from add: with no home the jump goes
// as its line's does, to the session of its agent on this machine's
// default server or through the agent laatmux made at its root (rowSpec),
// and with no agent either makes it a home with a shell as a worktree
// gets one, which the jump goes to from then on.
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
	if h.Paused {
		return &peer.PausedError{Name: h.Name}
	}
	how, err := jumpMode(h.Host, tmux.Parse(*server), rest)
	if err != nil {
		return err
	}
	if how == jumpSwitch {
		return switchDefault(ctx, h, rest)
	}
	hello, snap, err := snapshot(ctx, h.Host, "")
	if err != nil {
		return err
	}
	var spec workspace.Spec
	// A worktree before a main checkout, which another clone of the
	// repository can have on the branch.
	worktrees, mains := splitMains(snap.Worktrees)
	w, ok, err := matchWorktree(worktrees, cfg, rest)
	if err == nil && !ok {
		w, ok, err = matchMain(mains, cfg, h, rest)
	}
	if err != nil {
		return err
	}
	if !ok {
		// No reading of the target names a worktree: it is a managed
		// session's name, as before, or, last, once the host has said it
		// has no such session, a name the known set has for a
		// repository the host labels otherwise (matchKnown). A check
		// that could not be made is its error, as before.
		serr := checkSession(ctx, h.Host, rest)
		if serr != nil {
			if !errors.Is(serr, errNoSession) {
				return serr
			}
			if w, ok, err = matchKnown(worktrees, mains, cfg, h, lazyKnown(ctx, cfg), rest); err != nil {
				return err
			}
			if !ok {
				return serr
			}
		}
	}
	switch a := rows.JumpAgent(snap.Agents, w); {
	case ok && w.Main && a != nil:
		// A main checkout with an agent goes as its line goes (rowSpec):
		// to its home; with none, to its agent's plain session on this
		// machine's default server, the most recently active of several,
		// or through the workspace session attached to the session of the
		// agent laatmux made at its root, a split gone elsewhere having
		// taken the home.
		var session string
		if spec, session, err = rowSpec(cfg, h, rows.Row{Kind: rows.KindWorktree, Host: h.Name, Worktree: &w, Agent: a}); err != nil {
			return err
		}
		if session != "" {
			return switchDefault(ctx, h, session)
		}
	case ok:
		if w.Session == "" {
			// The records are the host's, its label in them.
			name := shellSession(rows.Row{Worktree: &w})
			refusal := addHint(cfg, h, w)
			if w.Main {
				refusal = mainNoAgent(h, w)
			}
			if name == "" || rows.JumpAgent(snap.Agents, w) != nil || !protocol.Has(hello.Capabilities, protocol.CapNew) {
				return errors.New(refusal)
			}
			made, err := newHome(ctx, h, w, name, func() protocol.Message { return snap })
			if noNew(err) {
				return errors.New(refusal)
			}
			if err != nil {
				return err
			}
			if made != "" {
				fmt.Println(made)
			}
			w.Session = name
		}
		spec = worktreeSpec(h, w)
	default:
		spec = attachSpec(h, rest)
	}
	name, created, err := workspace.Ensure(ctx, spec)
	if err = warnHook(err); err != nil {
		return err
	}
	return focus(ctx, name, created)
}

// switchDefault switches to a session on this machine's default server,
// the one jump runs in: a client belongs to one server, so switching
// only works when the server jump runs in is the default one. The
// target is printed as tmux.Printable shows it, here and in jump's
// other refusals: it can name a branch, and git takes a C1 control
// character and a byte that is not UTF-8 in one.
func switchDefault(ctx context.Context, h config.Host, session string) error {
	if !workspace.Server.HasSession(ctx, session) {
		return fmt.Errorf("%s: no such session on the default tmux server", tmux.Printable(h.Name+"/"+session))
	}
	if !workspace.Inside(ctx) {
		return fmt.Errorf("%s: is on the default tmux server; run jump from a client of it", tmux.Printable(h.Name+"/"+session))
	}
	return workspace.Switch(ctx, session)
}

// mainNoAgent says a main checkout with no home has no agent to jump to,
// where the jump makes it no home with a shell: add starts agents in
// worktrees.
func mainNoAgent(h config.Host, w protocol.Worktree) string {
	return mainName(h.Name, w) + " is the main checkout, and no agent runs in it"
}

// mainName is a main checkout as the refusals about it name it:
// <host>/<repo>/<branch>, or its root on a detached HEAD, as
// tmux.Printable shows it, since git takes a C1 control character and
// a byte that is not UTF-8 in a branch.
func mainName(host string, w protocol.Worktree) string {
	if w.Branch == "" {
		return tmux.Printable(w.Root) + " on " + host
	}
	return tmux.Printable(host + "/" + w.Repo + "/" + w.Branch)
}

// matchMain is matchWorktree over the main checkouts, whose ambiguity is
// two clones' checkouts on the branch: no label tells them apart, both
// clones of a repository the config lists carrying its name, but their
// agents' sessions do, which the error says how to reach.
func matchMain(mains []protocol.Worktree, cfg config.Config, h config.Host, rest string) (protocol.Worktree, bool, error) {
	w, ok, err := matchWorktree(mains, cfg, rest)
	if err == nil {
		return w, ok, nil
	}
	label, branch, _ := strings.Cut(rest, "/")
	local, known := cfg.RepoByName(label)
	var found []protocol.Worktree
	for _, m := range mains {
		if m.Branch == branch && (m.Repo == label || known && source.Same(m.Source, local.Source)) {
			found = append(found, m)
		}
	}
	err = twoMains(tmux.Printable(rest), found)
	if !h.Host.Local() {
		// A remote host's default server is not jumped to at all.
		return protocol.Worktree{}, false, err
	}
	return protocol.Worktree{}, false, fmt.Errorf("%w; jump --server default %s/<session> goes to the session of an agent in any of them", err, h.Name)
}

// matchKnown is the last reading of a jump target's <repo>/<branch>:
// the repository the known set (merged.Known) has by that name, for a
// name the config does not have, its worktree on the host for the
// branch, else its main checkout there, whatever the host labels it,
// as path finds it by source. It is taken only once every other
// reading has matched nothing and the target is no managed session, so
// a target that reached a worktree or a session reaches it still, and
// the merged stream is read only then. A name the known set has for
// no repository, or for two, is no match.
func matchKnown(worktrees, mains []protocol.Worktree, cfg config.Config, h config.Host, known func() merged.Known, rest string) (protocol.Worktree, bool, error) {
	label, branch, _ := strings.Cut(rest, "/")
	if branch == "" {
		return protocol.Worktree{}, false, nil
	}
	if _, ok := cfg.RepoByName(label); ok {
		// The config's name, read first by matchWorktree.
		return protocol.Worktree{}, false, nil
	}
	r, ok, err := known().ByName(label, h.Name)
	if !ok || err != nil {
		return protocol.Worktree{}, false, nil
	}
	// The config read with the known repository under the target's
	// name, so the readings match it by source.
	with := cfg
	with.Repos = append(slices.Clip(cfg.Repos), config.Repo{Source: r.Source, Name: label})
	w, ok, err := matchWorktree(worktrees, with, rest)
	if err == nil && !ok {
		w, ok, err = matchMain(mains, with, h, rest)
	}
	return w, ok, err
}

// splitMains is the records that are worktrees and those that are main
// checkouts, apart.
func splitMains(ws []protocol.Worktree) (worktrees, mains []protocol.Worktree) {
	for _, w := range ws {
		if w.Main {
			mains = append(mains, w)
		} else {
			worktrees = append(worktrees, w)
		}
	}
	return worktrees, mains
}

// attachSpec is the plain attachment to a managed session that is no
// worktree's, by the session's name as the host lists it: the local
// session is named after it as AttachName names it, and attaches to it
// and is tagged with it as listed.
func attachSpec(h config.Host, session string) workspace.Spec {
	return workspace.Spec{Host: h.Host, Managed: session, Name: workspace.AttachName(h.Name, session)}
}

// worktreeSpec is the local workspace session for a worktree record with
// a managed session. A detached worktree has no <repo>/<branch> form; it
// is reached by its session name. The managed session is <repo>/<encoded
// branch> as it was when add made it; the local name follows it rather
// than the record's branch, which is empty for a worktree detached
// since, as AttachName names it after a session the host lists. The
// source is the identity and comes from the record; a record without
// one leaves it empty, and Ensure keeps whatever the session already
// knows.
func worktreeSpec(h config.Host, w protocol.Worktree) workspace.Spec {
	spec := workspace.Spec{
		Host:    h.Host,
		Managed: w.Session,
		Name:    workspace.AttachName(h.Name, w.Session),
		Key:     protocol.SessionKey(w.EnvironmentID, w.Root),
		Branch:  w.Branch,
		Source:  w.Source,
	}
	return spec
}

// addHint says a worktree has no managed session and how add makes one,
// in the words z uses for a homeless worktree's line (addsSession). A
// detached worktree has no <repo>/<branch> and is named by its root.
// Either is put as tmux.Printable shows it: git takes a C1 control
// character and a byte that is not UTF-8 in a branch.
func addHint(cfg config.Config, h config.Host, w protocol.Worktree) string {
	name := tmux.Printable(h.Name + "/" + w.Repo + "/" + w.Branch)
	if w.Branch == "" {
		name = tmux.Printable(w.Root) + " on " + h.Name
	}
	return name + " has no managed session; " + addsSession(cfg, h, w, false)
}

// shellSession is the managed session a jump makes for a worktree or a
// main checkout with no home and no agent: the one add makes, by the
// name add gives it, a main checkout's in the same form
// (rows.Row.ShellSession). "" where the jump makes none: for a detached
// checkout, which add makes none for; for a branch only shown, which no
// command names, flagged so or, from an older daemon that sends no
// flag, with U+FFFD in it (worktree.CheckWire); and for a name new
// refuses, one tmux would not store as given, as an older host's label
// for a checkout its config does not list can make it.
func shellSession(r rows.Row) string {
	name := r.ShellSession()
	if name == "" || r.Worktree.BranchDisplayOnly || worktree.CheckWire(r.Worktree.Branch) != nil || tmux.CheckSessionName(name) != nil {
		return ""
	}
	return name
}

// newHome asks the host's daemon for the managed session a jump makes
// for a worktree or a main checkout with no home and no agent: name, at
// the root, with no command, so the host's default-shell runs in it, as
// new makes one.
// What was made is returned, for the message. The daemon refuses a name
// in use. A session of the name that the host's records, as records
// reads them then, place elsewhere (elsewhere) is refused as add refuses
// it; any other, one made at the root since the records were read, an
// add's say, is the worktree's to attach, and nothing is said made. A
// daemon without new is a *noNewError; one that answers as another
// environment than the worktree's is refused before anything is asked,
// the host entry having moved to another machine since the records. The
// round trip is bounded.
func newHome(ctx context.Context, h config.Host, w protocol.Worktree, name string, records func() protocol.Message) (made string, err error) {
	ctx, cancel := context.WithTimeout(ctx, newTimeout)
	defer cancel()
	_, err = newSession(ctx, h, w.EnvironmentID, protocol.Message{Name: name, Cwd: w.Root})
	switch {
	case err == nil:
		return fmt.Sprintf("made session %s on %s, a shell at %s", tmux.Printable(name), h.Name, tmux.Printable(w.Root)), nil
	case noNew(err):
		return "", err
	case ctx.Err() != nil:
		return "", fmt.Errorf("%s: no answer to new %s after %s", h.Name, tmux.Printable(name), newTimeout)
	case !strings.HasSuffix(err.Error(), ": duplicate session: "+name):
		// tmux's own words, the name as it stored it: a name new takes
		// is stored as given.
		return "", fmt.Errorf("%s: new %s: %w", h.Name, tmux.Printable(name), err)
	}
	if cwd, ok := elsewhere(records(), w, name); ok {
		return "", fmt.Errorf("%s: session %s runs in %s, not %s; name in use", h.Name, tmux.Printable(name), tmux.Printable(cwd), tmux.Printable(w.Root))
	}
	return "", nil
}

// newTimeout bounds the new round trip, a cold managed server's start
// included.
const newTimeout = 10 * time.Second

// elsewhere is where the host's records place the managed session name
// other than in the worktree: the root of another worktree whose home it
// is, two clones' worktrees on one branch being named alike; else the
// directory of an agent or a pane in it that the host attributes to
// another worktree, or, attributed to none, has outside the root, unless
// a record has the pane laatmux made at the root, a managed one whose
// record has the directory it was made at: a split of the worktree's own
// session gone elsewhere leaves the session the worktree's, where a
// split of another's that has gone to the root does not make it this
// worktree's. For a main checkout, whose root the host gives no agent of
// another managed session, a pane laatmux made at another directory,
// below the root too, places the session there. Not ok when none does,
// as for a session made at the root since the records were read, which
// they do not have yet.
func elsewhere(snap protocol.Message, w protocol.Worktree, name string) (string, bool) {
	for _, o := range snap.Worktrees {
		if o.Session == name && o.ID != w.ID {
			return o.Root, true
		}
	}
	type rec struct {
		cwd, worktreeID string
		managed         bool
	}
	var in []rec
	for _, a := range snap.Agents {
		if a.Server == protocol.ServerLaatmux && a.Session == name {
			in = append(in, rec{a.Cwd, a.WorktreeID, a.Managed})
		}
	}
	for _, p := range snap.Panes {
		if p.Server == protocol.ServerLaatmux && p.Session == name {
			in = append(in, rec{p.Cwd, p.WorktreeID, p.Managed})
		}
	}
	for _, r := range in {
		if r.managed && r.cwd == w.Root {
			return "", false
		}
	}
	for _, r := range in {
		inside := r.cwd == w.Root || strings.HasPrefix(r.cwd, strings.TrimSuffix(w.Root, "/")+"/")
		made := w.Main && r.managed && r.cwd != "" && r.cwd != w.Root
		if made || r.worktreeID != w.ID && (r.worktreeID != "" || r.cwd != "" && !inside) {
			return r.cwd, true
		}
	}
	return "", false
}

// addCommand is the add line for the worktree's branch on the host. Its
// --repo is resolved against this machine's config, so it names the
// source as this machine knows it, not by the host's label, and a source
// the config does not list as the record has it, which add knows as a
// checkout of the record's host; a record without a source leaves it to
// the reader, and the agent with it. The
// line is for pasting into a shell, and git takes branches such as it's
// and a$(x): each word is quoted as ShellJoin quotes it, only when it
// needs to be, and the placeholder the reader replaces is left as it is.
// git takes a C1 control character and a byte that is not UTF-8 in a
// branch too, which no plain quoting keeps from the terminal; such a
// branch is written as dollarQuote writes it.
// It names an agent only where add would refuse to pick one: no agent
// last used for the repository is still configured (in last, last.json
// as add reads it, by the config's source, else the record's), there is
// no default_agent, and more than one agent is configured. The agent
// named is then the first, which the add form preselects too.
func addCommand(cfg config.Config, h config.Host, w protocol.Worktree, last home.Last) string {
	quote := func(s string) string { return tmux.ShellJoin([]string{s}) }
	repo := "<repo>"
	if r := localRepoArg(cfg, w); r != "" {
		repo = quote(r)
	}
	branch := quote(w.Branch)
	if tmux.Printable(w.Branch) != w.Branch {
		branch = dollarQuote(w.Branch)
	}
	line := fmt.Sprintf("laatmux add %s --repo %s --host %s", branch, repo, quote(h.Name))
	if w.Source != "" {
		src := w.Source
		if r, ok := cfg.RepoBySource(w.Source); ok {
			src = r.Source
		}
		if _, _, err := cfg.DefaultAgent("", last.Get(src).Agent); err != nil && len(cfg.Agents) > 0 {
			line += " --agent " + quote(cfg.AgentNames()[0])
		}
	}
	return line
}

// dollarQuote is s as a $'...' word, which bash, zsh and ksh read back
// byte for byte: every byte that is not printable ASCII, and ', \ and
// !, written as \ and three octal digits, so the line has no control
// character and no byte that is not UTF-8 in it. Three digits, since
// ksh reads on through the hex digits after a \x; and no ' or ! inside,
// since bash 3.2's history expansion does not know $'...', reads \' as
// the end of a quoted string, and would expand a ! after it.
func dollarQuote(s string) string {
	var b strings.Builder
	b.WriteString("$'")
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c < 0x20 || c >= 0x7f || c == '\'' || c == '\\' || c == '!':
			fmt.Fprintf(&b, `\%03o`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// localRepoArg is the record's repository as this machine names it: its
// name in the config when the config lists the source, else the source
// itself, which the add form matches against the known sources and
// --repo takes for any known repository: the record's host has a
// checkout of it.
func localRepoArg(cfg config.Config, w protocol.Worktree) string {
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
// same workspace. Two clones of one repository can each have a worktree
// for the branch: two matches in a pass are an error naming the roots,
// not a pick that depends on the order the records came in.
func matchWorktree(ws []protocol.Worktree, cfg config.Config, rest string) (protocol.Worktree, bool, error) {
	label, branch, _ := strings.Cut(rest, "/")
	pass := func(match func(protocol.Worktree) bool) (protocol.Worktree, bool, error) {
		var found []protocol.Worktree
		for _, w := range ws {
			if match(w) {
				found = append(found, w)
			}
		}
		if len(found) > 1 {
			// The later readings may tell them apart: this machine's
			// name can be one clone's host label, and that clone's
			// session is the target.
			var narrowed []protocol.Worktree
			for _, w := range found {
				if w.Repo == label || w.Session == rest {
					narrowed = append(narrowed, w)
				}
			}
			if len(narrowed) == 1 {
				return narrowed[0], true, nil
			}
		}
		switch len(found) {
		case 0:
			return protocol.Worktree{}, false, nil
		case 1:
			return found[0], true, nil
		}
		roots := make([]string, len(found))
		for i, w := range found {
			roots[i] = tmux.Printable(w.Root)
		}
		sort.Strings(roots)
		return protocol.Worktree{}, false, fmt.Errorf("%s matches worktrees at %s, in two clones of the repository; name one by the host's label for its clone", tmux.Printable(rest), strings.Join(roots, " and "))
	}
	if local, ok := cfg.RepoByName(label); ok && branch != "" {
		if w, ok, err := pass(func(w protocol.Worktree) bool { return w.Branch == branch && source.Same(w.Source, local.Source) }); ok || err != nil {
			return w, ok, err
		}
	}
	if branch != "" {
		if w, ok, err := pass(func(w protocol.Worktree) bool { return w.Branch == branch && w.Repo == label }); ok || err != nil {
			return w, ok, err
		}
	}
	return pass(func(w protocol.Worktree) bool { return w.Session != "" && w.Session == rest })
}

type jumpKind int

const (
	jumpAttach jumpKind = iota // a local session attached to the managed server
	jumpSwitch                 // switch-client within this machine's default server
)

// jumpMode decides how a session is reached, or that it is not: the managed
// server anywhere is attached; the default server on this machine is
// switched to; everything else is observed only. A session whose name no
// target reaches (tmux.CheckTarget), one made by hand on tmux 3.7 with a
// : in it say, is not reached either: the preflight, the attach and the
// switch could not name it, and could name another session. A
// <repo>/<branch> jump target is never refused so, since neither a label
// nor a git branch can have a : or start with a $.
func jumpMode(h peer.Host, srv tmux.Server, session string) (jumpKind, error) {
	var how jumpKind
	switch {
	case srv.Managed():
		how = jumpAttach
	case srv == tmux.DefaultServer && h.Local():
		how = jumpSwitch
	case srv == tmux.DefaultServer:
		return 0, fmt.Errorf("%s: on %s's default tmux server, which laatmux only observes; attach is limited to managed sessions", tmux.Printable(h.Name+"/"+session), h.Name)
	default:
		return 0, fmt.Errorf("%s: tmux server %s is not managed by laatmux; attach is limited to managed sessions", tmux.Printable(h.Name+"/"+session), srv.Label())
	}
	if err := tmux.CheckTarget(session); err != nil {
		return 0, fmt.Errorf("%s: %w", h.Name, err)
	}
	return how, nil
}

// checkSession fails early when the target session does not exist, so jump
// reports it instead of opening a window that exits at once. A transport
// failure is reported as such, with ssh's own diagnostics, and the check is
// bounded so a stalled connection cannot block jump before the attach
// window's own keepalive protection applies.
func checkSession(ctx context.Context, h peer.Host, session string) error {
	if h.Local() {
		if !tmux.LaatmuxServer.HasSession(ctx, session) {
			return fmt.Errorf("%s: %w", tmux.Printable(h.Name+"/"+session), errNoSession)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	argv := client.SSH(h.SSH, client.SSHOptions{ConnectTimeout: 10 * time.Second, KeepAlive: 5 * time.Second, KeepAliveCount: 2},
		tmux.ShellJoin(append([]string{"tmux"}, tmux.LaatmuxServer.ArgsBare("has-session", "-t", tmux.SessionTarget(session))...)))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	// After the deadline kills ssh, do not wait on its stderr pipe for
	// anything it may have left behind.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return classifyPreflight(h.Name, session, err, ctx.Err(), strings.TrimSpace(stderr.String()))
}

const preflightTimeout = 15 * time.Second

// errNoSession is checkSession's answer that the session is not there,
// as against a check that could not be made.
var errNoSession = errors.New("no such session on the laatmux tmux server")

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
		return fmt.Errorf("%s: %w", tmux.Printable(host+"/"+session), errNoSession)
	}
	if stderr == "" {
		stderr = runErr.Error()
	}
	return fmt.Errorf("%s: ssh failed: %s", host, stderr)
}
