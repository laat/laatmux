package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
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

// path and rm find a record by source, whatever the host calls it; a
// label alone, on a record without a source, matches nothing.
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
	sourceless := []protocol.Worktree{{Repo: "mine", Branch: "fix", Root: "/r/old"}}
	if w, ok, _ := findWorktree(sourceless, mine, "fix"); ok {
		t.Errorf("a record without a source matched by label: %+v", w)
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
	// A root with an ESC in it is named quoted.
	esc := []protocol.Worktree{two[0], two[1]}
	esc[0].Root = "/r/a\x1b]0;x\x07"
	if _, _, err := findWorktree(esc, mine, "fix"); err == nil || !strings.Contains(err.Error(), strconv.Quote(esc[0].Root)+" and /r/b") {
		t.Errorf("two clones, a root with an ESC: %v", err)
	}
	// A branch with a C1 control character, which git takes, is named
	// quoted with its repository.
	c1 := []protocol.Worktree{two[0], two[1]}
	c1[0].Branch, c1[1].Branch = "f\u009b31m", "f\u009b31m"
	if _, _, err := findWorktree(c1, mine, "f\u009b31m"); err == nil || !strings.HasPrefix(err.Error(), strconv.Quote("mine/f\u009b31m")+" has worktrees at /r/a and /r/b") {
		t.Errorf("two clones, a branch with a C1 control: %v", err)
	}
	// run's and path's error for a branch with no worktree, and add's
	// ready line, quote it too.
	if err := noWorktree(mine, "f\u009b31m", "vm"); err.Error() != "no worktree for "+strconv.Quote("mine/f\u009b31m")+" on vm" {
		t.Errorf("no worktree: %v", err)
	}
	res := command.Added{Branch: "f\u009b31m", Root: "/w/mine/f\u009b31m", Managed: "mine/f%c2%9b31m"}
	if got, want := readyLine("mine", res), strconv.Quote("mine/f\u009b31m")+" ready: "+strconv.Quote(res.Root)+", session mine/f%c2%9b31m"; got != want {
		t.Errorf("ready line %q, want %q", got, want)
	}
}

// ls pairs a worktree with the agent in its managed session, lists a
// worktree without an agent and an agent without a worktree on their own,
// marks settled workspaces, and reports a local session whose workspace
// is gone from a connected host as orphaned; from the merged stream, as
// ls reads it.
func TestRender(t *testing.T) {
	m := merged.New()
	now := time.Now()
	hosts := []protocol.HostStatus{
		{Name: "vm", Connected: true, Listed: true, Version: "v", EnvironmentID: "env1", Capabilities: []string{protocol.CapWorktrees}},
		{Name: "box", Error: "unreachable"},
		// A connected host whose snapshot has not arrived yet, or whose
		// daemon publishes no worktrees, says nothing about its
		// workspaces.
		{Name: "slow", Connected: true, Version: "v", EnvironmentID: "env3", Capabilities: []string{protocol.CapWorktrees}},
		{Name: "old", Connected: true, Listed: true, Version: "v", EnvironmentID: "env4"},
	}
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: hosts,
		Sessions: []protocol.Session{
			{Name: "vm/proj/old", Key: "env1//r/old", Host: "vm", Settled: true},
			{Name: "vm/proj/gone", Key: "env1//r/gone", Host: "vm"},
			{Name: "box/proj/x", Key: "env2//r/x", Host: "box"},
			{Name: "slow/proj/y", Key: "env3//r/y", Host: "slow"},
			{Name: "old/proj/z", Key: "env4//r/z", Host: "old"},
		},
		Agents: []protocol.Agent{
			{ID: "env1/laatmux/%1", EnvironmentID: "env1", Server: "laatmux", Session: "proj/fix", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Managed: true, Title: "fixing"},
			{ID: "env1/laatmux/%2", EnvironmentID: "env1", Server: "laatmux", Session: "proj/old", Agent: "codex", Activity: protocol.Idle, ActivityAt: now, Managed: true},
			{ID: "env1/laatmux/%3", EnvironmentID: "env1", Server: "laatmux", Session: "scratch", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Managed: true},
			{ID: "env1/default/%4", EnvironmentID: "env1", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now},
		},
		Worktrees: []protocol.Worktree{
			{ID: "env1/worktree//r/fix", EnvironmentID: "env1", Repo: "proj", Branch: "fix", Root: "/r/fix", Session: "proj/fix"},
			{ID: "env1/worktree//r/old", EnvironmentID: "env1", Repo: "proj", Branch: "old", Root: "/r/old", Session: "proj/old"},
			{ID: "env1/worktree//r/bare", EnvironmentID: "env1", Repo: "proj", Branch: "bare", Root: "/r/bare"},
			{ID: "env1/worktree//r/shell", EnvironmentID: "env1", Repo: "proj", Branch: "shell", Root: "/r/shell", Session: "proj/shell"},
		},
	})
	out := render(m.Status(""))
	if strings.Contains(out, "slow/proj/y") || strings.Contains(out, "old/proj/z") {
		t.Errorf("workspace listed as orphaned without evidence:\n%s", out)
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
	// The tree: the repository, its worktrees with their host, and each
	// worktree's agent on the line under it.
	if find("proj") > find("fix (vm)") {
		t.Error("the repository line not over its worktrees")
	}
	fix := lines[find("fix (vm)")+1]
	if !strings.Contains(fix, "working") || !strings.Contains(fix, "claude") || !strings.Contains(fix, "fixing") {
		t.Errorf("worktree not paired with its agent: %q", fix)
	}
	if !strings.Contains(lines[find("bare (vm)")], "no session") {
		t.Errorf("worktree without agent: %q", lines[find("bare (vm)")])
	}
	if !strings.Contains(lines[find("shell (vm)")], "no agent") {
		t.Errorf("worktree with a session but no agent: %q", lines[find("shell (vm)")])
	}
	if find("scratch") < find("other sessions") {
		t.Errorf("managed agent without worktree not in other sessions: %q", lines[find("scratch")])
	}
	if !strings.Contains(lines[find("notes")], "(vm/default)") {
		t.Errorf("observed agent names its server: %q", lines[find("notes")])
	}
	if !strings.Contains(lines[find("old (vm)")], "settled") {
		t.Error("settled workspace not marked")
	}
	if !strings.Contains(lines[find("vm/proj/gone")], "worktree gone") || strings.Contains(out, "box/proj/x") {
		t.Errorf("orphaned detection wrong:\n%s", out)
	}
}

