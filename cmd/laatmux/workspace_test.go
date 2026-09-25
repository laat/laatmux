package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
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
	// This machine calls the source "mine"; the host calls it "proj".
	cfg := config.Config{Repos: []config.Repo{{Source: "git@x:o/proj.git", Name: "mine"}}}
	ws := []protocol.Worktree{
		{Repo: "proj", Source: "git@x:o/proj.git", Branch: "fix/v1.2", Root: "/r/a", Session: "proj/fix/v1%2e2"},
		{Repo: "proj", Source: "git@x:o/proj.git", Branch: "", Root: "/r/detached"},
		{Repo: "proj", Source: "git@x:o/proj.git", Branch: "main", Root: "/r/b"},
	}
	if w, ok, _ := matchWorktree(ws, cfg, "proj/fix/v1.2"); !ok || w.Root != "/r/a" {
		t.Errorf("by the host's label: %+v %v", w, ok)
	}
	if w, ok, _ := matchWorktree(ws, cfg, "mine/fix/v1.2"); !ok || w.Root != "/r/a" {
		t.Errorf("by this machine's label: %+v %v", w, ok)
	}
	if w, ok, _ := matchWorktree(ws, cfg, "proj/fix/v1%2e2"); !ok || w.Root != "/r/a" {
		t.Errorf("by session name: %+v %v", w, ok)
	}
	if _, ok, _ := matchWorktree(ws, cfg, "proj/"); ok {
		t.Error("detached worktree matched by empty branch")
	}
	// A worktree detached in place keeps its session and is still reached
	// by the session's name.
	detached := []protocol.Worktree{{Repo: "proj", Source: "git@x:o/proj.git", Branch: "", Root: "/r/d", Session: "proj/was-fix"}}
	if w, ok, _ := matchWorktree(detached, cfg, "proj/was-fix"); !ok || w.Root != "/r/d" {
		t.Errorf("detached worktree by session name: %+v %v", w, ok)
	}
	// This machine's label wins over the host's when they name different
	// sources, in either record order.
	clash := []protocol.Worktree{
		{Repo: "proj", Source: "git@x:o/other.git", Branch: "fix", Root: "/r/host-label"},
		{Repo: "theirs", Source: "git@x:o/proj.git", Branch: "fix", Root: "/r/local-label"},
	}
	for _, order := range [][]protocol.Worktree{clash, {clash[1], clash[0]}} {
		if w, ok, _ := matchWorktree(order, cfg, "mine/fix"); !ok || w.Root != "/r/local-label" {
			t.Errorf("local label: %+v %v", w, ok)
		}
		if w, ok, _ := matchWorktree(order, cfg, "proj/fix"); !ok || w.Root != "/r/host-label" {
			t.Errorf("host label when this machine has none: %+v %v", w, ok)
		}
	}
	both := config.Config{Repos: []config.Repo{{Source: "git@x:o/proj.git", Name: "proj"}}}
	for _, order := range [][]protocol.Worktree{clash, {clash[1], clash[0]}} {
		if w, ok, _ := matchWorktree(order, both, "proj/fix"); !ok || w.Root != "/r/local-label" {
			t.Errorf("local label over host label: %+v %v", w, ok)
		}
	}
	if w, ok, _ := matchWorktree(ws, cfg, "proj/main"); !ok || w.Session != "" {
		t.Errorf("worktree without session: %+v %v", w, ok)
	}
	// A branch written as is wins over another branch's encoded session
	// name, in either record order.
	ambiguous := []protocol.Worktree{
		{Repo: "proj", Branch: "a.b", Root: "/r/dot", Session: "proj/a%2eb"},
		{Repo: "proj", Branch: "a%2eb", Root: "/r/pct", Session: "proj/a%252eb"},
	}
	for _, order := range [][]protocol.Worktree{ambiguous, {ambiguous[1], ambiguous[0]}} {
		if w, ok, _ := matchWorktree(order, cfg, "proj/a%2eb"); !ok || w.Root != "/r/pct" {
			t.Errorf("raw branch target: %+v %v", w, ok)
		}
		if w, ok, _ := matchWorktree(order, cfg, "proj/a.b"); !ok || w.Root != "/r/dot" {
			t.Errorf("dotted branch target: %+v %v", w, ok)
		}
	}
}

