package rows

import (
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/workspace"
)

// The join: worktrees pair with the agent in the session the record
// names, leftovers are listed on their own, local sessions join by key
// or attach tag, and stale sessions are those gone from a host that can
// say so.
func TestBuild(t *testing.T) {
	now := time.Now()
	in := Input{
		Hosts: []Host{
			{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true},
			{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true},
			{Name: "box", EnvironmentID: "benv", Error: "unreachable"},
			{Name: "slow", EnvironmentID: "senv", Connected: true, Worktrees: true},
			{Name: "old", EnvironmentID: "oenv", Connected: true, Listed: true},
		},
		Agents: []protocol.Agent{
			{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/fix", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, Title: "fixing"},
			{ID: "venv/laatmux/%2", EnvironmentID: "venv", Session: "proj/old", Agent: "codex", Activity: protocol.Idle, ActivityAt: now.Add(-time.Hour), Liveness: protocol.Alive, Managed: true},
			{ID: "venv/laatmux/%3", EnvironmentID: "venv", Session: "scratch", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
			{ID: "venv/laatmux/%5", EnvironmentID: "venv", Session: "proj/dead", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Gone, Managed: true},
			{ID: "menv/default/%4", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now, Liveness: protocol.Alive},
			{ID: "venv/default/%6", EnvironmentID: "venv", Server: "default", Session: "remote-notes", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive},
			{ID: "benv/laatmux/%7", EnvironmentID: "benv", Session: "proj/down", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
		},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//r/fix", EnvironmentID: "venv", Repo: "proj", Branch: "fix", Root: "/r/fix", Session: "proj/fix"},
			{ID: "venv/worktree//r/old", EnvironmentID: "venv", Repo: "proj", Branch: "old", Root: "/r/old", Session: "proj/old"},
			{ID: "venv/worktree//r/bare", EnvironmentID: "venv", Repo: "proj", Branch: "bare", Root: "/r/bare"},
			{ID: "venv/worktree//r/shell", EnvironmentID: "venv", Repo: "proj", Branch: "shell", Root: "/r/shell", Session: "proj/shell"},
			{ID: "venv/worktree//r/dead", EnvironmentID: "venv", Repo: "proj", Branch: "dead", Root: "/r/dead", Session: "proj/dead"},
			{ID: "venv/worktree//r/det", EnvironmentID: "venv", Repo: "proj", Branch: "", Root: "/r/det"},
			{ID: "benv/worktree//r/down", EnvironmentID: "benv", Repo: "proj", Branch: "down", Root: "/r/down", Session: "proj/down"},
		},
		Locals: []workspace.Local{
			{Name: "vm/proj/fix", Key: "venv//r/fix", Host: "vm"},
			{Name: "vm/proj/old", Key: "venv//r/old", Host: "vm", Settled: true},
			{Name: "vm/proj/gone", Key: "venv//r/gone", Host: "vm"},
			{Name: "vm/scratch", Attach: "vm/scratch", Host: "vm"},
			{Name: "box/proj/x", Key: "benv//r/x", Host: "box"},
			{Name: "slow/proj/y", Key: "senv//r/y", Host: "slow"},
			{Name: "old/proj/z", Key: "oenv//r/z", Host: "old"},
			{Name: "notes"},
		},
		Current: "vm/proj/fix",
	}
	got := Build(in)
	byName := map[string]Row{}
	for _, r := range got.All() {
		byName[r.Name] = r
	}
	names := func(rs []Row) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return strings.Join(out, " ")
	}
	// Order: blocked, working, idle (most recent first), then rows with
	// no live agent by host then name.
	if want := "notes proj/down proj/fix proj/dead remote-notes scratch proj (detached) /r/det proj/bare proj/shell"; names(got.Main) != want {
		t.Errorf("main = %q\nwant  %q", names(got.Main), want)
	}
	if want := "proj/old"; names(got.Settled) != want {
		t.Errorf("settled = %q, want %q", names(got.Settled), want)
	}
	if want := "vm/proj/gone"; names(got.Stale) != want {
		t.Errorf("stale = %q, want %q: a host down, unlisted or without worktrees says nothing", names(got.Stale), want)
	}
	fix := byName["proj/fix"]
	if fix.Agent == nil || fix.Agent.Title != "fixing" || fix.Local == nil || !fix.Current || fix.Dim || fix.Host != "vm" || fix.HostDown {
		t.Errorf("worktree with agent and local session: %+v", fix)
	}
	if r := byName["proj/old"]; !r.Settled || !r.Dim || r.Agent == nil {
		t.Errorf("settled: %+v", r)
	}
	if r := byName["proj/bare"]; r.Agent != nil || r.State() != "no session" || !r.Dim {
		t.Errorf("worktree without session: %+v", r)
	}
	if r := byName["proj/shell"]; r.Agent != nil || r.State() != "no agent" || !r.Dim {
		t.Errorf("worktree with a session but no agent: %+v", r)
	}
	if r := byName["proj/dead"]; r.Agent == nil || !r.Dim {
		t.Errorf("gone agent not dim: %+v", r)
	}
	if r := byName["scratch"]; r.Worktree != nil || r.Local == nil || r.Local.Attach != "vm/scratch" {
		t.Errorf("managed agent without worktree joins its attachment: %+v", r)
	}
	if r := byName["notes"]; r.Local == nil || r.Local.Name != "notes" || r.Dim {
		t.Errorf("observed agent on the local default server is a local session: %+v", r)
	}
	if r := byName["remote-notes"]; r.Local != nil {
		t.Errorf("observed agent on a remote default server has no local session: %+v", r)
	}
	if r := byName["proj/down"]; !r.HostDown || !r.Dim {
		t.Errorf("host down not dim: %+v", r)
	}
	if r := byName["vm/proj/gone"]; !r.Stale || !r.Dim || r.State() != "no worktree" || r.Local == nil {
		t.Errorf("stale: %+v", r)
	}
	if r := byName["proj (detached) /r/det"]; r.Worktree == nil {
		t.Errorf("detached worktree: %+v", r)
	}
	if got.Main[0].Mark() != "!" || fix.Mark() != "*" || byName["scratch"].Mark() != "-" || byName["proj/bare"].Mark() != " " {
		t.Error("marks")
	}
	if byName["proj/bare"].AgentName() != "" || fix.AgentName() != "claude" {
		t.Error("agent names")
	}
}

// A record no host record claims has no host and counts as down; two
// hosts with one environment id attribute to the first by name.
func TestBuildAttribution(t *testing.T) {
	got := Build(Input{
		Hosts:     []Host{{Name: "b", EnvironmentID: "e", Connected: true, Listed: true}, {Name: "a", EnvironmentID: "e", Connected: true, Listed: true}},
		Worktrees: []protocol.Worktree{{ID: "x", EnvironmentID: "e", Repo: "p", Branch: "b", Root: "/r"}, {ID: "y", EnvironmentID: "other", Repo: "p", Branch: "c", Root: "/s"}},
	})
	if len(got.Main) != 2 || got.Main[0].Host != "" || !got.Main[0].HostDown || got.Main[1].Host != "a" {
		t.Errorf("rows = %+v", got.Main)
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Second: " 5s", 3 * time.Minute: " 3m", 26 * time.Hour: "26h", -time.Second: " 0s"} {
		if got := Ago(d); got != want {
			t.Errorf("Ago(%s) = %q, want %q", d, got, want)
		}
	}
}