// A record whose environment no host record claims prints its host as
// ?, as the view draws it: an agent in other sessions with its server
// after it when observed, alone when managed, and a worktree line.
func TestRenderUnclaimedHost(t *testing.T) {
	m := merged.New()
	now := time.Now()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts: []protocol.HostStatus{{Name: "vm", Connected: true, Listed: true, Version: "v", EnvironmentID: "env1", Capabilities: []string{protocol.CapWorktrees}}},
		Agents: []protocol.Agent{
			{ID: "xenv/work/%4", EnvironmentID: "xenv", Server: "work", Session: "stray", Agent: "claude", Activity: protocol.Idle, ActivityAt: now},
			{ID: "xenv/laatmux/%5", EnvironmentID: "xenv", Server: "laatmux", Session: "loose", Agent: "codex", Activity: protocol.Idle, ActivityAt: now, Managed: true},
		},
		Worktrees: []protocol.Worktree{{ID: "xenv/worktree//r/lost", EnvironmentID: "xenv", Repo: "proj", Branch: "lost", Root: "/r/lost"}},
	})
	out := render(m.Status(""))
	for _, s := range []string{"stray (?/work, host down)", "loose (?, host down)", "lost (?, host down)"} {
		if !strings.Contains(out, s) {
			t.Errorf("render lacks %q:\n%s", s, out)
		}
	}
}

// A detached worktree's label, the root of a gone one, a pane's title
// and a task's last progress line are printed as tmux.Printable shows
// them: a directory can be named with a tab or an ESC, a program in the
// pane sets its title, and a setup command writes the progress line.
func TestRenderControlBytesQuoted(t *testing.T) {
	m := merged.New()
	odd, gone, title, detail := "/r/a\tb\x1b[31m", "/r/g\tone\x1b[2J", "t\x1b]0;x\x07", "out\x1b[1mput\tline"
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Pendings: []protocol.Pending{{ID: "add-z", Host: "vm", EnvironmentID: "env1", Source: "s", Repo: "proj", Branch: "z", Taken: true, Sent: true, Reachable: true, Stage: protocol.StageSetup, State: protocol.StateOutput, Detail: detail}},
		Hosts:    []protocol.HostStatus{{Name: "vm", Connected: true, Listed: true, Version: "v", EnvironmentID: "env1", Capabilities: []string{protocol.CapWorktrees}}},
		Sessions: []protocol.Session{{Name: "vm/proj/gone", Key: "env1/" + gone, Host: "vm"}},
		Agents: []protocol.Agent{
			{ID: "env1/laatmux/%1", EnvironmentID: "env1", Server: "laatmux", Session: "proj/a", Agent: "claude", Activity: protocol.Idle, ActivityAt: time.Now(), Managed: true, Title: title},
		},
		Worktrees: []protocol.Worktree{{ID: "env1/worktree/" + odd, EnvironmentID: "env1", Repo: "proj", Root: odd, Session: "proj/a"}},
	})
	out := render(m.Status(""))
	for _, want := range []string{strconv.Quote("a\tb\x1b[31m") + " (vm)", "worktree gone " + strconv.Quote(gone), strconv.Quote(title), "adding: setup  " + strconv.Quote(detail)} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "\t\x1b\x07") {
		t.Errorf("render has a raw control byte: %q", out)
	}
}