// path and rm find a record by source, whatever the host calls it, and by
// label only for a record from a daemon that carries no source.
func TestFindWorktreeBySource(t *testing.T) {
	mine := config.Repo{Source: "git@x:o/proj.git", Name: "mine"}
	other := config.Repo{Source: "git@x:o/other.git", Name: "proj"}
	ws := []protocol.Worktree{
		{Repo: "proj", Source: "git@x:o/proj.git", Branch: "fix", Root: "/r/proj"},
		{Repo: "other", Source: "git@x:o/other.git", Branch: "fix", Root: "/r/other"},
	}
	if w, ok, _ := findWorktree(ws, mine, "fix"); !ok || w.Root != "/r/proj" {
		t.Errorf("by source under another label: %+v %v", w, ok)
	}
	if w, ok, _ := findWorktree(ws, other, "fix"); !ok || w.Root != "/r/other" {
		t.Errorf("a colliding label did not win over the source: %+v %v", w, ok)
	}
	old := []protocol.Worktree{{Repo: "mine", Branch: "fix", Root: "/r/old"}}
	if w, ok, _ := findWorktree(old, mine, "fix"); !ok || w.Root != "/r/old" {
		t.Errorf("older record by label: %+v %v", w, ok)
	}
	if _, ok, _ := findWorktree(old, other, "fix"); ok {
		t.Error("older record matched a different label")
	}
	// Two clones of one repository, each with the branch, in either
	// order: an error naming both roots, not the first.
	two := []protocol.Worktree{
		{Repo: "proj", Source: "git@x:o/proj.git", Branch: "fix", Root: "/r/a"},
		{Repo: "proj2", Source: "https://x/o/proj", Branch: "fix", Root: "/r/b"},
	}
	for _, ws := range [][]protocol.Worktree{two, {two[1], two[0]}} {
		if w, ok, err := findWorktree(ws, mine, "fix"); ok || err == nil || !strings.Contains(err.Error(), "/r/a and /r/b") {
			t.Errorf("two clones: %+v %v %v", w, ok, err)
		}
	}
}

