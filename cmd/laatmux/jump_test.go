package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	"github.com/laat/laatmux/internal/worktree"
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
	// absent is that tmux said the session or its server is not there
	// (errNoSession), which jump's last reading waits for; a tmux that
	// could not tell is refused in the same words, but not absent.
	cases := []struct {
		name, script, want string
		absent             bool
	}{
		{"absent", `printf "can't find session: =x:\n" >&2; exit 1`, "vm/x: no such session", true},
		{"refused", `printf "ssh: connect to host vm port 22: Connection refused\n" >&2; exit 255`, "vm: ssh failed: ssh: connect to host vm port 22: Connection refused", false},
		{"no server", `printf "no server running on /tmp/tmux-1001/laatmux\n" >&2; exit 1`, "vm/x: no such session", true},
		{"no socket", `printf "error connecting to /tmp/tmux-1001/laatmux (No such file or directory)\n" >&2; exit 1`, "vm/x: no such session", true},
		{"denied", `printf "error connecting to /tmp/tmux-1001/laatmux (Permission denied)\n" >&2; exit 1`, "vm/x: no such session", false},
		{"ok", `exit 0`, "", false},
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
			case errors.Is(err, errNoSession) != c.absent:
				t.Fatalf("%v: absent %v, want %v", err, errors.Is(err, errNoSession), c.absent)
			}
		})
	}
}