// A host down whose status upsert keeps its environment id, as the
// daemon's does: its cached records stay attributed to it and the tree
// marks them host down, and the host line says why.
func TestHostDownKeepsIdentity(t *testing.T) {
	m := merged.New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts:     []protocol.HostStatus{{Name: "vm", Connected: true, Listed: true, Version: "v", EnvironmentID: "env1", Capabilities: []string{protocol.CapWorktrees}}},
		Worktrees: []protocol.Worktree{{ID: "env1/worktree//r/x", EnvironmentID: "env1", Repo: "proj", Branch: "x", Root: "/r/x"}}})
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, HostStatus: &protocol.HostStatus{Name: "vm", Error: "disconnected", Version: "v", EnvironmentID: "env1", Capabilities: []string{protocol.CapWorktrees}}})
	out := render(m.Status(""))
	if !strings.Contains(out, "vm  DOWN  disconnected") || !strings.Contains(out, "x (vm, host down)") {
		t.Errorf("records lost their host:\n%s", out)
	}
	if st, _ := m.Status("").Host("vm"); st.EnvID != "env1" || st.Version != "v" || st.Connected || st.Listed {
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
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg := config.Config{Repos: []config.Repo{{Source: "git@x:o/proj.git", Name: "mine"}}, Agents: map[string]config.Agent{"claude": {Cmd: []string{"claude"}}}}
	cases := []struct {
		w    protocol.Worktree
		want string
	}{
		{protocol.Worktree{Repo: "proj", Source: "git@x:o/proj.git"}, "mine"},
		{protocol.Worktree{Repo: "proj", Source: "git@x:o/unknown.git"}, "git@x:o/unknown.git"},
		{protocol.Worktree{Repo: "proj"}, ""},
	}
	for _, c := range cases {
		if got := localRepoArg(cfg, c.w); got != c.want {
			t.Errorf("localRepoArg(%+v) = %q, want %q", c.w, got, c.want)
		}
	}
	// The hint for a record without a source leaves --repo to the reader
	// rather than print it empty.
	h := config.Host{Host: peer.Host{Name: "vm"}, Repos: "/r", Worktrees: "/w"}
	if got, want := addHint(cfg, h, protocol.Worktree{Repo: "proj", Branch: "fix"}), "vm/proj/fix has no managed session; laatmux add fix --repo <repo> --host vm makes one"; got != want {
		t.Errorf("addHint = %q, want %q", got, want)
	}
}

// add with a last.json it cannot parse says which file, as the add
// form does: the decoder's error alone names none. It fails there,
// before anything reaches a daemon, with --host given too, and with
// the agent given or a command in its place: add writes the file back
// after the host's work, so a file it cannot read must stop it first.
// An add that got past the file would reach the stand-in daemon, whose
// hello has no capabilities, and fail on another error rather than
// start a real daemon.
func TestAddBadLastNamesFile(t *testing.T) {
	startFakeDaemon(t, nil, nil)
	dir := os.Getenv("LAATMUX_HOME")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("hosts:\n  - name: mac\n    repos: /r\n    worktrees: /w\nagents:\n  claude: {cmd: [claude]}\nrepos:\n  - git@x:o/proj.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", cfgPath)
	path := filepath.Join(dir, "last.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "laatmux: " + path + ": " + json.Unmarshal([]byte("not json"), &home.Last{}).Error() + "\n"
	for _, args := range [][]string{
		{"b", "--repo", "proj", "--host", "mac"},
		{"b", "--repo", "proj", "--host", "mac", "--agent", "claude"},
		{"b", "--repo", "proj", "--host", "mac", "--", "true"},
	} {
		var b strings.Builder
		report(&b, cmdAdd(context.Background(), args))
		if b.String() != want {
			t.Errorf("add %v printed %q, want %q", args, b.String(), want)
		}
	}
}

// hostFor takes the last-used host without the flag, and gives the
// repository's defaults back with withLast whether or not the flag is
// given; with the flag and without withLast it gives no defaults back
// (TestExplicitHostSkipsBadLast shows it does not read the file then).
func TestHostFor(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	const src = "git@x:o/proj.git"
	cfg := config.Config{Hosts: []config.Host{
		{Host: peer.Host{Name: "mac"}, Repos: "/r", Worktrees: "/w"},
		{Host: peer.Host{Name: "vm", SSH: "vm"}, Repos: "/r", Worktrees: "/w"},
	}}
	repo := config.Repo{Name: "proj", Source: src}
	used := home.LastRepo{Host: "vm", Agent: "codex"}
	if err := home.UpdateLast(func(l *home.Last) { l.Set(src, used) }); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		flag     string
		withLast bool
		host     string
		lr       home.LastRepo
	}{
		{"", false, "vm", used},
		{"", true, "vm", used},
		{"mac", true, "mac", used},
		{"mac", false, "mac", home.LastRepo{}},
	} {
		if h, lr, err := hostFor(cfg, c.flag, repo, c.withLast); err != nil || h.Name != c.host || lr != c.lr {
			t.Errorf("hostFor(%q, %v) = %s %+v %v, want %s %+v", c.flag, c.withLast, h.Name, lr, err, c.host, c.lr)
		}
	}
}