// ls pairs a worktree with the agent in its managed session, lists a
// worktree without an agent and an agent without a worktree on their own,
// moves settled workspaces to their section, and reports a local session
// whose workspace is gone from a connected host as stale.
func TestRender(t *testing.T) {
	m := newMerged()
	m.setHost("vm", hostState{Connected: true, Version: "v", EnvID: "env1", Worktrees: true})
	m.setHost("box", hostState{Error: "unreachable"})
	now := time.Now()
	m.apply("vm", protocol.Message{Type: protocol.TypeSnapshot,
		Agents: []protocol.Agent{
			{ID: "env1/laatmux/%1", EnvironmentID: "env1", Session: "proj/fix", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Managed: true, Title: "fixing"},
			{ID: "env1/laatmux/%2", EnvironmentID: "env1", Session: "proj/old", Agent: "codex", Activity: protocol.Idle, ActivityAt: now, Managed: true},
			{ID: "env1/laatmux/%3", EnvironmentID: "env1", Session: "scratch", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Managed: true},
			{ID: "env1/default/%4", EnvironmentID: "env1", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now},
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
		{Name: "slow/proj/y", Key: "env3//r/y", Host: "slow"},
		{Name: "old/proj/z", Key: "env4//r/z", Host: "old"},
	}
	// A connected host whose snapshot has not arrived yet, or whose
	// daemon publishes no worktrees, says nothing about its workspaces.
	m.setHost("slow", hostState{Connected: true, Version: "v", EnvID: "env3", Worktrees: true})
	m.setHost("old", hostState{Connected: true, Version: "v", EnvID: "env4"})
	m.apply("old", protocol.Message{Type: protocol.TypeSnapshot})
	out := m.render(locals)
	if strings.Contains(out, "slow/proj/y") || strings.Contains(out, "old/proj/z") {
		t.Errorf("workspace listed as stale without evidence:\n%s", out)
	}
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

// On the direct path a host that drops keeps its identity, so its cached
// records stay attributed to it and show as its with the host down.
func TestSetHostErrKeepsIdentity(t *testing.T) {
	m := newMerged()
	m.setHost("vm", hostState{Connected: true, Version: "v", EnvID: "env1", Worktrees: true})
	m.apply("vm", protocol.Message{Type: protocol.TypeSnapshot,
		Worktrees: []protocol.Worktree{{ID: "env1/worktree//r/x", EnvironmentID: "env1", Repo: "proj", Branch: "x", Root: "/r/x"}}})
	m.setHostErr("vm", false, "disconnected")
	out := m.render(nil)
	if !strings.Contains(out, "vm  DOWN  disconnected") || !strings.Contains(out, "proj/x") || !strings.Contains(out, "@vm (host down)") {
		t.Errorf("records lost their host:\n%s", out)
	}
	if st := m.hosts["vm"]; st.EnvID != "env1" || st.Version != "v" || st.Connected || st.Listed {
		t.Errorf("host state = %+v", st)
	}
}

// The more specific directory wins when worktrees is nested under repos.
func TestLabelUnderNested(t *testing.T) {
	cfg := config.Config{Hosts: []config.Host{{Repos: "/src", Worktrees: "/src/worktrees"}}}
	cases := map[string]string{
		"/src/worktrees/proj/topic": "proj",
		"/src/proj":                 "proj",
		"/src/proj/sub/dir":         "proj",
		"/src/worktrees":            "",
		"/elsewhere/proj":           "",
	}
	for dir, want := range cases {
		got, ok := labelUnder(cfg, dir)
		if got != want || ok != (want != "") {
			t.Errorf("labelUnder(%q) = %q, %v; want %q", dir, got, ok, want)
		}
	}
}

func TestLocalRepoArg(t *testing.T) {
	cfg := config.Config{Repos: []config.Repo{{Source: "git@x:o/proj.git", Name: "mine"}}}
	cases := []struct {
		w    protocol.Worktree
		want string
	}{
		{protocol.Worktree{Repo: "proj", Source: "git@x:o/proj.git"}, "mine"},
		{protocol.Worktree{Repo: "proj", Source: "git@x:o/unknown.git"}, "git@x:o/unknown.git"},
		{protocol.Worktree{Repo: "proj"}, "proj"},
	}
	for _, c := range cases {
		if got := localRepoArg(cfg, c.w); got != c.want {
			t.Errorf("localRepoArg(%+v) = %q, want %q", c.w, got, c.want)
		}
	}
}

// Only git's own word that there is no origin, or no repository, lets
// resolution fall back to the directory label; a git that cannot run or
// read the repository is an error.
func TestOriginOf(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if o, err := originOf(ctx, dir); err != nil || o != "" {
		t.Errorf("no repository: %q %v", o, err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if o, err := originOf(ctx, dir); err != nil || o != "" {
		t.Errorf("no origin: %q %v", o, err)
	}
	run("remote", "add", "origin", "git@x:o/proj.git")
	if o, err := originOf(ctx, dir); err != nil || o != "git@x:o/proj.git" {
		t.Errorf("origin: %q %v", o, err)
	}
	// A repository git cannot read is an error, not a missing origin.
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[core\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := originOf(ctx, dir); err == nil {
		t.Error("broken config read as no origin")
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := originOf(ctx, dir); err == nil {
		t.Error("missing git read as no origin")
	}
}

// A jump target two clones of one repository both match is an error
// naming the roots, whatever order the records came in.
func TestMatchWorktreeAmbiguous(t *testing.T) {
	cfg, err := config.Parse([]byte("repos:\n  - source: git@x:o/proj.git\n    name: mine\n"))
	if err != nil {
		t.Fatal(err)
	}
	two := []protocol.Worktree{
		{Repo: "proj", Source: "git@x:o/proj.git", Branch: "topic", Root: "/r/a", Session: "proj/topic"},
		{Repo: "proj2", Source: "https://x/o/proj", Branch: "topic", Root: "/r/b", Session: "proj2/topic"},
	}
	for _, ws := range [][]protocol.Worktree{two, {two[1], two[0]}} {
		if w, ok, err := matchWorktree(ws, cfg, "mine/topic"); ok || err == nil || !strings.Contains(err.Error(), "/r/a and /r/b") {
			t.Errorf("two clones: %+v %v %v", w, ok, err)
		}
		// The host's label or the session name tells them apart.
		if w, ok, err := matchWorktree(ws, cfg, "proj2/topic"); err != nil || !ok || w.Root != "/r/b" {
			t.Errorf("by the host's label: %+v %v %v", w, ok, err)
		}
	}
	// This machine's name is one clone's host label: the target is that
	// clone's, not a dead end.
	same, err := config.Parse([]byte("repos:\n  - source: git@x:o/proj.git\n    name: proj\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range [][]protocol.Worktree{two, {two[1], two[0]}} {
		if w, ok, err := matchWorktree(ws, same, "proj/topic"); err != nil || !ok || w.Root != "/r/a" {
			t.Errorf("local name equal to a host label: %+v %v %v", w, ok, err)
		}
	}
}
