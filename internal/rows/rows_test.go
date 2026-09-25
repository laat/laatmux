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
	if r := byName["vm/proj/gone"]; !r.Stale || !r.Dim || r.State() != "no worktree" || r.Local == nil || r.Host != "vm" {
		t.Errorf("stale: %+v", r)
	}
	// A stale session tagged with a host's old name is the host's that
	// answers for its environment id now.
	renamed := Build(Input{
		Hosts:  []Host{{Name: "box", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Locals: []workspace.Local{{Name: "vm/proj/gone", Key: "venv//r/gone", Host: "vm"}},
	})
	if len(renamed.Stale) != 1 || renamed.Stale[0].Host != "box" || renamed.Stale[0].HostDown {
		t.Errorf("stale row after a host rename: %+v", renamed.Stale)
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

// The join is by environment id: two hosts that are down keep their
// records paired with their own agents, and the same session name on
// two hosts never crosses.
func TestBuildJoinByEnvironment(t *testing.T) {
	now := time.Now()
	got := Build(Input{
		Hosts: []Host{{Name: "a", EnvironmentID: "aenv", Error: "down"}, {Name: "b", EnvironmentID: "benv", Error: "down"}},
		Agents: []protocol.Agent{
			{ID: "aenv/laatmux/%1", EnvironmentID: "aenv", Session: "proj/x", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
			{ID: "benv/laatmux/%1", EnvironmentID: "benv", Session: "proj/x", Agent: "codex", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
		},
		Worktrees: []protocol.Worktree{
			{ID: "aenv/worktree//r/x", EnvironmentID: "aenv", Repo: "proj", Branch: "x", Root: "/r/x", Session: "proj/x"},
			{ID: "benv/worktree//r/x", EnvironmentID: "benv", Repo: "proj", Branch: "x", Root: "/r/x", Session: "proj/x"},
		},
	})
	if len(got.Main) != 2 {
		t.Fatalf("rows = %+v", got.Main)
	}
	for _, r := range got.Main {
		if r.Agent == nil || r.Agent.EnvironmentID != r.Worktree.EnvironmentID || !r.HostDown || !r.Dim {
			t.Errorf("row %+v paired with %+v", r.Worktree, r.Agent)
		}
	}
	if got.Main[0].ID() == got.Main[1].ID() {
		t.Error("ids collide")
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Second: " 5s", 3 * time.Minute: " 3m", 26 * time.Hour: "26h", -time.Second: " 0s"} {
		if got := Ago(d); got != want {
			t.Errorf("Ago(%s) = %q, want %q", d, got, want)
		}
	}
}

// Pending tasks: first in the main group, newest first; a task stands
// for the worktree row at its root, which is not drawn while any task
// for it stands, and takes that row's agent and local session; a task
// whose host is gone from the config says so first.
func TestBuildPending(t *testing.T) {
	now := time.Now()
	in := Input{
		Hosts: []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Agents: []protocol.Agent{
			{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/task", Cwd: "/r/task", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
			{ID: "venv/laatmux/%2", EnvironmentID: "venv", Session: "proj/other", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
		},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//r/task", EnvironmentID: "venv", Repo: "proj", Branch: "task", Root: "/r/task", Session: "proj/task"},
			{ID: "venv/worktree//r/other", EnvironmentID: "venv", Repo: "proj", Branch: "other", Root: "/r/other", Session: "proj/other"},
		},
		Locals: []workspace.Local{{Name: "vm/proj/task", Key: "venv//r/task", Host: "vm", Settled: true}},
		Pendings: []protocol.Pending{
			{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "task", Root: "/r/task", Session: "proj/task", Taken: true, Reachable: true,
				Done: true, OK: true, Prompt: protocol.DeliveryDelivered, SubmittedAt: now.Add(-time.Minute)},
			{ID: "add-2", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "task", Root: "/r/task", Taken: true, Reachable: true,
				Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, Error: "the pane was not ready", SubmittedAt: now.Add(-2 * time.Minute)},
			{ID: "add-3", Host: "vm", Repo: "proj", Branch: "new", Taken: true, Reachable: true, Stage: protocol.StageFetch, SubmittedAt: now},
			{ID: "add-4", Host: "gone", Repo: "proj", Branch: "lost", SubmittedAt: now.Add(-time.Hour)},
		},
		Current: "vm/proj/task",
	}
	got := Build(in)
	var ids []string
	for _, r := range got.Main {
		ids = append(ids, r.ID())
	}
	want := "add-3 add-1 add-2 add-4 venv/worktree//r/other"
	if strings.Join(ids, " ") != want {
		t.Fatalf("main %q, want %q", strings.Join(ids, " "), want)
	}
	if len(got.Settled)+len(got.Stale) != 0 {
		t.Fatalf("settled %d stale %d: the settled worktree hides behind its tasks", len(got.Settled), len(got.Stale))
	}
	byID := map[string]Row{}
	for _, r := range got.Main {
		byID[r.ID()] = r
	}
	// Both tasks for the one branch stand for its worktree row, carry
	// its agent and session, and alias it.
	for _, id := range []string{"add-1", "add-2"} {
		r := byID[id]
		if r.Alias() != "venv/worktree//r/task" || r.Worktree == nil || r.Agent == nil || r.Local == nil || !r.Current || r.Settled {
			t.Fatalf("%s: alias %q worktree %v agent %v local %v current %v settled %v", id, r.Alias(), r.Worktree, r.Agent, r.Local, r.Current, r.Settled)
		}
	}
	// Running and complete: the spinner's mark, not dim; the ones that
	// need the user are dim with "!".
	for _, c := range []struct {
		id, mark, state, detail string
		dim                     bool
	}{
		{"add-1", "*", "done, awaiting the listing", "", false},
		{"add-2", "!", "prompt not delivered", "the pane was not ready", true},
		{"add-3", "*", "adding: fetch", "", false},
		{"add-4", "!", "host removed", "", true},
	} {
		r := byID[c.id]
		if r.Mark() != c.mark || r.State() != c.state || r.Detail() != c.detail || r.Dim != c.dim || r.Name == "" {
			t.Errorf("%s: mark %q state %q detail %q dim %v", c.id, r.Mark(), r.State(), r.Detail(), r.Dim)
		}
	}
	if !byID["add-4"].Removed || byID["add-3"].Removed {
		t.Error("removed is the host's absence from the host list")
	}
	if byID["add-3"].Alias() != "" {
		t.Error("an alias before the root is known")
	}
	// The agent and the local session before the worktree listing: the
	// task that reported the session takes the agent, both tasks for
	// the root take the local session, and neither is a row of its own.
	in.Worktrees = in.Worktrees[1:]
	got = Build(in)
	for _, r := range got.All() {
		if r.Pending == nil && ((r.Agent != nil && r.Agent.Session == "proj/task") || (r.Local != nil && r.Local.Name == "vm/proj/task")) {
			t.Fatalf("the task's agent or session is a row of its own: %q stale %v", r.ID(), r.Stale)
		}
	}
	for _, r := range got.Main {
		if r.ID() == "add-1" && (r.Agent == nil || r.Agent.Session != "proj/task") {
			t.Fatalf("%s did not take the agent in its session", r.ID())
		}
		if (r.ID() == "add-1" || r.ID() == "add-2") && (r.Local == nil || !r.Current) {
			t.Fatalf("%s did not take the local session: %v current %v", r.ID(), r.Local, r.Current)
		}
	}
	// A later session that took the name, in another directory, is not
	// the task's: it stays a row of its own.
	in.Agents[0].Cwd = "/scratch"
	got = Build(in)
	own := false
	for _, r := range got.All() {
		if r.Pending == nil && r.Agent != nil && r.Agent.Session == "proj/task" {
			own = true
		}
		if r.ID() == "add-1" && r.Agent != nil {
			t.Fatal("the task took an agent in another directory")
		}
	}
	if !own {
		t.Fatal("the agent in another directory has no row")
	}
	// The host answering as another machine: the task says so and
	// needs the user, before the relay has recorded it.
	in.Agents[0].Cwd = "/r/task"
	in.Hosts[0].EnvironmentID = "wenv"
	got = Build(in)
	for _, r := range got.Main {
		if r.ID() == "add-1" && (!r.Replaced || r.State() != "host replaced" || !r.NeedsUser()) {
			t.Fatalf("replaced: %v %q needs %v", r.Replaced, r.State(), r.NeedsUser())
		}
		if r.ID() == "add-3" && r.Replaced {
			t.Fatal("a task with no environment yet is replaced")
		}
	}
}

// PendingState says where a task is in a few words, the detail or the
// reason apart.
func TestPendingState(t *testing.T) {
	for _, c := range []struct {
		p             protocol.Pending
		removed       bool
		state, detail string
	}{
		{protocol.Pending{Mismatch: "venv is now wenv"}, false, "host replaced", "venv is now wenv"},
		{protocol.Pending{Done: true, Stage: protocol.StageFetch, Error: "no such ref"}, false, "failed at fetch", "no such ref"},
		{protocol.Pending{Done: true, Error: "outcome unknown: the daemon no longer knows it"}, false, "outcome unknown", "the daemon no longer knows it"},
		{protocol.Pending{Done: true, OK: true, Gone: true}, false, "done, worktree gone", ""},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryUnknown, AttemptOpen: true, Attempt: 2}, false, "delivering the prompt", "attempt 2"},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryUnknown, Error: "sent, not seen"}, false, "prompt delivery unknown", "sent, not seen"},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, AttemptError: "recovery expired", Error: "x"}, false, "prompt not delivered", "recovery expired"},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryNone, ListingError: "git failed"}, false, "done, awaiting the listing", "git failed"},
		{protocol.Pending{Done: true, OK: true, Prompt: protocol.DeliveryNone, Listed: true}, false, "done", ""},
		{protocol.Pending{Unreachable: "ssh: timeout"}, false, "host unreachable, retrying", "ssh: timeout"},
		{protocol.Pending{}, false, "submitted", ""},
		{protocol.Pending{Taken: true, Reachable: true, Stage: protocol.StageAgent, Detail: "starting claude"}, false, "adding: agent", "starting claude"},
		{protocol.Pending{Taken: true, Reachable: true}, true, "host removed", ""},
	} {
		s, d := PendingState(c.p, c.removed)
		if s != c.state || d != c.detail {
			t.Errorf("%+v: %q %q, want %q %q", c.p, s, d, c.state, c.detail)
		}
	}
}

