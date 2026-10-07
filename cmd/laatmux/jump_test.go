package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/term"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/workspace"
)

// A fake ssh on PATH scripted through an env var: exit code, stderr, delay.
func fakeSSH(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "ssh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCheckSessionDistinguishesFailures(t *testing.T) {
	h := peer.Host{Name: "vm", SSH: "vm"}
	cases := []struct {
		name, script, want string
	}{
		{"absent", `printf "cannot find session: =x\n" >&2; exit 1`, "vm/x: no such session"},
		{"refused", `printf "ssh: connect to host vm port 22: Connection refused\n" >&2; exit 255`, "vm: ssh failed: ssh: connect to host vm port 22: Connection refused"},
		{"no server", `printf "no server running on /tmp/tmux-1001/laatmux\n" >&2; exit 1`, "vm/x: no such session"},
		{"ok", `exit 0`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeSSH(t, c.script)
			err := checkSession(context.Background(), h, "x")
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

func TestCheckSessionTimesOut(t *testing.T) {
	fakeSSH(t, `sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := checkSession(ctx, peer.Host{Name: "vm", SSH: "vm"}, "x")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("preflight did not honour the deadline")
	}
	_ = errors.New
}

// The preflight names the session as the attach does, =name:, so a
// session a.b on a host's tmux 3.7 is found, where =a.b looked for pane
// b of window a.
func TestCheckSessionNamesExactly(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	fakeSSH(t, `printf '%s\n' "$@" > '`+argv+`'`)
	if err := checkSession(context.Background(), peer.Host{Name: "vm", SSH: "vm"}, "a.b"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argv)
	if err != nil || !strings.HasSuffix(string(got), "\ntmux -L laatmux has-session -t '=a.b:'\n") {
		t.Fatalf("ssh got %q %v", got, err)
	}
}

// Issue 3, option B: only the managed server is attached; the local default
// server is switched to; a remote default server or any other server is
// refused as unmanaged. A session no target reaches, one with a : that
// tmux 3.7 keeps or one whose name starts with a $, is refused on a
// server that would be attached or switched to.
func TestJumpMode(t *testing.T) {
	mac := peer.Host{Name: "mac"}
	vm := peer.Host{Name: "vm", SSH: "vm"}
	cases := []struct {
		h       peer.Host
		srv     string
		session string
		want    jumpKind
		err     string
	}{
		{mac, "laatmux", "x", jumpAttach, ""},
		{vm, "laatmux", "x", jumpAttach, ""},
		{mac, "default", "x", jumpSwitch, ""},
		{vm, "default", "x", 0, "vm/x: on vm's default tmux server"},
		{mac, "work", "x", 0, "tmux server work is not managed"},
		{vm, "/tmp/sock", "x", 0, "tmux server /tmp/sock is not managed"},
		{vm, "laatmux", "a.b", jumpAttach, ""},
		{mac, "default", "a.b", jumpSwitch, ""},
		{vm, "laatmux", "c:d", 0, `vm: session "c:d" has a :`},
		{mac, "default", "c:d", 0, `mac: session "c:d" has a :`},
		{mac, "default", "$0", 0, `mac: session "$0" starts with a $`},
		{vm, "default", "c:d", 0, "vm/c:d: on vm's default tmux server"},
	}
	for _, c := range cases {
		got, err := jumpMode(c.h, tmux.Parse(c.srv), c.session)
		switch {
		case c.err == "" && (err != nil || got != c.want):
			t.Errorf("%s --server %s %s: got %v, %v", c.h.Name, c.srv, c.session, got, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s --server %s %s: got %v, want %q", c.h.Name, c.srv, c.session, err, c.err)
		}
	}
}

// A worktree row with no home session whose agent runs elsewhere is
// jumped to through the agent, not answered with the add hint: here an
// agent on a remote host's default server, which jump refuses as such.
func TestJumpRowWorktreeThroughAgent(t *testing.T) {
	cfg := config.Config{Hosts: []config.Host{{Host: peer.Host{Name: "vm", SSH: "vm"}}}}
	w := protocol.Worktree{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/w/a"}
	a := protocol.Agent{ID: "venv/default/%1", EnvironmentID: "venv", Server: "default", Session: "notes", WorktreeID: w.ID}
	err := jumpRow(context.Background(), cfg, rows.Row{Host: "vm", Name: "proj/a", Worktree: &w, Agent: &a})
	if err == nil || !strings.Contains(err.Error(), "only observes") {
		t.Fatalf("jump through the agent: %v", err)
	}
	err = jumpRow(context.Background(), cfg, rows.Row{Host: "vm", Name: "proj/a", Worktree: &w})
	if err == nil || !strings.Contains(err.Error(), "has no managed session") {
		t.Fatalf("no agent: %v", err)
	}
}

// A worktree with no home and no agent: enter on its line refuses with
// how add makes it a session, z on the line says the same, and so does
// laatmux jump. The add line comes only when add can run it: on vm, a
// host whose entry here has the directories, the add line for the
// branch; a detached worktree, named by its root, needs a branch
// checked out first, and a root with an ESC in it is named quoted;
// box's entry has no directories, which add needs first; a repository
// this machine's config does not list is one
// --repo refuses, while one it lists under another form of the source
// is named by its label here. A worktree that lacks more than one is
// told all of them. The add line pastes into a shell: a branch git
// takes with a ' or a $( in it is quoted, an ordinary one is not, and
// a record without a source leaves the <repo> placeholder bare, and the
// agent with it. The line names an agent only where add would not pick
// one, and with no agent configured, add needs one first.
func TestAddHintCanRun(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	d := &dash{ctx: context.Background(), cfg: dashConfig(t), st: merged.New()}
	src := "git@github.com:laat/proj.git"
	bv := protocol.Worktree{ID: "venv/worktree//w/b", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "b", Root: "/w/b"}
	bb := bv
	bb.ID, bb.EnvironmentID = "benv/worktree//w/b", "benv"
	det := protocol.Worktree{ID: "menv/worktree//w/det", EnvironmentID: "menv", Repo: "proj", Source: src, Root: "/w/det"}
	detBox := det
	detBox.ID, detBox.EnvironmentID = "benv/worktree//w/det", "benv"
	other := protocol.Worktree{ID: "venv/worktree//w/o", EnvironmentID: "venv", Repo: "other", Source: "git@github.com:laat/other.git", Branch: "b", Root: "/w/o"}
	https := bv
	https.Source = "https://github.com/laat/proj"
	detOther := other
	detOther.ID, detOther.EnvironmentID, detOther.Branch = "benv/worktree//w/o", "benv", ""
	detCtl := det
	detCtl.ID, detCtl.Root = "menv/worktree//w/a\x1b]0;x\x07b", "/w/a\x1b]0;x\x07b"
	apos := bv
	apos.ID, apos.Branch, apos.Root = "venv/worktree//w/q", "it's", "/w/q"
	subst := bv
	subst.ID, subst.Branch, subst.Root = "venv/worktree//w/s", "a$(x)", "/w/s"
	eq := bv
	eq.ID, eq.Branch, eq.Root = "venv/worktree//w/e", "a==ls", "/w/e"
	noSrc := bv
	noSrc.Source = ""
	host := func(name, env string) rows.Host {
		return rows.Host{Name: name, Local: name == "mac", EnvironmentID: env, Connected: true, Listed: true, Worktrees: true, Attribution: true}
	}
	onVM := "vm/proj/b has no managed session; laatmux add b --repo proj --host vm --agent claude makes one"
	onBox := "box/proj/b has no managed session; laatmux add makes one once host box has repos and worktrees directories in the config"
	onOther := "vm/other/b has no managed session; laatmux add makes one once git@github.com:laat/other.git is a repository in the config"
	onApos := `vm/proj/it's has no managed session; laatmux add 'it'\''s' --repo proj --host vm --agent claude makes one`
	onSubst := "vm/proj/a$(x) has no managed session; laatmux add 'a$(x)' --repo proj --host vm --agent claude makes one"
	check := func(host rows.Host, w protocol.Worktree, want string) {
		t.Helper()
		in := rows.Input{Hosts: []rows.Host{host}, Worktrees: []protocol.Worktree{w}}
		m := &view.Model{Width: 100, Height: 20, ShowHidden: true, View: view.ViewTree}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		m.Render()
		if !m.Select(w.ID) {
			t.Fatalf("no line %s", w.ID)
		}
		d.jumpAction(m, view.Action{Kind: view.ActionJump})
		if m.Message != want {
			t.Errorf("enter on %s: %q, want %q", w.ID, m.Message, want)
		}
		d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Rune: 'z'}})
		if want := m.Selection().Name + ": no local workspace session; " + want; m.Message != want {
			t.Errorf("z on %s: %q, want %q", w.ID, m.Message, want)
		}
	}
	for _, c := range []struct {
		host rows.Host
		w    protocol.Worktree
		want string
	}{
		{host("vm", "venv"), bv, onVM},
		{host("box", "benv"), bb, onBox},
		{host("vm", "venv"), https, onVM},
		{host("vm", "venv"), other, onOther},
		{host("vm", "venv"), apos, onApos},
		{host("vm", "venv"), subst, onSubst},
		// zsh's magic_equal_subst would read a bare a==ls as a=/bin/ls.
		{host("vm", "venv"), eq, "vm/proj/a==ls has no managed session; laatmux add 'a==ls' --repo proj --host vm --agent claude makes one"},
		{host("vm", "venv"), noSrc, "vm/proj/b has no managed session; laatmux add b --repo <repo> --host vm makes one"},
		{host("mac", "menv"), det, "/w/det on mac has no managed session; laatmux add makes one once a branch is checked out in /w/det"},
		{host("mac", "menv"), detCtl, `"/w/a\x1b]0;x\ab" on mac has no managed session; laatmux add makes one once a branch is checked out in "/w/a\x1b]0;x\ab"`},
		{host("box", "benv"), detBox, "/w/det on box has no managed session; laatmux add makes one once a branch is checked out in /w/det and host box has repos and worktrees directories in the config"},
		{host("box", "benv"), detOther, "/w/o on box has no managed session; laatmux add makes one once a branch is checked out in /w/o, host box has repos and worktrees directories in the config, and git@github.com:laat/other.git is a repository in the config"},
	} {
		check(c.host, c.w, c.want)
	}
	// The agent add would pick itself is left to it: the one last used
	// for the repository, kept under the source as this machine's config
	// has it, which the https record finds too; else default_agent. One
	// last used that is no longer configured is passed over, as add
	// passes it over.
	plain := "vm/proj/b has no managed session; laatmux add b --repo proj --host vm makes one"
	last := func(agent string) {
		t.Helper()
		if err := home.UpdateLast(func(l *home.Last) { l.Set(src, home.LastRepo{Agent: agent}) }); err != nil {
			t.Fatal(err)
		}
	}
	last("codex")
	check(host("vm", "venv"), bv, plain)
	check(host("vm", "venv"), https, plain)
	last("gone")
	check(host("vm", "venv"), bv, onVM)
	last("")
	d.cfg.DefaultAgentName = "codex"
	check(host("vm", "venv"), bv, plain)
	// A last.json add cannot read or parse is a need too, as the add form
	// refuses on it: add reads it before it picks the host, and the line
	// would fail there. It is the one under LAATMUX_HOME as it is then,
	// which the fake daemon below moves.
	badLast := func() string {
		t.Helper()
		p := filepath.Join(os.Getenv("LAATMUX_HOME"), "last.json")
		if err := os.WriteFile(p, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	onBadLast := func(p string) string {
		return "vm/proj/b has no managed session; laatmux add makes one once " + p + " is readable JSON"
	}
	lastFile := badLast()
	check(host("vm", "venv"), bv, onBadLast(lastFile))
	check(host("box", "benv"), detOther, "/w/o on box has no managed session; laatmux add makes one once a branch is checked out in /w/o, host box has repos and worktrees directories in the config, "+other.Source+" is a repository in the config, and "+lastFile+" is readable JSON")
	// One that is not a file cannot be read at all.
	if err := os.Remove(lastFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lastFile, 0o700); err != nil {
		t.Fatal(err)
	}
	check(host("vm", "venv"), bv, onBadLast(lastFile))
	if err := os.Remove(lastFile); err != nil {
		t.Fatal(err)
	}
	d.cfg.DefaultAgentName, d.cfg.Agents = "", nil
	check(host("vm", "venv"), bv, "vm/proj/b has no managed session; laatmux add makes one once an agent is in the config")
	check(host("box", "benv"), detOther, "/w/o on box has no managed session; laatmux add makes one once a branch is checked out in /w/o, host box has repos and worktrees directories in the config, git@github.com:laat/other.git is a repository in the config, and an agent is in the config")
	// jump, from the hosts' records through the local daemon. A
	// detached worktree is not matched by jump at all.
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type == protocol.TypeSubscribe {
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Hosts: []protocol.HostStatus{
				{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Capabilities: []string{"status", "worktrees"}},
				{Name: "box", SSH: "box", EnvironmentID: "benv", Connected: true, Listed: true, Capabilities: []string{"status", "worktrees"}},
			}, Worktrees: []protocol.Worktree{bv, bb, other, apos, subst}})
		}
		return true
	})
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("hosts:\n  - name: mac\n  - name: vm\n    ssh: vm\n    repos: /r\n    worktrees: /w\n  - name: box\n    ssh: box\nagents:\n  claude: {cmd: [claude]}\n  codex: {cmd: [codex]}\nrepos:\n  - "+src+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", cfgPath)
	for target, want := range map[string]string{"vm/proj/b": onVM, "box/proj/b": onBox, "vm/other/b": onOther, "vm/proj/it's": onApos, "vm/proj/a$(x)": onSubst} {
		if err := cmdJump(context.Background(), []string{target}); err == nil || err.Error() != want {
			t.Errorf("jump %s: %v, want %q", target, err, want)
		}
	}
	want := onBadLast(badLast())
	if err := cmdJump(context.Background(), []string{"vm/proj/b"}); err == nil || err.Error() != want {
		t.Errorf("jump vm/proj/b with last.json unreadable: %v, want %q", err, want)
	}
}