// This machine's check is as a remote host's: a tmux that says it has
// no such session or no server is errNoSession, one that could not tell
// is refused in the same words but not absent. The tmux is a stand-in.
func TestCheckSessionLocal(t *testing.T) {
	for _, c := range []struct {
		script string
		absent bool
	}{
		{`printf "can't find session: =x:\n" >&2; exit 1`, true},
		{`printf "no server running on /tmp/tmux-1001/laatmux\n" >&2; exit 1`, true},
		{`printf "error connecting to /tmp/tmux-1001/laatmux (Permission denied)\n" >&2; exit 1`, false},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\n"+c.script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		err := checkSession(context.Background(), peer.Host{Name: "mac"}, "x")
		if err == nil || err.Error() != "mac/x: no such session on the laatmux tmux server" || errors.Is(err, errNoSession) != c.absent {
			t.Errorf("%s: %v, absent %v", c.script, err, errors.Is(err, errNoSession))
		}
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
		{mac, "laatmux", "", 0, "mac: session name required"},
		{vm, "default", "c:d", 0, "vm/c:d: on vm's default tmux server"},
		// A target naming a branch with a C1 control character, which
		// git takes, is printed quoted.
		{vm, "default", "proj/b\u009b2J", 0, `"vm/proj/b\u009b2J": on vm's default tmux server`},
		{mac, "work", "proj/b\u009b2J", 0, `"mac/proj/b\u009b2J": tmux server work is not managed`},
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

// A main checkout's line with no home jumps to its agent's session on
// this machine's default server, with no workspace session; one on a
// remote host's default server is refused as jump refuses it; with no
// agent the jump says so, not how add would start one, and so does S's
// lookup, each a refusal the view makes a home for where it can, named
// as add would name a worktree's on the branch. With a home, the shell
// session a jump made, the line goes to its workspace session, keyed by
// the root, whatever agent runs in a plain session.
func TestJumpRowMainCheckout(t *testing.T) {
	mac := config.Host{Host: peer.Host{Name: "mac"}, Repos: config.Paths{"/r"}, Worktrees: "/w"}
	vm := config.Host{Host: peer.Host{Name: "vm", SSH: "vm"}}
	cfg := config.Config{Hosts: []config.Host{mac, vm}}
	w := protocol.Worktree{ID: "menv/checkout//r/proj", EnvironmentID: "menv", Repo: "proj", Branch: "main", Root: "/r/proj", Main: true}
	a := protocol.Agent{ID: "menv/default/%1", EnvironmentID: "menv", Server: "default", Session: "work", WorktreeID: w.ID}
	spec, session, err := rowSpec(cfg, mac, rows.Row{Kind: rows.KindWorktree, Host: "mac", Worktree: &w, Agent: &a})
	if err != nil || session != "work" || spec.Key != "" {
		t.Fatalf("local: spec %+v session %q err %v", spec, session, err)
	}
	if _, _, err := rowSpec(cfg, vm, rows.Row{Kind: rows.KindWorktree, Host: "vm", Worktree: &w, Agent: &a}); err == nil || !strings.Contains(err.Error(), "only observes") {
		t.Fatalf("remote: %v", err)
	}
	err = jumpRow(context.Background(), cfg, rows.Row{Kind: rows.KindWorktree, Host: "mac", Name: "proj/main", Worktree: &w})
	var nh *noHome
	if err == nil || err.Error() != "mac/proj/main is the main checkout, and no agent runs in it" || !errors.As(err, &nh) || nh.name != "proj/main" || nh.w.ID != w.ID {
		t.Fatalf("no agent: %v", err)
	}
	if _, err := localSpec(cfg, rows.Row{Kind: rows.KindWorktree, Host: "mac", Worktree: &w, Agent: &a}); err == nil || err.Error() != "mac/proj/main is the main checkout, which has no workspace session" || errors.As(err, &nh) {
		t.Fatalf("shell, an agent in a plain session: %v", err)
	}
	nh = nil
	if _, err := localSpec(cfg, rows.Row{Kind: rows.KindWorktree, Host: "mac", Worktree: &w}); err == nil || err.Error() != "mac/proj/main is the main checkout, which has no workspace session" || !errors.As(err, &nh) || nh.name != "proj/main" {
		t.Fatalf("shell, no agent: %v", err)
	}
	// A detached checkout gets no name a jump would make.
	detached := w
	detached.Branch = ""
	if err := jumpRow(context.Background(), cfg, rows.Row{Kind: rows.KindWorktree, Host: "mac", Worktree: &detached}); !errors.As(err, &nh) || nh.name != "" || err.Error() != "/r/proj on mac is the main checkout, and no agent runs in it" {
		t.Fatalf("detached: %v", err)
	}
	homed := w
	homed.Session = "proj/main"
	spec, session, err = rowSpec(cfg, mac, rows.Row{Kind: rows.KindWorktree, Host: "mac", Worktree: &homed, Agent: &a})
	if err != nil || session != "" || spec.Managed != "proj/main" || spec.Name != "mac/proj/main" || spec.Key != "menv//r/proj" {
		t.Fatalf("with a home: spec %+v session %q err %v", spec, session, err)
	}
	if spec, err := localSpec(cfg, rows.Row{Kind: rows.KindWorktree, Host: "mac", Worktree: &homed}); err != nil || spec.Key != "menv//r/proj" {
		t.Fatalf("shell, with a home: %+v %v", spec, err)
	}
}

// A user's after-list-sessions hook that fails after list-sessions
// printed: the dashboard's jump, its pane jump and its shell's lookup
// take the session Ensure made from the listing, and the view says
// nothing of the hook. Run outside tmux, each jump gets as far as
// saying how to attach.
func TestDashEnsureHookFails(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	if _, err := workspace.Server.Run(ctx, "set-hook", "-g", "after-list-sessions", "select-window -t nosuch:9"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { workspace.Server.Run(context.Background(), "set-hook", "-gu", "after-list-sessions") })
	h := config.Host{Host: peer.Host{Name: "mac"}}
	cfg := config.Config{Hosts: []config.Host{h}}
	worktree := func(branch string) *protocol.Worktree {
		root := "/w/proj/" + branch
		return &protocol.Worktree{ID: "menv/worktree/" + root, EnvironmentID: "menv", Repo: "proj", Branch: branch, Root: root, Session: "proj/" + branch}
	}
	if err := jumpRow(ctx, cfg, rows.Row{Kind: rows.KindWorktree, Host: "mac", Name: "proj/a", Worktree: worktree("a")}); err == nil || !strings.Contains(err.Error(), "; attach with: ") {
		t.Errorf("jump: %v, want the attach hint", err)
	}
	a := &protocol.Agent{ID: "menv/laatmux/%1", EnvironmentID: "menv", Server: "laatmux", Session: "scratch", PaneID: "%1"}
	if _, err := jumpPane(ctx, cfg, nil, rows.Row{Kind: rows.KindAgent, Host: "mac", Name: "scratch", Agent: a}, paneTarget{"laatmux", "scratch", "%1"}); err == nil || !strings.Contains(err.Error(), "; attach with: ") {
		t.Errorf("pane jump: %v, want the attach hint", err)
	}
	d := &dash{ctx: ctx, cfg: cfg, st: merged.New()}
	if l, err := d.localFor(rows.Row{Kind: rows.KindWorktree, Host: "mac", Name: "proj/b", Worktree: worktree("b")}); err != nil || l.Name != "mac/proj/b" {
		t.Errorf("the shell's session: %+v %v, want mac/proj/b", l, err)
	}
	for _, name := range []string{"mac/proj/a", "mac/scratch", "mac/proj/b"} {
		if !workspace.Server.HasSession(ctx, name) {
			t.Errorf("no session %s", name)
		}
	}
}

// The preflight's "no such session" names the target quoted when it
// has a C1 control character, as a branch can.
func TestClassifyPreflightQuotes(t *testing.T) {
	err := exec.Command("sh", "-c", "exit 1").Run()
	if got := classifyPreflight("vm", "proj/b\u009b2J", err, nil, ""); got == nil || got.Error() != `"vm/proj/b\u009b2J": no such session on the laatmux tmux server` {
		t.Errorf("classifyPreflight = %v", got)
	}
}

// dollarQuote's word is read back byte for byte by bash, zsh and ksh,
// whichever are installed and know $'...', for every byte but NUL
// followed by digits, which a greedy escape would take; and it holds no
// ' or ! but its quotes, which bash 3.2's history expansion would
// misread. No user startup file runs: zsh -f, and no BASH_ENV or ENV.
func TestDollarQuote(t *testing.T) {
	var b strings.Builder
	for c := 1; c < 256; c++ {
		b.WriteByte(byte(c))
		b.WriteString("31")
	}
	s := b.String()
	word := dollarQuote(s)
	if !strings.HasPrefix(word, "$'") || !strings.HasSuffix(word, "'") || strings.ContainsAny(word[2:len(word)-1], "'!") {
		t.Fatalf("word %q", word)
	}
	if strings.ContainsFunc(word, func(r rune) bool { return r < 0x20 || r >= 0x7f }) {
		t.Fatalf("word has a byte that is not printable ASCII: %q", word)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "BASH_ENV=") && !strings.HasPrefix(kv, "ENV=") {
			env = append(env, kv)
		}
	}
	run := func(sh []string, script string) (string, error) {
		cmd := exec.Command(sh[0], append(sh[1:], "-c", script)...)
		cmd.Env = env
		out, err := cmd.Output()
		return string(out), err
	}
	ran := 0
	for _, sh := range [][]string{{"bash"}, {"zsh", "-f"}, {"ksh"}} {
		path, err := exec.LookPath(sh[0])
		if err != nil {
			continue
		}
		sh[0] = path
		// A ksh derived from pdksh has no $'...'.
		if out, err := run(sh, `printf %s $'\101'`); err != nil || out != "A" {
			continue
		}
		ran++
		if out, err := run(sh, "printf %s "+word); err != nil || out != s {
			t.Errorf("%s read back %q, %v", sh[0], out, err)
		}
	}
	if ran == 0 {
		t.Skip("no bash, zsh or ksh that knows $'...'")
	}
}

// A worktree with no home and no agent: enter on its line refuses with
// how add makes it a session, z on the line says the same, and so does
// laatmux jump. The add line comes only when add can run it: on vm, a
// host whose entry here has the directories, the add line for the
// branch; a detached worktree, named by its root, needs a branch
// checked out first, and a root with an ESC in it is named quoted;
// box's entry has no directories, which add needs first; a repository
// this machine's config does not list is named by its source, which
// --repo takes as the host's checkout, while one it lists under another
// form of the source is named by its label here. A worktree that lacks
// more than one is
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
	// git takes a C1 control character in a branch: the name is quoted
	// as tmux.Printable quotes it, and the add line has the branch as a
	// $'...' word, which the shell reads back byte for byte.
	ctl := bv
	ctl.ID, ctl.Branch, ctl.Root = "venv/worktree//w/c", "it's\u009b31m\\x", "/w/c"
	noSrc := bv
	noSrc.Source = ""
	shown := bv
	shown.ID, shown.Branch, shown.BranchDisplayOnly, shown.Root = "venv/worktree//w/hand", `"a\xffb"`, true, "/w/hand"
	host := func(name, env string) rows.Host {
		return rows.Host{Name: name, Local: name == "mac", EnvironmentID: env, Connected: true, Listed: true, Worktrees: true, Attribution: true}
	}
	onVM := "vm/proj/b has no managed session; laatmux add b --repo proj --host vm --agent claude makes one"
	onBox := "box/proj/b has no managed session; laatmux add makes one once host box has repos and worktrees directories in the config"
	onOther := "vm/other/b has no managed session; laatmux add b --repo git@github.com:laat/other.git --host vm --agent claude makes one"
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
		{host("vm", "venv"), ctl, `"vm/proj/it's\u009b31m\\x" has no managed session; laatmux add $'it\047s\302\23331m\134x' --repo proj --host vm --agent claude makes one`},
		{host("vm", "venv"), noSrc, "vm/proj/b has no managed session; laatmux add b --repo <repo> --host vm makes one"},
		// A branch only shown is no branch add can name (#304).
		{host("vm", "venv"), shown, `vm/proj/"a\xffb" has no managed session; laatmux add makes one once a branch laatmux can carry, valid UTF-8 without U+FFFD, is checked out in /w/hand instead of "a\xffb"`},
		{host("mac", "menv"), det, "/w/det on mac has no managed session; laatmux add makes one once a branch is checked out in /w/det"},
		{host("mac", "menv"), detCtl, `"/w/a\x1b]0;x\ab" on mac has no managed session; laatmux add makes one once a branch is checked out in "/w/a\x1b]0;x\ab"`},
		{host("box", "benv"), detBox, "/w/det on box has no managed session; laatmux add makes one once a branch is checked out in /w/det and host box has repos and worktrees directories in the config"},
		{host("box", "benv"), detOther, "/w/o on box has no managed session; laatmux add makes one once a branch is checked out in /w/o and host box has repos and worktrees directories in the config"},
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
	check(host("box", "benv"), detOther, "/w/o on box has no managed session; laatmux add makes one once a branch is checked out in /w/o, host box has repos and worktrees directories in the config, and "+lastFile+" is readable JSON")
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
	// Under a state directory with a tab and an ESC in its name, the
	// file is named as tmux.Printable shows it.
	was := os.Getenv("LAATMUX_HOME")
	odd := filepath.Join(t.TempDir(), "st\tate\x1b[31m")
	if err := os.Mkdir(odd, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_HOME", odd)
	check(host("vm", "venv"), bv, onBadLast(strconv.Quote(badLast())))
	t.Setenv("LAATMUX_HOME", was)
	d.cfg.DefaultAgentName, d.cfg.Agents = "", nil
	check(host("vm", "venv"), bv, "vm/proj/b has no managed session; laatmux add makes one once an agent is in the config")
	check(host("box", "benv"), detOther, "/w/o on box has no managed session; laatmux add makes one once a branch is checked out in /w/o, host box has repos and worktrees directories in the config, and an agent is in the config")
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

// jump to a worktree with no managed session and no agent asks the
// host's daemon for one, named as add names it, by the host's label and
// the branch encoded, at the root, with no command; then makes the
// workspace session attached to it, switches there, and says what it
// made. A name in use that no record places elsewhere, a session made
// since the listing, is attached, and so is one the records have as the
// worktree's: its own managed pane at the root beside a split gone
// elsewhere, a pane of it under the root, agents attributed to none
// under the root or with no directory. One another worktree has as its
// home, or in which an agent or a pane of another worktree runs, one
// nested in the root too, and another clone's whose split has gone to
// the root, is refused as add refuses it. A branch only shown, flagged
// or with U+FFFD from an older daemon, a name tmux would not store as
// given, a worktree whose agent is on a default server or in a managed
// session that lost its home, a host whose daemon lacks new, by the
// records or by its hello, keep the add hint, and nothing is made.
func TestJumpMakesShellSession(t *testing.T) {
	log := fakeDefaultTmux(t)
	src, fork := "git@github.com:laat/proj.git", "git@github.com:fork/proj.git"
	wt := func(env, repo, branch, root string) protocol.Worktree {
		return protocol.Worktree{ID: env + "/worktree/" + root, EnvironmentID: env, Repo: repo, Source: src, Branch: branch, Root: root}
	}
	b, dotted, taken, busy, rooted := wt("menv", "proj", "b", "/w/b"), wt("menv", "proj", "a.b$c", "/w/ab"), wt("menv", "proj", "taken", "/w/taken"), wt("menv", "proj", "busy", "/w/busy"), wt("menv", "proj", "rooted", "/w/rooted")
	c := wt("menv", "proj-host", "c", "/w/c")
	// Another clone's worktrees on the branches of clone, lost and pn,
	// whose sessions are named as theirs would be: its home, its agent
	// with the home lost, a pane of it.
	clone, lost, pn := wt("menv", "proj", "clone", "/w/clone"), wt("menv", "proj", "lost", "/w/lost"), wt("menv", "proj", "pn", "/w/pn")
	other, otherLost, otherPn := wt("menv", "proj", "clone", "/w2/clone"), wt("menv", "proj", "lost", "/w2/lost"), wt("menv", "proj", "pn", "/w2/pn")
	other.Source, otherLost.Source, otherPn.Source, other.Session = fork, fork, fork, "proj/clone"
	shown := wt("menv", "proj", `"a\xffb"`, "/w/hand")
	shown.BranchDisplayOnly = true
	mangled := wt("menv", "proj", "a�b", "/w/mangled")
	// An older host labels a checkout its config does not list by its
	// directory, a . kept.
	dir := wt("menv", "next.js", "nb", "/w/nb")
	onBox := wt("benv", "proj", "b", "/w/b")
	// Sessions in use that are the worktree's by their records: its own
	// pane at the root beside a split gone to c; a pane it has in a
	// directory under the root; agents the host attributes to none, under
	// the root and with no directory. And one of a worktree nested in
	// nest's root, which is that worktree's; and another clone's, made at
	// its own root, a split of which has gone to rs's root.
	split, sub, loose, nest, rs := wt("menv", "proj", "split", "/w/split"), wt("menv", "proj", "sub", "/w/sub"), wt("menv", "proj", "loose", "/w/loose"), wt("menv", "proj", "nest", "/w/nest"), wt("menv", "proj", "rs", "/w/rs")
	inner, otherRs := wt("menv", "proj", "inner", "/w/nest/inner"), wt("menv", "proj", "rs", "/w2/rs")
	otherRs.Source = fork
	pane := func(n int, session, cwd, worktreeID string, managed bool) protocol.Pane {
		id := fmt.Sprintf("%%%d", n)
		return protocol.Pane{ID: "menv/pane/laatmux/" + id, EnvironmentID: "menv", Server: "laatmux", Session: session, PaneID: id, Cwd: cwd, WorktreeID: worktreeID, Managed: managed}
	}
	caps := []string{protocol.CapStatus, protocol.CapWorktrees, protocol.CapAttribution, protocol.CapNew}
	snap := protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{
		{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Capabilities: caps},
		{Name: "box", SSH: "box", EnvironmentID: "benv", Connected: true, Listed: true, Capabilities: caps[:3]},
	}, Worktrees: []protocol.Worktree{b, dotted, taken, busy, rooted, c, clone, lost, pn, other, otherLost, otherPn, shown, mangled, dir, onBox, split, sub, loose, nest, inner, rs, otherRs},
		Agents: []protocol.Agent{
			{ID: "menv/default/%1", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", WorktreeID: busy.ID, Cwd: busy.Root},
			{ID: "menv/laatmux/%2", EnvironmentID: "menv", Server: "laatmux", Session: "old/rooted", Agent: "claude", Managed: true, WorktreeID: rooted.ID, Cwd: rooted.Root},
			{ID: "menv/laatmux/%3", EnvironmentID: "menv", Server: "laatmux", Session: "proj/lost", Agent: "claude", Managed: true, WorktreeID: otherLost.ID, Cwd: otherLost.Root},
			{ID: "menv/laatmux/%10", EnvironmentID: "menv", Server: "laatmux", Session: "proj/loose", Agent: "claude", Cwd: "/w/loose/x"},
			{ID: "menv/laatmux/%11", EnvironmentID: "menv", Server: "laatmux", Session: "proj/loose", Agent: "codex"},
			// An agent of sub's in a pane new made below its root: the
			// worktree's, unlike a main checkout's.
			{ID: "menv/laatmux/%13", EnvironmentID: "menv", Server: "laatmux", Session: "proj/sub", Agent: "claude", Managed: true, WorktreeID: sub.ID, Cwd: "/w/sub/src"},
		},
		Panes: []protocol.Pane{
			pane(4, "proj/pn", "/w2/pn/src", otherPn.ID, false),
			pane(5, "proj/split", "/w/split", split.ID, true), pane(6, "proj/split", "/w/c", c.ID, false),
			pane(7, "proj/sub", "/w/sub/src", sub.ID, false),
			pane(8, "proj/nest", "/w/nest/inner", inner.ID, false),
			pane(9, "proj/rs", "/w2/rs", otherRs.ID, true), pane(12, "proj/rs", "/w/rs", rs.ID, false),
		}}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("hosts:\n  - name: mac\n    repos: /r\n    worktrees: /w\n  - name: box\n    ssh: box\nagents:\n  claude: {cmd: [claude]}\n  codex: {cmd: [codex]}\nrepos:\n  - "+src+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", cfgPath)
	requests := fakeNew(t, "menv", []string{protocol.CapStatus, protocol.CapMerged, protocol.CapNew}, &snap, inUse("proj/taken", "proj/clone", "proj/lost", "proj/pn", "proj/split", "proj/sub", "proj/loose", "proj/nest", "proj/rs"))
	jump := func(target string) (out, cmds, req string, err error) {
		t.Helper()
		os.Remove(log)
		f, ferr := os.Create(filepath.Join(t.TempDir(), "stdout"))
		if ferr != nil {
			t.Fatal(ferr)
		}
		was := os.Stdout
		os.Stdout = f
		err = cmdJump(context.Background(), []string{target})
		os.Stdout = was
		f.Close()
		got, _ := os.ReadFile(f.Name())
		tm, _ := os.ReadFile(log)
		return string(got), string(tm), asked(requests), err
	}
	for _, k := range []struct{ target, req, made, local, managed string }{
		{"mac/proj/b", `proj/b /w/b mac []`, "made session proj/b on mac, a shell at /w/b\n", "mac/proj/b", "proj/b"},
		{"mac/proj/a.b$c", `proj/a%2eb%24c /w/ab mac []`, "made session proj/a%2eb%24c on mac, a shell at /w/ab\n", "mac/proj/a%2eb%24c", "proj/a%2eb%24c"},
		// By the host's label, as add named it, the target by this
		// machine's.
		{"mac/proj/c", `proj-host/c /w/c mac []`, "made session proj-host/c on mac, a shell at /w/c\n", "mac/proj-host/c", "proj-host/c"},
		// Made since the listing: attached, nothing said made.
		{"mac/proj/taken", `proj/taken /w/taken mac []`, "", "mac/proj/taken", "proj/taken"},
		// In use, and the worktree's by its records.
		{"mac/proj/split", `proj/split /w/split mac []`, "", "mac/proj/split", "proj/split"},
		{"mac/proj/sub", `proj/sub /w/sub mac []`, "", "mac/proj/sub", "proj/sub"},
		{"mac/proj/loose", `proj/loose /w/loose mac []`, "", "mac/proj/loose", "proj/loose"},
	} {
		out, cmds, req, err := jump(k.target)
		if err != nil || req != k.req || out != k.made {
			t.Errorf("jump %s: %v, asked %q, printed %q", k.target, err, req, out)
		}
		for _, want := range []string{"new-session -d -s " + k.local + " ", "@laatmux_attach_target " + k.managed + " ", "switch-client -t =" + k.local + ":"} {
			if !strings.Contains(cmds, want) {
				t.Errorf("jump %s ran %q, want %q in it", k.target, cmds, want)
			}
		}
	}
	for _, k := range []struct{ target, req, in, root string }{
		{"mac/proj/clone", `proj/clone /w/clone mac []`, "/w2/clone", "/w/clone"},
		{"mac/proj/lost", `proj/lost /w/lost mac []`, "/w2/lost", "/w/lost"},
		{"mac/proj/pn", `proj/pn /w/pn mac []`, "/w2/pn/src", "/w/pn"},
		{"mac/proj/nest", `proj/nest /w/nest mac []`, "/w/nest/inner", "/w/nest"},
		{"mac/proj/rs", `proj/rs /w/rs mac []`, "/w2/rs", "/w/rs"},
	} {
		want := "mac: session " + strings.TrimPrefix(k.target, "mac/") + " runs in " + k.in + ", not " + k.root + "; name in use"
		if out, cmds, req, err := jump(k.target); err == nil || err.Error() != want || req != k.req || out != "" || cmds != "" {
			t.Errorf("jump %s: %v, asked %q, printed %q, tmux %q; want %q", k.target, err, req, out, cmds, want)
		}
	}
	hints := map[string]string{
		"mac/proj/busy":     "mac/proj/busy has no managed session; laatmux add busy --repo proj --host mac --agent claude makes one",
		"mac/proj/rooted":   "mac/proj/rooted has no managed session; laatmux add rooted --repo proj --host mac --agent claude makes one",
		`mac/proj/"a\xffb"`: `mac/proj/"a\xffb" has no managed session; laatmux add makes one once a branch laatmux can carry, valid UTF-8 without U+FFFD, is checked out in /w/hand instead of "a\xffb"`,
		"mac/proj/a�b":      "mac/proj/a�b has no managed session; laatmux add a�b --repo proj --host mac --agent claude makes one",
		"mac/next.js/nb":    "mac/next.js/nb has no managed session; laatmux add nb --repo proj --host mac --agent claude makes one",
		"box/proj/b":        "box/proj/b has no managed session; laatmux add makes one once host box has repos and worktrees directories in the config",
	}
	for target, want := range hints {
		if out, cmds, req, err := jump(target); err == nil || err.Error() != want || req != "" || out != "" || cmds != "" {
			t.Errorf("jump %s: %v, asked %q, printed %q, tmux %q; want %q", target, err, req, out, cmds, want)
		}
	}
	// A daemon whose hello lacks new, the records' notwithstanding: an
	// older build answering since.
	requests = fakeNew(t, "menv", []string{protocol.CapStatus, protocol.CapMerged}, &snap, nil)
	want := "mac/proj/b has no managed session; laatmux add b --repo proj --host mac --agent claude makes one"
	if out, cmds, req, err := jump("mac/proj/b"); err == nil || err.Error() != want || req != "" || out != "" || cmds != "" {
		t.Errorf("jump with no new in the hello: %v, asked %q, printed %q, tmux %q; want %q", err, req, out, cmds, want)
	}
	// A daemon that answers as another machine than the records', the
	// host entry moved since: nothing is asked of it.
	requests = fakeNew(t, "other", []string{protocol.CapStatus, protocol.CapMerged, protocol.CapNew}, &snap, nil)
	want = "mac: new proj/b: answers as environment other, not menv the request was resolved for"
	if out, cmds, req, err := jump("mac/proj/b"); err == nil || err.Error() != want || req != "" || out != "" || cmds != "" {
		t.Errorf("jump to another machine: %v, asked %q, printed %q, tmux %q; want %q", err, req, out, cmds, want)
	}
}

// jump to a main checkout with no home and no agent, <host>/<repo>/<the
// branch it has>, asks the host's daemon for a session with a shell at
// its root, named as add would name a worktree's on the branch, then
// makes the workspace session keyed by the root attached to it,
// switches there and says what it made; a name in use that no record
// places elsewhere is attached, one in which an agent of another
// worktree runs refused, and so is one whose agent runs in a pane
// laatmux made below the root. One with a home goes to its workspace
// session and asks nothing, with an agent in the home and another in a plain
// session; one with an agent in a plain session and no home
// switches to that session, as before; one whose home a split took
// attaches the workspace session named after it to the session of the
// agent laatmux made at its root. A host whose daemon lacks new
// keeps the refusal that no agent runs in it (a detached checkout, which
// no target names by a branch, is TestJumpRowMainCheckout's).
func TestJumpMainCheckoutShell(t *testing.T) {
	log := fakeDefaultTmux(t)
	src := "git@github.com:laat/proj.git"
	main := func(repo, branch, root string) protocol.Worktree {
		return protocol.Worktree{ID: "menv/checkout/" + root, EnvironmentID: "menv", Repo: repo, Source: src, Branch: branch, Root: root, Main: true}
	}
	proj, taken, busy, homed, plain, det := main("proj", "main", "/r/proj"), main("lib", "dev", "/r/lib"), main("app", "main", "/r/app"), main("tool", "main", "/r/tool"), main("note", "main", "/r/note"), main("dots", "", "/r/dots")
	homed.Session = "tool/main"
	lost, below := main("kit", "main", "/r/kit"), main("sub", "main", "/r/sub")
	x := protocol.Worktree{ID: "menv/worktree//w/x", EnvironmentID: "menv", Repo: "proj", Source: src, Branch: "x", Root: "/w/x", Session: "proj/x"}
	caps := []string{protocol.CapStatus, protocol.CapWorktrees, protocol.CapAttribution, protocol.CapCheckouts, protocol.CapNew}
	snap := protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Capabilities: caps}},
		Worktrees: []protocol.Worktree{proj, taken, busy, homed, plain, det, lost, below, x},
		Agents: []protocol.Agent{
			// x's agent, moved by hand into the session app/main would be.
			{ID: "menv/laatmux/%2", EnvironmentID: "menv", Server: "laatmux", Session: "app/main", Agent: "claude", WorktreeID: x.ID, Cwd: "/w/x"},
			{ID: "menv/default/%3", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", WorktreeID: plain.ID, Cwd: plain.Root},
			// An agent in tool's home and one in a plain session: the
			// home is gone to all the same.
			{ID: "menv/laatmux/%4", EnvironmentID: "menv", Server: "laatmux", Session: "tool/main", Agent: "codex", Managed: true, WorktreeID: homed.ID, Cwd: homed.Root},
			{ID: "menv/default/%5", EnvironmentID: "menv", Server: "default", Session: "work", Agent: "claude", Activity: protocol.Working, WorktreeID: homed.ID, Cwd: homed.Root},
			// The agent laatmux made at kit's root, a split gone elsewhere
			// having taken the home.
			{ID: "menv/laatmux/%6", EnvironmentID: "menv", Server: "laatmux", Session: "kit/old", Agent: "claude", Managed: true, WorktreeID: lost.ID, Cwd: lost.Root},
			// A session new made below sub's root under the name sub's
			// shell session would have, its agent the checkout's not.
			{ID: "menv/laatmux/%7", EnvironmentID: "menv", Server: "laatmux", Session: "sub/main", Agent: "claude", Managed: true, Cwd: "/r/sub/x"},
		}}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("hosts:\n  - name: mac\n    repos: /r\n    worktrees: /w\nagents:\n  claude: {cmd: [claude]}\nrepos:\n  - "+src+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", cfgPath)
	requests := fakeNew(t, "menv", []string{protocol.CapStatus, protocol.CapMerged, protocol.CapCheckouts, protocol.CapNew}, &snap, inUse("lib/dev", "app/main", "sub/main"))
	jump := func(target string) (out, cmds, req string, err error) {
		t.Helper()
		os.Remove(log)
		f, ferr := os.Create(filepath.Join(t.TempDir(), "stdout"))
		if ferr != nil {
			t.Fatal(ferr)
		}
		was := os.Stdout
		os.Stdout = f
		err = cmdJump(context.Background(), []string{target})
		os.Stdout = was
		f.Close()
		got, _ := os.ReadFile(f.Name())
		tm, _ := os.ReadFile(log)
		return string(got), string(tm), asked(requests), err
	}
	for _, k := range []struct{ target, req, made, managed string }{
		{"mac/proj/main", `proj/main /r/proj mac []`, "made session proj/main on mac, a shell at /r/proj\n", "proj/main"},
		// Made since the listing: attached, nothing said made.
		{"mac/lib/dev", `lib/dev /r/lib mac []`, "", "lib/dev"},
		// The home: nothing asked.
		{"mac/tool/main", "", "", "tool/main"},
	} {
		out, cmds, req, err := jump(k.target)
		if err != nil || req != k.req || out != k.made {
			t.Errorf("jump %s: %v, asked %q, printed %q", k.target, err, req, out)
		}
		for _, want := range []string{"new-session -d -s mac/" + k.managed + " ", "@laatmux_attach_target " + k.managed + " ", "@laatmux_workspace menv//r/", "switch-client -t =mac/" + k.managed + ":"} {
			if !strings.Contains(cmds, want) {
				t.Errorf("jump %s ran %q, want %q in it", k.target, cmds, want)
			}
		}
	}
	for _, k := range []struct{ target, req, want string }{
		{"mac/app/main", `app/main /r/app mac []`, "mac: session app/main runs in /w/x, not /r/app; name in use"},
		{"mac/sub/main", `sub/main /r/sub mac []`, "mac: session sub/main runs in /r/sub/x, not /r/sub; name in use"},
	} {
		if out, cmds, req, err := jump(k.target); err == nil || err.Error() != k.want || req != k.req || out != "" || cmds != "" {
			t.Errorf("jump to a name in use elsewhere: %v, asked %q, printed %q, tmux %q; want %q", err, req, out, cmds, k.want)
		}
	}
	var want string
	if out, cmds, req, err := jump("mac/note/main"); err != nil || req != "" || out != "" || !strings.Contains(cmds, "switch-client -t =notes:") || strings.Contains(cmds, "new-session") {
		t.Errorf("jump with an agent in a plain session: %v, asked %q, printed %q, tmux %q", err, req, out, cmds)
	}
	out, cmds, req, err := jump("mac/kit/main")
	if err != nil || req != "" || out != "" {
		t.Errorf("jump with the home lost: %v, asked %q, printed %q", err, req, out)
	}
	for _, want := range []string{"new-session -d -s mac/kit/main ", "@laatmux_workspace menv//r/kit ", "@laatmux_attach_target kit/old ", "switch-client -t =mac/kit/main:"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("jump with the home lost ran %q, want %q in it", cmds, want)
		}
	}
	requests = fakeNew(t, "menv", []string{protocol.CapStatus, protocol.CapMerged, protocol.CapCheckouts}, &snap, nil)
	want = "mac/proj/main is the main checkout, and no agent runs in it"
	if out, cmds, req, err := jump("mac/proj/main"); err == nil || err.Error() != want || req != "" || out != "" || cmds != "" {
		t.Errorf("jump with no new in the hello: %v, asked %q, printed %q, tmux %q; want %q", err, req, out, cmds, want)
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

// A checkout the config does not list, cloned by hand as my.repo, is
// labelled my_repo in the host's listing, and the workspace session a
// jump to its worktree makes once the home is lost, named after the
// worktree, is mac/my_repo/b, which tmux stores as given: the jump finds
// it again. Named after the directory, mac/my.repo/b, it was stored as
// mac/my_repo/b by tmux before 3.7, the tags in new-session's own
// sequence found no session, and the jump failed and left the session
// untagged.
func TestEnsureUnlistedDottedCheckout(t *testing.T) {
	isolatedDefault(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dirs := config.Dirs{Repos: []string{filepath.Join(base, "repos")}, Worktrees: filepath.Join(base, "worktrees")}
	checkout := filepath.Join(dirs.Repos[0], "my.repo")
	root := dirs.Worktree("my_repo", "b")
	for _, args := range [][]string{
		{"init", "-q", checkout},
		{"-C", checkout, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", checkout, "remote", "add", "origin", "https://example.com/o/my.repo"},
		{"-C", checkout, "worktree", "add", "-q", "-b", "b", root},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	recs, err := worktree.New(dirs, nil).List(ctx)
	if err != nil || len(recs) != 1 || recs[0].Repo != "my_repo" {
		t.Fatalf("list %+v %v", recs, err)
	}
	h := config.Host{Host: peer.Host{Name: "mac"}}
	cfg := config.Config{Hosts: []config.Host{h}}
	w := protocol.Worktree{ID: "env/worktree/" + root, EnvironmentID: "env", Repo: recs[0].Repo, Source: recs[0].Source, Branch: recs[0].Branch, Root: recs[0].Root}
	a := protocol.Agent{ID: "env/laatmux/%1", EnvironmentID: "env", Server: "laatmux", Session: "m1", WorktreeID: w.ID}
	spec, _, err := rowSpec(cfg, h, rows.Row{Host: "mac", Worktree: &w, Agent: &a})
	if err != nil || spec.Name != "mac/my_repo/b" {
		t.Fatalf("spec %+v %v", spec, err)
	}
	name, created, err := workspace.Ensure(ctx, spec)
	if err != nil || !created || name != spec.Name {
		t.Fatalf("ensure: %q %v %v", name, created, err)
	}
	if _, err := workspace.Server.Run(ctx, "has-session", "-t", "="+name+":"); err != nil {
		t.Fatalf("has-session %q: %v", name, err)
	}
	if name, created, err := workspace.Ensure(ctx, spec); err != nil || created || name != spec.Name {
		t.Fatalf("ensure again: %q %v %v", name, created, err)
	}
}

// A session on a host made as a\b or with a tab, whose name tmux lists
// escaped, is attached from a local session made under the name the
// jump computes, which has-session finds by that name, tagged with the
// session's name as the host lists it, whose attach pane becomes a
// client of the session, and found by the tag again. An
// agent's row, a pane in it, a jump by the session's name and a
// worktree whose home it is name the local session alike. The host is
// this machine, its managed server as isolated as the default one, and
// its session names are read as the daemon reads them. c$xd is listed
// as c\$xd on tmux 3.2 to 3.4, which tmux 3.4 prints as c\\$xd and
// ListPanes reads as stored, and its local name then needs the $
// encoded too; its attach tag reads back as written, through Query.
// h\##{x} is stored, and listed, as h\\#{x}: its local name keeps the
// #, which new-session is given as FormatLiteral writes it. a.b, which
// tmux 3.7 keeps, has its local name with the . encoded, mac/a%2eb,
// which every tmux stores as given, where mac/a.b was refused as a
// plain attachment's name; the attach target =a.b: and the attach tag
// have it as listed. A tmux before 3.7 stores it as a_b, and it is left
// out there.
func TestEnsureListedHostSessionName(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	host := tmux.LaatmuxServer
	t.Cleanup(func() { _, _ = host.Run(context.Background(), "kill-server") })
	made := 0
	for i, name := range []string{`a\b`, "tab\tx", "c$xd", `h\##{x}`, "a.b"} {
		args := []string{"new-session", "-d", "-s", name, "-P", "-F", "#{session_id}" + tmux.Sep + "#{session_name}", "sleep 1000"}
		if i == 0 {
			args = append([]string{"-f", "/dev/null"}, args...)
		}
		out, err := host.Run(ctx, args...)
		if err != nil {
			// tmux 3.7 refuses a control byte in a session name, so a
			// host there has no session with a tab.
			if name == "tab\tx" && strings.Contains(err.Error(), "invalid session name") {
				t.Logf("%q: %v", name, err)
				continue
			}
			t.Fatal(err)
		}
		if id, stored, _ := strings.Cut(strings.TrimSpace(string(out)), tmux.Sep); name == "a.b" && stored != name {
			t.Logf("%q stored as %q, by a tmux before 3.7", name, stored)
			if _, err := host.Run(ctx, "kill-session", "-t", id); err != nil {
				t.Fatal(err)
			}
			continue
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
		// The attach pane becomes a client of the session on the host.
		var clients string
		for i := 0; i < 250 && clients == ""; i++ {
			out, _ := host.Run(ctx, "list-clients", "-t", p.ID, "-F", "#{client_name}")
			clients = strings.TrimSpace(string(out))
			if clients == "" {
				time.Sleep(20 * time.Millisecond)
			}
		}
		if clients == "" {
			t.Errorf("%q: %s attached no client to it", listed, name)
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
	bare := &rows.Row{Kind: rows.KindWorktree, Depth: 1, Host: "vm", Worktree: &lost}
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
		// A homeless line with no agent, the session named after it: its
		// workspace session, attached to that session.
		{bare, rows.Row{Kind: rows.KindAgent, Worktree: other, Agent: a}, paneTarget{"laatmux", "laatmux/x", "%1"}, "vm/laatmux/x", "laatmux/x", "venv//r/x"},
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
	// though second; a homeless line with no agent, whose Home is "",
	// which the session it is named after finds, laatmux/d, and laatmux/h
	// over the homeless line g whose root agent is in it, though second:
	// the session add made for it is still its home for the viewer; and
	// none for the session homeless c is named after, laatmux/c, which
	// is not its Home, laatmux/x-2.
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
		homeless("g", "laatmux/h"), homeless("h", ""),
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
		{"vm", "laatmux/d", "venv/worktree//r/d", task}, {"vm", "laatmux/h", "venv/worktree//r/h", task}, {"vm", "laatmux/c", "", task},
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