// rm, path and run take only the host from last.json, so with --host
// given one they cannot parse does not stop them; without the flag
// they need the last-used host and say which file they could not read.
func TestExplicitHostSkipsBadLast(t *testing.T) {
	const src, root = "git@x:o/proj.git", "/w/proj/b"
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged, protocol.CapFollow, protocol.CapRm, protocol.CapRun}, func(pc *protocol.Conn, m protocol.Message) bool {
		switch m.Type {
		case protocol.TypeSubscribe:
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, Hosts: []protocol.HostStatus{
				{Name: "mac", EnvironmentID: "lenv", Connected: true, Listed: true, Capabilities: []string{protocol.CapStatus, protocol.CapWorktrees, protocol.CapRm}},
			}, Worktrees: []protocol.Worktree{{ID: "lenv/worktree/" + root, EnvironmentID: "lenv", Repo: "proj", Branch: "b", Root: root, Source: src}}})
		case protocol.TypeRm, protocol.TypeRun:
			pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Root: root})
		}
		return true
	})
	dir := os.Getenv("LAATMUX_HOME")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("hosts:\n  - name: mac\n    repos: /r\n    worktrees: /w\nrepos:\n  - "+src+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", cfgPath)
	path := filepath.Join(dir, "last.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := path + ": " + json.Unmarshal([]byte("not json"), &home.Last{}).Error()
	ctx := context.Background()
	for _, c := range []struct {
		name string
		cmd  func(context.Context, []string) error
		args []string
	}{
		{"rm", cmdRm, []string{"proj/b"}},
		{"path", cmdPath, []string{"proj/b"}},
		{"run", cmdRun, []string{"proj/b", "--", "true"}},
	} {
		withHost := append([]string{"proj/b", "--host", "mac"}, c.args[1:]...)
		if err := c.cmd(ctx, withHost); err != nil {
			t.Errorf("%s %v: %v", c.name, withHost, err)
		}
		if err := c.cmd(ctx, c.args); err == nil || err.Error() != want {
			t.Errorf("%s %v: %v, want %q", c.name, c.args, err, want)
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
	// A directory gone, with a tab and an ESC in its name, is named
	// quoted, and so is git's message that repeats it.
	gone := filepath.Join(t.TempDir(), "a\tb\x1b]0;x\x07c")
	if _, err := originOf(ctx, gone); err == nil || !strings.HasPrefix(err.Error(), strconv.Quote(gone)+": cannot read git origin: \"") || strings.ContainsFunc(err.Error(), unicode.IsControl) {
		t.Errorf("a directory gone: %v", err)
	}
	// git's message of two lines keeps them, each quoted on its own.
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "git"), []byte("#!/bin/sh\nprintf 'fatal: a\\tb\\nhint: plain\\n' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake)
	if _, err := originOf(ctx, dir); err == nil || err.Error() != dir+": cannot read git origin: "+strconv.Quote("fatal: a\tb")+"\nhint: plain" {
		t.Errorf("git's message of two lines: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := originOf(ctx, dir); err == nil {
		t.Error("missing git read as no origin")
	}
}

// git's word that a directory is in no repository is matched in English,
// so under a translated locale the directory still has no origin rather
// than an error.
func TestOriginOfLocale(t *testing.T) {
	ctx := context.Background()
	// A worktree whose repository is gone: git reads its .git file and
	// says the directory is not a repository.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+filepath.Join(dir, "gone")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")
	// This machine's git, when it has the locale and its translation.
	out, err := exec.Command("git", "-C", dir, "config", "--get", "remote.origin.url").CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 128 && !strings.Contains(string(out), "not a git repository") {
		if o, err := originOf(ctx, dir); err != nil || o != "" {
			t.Errorf("git in German, no repository: %q %v", o, err)
		}
	} else {
		t.Logf("git does not translate here: %v %q", err, out)
	}
	// A git that translates unless its locale is C, on any machine.
	bin := t.TempDir()
	script := `#!/bin/sh
case "${LC_ALL:-${LC_MESSAGES:-$LANG}}" in
C) echo "fatal: not a git repository: $2/gone" >&2 ;;
*) echo "fatal: Kein Git-Repository: $2/gone" >&2 ;;
esac
exit 128
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if o, err := originOf(ctx, dir); err != nil || o != "" {
		t.Errorf("translating git, no repository: %q %v", o, err)
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
	// A root with an ESC in it is named quoted.
	esc := []protocol.Worktree{two[0], two[1]}
	esc[0].Root = "/r/a\x1b]0;x\x07"
	if _, _, err := matchWorktree(esc, cfg, "mine/topic"); err == nil || !strings.Contains(err.Error(), strconv.Quote(esc[0].Root)+" and /r/b") {
		t.Errorf("two clones, a root with an ESC: %v", err)
	}
	// The target with a branch that has a C1 control character, which
	// git takes, is named quoted.
	c1 := []protocol.Worktree{two[0], two[1]}
	c1[0].Branch, c1[1].Branch = "t\u009b31m", "t\u009b31m"
	if _, _, err := matchWorktree(c1, cfg, "mine/t\u009b31m"); err == nil || !strings.HasPrefix(err.Error(), strconv.Quote("mine/t\u009b31m")+" matches worktrees at /r/a and /r/b") {
		t.Errorf("two clones, a branch with a C1 control: %v", err)
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

// ls and watch read the merged stream only: with the local daemon
// unable to start, here because the state directory is a file so the
// dial's start fails before anything runs, both fail at once with the
// dial's reason and what to do, and watch draws nothing. Before, ls
// listed the hosts as down and watch redrew that until stopped.
func TestLsWatchNeedTheDaemon(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("hosts:\n  - name: box\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_HOME", filepath.Join(file, "home"))
	t.Setenv("LAATMUX_CONFIG", filepath.Join(dir, "config.yaml"))
	t.Setenv("TMUX", "")
	start := time.Now()
	err := cmdLs(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "local daemon: ") || !strings.Contains(err.Error(), "not a directory") || !strings.Contains(err.Error(), "laatmux stop") || !strings.Contains(err.Error(), "daemon.log") {
		t.Fatalf("ls without a daemon: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = cmdWatch(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "local daemon: ") || ctx.Err() != nil {
		t.Fatalf("watch without a daemon: %v (context %v)", err, ctx.Err())
	}
	if time.Since(start) > time.Second {
		t.Errorf("ls and watch took %s to refuse", time.Since(start))
	}
	// A runtime record naming a live pid, this test's own, at a socket
	// nothing answers on, as a crash leaves one when the pid is reused,
	// and then with the pid holding the startup lock, as a daemon that
	// does not answer does. The log made a directory keeps the dial's
	// start from running anything. Neither names the pid nor suggests
	// a kill: a record is no proof of a daemon, and the lock is not
	// probed, laatmux stop being the one command that checks a pid is
	// the daemon's. The path is taken out of the message before
	// the pid is looked for, its random part being digits too.
	home2 := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home2, "daemon.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_HOME", home2)
	if err := home.WriteRuntime(home.Runtime{Address: "unix:" + filepath.Join(dir, "none.sock"), PID: os.Getpid(), Version: "x"}); err != nil {
		t.Fatal(err)
	}
	noPID := func(what string) {
		t.Helper()
		err := cmdLs(context.Background(), nil)
		if err == nil {
			t.Fatalf("ls %s: no error", what)
		}
		msg := strings.ReplaceAll(err.Error(), dir, "")
		if !strings.Contains(msg, "local daemon: ") || strings.Contains(msg, fmt.Sprint(os.Getpid())) || strings.Contains(msg, "kill") {
			t.Fatalf("ls %s: %v", what, err)
		}
	}
	noPID("with a stale record naming a live pid")
	lock, err := home.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	noPID("with the recorded pid holding the lock")
}