// A worktree row with no home session whose agent is in a managed
// session attaches through the worktree's own workspace session, keyed
// by the worktree, so it never collides with the name that session has.
func TestRowSpecWorktreeThroughManagedAgent(t *testing.T) {
	h := config.Host{Host: peer.Host{Name: "vm", SSH: "vm"}}
	cfg := config.Config{Hosts: []config.Host{h}}
	w := protocol.Worktree{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/w/a", Source: "git@example.com:o/proj.git"}
	a := protocol.Agent{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "proj/a", WorktreeID: w.ID}
	spec, session, err := rowSpec(cfg, h, rows.Row{Host: "vm", Name: "proj/a", Worktree: &w, Agent: &a})
	if err != nil || session != "" || spec.Key != "venv//w/a" || spec.Managed != "proj/a" || spec.Name != "vm/proj/a" || spec.Branch != "a" {
		t.Fatalf("spec %+v session %q err %v", spec, session, err)
	}
	// With the home back the spec is the same session's.
	w.Session = "proj/a"
	home, _, err := rowSpec(cfg, h, rows.Row{Host: "vm", Name: "proj/a", Worktree: &w, Agent: &a})
	if err != nil || home != spec {
		t.Fatalf("home %+v, without %+v, err %v", home, spec, err)
	}
	// Two worktrees whose agents were moved into one managed session
	// by hand keep a local session each.
	w.Session = ""
	w2 := protocol.Worktree{ID: "venv/worktree//w/b", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/w/b"}
	a.Session = "shared"
	a2 := protocol.Agent{ID: "venv/laatmux/%2", EnvironmentID: "venv", Server: "laatmux", Session: "shared", WorktreeID: w2.ID}
	s1, _, err1 := rowSpec(cfg, h, rows.Row{Host: "vm", Worktree: &w, Agent: &a})
	s2, _, err2 := rowSpec(cfg, h, rows.Row{Host: "vm", Worktree: &w2, Agent: &a2})
	if err1 != nil || err2 != nil || s1.Name == s2.Name || s1.Key == s2.Key || s1.Managed != "shared" || s2.Managed != "shared" {
		t.Fatalf("shared session: %+v %+v %v %v", s1, s2, err1, err2)
	}
	// The name is encoded as add encodes it: tmux takes no dot in one.
	w.Branch = "fix/v1.2"
	s3, _, err := rowSpec(cfg, h, rows.Row{Host: "vm", Worktree: &w, Agent: &a})
	if err != nil || strings.Contains(s3.Name, ".") || s3.Name != workspace.SessionName("vm", "proj", "fix/v1.2") {
		t.Fatalf("encoded branch: %+v %v", s3, err)
	}
	w.Branch, w.Root = "", "/w/x.y"
	s4, _, err := rowSpec(cfg, h, rows.Row{Host: "vm", Worktree: &w, Agent: &a})
	if err != nil || strings.Contains(s4.Name, ".") {
		t.Fatalf("detached: %+v %v", s4, err)
	}
}

// A detached worktree whose directory name has a \, a control byte,
// DEL, a byte that is not UTF-8, a $ before a letter, _ or { or a C1
// control character gets a workspace session that the name its jump
// computed finds: tmux stores a \ in a session name doubled, tmux 3.2
// to 3.4 store such a $ as \$, tmux 3.3 escapes a C1 control, and the
// others are escaped by vis(3), so a session made under the name as
// given was not found by it, and the tags set in new-session's own
// sequence found no session. A valid multibyte UTF-8 name is kept as
// given. The session is found by its key the second time: the key is
// stored with the root encoded where it has a byte tmux 3.4 and 3.5
// read back escaped, a newline or the field separator, which would
// split the session's line (#214), and a key with such a $ is read
// back as written since its \$ is undone (#227).
func TestEnsureDetachedRootWithEscapedByte(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	h := config.Host{Host: peer.Host{Name: "mac"}}
	cfg := config.Config{Hosts: []config.Host{h}}
	for i, c := range []struct {
		dir   string
		again bool
	}{
		{`a\b`, true}, {"tab\tx", true}, {"blåbær", true}, {"c1\xc2\x85x", true},
		{"a\x01b", true}, {"del\x7f", true}, {"a\xffb", true}, {"nl\nx", true}, {"sep" + tmux.Sep + "x", true},
		{"$x", true},
	} {
		root := "/w/proj/" + c.dir
		w := protocol.Worktree{ID: "env/worktree/" + root, EnvironmentID: "env", Repo: "proj", Root: root}
		a := protocol.Agent{ID: fmt.Sprintf("env/laatmux/%%%d", i), EnvironmentID: "env", Server: "laatmux", Session: fmt.Sprintf("m%d", i), WorktreeID: w.ID}
		spec, _, err := rowSpec(cfg, h, rows.Row{Host: "mac", Worktree: &w, Agent: &a})
		if err != nil {
			t.Fatal(err)
		}
		name, created, err := workspace.Ensure(ctx, spec)
		if err != nil || !created || name != spec.Name {
			t.Errorf("%q: ensure %q: %q %v %v", c.dir, spec.Name, name, created, err)
			continue
		}
		if _, err := workspace.Server.Run(ctx, "has-session", "-t", "="+name+":"); err != nil {
			t.Errorf("%q: has-session %q: %v", c.dir, name, err)
		}
		if !c.again {
			continue
		}
		locals, err := workspace.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if l, ok := workspace.Find(locals, spec.Key, ""); !ok || l.Name != spec.Name {
			t.Errorf("%q: key %q not read back: %+v", c.dir, spec.Key, locals)
		}
		if name, created, err := workspace.Ensure(ctx, spec); err != nil || created || name != spec.Name {
			t.Errorf("%q: ensure again: %q %v %v", c.dir, name, created, err)
		}
	}
}

// A session on a host made as a\b or with a tab, whose name tmux lists
// escaped, is attached from a local session made under the name the
// jump computes, which has-session finds by that name, tagged with the
// session's name as the host lists it, and found by the tag again. An
// agent's row, a pane in it, a jump by the session's name and a
// worktree whose home it is name the local session alike. The host is
// this machine, its managed server as isolated as the default one, and
// its session names are read as the daemon reads them. c$xd is listed
// as c\$xd on tmux 3.2 to 3.4, which tmux 3.4 prints as c\\$xd and
// ListPanes reads as stored, and its local name then needs the $
// encoded too; its attach tag reads back as written, through Query.
// h\##{x} is stored, and listed, as h\\#{x}: its local name keeps the
// #, which new-session is given as FormatLiteral writes it.
func TestEnsureListedHostSessionName(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	host := tmux.LaatmuxServer
	t.Cleanup(func() { _, _ = host.Run(context.Background(), "kill-server") })
	made := 0
	for i, name := range []string{`a\b`, "tab\tx", "c$xd", `h\##{x}`} {
		args := []string{"new-session", "-d", "-s", name, "sleep 1000"}
		if i == 0 {
			args = append([]string{"-f", "/dev/null"}, args...)
		}
		if _, err := host.Run(ctx, args...); err != nil {
			// tmux 3.7 refuses a control byte in a session name, so a
			// host there has no session with a tab.
			if name == "tab\tx" && strings.Contains(err.Error(), "invalid session name") {
				t.Logf("%q: %v", name, err)
				continue
			}
			t.Fatal(err)
		}
		made++
	}
	panes, err := host.ListPanes(ctx)
	if err != nil || len(panes) != made {
		t.Fatalf("panes %+v %v", panes, err)
	}
	h := config.Host{Host: peer.Host{Name: "mac"}}
	cfg := config.Config{Hosts: []config.Host{h}}
	for _, p := range panes {
		listed := p.Session
		row := rows.Row{Kind: rows.KindAgent, Host: "mac", Agent: &protocol.Agent{ID: "env/laatmux/" + p.ID, EnvironmentID: "env", Server: "laatmux", Session: listed, PaneID: p.ID}}
		spec, _, err := rowSpec(cfg, h, row)
		if err != nil || spec.Managed != listed || spec.Key != "" {
			t.Errorf("%q: spec %+v %v", listed, spec, err)
			continue
		}
		if got := paneSpec(cfg, h, nil, row, paneTarget{"laatmux", listed, p.ID}); got != spec {
			t.Errorf("%q: the pane's spec %+v, the row's %+v", listed, got, spec)
		}
		if got := attachSpec(h, listed); got != spec {
			t.Errorf("%q: the jump's spec %+v, the row's %+v", listed, got, spec)
		}
		if got := worktreeSpec(h, protocol.Worktree{Session: listed}); got.Name != spec.Name || got.Managed != listed {
			t.Errorf("%q: the worktree's spec %+v, the row's %+v", listed, got, spec)
		}
		name, created, err := workspace.Ensure(ctx, spec)
		if err != nil || !created || name != spec.Name {
			t.Errorf("%q: ensure %q: %q %v %v", listed, spec.Name, name, created, err)
			continue
		}
		if _, err := workspace.Server.Run(ctx, "has-session", "-t", "="+name+":"); err != nil {
			t.Errorf("%q: has-session %q: %v", listed, name, err)
		}
		out, err := workspace.Server.Run(ctx, "show-options", "-v", "-t", "="+name+":", "@laatmux_host")
		if err != nil || strings.TrimSpace(string(out)) != "mac" {
			t.Errorf("%q: %s host tag %q %v", listed, name, out, err)
		}
		out, err = workspace.Server.Query(ctx, "#{@laatmux_attach}", "display-message", "-p", "-t", "="+name+":")
		if tag := strings.TrimSpace(string(out)); err != nil || tag != "mac/"+listed {
			t.Errorf("%q: %s attach tag %q %v", listed, name, out, err)
		}
		if name, created, err := workspace.Ensure(ctx, spec); err != nil || created || name != spec.Name {
			t.Errorf("%q: ensure again: %q %v %v", listed, name, created, err)
		}
	}
}

// A worktree's workspace session is named alike by the jump from its
// home session, by the jump from its root agent's session once the home
// is lost, and by add, which names it by the branch: from the managed
// session's name as SessionName makes it, and as an older build made it
// for a branch with a $, which it kept as it was.
func TestWorktreeSessionNamesAlike(t *testing.T) {
	h := config.Host{Host: peer.Host{Name: "mac"}}
	for _, c := range []struct{ branch, session string }{
		{"main", ""}, {"fix/v1.2", ""}, {"a%5cb", ""}, {"fix$HOME", ""}, {"fix$HOME", "proj/fix$HOME"}, {"v$1", "proj/v$1"},
	} {
		if c.session == "" {
			c.session = tmux.SessionName("proj", c.branch)
		}
		w := protocol.Worktree{Repo: "proj", Branch: c.branch, Session: c.session}
		home, lost := worktreeSpec(h, w).Name, worktreeSessionName(h, w)
		if add := workspace.SessionName("mac", "proj", c.branch); home != add || lost != add {
			t.Errorf("%q in %q: home %q, lost %q, add %q", c.branch, c.session, home, lost, add)
		}
	}
}

// A pane jump: a tile, an agent node and a pane node name their pane; a
// worktree line, a task and a run do not. A pane on a remote host's
// default server is refused as jump refuses it.
func TestPaneJumpRouting(t *testing.T) {
	a := &protocol.Agent{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "proj/x", PaneID: "%1"}
	p := &protocol.Pane{ID: "venv/pane/laatmux/%2", EnvironmentID: "venv", Server: "laatmux", Session: "proj/x", PaneID: "%2"}
	for _, c := range []struct {
		row  rows.Row
		want string
	}{
		{rows.Row{Kind: rows.KindTile, Agent: a}, "%1"},
		{rows.Row{Kind: rows.KindAgent, Agent: a}, "%1"},
		{rows.Row{Kind: rows.KindPane, Pane: p}, "%2"},
		{rows.Row{Kind: rows.KindWorktree, Agent: a}, ""},
		{rows.Row{Kind: rows.KindTile, Agent: a, Pending: &protocol.Pending{ID: "t"}}, ""},
		{rows.Row{Kind: rows.KindRun, Run: &protocol.Run{ID: "r"}}, ""},
	} {
		pt, ok := paneOf(c.row)
		if got := pt.paneID; got != c.want || ok != (c.want != "") {
			t.Errorf("%v: %q ok %v", c.row.Kind, got, ok)
		}
	}
	cfg := config.Config{Hosts: []config.Host{{Host: peer.Host{Name: "vm", SSH: "vm"}}}}
	// The session a pane's jump attaches, by the pane's session: the
	// worktree's workspace session from its home; from its root agent's
	// session when the home is lost; a task's before the listing; a
	// pane of one worktree in another's session, the other's; a task's
	// by name; and a plain one otherwise.
	h := cfg.Hosts[0]
	wt := &protocol.Worktree{ID: "venv/worktree//r/x", EnvironmentID: "venv", Repo: "laatmux", Source: "git@github.com:laat/laatmux.git", Branch: "x", Root: "/r/x", Session: "laatmux/x"}
	other := &protocol.Worktree{ID: "venv/worktree//r/y", EnvironmentID: "venv", Repo: "laatmux", Source: "git@github.com:laat/laatmux.git", Branch: "y", Root: "/r/y", Session: "laatmux/y"}
	lost := *wt
	lost.Session = ""
	home := &rows.Row{Kind: rows.KindWorktree, Depth: 1, Host: "vm", Worktree: wt}
	lostLine := &rows.Row{Kind: rows.KindWorktree, Depth: 1, Host: "vm", Worktree: &lost, Agent: &protocol.Agent{ID: "venv/laatmux/%4", Server: "laatmux", Session: "laatmux/x-2", Managed: true, Cwd: "/r/x"}}
	task := &rows.Row{Kind: rows.KindTask, Depth: 1, Host: "vm", Pending: &protocol.Pending{ID: "add-1", Host: "vm", EnvironmentID: "venv", Source: "git@github.com:laat/laatmux.git", Repo: "laatmux", Branch: "z", Root: "/r/z", Session: "laatmux/z", Taken: true}}
	// A task standing for a worktree listed without a home, before the
	// add's agent is identified: the task's session is the home.
	listed := &protocol.Worktree{ID: "venv/worktree//r/z", EnvironmentID: "venv", Repo: "laatmux", Source: "git@github.com:laat/laatmux.git", Branch: "z", Root: "/r/z"}
	owner := &rows.Row{Kind: rows.KindTask, Depth: 1, Host: "vm", Worktree: listed, Pending: task.Pending}
	moved := &rows.Row{Kind: rows.KindTask, Depth: 1, Host: "vm", Worktree: listed, Pending: task.Pending, Agent: &protocol.Agent{ID: "venv/laatmux/%5", Server: "laatmux", Session: "laatmux/z-2", Managed: true, Cwd: "/r/z"}}
	otherLine := &rows.Row{Kind: rows.KindWorktree, Depth: 1, Host: "vm", Worktree: other}
	for _, c := range []struct {
		line    *rows.Row
		row     rows.Row
		target  paneTarget
		name    string
		managed string
		key     string
	}{
		{home, rows.Row{Kind: rows.KindPane, Worktree: wt, Pane: p}, paneTarget{"laatmux", "laatmux/x", "%2"}, "vm/laatmux/x", "laatmux/x", "venv//r/x"},
		{lostLine, rows.Row{Kind: rows.KindAgent, Worktree: &lost, Agent: a}, paneTarget{"laatmux", "laatmux/x-2", "%1"}, "vm/laatmux/x", "laatmux/x-2", "venv//r/x"},
		{task, rows.Row{Kind: rows.KindAgent, Agent: a}, paneTarget{"laatmux", "laatmux/z", "%1"}, "vm/laatmux/z", "laatmux/z", "venv//r/z"},
		{otherLine, rows.Row{Kind: rows.KindAgent, Worktree: wt, Agent: a}, paneTarget{"laatmux", "laatmux/y", "%1"}, "vm/laatmux/y", "laatmux/y", "venv//r/y"},
		{owner, rows.Row{Kind: rows.KindPane, Pane: p}, paneTarget{"laatmux", "laatmux/z", "%2"}, "vm/laatmux/z", "laatmux/z", "venv//r/z"},
		// The task's root agent went to another session than the task
		// recorded: the pane's session, not the record's.
		{moved, rows.Row{Kind: rows.KindAgent, Agent: a}, paneTarget{"laatmux", "laatmux/z-2", "%1"}, "vm/laatmux/z", "laatmux/z-2", "venv//r/z"},
		{nil, rows.Row{Kind: rows.KindAgent, Worktree: &lost, Agent: a}, paneTarget{"laatmux", "scratch", "%1"}, "vm/scratch", "scratch", ""},
	} {
		spec := paneSpec(cfg, h, c.line, c.row, c.target)
		if spec.Managed != c.managed || spec.Name != c.name || spec.Key != c.key {
			t.Errorf("%v in %s: spec %+v", c.row.Kind, c.target.session, spec)
		}
	}
	// The task line's own jump attaches the session Home names, and
	// the pane jump routed to the line names the local session as the
	// line's own does: a task's session under any name, a home renamed.
	foo := *moved
	foo.Pending = &protocol.Pending{}
	*foo.Pending = *task.Pending
	foo.Pending.Session = "laatmux/foo"
	foo.Agent = &protocol.Agent{ID: "venv/laatmux/%5", Server: "laatmux", Session: "laatmux/foo", Managed: true, Cwd: "/r/z"}
	loose := *task
	loose.Pending = foo.Pending
	renamed := *owner
	renamed.Worktree = &protocol.Worktree{}
	*renamed.Worktree = *listed
	renamed.Worktree.Session = "foo"
	for _, line := range []*rows.Row{task, owner, moved, &foo, &loose, &renamed} {
		target, err := pendingTarget(*line)
		if err != nil {
			t.Errorf("%s's target: %v", line.Pending.Session, err)
			continue
		}
		spec, session, err := rowSpec(cfg, h, target)
		if err != nil || session != "" || spec.Managed != line.Home() {
			t.Errorf("%s's jump: %+v %q %v, home %q", line.Pending.Session, spec, session, err, line.Home())
		}
		if got := paneSpec(cfg, h, line, rows.Row{Kind: rows.KindAgent, Agent: a}, paneTarget{"laatmux", line.Home(), "%1"}); got != spec {
			t.Errorf("%s: the pane jump's spec %+v, the line's %+v", line.Pending.Session, got, spec)
		}
	}
	// The line a pane's session routes by, from the tree. Before the
	// lines whose own sessions laatmux/x, laatmux/z and laatmux/foo are,
	// homeless lines whose root agents were moved into them, which those
	// lines win: a worktree's home also over a line the session is named
	// after, a second checkout of branch x, and a task also once its
	// root agent is identified in its session. After lostLine, another
	// homeless line whose root agent is in laatmux/x-2, which the first
	// of the two wins; two homeless lines whose root agents share
	// laatmux/w%2e1, which the one it is named after wins, branch w.1,
	// though second; homeless q, whose session laatmux/q holds the
	// task's root agent, moved out of the task's session, which q wins,
	// though second; and a homeless line with no agent, whose Home is
	// "", which no session finds.
	homeless := func(dir, session string) rows.Row {
		root := "/r/" + dir
		r := rows.Row{Kind: rows.KindWorktree, Depth: 1, Host: "vm", Worktree: &protocol.Worktree{ID: "venv/worktree/" + root, EnvironmentID: "venv", Repo: "laatmux", Branch: dir, Root: root}}
		if session != "" {
			r.Agent = &protocol.Agent{ID: "venv/laatmux/%" + dir, Server: "laatmux", Session: session, Managed: true, Cwd: root}
		}
		return r
	}
	identified := *moved
	identified.Agent = &protocol.Agent{ID: "venv/laatmux/%5", Server: "laatmux", Session: "laatmux/z", Managed: true, Cwd: "/r/z"}
	movedQ := *moved
	movedQ.Agent = &protocol.Agent{ID: "venv/laatmux/%5", Server: "laatmux", Session: "laatmux/q", Managed: true, Cwd: "/r/z"}
	strayX := homeless("a", "laatmux/x")
	strayX.Worktree.Branch = "x"
	namedW := homeless("w.1", "laatmux/w%2e1")
	m := &view.Model{Tree: []rows.Row{
		{Kind: rows.KindRepo, Depth: 0, Node: "repo/x"}, homeless("d", ""), strayX, homeless("b", "laatmux/z"), homeless("f", "laatmux/foo"),
		*home, *lostLine, *task, *otherLine, homeless("c", "laatmux/x-2"), homeless("e", "laatmux/w%2e1"), namedW, homeless("q", "laatmux/q"),
		{Kind: rows.KindWorktree, Depth: 1, Host: "mac", Worktree: &protocol.Worktree{ID: "menv/worktree//r/x", EnvironmentID: "menv", Session: "laatmux/x"}},
		// A worktree no configured host claims: none of another unclaimed
		// machine's sessions of the same name is its.
		{Kind: rows.KindWorktree, Depth: 1, Worktree: &protocol.Worktree{ID: "xenv/worktree//r/u", EnvironmentID: "xenv", Session: "laatmux/u"}},
	}}
	for _, c := range []struct {
		host, session, want string
		at                  *rows.Row // the task's line
	}{
		{"vm", "laatmux/x", home.Worktree.ID, task}, {"vm", "laatmux/x-2", lostLine.Worktree.ID, task}, {"vm", "laatmux/z", "add-1", task}, {"vm", "laatmux/z", "add-1", owner},
		{"vm", "laatmux/z", "add-1", &identified}, {"vm", "laatmux/foo", "add-1", &foo}, {"vm", "laatmux/w%2e1", namedW.Worktree.ID, task},
		{"vm", "laatmux/q", "venv/worktree//r/q", &movedQ},
		{"vm", "laatmux/y", other.ID, task}, {"vm", "scratch", "", task}, {"mac", "laatmux/x", "menv/worktree//r/x", task}, {"vm", "", "", task},
		{"vm", "laatmux/z-2", "add-1", moved}, {"", "laatmux/u", "", task},
	} {
		m.Tree[7] = *c.at
		got := ""
		if l := m.LineFor(c.host, c.session); l != nil {
			got = l.ID()
		}
		if got != c.want {
			t.Errorf("LineFor(%s, %s) = %q, want %q", c.host, c.session, got, c.want)
		}
	}
	remote := &protocol.Agent{ID: "venv/default/%3", EnvironmentID: "venv", Server: "default", Session: "notes", PaneID: "%3"}
	_, err := jumpPane(context.Background(), cfg, nil, rows.Row{Kind: rows.KindTile, Host: "vm", Name: "notes", Agent: remote}, paneTarget{"default", "notes", "%3"})
	if err == nil || !strings.Contains(err.Error(), "only observes") {
		t.Errorf("a remote default server: %v", err)
	}
}

// The select round trip: a daemon with the capability answers, one
// without is asked nothing, and one that withholds the result holds
// the caller only until the context ends.
func TestSelectRemote(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	hold := make(chan struct{})
	serve := func(pc *protocol.Conn, m protocol.Message) bool {
		if m.Type != protocol.TypeSelect {
			return true
		}
		mu.Lock()
		asked = append(asked, m.PaneID)
		mu.Unlock()
		if m.PaneID == "%hold" {
			<-hold
			return false
		}
		pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: m.PaneID != "%gone", Error: "pane %gone: no such pane"})
		return true
	}
	local := peer.Host{Name: "mac"}
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapSelect}, serve)
	ctx := context.Background()
	if err := selectRemote(ctx, local, "%1"); err != nil {
		t.Errorf("select: %v", err)
	}
	if err := selectRemote(ctx, local, "%gone"); err == nil || !strings.Contains(err.Error(), "no such pane") {
		t.Errorf("a pane gone: %v", err)
	}
	sctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := selectRemote(sctx, local, "%hold")
	close(hold)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("a withheld result: %v after %v", err, time.Since(start))
	}
	mu.Lock()
	got := strings.Join(asked, " ")
	mu.Unlock()
	if got != "%1 %gone %hold" {
		t.Errorf("asked %q", got)
	}
	// Without the capability nothing is asked.
	startFakeDaemon(t, []string{protocol.CapStatus}, serve)
	if err := selectRemote(ctx, local, "%2"); err != nil {
		t.Errorf("without select: %v", err)
	}
	mu.Lock()
	got = strings.Join(asked, " ")
	mu.Unlock()
	if got != "%1 %gone %hold" {
		t.Errorf("asked %q without the capability", got)
	}
}
