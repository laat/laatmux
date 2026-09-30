package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
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
	h := client.Host{Name: "vm", SSH: "vm"}
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
	err := checkSession(ctx, client.Host{Name: "vm", SSH: "vm"}, "x")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("preflight did not honour the deadline")
	}
	_ = errors.New
}

// Issue 3, option B: only the managed server is attached; the local default
// server is switched to; a remote default server or any other server is
// refused as unmanaged.
func TestJumpMode(t *testing.T) {
	mac := client.Host{Name: "mac"}
	vm := client.Host{Name: "vm", SSH: "vm"}
	cases := []struct {
		h    client.Host
		srv  string
		want jumpKind
		err  string
	}{
		{mac, "laatmux", jumpAttach, ""},
		{vm, "laatmux", jumpAttach, ""},
		{mac, "default", jumpSwitch, ""},
		{vm, "default", 0, "vm/x: on vm's default tmux server"},
		{mac, "work", 0, "tmux server work is not managed"},
		{vm, "/tmp/sock", 0, "tmux server /tmp/sock is not managed"},
	}
	for _, c := range cases {
		got, err := jumpMode(c.h, tmux.Parse(c.srv), "x")
		switch {
		case c.err == "" && (err != nil || got != c.want):
			t.Errorf("%s --server %s: got %v, %v", c.h.Name, c.srv, got, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s --server %s: got %v, want %q", c.h.Name, c.srv, err, c.err)
		}
	}
}

// A worktree row with no home session whose agent runs elsewhere is
// jumped to through the agent, not answered with the add hint: here an
// agent on a remote host's default server, which jump refuses as such.
func TestJumpRowWorktreeThroughAgent(t *testing.T) {
	cfg := config.Config{Hosts: []config.Host{{Host: client.Host{Name: "vm", SSH: "vm"}}}}
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

// A worktree row with no home session whose agent is in a managed
// session attaches through the worktree's own workspace session, keyed
// by the worktree, so it never collides with the name that session has.
func TestRowSpecWorktreeThroughManagedAgent(t *testing.T) {
	h := config.Host{Host: client.Host{Name: "vm", SSH: "vm"}}
	cfg := config.Config{Hosts: []config.Host{h}}
	w := protocol.Worktree{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/w/a", Source: "git@example.com:o/proj.git"}
	a := protocol.Agent{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/a", WorktreeID: w.ID}
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
	a2 := protocol.Agent{ID: "venv/laatmux/%2", EnvironmentID: "venv", Session: "shared", WorktreeID: w2.ID}
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

// A pane jump: a tile, an agent node and a pane node name their pane; a
// worktree line, a task and a run do not. A pane on a remote host's
// default server is refused as jump refuses it.
func TestPaneJumpRouting(t *testing.T) {
	a := &protocol.Agent{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/x", PaneID: "%1"}
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
	cfg := config.Config{Hosts: []config.Host{{Host: client.Host{Name: "vm", SSH: "vm"}}}}
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
	lostLine := &rows.Row{Kind: rows.KindWorktree, Depth: 1, Host: "vm", Worktree: &lost, Agent: &protocol.Agent{ID: "venv/laatmux/%4", Session: "laatmux/x-2", Managed: true, Cwd: "/r/x"}}
	task := &rows.Row{Kind: rows.KindTask, Depth: 1, Host: "vm", Pending: &protocol.Pending{ID: "add-1", Host: "vm", EnvironmentID: "venv", Source: "git@github.com:laat/laatmux.git", Repo: "laatmux", Branch: "z", Root: "/r/z", Session: "laatmux/z", Taken: true}}
	// A task standing for a worktree listed without a home, before the
	// add's agent is identified: the task's session is the home.
	listed := &protocol.Worktree{ID: "venv/worktree//r/z", EnvironmentID: "venv", Repo: "laatmux", Source: "git@github.com:laat/laatmux.git", Branch: "z", Root: "/r/z"}
	owner := &rows.Row{Kind: rows.KindTask, Depth: 1, Host: "vm", Worktree: listed, Pending: task.Pending}
	moved := &rows.Row{Kind: rows.KindTask, Depth: 1, Host: "vm", Worktree: listed, Pending: task.Pending, Agent: &protocol.Agent{ID: "venv/laatmux/%5", Session: "laatmux/z-2", Managed: true, Cwd: "/r/z"}}
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
	foo.Agent = &protocol.Agent{ID: "venv/laatmux/%5", Session: "laatmux/foo", Managed: true, Cwd: "/r/z"}
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
	// The line a pane's session routes by, from the tree.
	m := &view.Model{Tree: []rows.Row{
		{Kind: rows.KindRepo, Depth: 0, Node: "repo/x"}, *home, *lostLine, *task, *otherLine,
		{Kind: rows.KindWorktree, Depth: 1, Host: "mac", Worktree: &protocol.Worktree{ID: "menv/worktree//r/x", EnvironmentID: "menv", Session: "laatmux/x"}},
	}}
	for _, c := range []struct{ host, session, want string }{
		{"vm", "laatmux/x", home.Worktree.ID}, {"vm", "laatmux/x-2", lostLine.Worktree.ID}, {"vm", "laatmux/z", "add-1"},
		{"vm", "laatmux/y", other.ID}, {"vm", "scratch", ""}, {"mac", "laatmux/x", "menv/worktree//r/x"}, {"vm", "", ""},
		{"vm", "laatmux/z-2", "add-1"},
	} {
		m.Tree[3] = *task
		switch c.session {
		case "laatmux/z":
			m.Tree[3] = *owner
			c.want = "add-1"
		case "laatmux/z-2":
			m.Tree[3] = *moved
		}
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
	local := client.Host{Name: "mac"}
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
