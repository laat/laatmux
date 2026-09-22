package main

import (
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

func TestSplitRepoBranch(t *testing.T) {
	repo, branch, err := splitRepoBranch("proj/feat/x.y")
	if err != nil || repo != "proj" || branch != "feat/x.y" {
		t.Fatalf("got %q %q %v", repo, branch, err)
	}
	for _, bad := range []string{"proj", "proj/", "/x", ""} {
		if _, _, err := splitRepoBranch(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMatchWorktree(t *testing.T) {
	ws := []protocol.Worktree{
		{Repo: "proj", Branch: "fix/v1.2", Root: "/r/a", Session: "proj/fix/v1%2e2"},
		{Repo: "proj", Branch: "", Root: "/r/detached"},
		{Repo: "proj", Branch: "main", Root: "/r/b"},
	}
	if w, ok := matchWorktree(ws, "proj/fix/v1.2"); !ok || w.Root != "/r/a" {
		t.Errorf("by repo/branch: %+v %v", w, ok)
	}
	if w, ok := matchWorktree(ws, "proj/fix/v1%2e2"); !ok || w.Root != "/r/a" {
		t.Errorf("by session name: %+v %v", w, ok)
	}
	if _, ok := matchWorktree(ws, "proj/"); ok {
		t.Error("detached worktree matched by empty branch")
	}
	if w, ok := matchWorktree(ws, "proj/main"); !ok || w.Session != "" {
		t.Errorf("worktree without session: %+v %v", w, ok)
	}
}

// ls pairs a worktree with the agent in its managed session, lists a
// worktree without an agent and an agent without a worktree on their own,
// moves settled workspaces to their section, and reports a local session
// whose workspace is gone from a connected host as stale.
func TestRender(t *testing.T) {
	m := newMerged()
	m.setHost("vm", hostState{Connected: true, Version: "v", EnvID: "env1"})
	m.setHost("box", hostState{Error: "unreachable"})
	now := time.Now()
	m.apply("vm", protocol.Message{Type: protocol.TypeSnapshot,
		Agents: []protocol.Agent{
			{ID: "env1/laatmux/%1", Session: "proj/fix", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Managed: true, Title: "fixing"},
			{ID: "env1/laatmux/%2", Session: "proj/old", Agent: "codex", Activity: protocol.Idle, ActivityAt: now, Managed: true},
			{ID: "env1/laatmux/%3", Session: "scratch", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Managed: true},
			{ID: "env1/default/%4", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now},
		},
		Worktrees: []protocol.Worktree{
			{ID: "env1/worktree//r/fix", EnvironmentID: "env1", Repo: "proj", Branch: "fix", Root: "/r/fix", Session: "proj/fix"},
			{ID: "env1/worktree//r/old", EnvironmentID: "env1", Repo: "proj", Branch: "old", Root: "/r/old", Session: "proj/old"},
			{ID: "env1/worktree//r/bare", EnvironmentID: "env1", Repo: "proj", Branch: "bare", Root: "/r/bare"},
			{ID: "env1/worktree//r/shell", EnvironmentID: "env1", Repo: "proj", Branch: "shell", Root: "/r/shell", Session: "proj/shell"},
		},
	})
	locals := []workspace.Local{
		{Name: "vm/proj/old", Key: "env1//r/old", Host: "vm", Settled: true},
		{Name: "vm/proj/gone", Key: "env1//r/gone", Host: "vm"},
		{Name: "box/proj/x", Key: "env2//r/x", Host: "box"},
	}
	out := m.render(locals)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	find := func(sub string) int {
		for i, l := range lines {
			if strings.Contains(l, sub) {
				return i
			}
		}
		t.Fatalf("no line containing %q in:\n%s", sub, out)
		return -1
	}
	fix := lines[find("proj/fix")]
	if !strings.Contains(fix, "working") || !strings.Contains(fix, "claude") || !strings.Contains(fix, "fixing") {
		t.Errorf("worktree not paired with its agent: %q", fix)
	}
	if !strings.Contains(lines[find("proj/bare")], "no session") {
		t.Errorf("worktree without agent: %q", lines[find("proj/bare")])
	}
	if !strings.Contains(lines[find("proj/shell")], "no agent") {
		t.Errorf("worktree with a session but no agent: %q", lines[find("proj/shell")])
	}
	if !strings.Contains(lines[find("scratch")], "(no worktree)") {
		t.Errorf("managed agent without worktree: %q", lines[find("scratch")])
	}
	if !strings.Contains(lines[find("notes")], "@vm/default") {
		t.Errorf("observed agent names its server: %q", lines[find("notes")])
	}
	if find("settled") > find("proj/old") {
		t.Error("settled workspace listed before the settled header")
	}
	if find("proj/gone") < find("stale") || strings.Contains(out, "box/proj/x") {
		t.Errorf("stale detection wrong:\n%s", out)
	}
	if find("blocked") > find("proj/fix") {
		t.Error("blocked agent not first")
	}
}

// Progress replayed after a reconnect is printed once.
func TestStreamDedupe(t *testing.T) {
	var got []string
	f := &replayFilter{fn: func(p protocol.Message) { got = append(got, p.Detail) }}
	for _, d := range []string{"a", "b"} {
		f.pass(protocol.Message{Detail: d})
	}
	f.reset() // reconnect: the daemon replays from the start
	for _, d := range []string{"a", "b", "c"} {
		f.pass(protocol.Message{Detail: d})
	}
	if strings.Join(got, "") != "abc" {
		t.Fatalf("got %v", got)
	}
}