// A task that can no longer become the worktree row at its root does
// not stand for it: a worktree made again after one was gone, or after
// an add failed, is drawn, with its agent, beside the task's own row.
func TestPendingThatCannotStand(t *testing.T) {
	now := time.Now()
	for _, p := range []protocol.Pending{
		{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Session: "proj/b", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNone, Listed: true, Gone: true, SubmittedAt: now},
		{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Session: "proj/b", Taken: true, Done: true, Stage: protocol.StageAgent, Error: "failed at agent: new-session: exit 1", SubmittedAt: now},
	} {
		got := Build(Input{
			Hosts:     []Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
			Agents:    []protocol.Agent{{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/b", Cwd: "/r/b", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true}},
			Worktrees: []protocol.Worktree{{ID: "venv/worktree//r/b", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Session: "proj/b"}},
			Pendings:  []protocol.Pending{p},
		})
		var task, wt *Row
		for i := range got.Main {
			switch got.Main[i].ID() {
			case "add-1":
				task = &got.Main[i]
			case "venv/worktree//r/b":
				wt = &got.Main[i]
			}
		}
		if task == nil || wt == nil || wt.Agent == nil || task.Agent != nil || task.Worktree != nil {
			t.Fatalf("%s: task %+v worktree %+v", p.Error, task, wt)
		}
		if p.Error != "" && (task.State() != "failed at agent" || task.Detail() != "new-session: exit 1") {
			t.Fatalf("failed: %q %q", task.State(), task.Detail())
		}
	}
}

// A task whose host was renamed in the config is on a removed host: it
// does not stand for the worktree row the new name lists, which is
// drawn with its agent.
func TestPendingOnRenamedHost(t *testing.T) {
	now := time.Now()
	got := Build(Input{
		Hosts:     []Host{{Name: "new", EnvironmentID: "env", Connected: true, Listed: true, Worktrees: true}},
		Agents:    []protocol.Agent{{ID: "env/laatmux/%1", EnvironmentID: "env", Session: "proj/b", Cwd: "/r", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true}},
		Worktrees: []protocol.Worktree{{ID: "env/worktree//r", EnvironmentID: "env", Repo: "proj", Branch: "b", Root: "/r", Session: "proj/b"}},
		Pendings:  []protocol.Pending{{ID: "add-1", Host: "old", EnvironmentID: "env", Repo: "proj", Branch: "b", Root: "/r", Session: "proj/b", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, SubmittedAt: now}},
	})
	if len(got.Main) != 2 {
		t.Fatalf("%d rows", len(got.Main))
	}
	for _, r := range got.Main {
		if r.Pending != nil && (!r.Removed || r.Agent != nil || r.Alias() != "") {
			t.Fatalf("task: removed %v agent %v alias %q", r.Removed, r.Agent, r.Alias())
		}
		if r.Pending == nil && (r.Agent == nil || r.Host != "new") {
			t.Fatalf("worktree row: %+v", r)
		}
	}
}
