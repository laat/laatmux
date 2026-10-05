package main

import (
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/view"
)

// ls's host lines and the dashboard's header: the local daemon down
// first, then a line per host by name, connected with its version,
// connected with the snapshot pending, down with its error and the
// reconnect noted, or connecting; a failed session listing last. The
// dashboard leaves a connected and listed host out.
func TestRenderHosts(t *testing.T) {
	m := merged.New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot, SessionsError: "tmux: permission denied",
		Hosts: []protocol.HostStatus{
			{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Version: "v1"},
			{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Version: "v2"},
			{Name: "box", SSH: "box", EnvironmentID: "benv", Error: "disconnected", Reconnecting: true},
			{Name: "new", SSH: "new"},
		},
		Sessions:  []protocol.Session{{Name: "mac/proj/x", Key: "menv//w/proj/x", Host: "mac", Settled: true}},
		Worktrees: []protocol.Worktree{{ID: "menv/worktree//w/proj/x", EnvironmentID: "menv", Repo: "proj", Branch: "x", Root: "/w/proj/x", Session: "proj/x"}},
	})
	out := render(m.Status(""))
	want := "box  DOWN  disconnected (reconnecting)\nmac  connected  v2\nnew  connecting\nvm  connected  v1  (snapshot pending)\n\nproj\n"
	if !strings.HasPrefix(out, want) || !strings.HasSuffix(out, "\nlocal sessions not listed: tmux: permission denied\n") {
		t.Errorf("render:\n%s", out)
	}
	for _, s := range []string{"x (mac)", "settled"} {
		if !strings.Contains(out, s) {
			t.Errorf("render lacks %q:\n%s", s, out)
		}
	}
	v := &view.Model{}
	fill(v, m.Status(""))
	var header []string
	for _, h := range v.Header {
		header = append(header, h.Text)
	}
	if got := strings.Join(header, "|"); got != "box  DOWN  disconnected (reconnecting)|new  connecting|vm  connected  (snapshot pending)|local sessions not listed: tmux: permission denied" {
		t.Errorf("header: %s", got)
	}
	if !v.Header[0].Down || v.Header[1].Down || v.Loading {
		t.Errorf("header flags: %+v, loading %v", v.Header, v.Loading)
	}
	// Before the snapshot the dashboard is loading.
	v, m2 := &view.Model{}, merged.New()
	fill(v, m2.Status(""))
	if !v.Loading || len(v.Header) != 0 {
		t.Errorf("before a snapshot: loading %v, header %v", v.Loading, v.Header)
	}
	// A watch or dashboard whose daemon went away says so first, over
	// the hosts it last saw.
	s := m.Status("")
	s.DaemonErr = "disconnected; reconnecting"
	if out := render(s); !strings.HasPrefix(out, "local daemon  DOWN  disconnected; reconnecting\nbox  DOWN") {
		t.Errorf("render with the daemon down:\n%s", out)
	}
	fill(v, s)
	if len(v.Header) != 5 || v.Header[0].Text != "local daemon  DOWN  disconnected; reconnecting" || !v.Header[0].Down || v.Header[1].Text != "box  DOWN  disconnected (reconnecting)" {
		t.Errorf("header with the daemon down: %+v", v.Header)
	}
}

// A one-shot client that gave up on a host says so in its row, the
// reconnect note gone; an orphaned local session is judged only against
// a host that is connected and listed.
func TestRenderTimedOutAndOrphaned(t *testing.T) {
	m := merged.New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts: []protocol.HostStatus{
			{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Capabilities: []string{"status", "worktrees"}},
			{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Capabilities: []string{"status", "worktrees"}},
			{Name: "box", SSH: "box", EnvironmentID: "benv", Error: "disconnected", Reconnecting: true, Capabilities: []string{"status", "worktrees"}},
		},
		Sessions: []protocol.Session{
			{Name: "mac/proj/gone", Key: "menv//w/proj/gone", Host: "mac"},
			{Name: "vm/proj/maybe", Key: "venv//r/proj/maybe", Host: "vm"},
		},
	})
	m.TimedOut(m.Waiting(), 20*time.Second)
	out := render(m.Status(""))
	if !strings.Contains(out, "vm  DOWN  no snapshot after 20s\n") || !strings.Contains(out, "box  DOWN  no snapshot after 20s\n") || strings.Contains(out, "reconnecting") {
		t.Errorf("timed out hosts not marked:\n%s", out)
	}
	if !strings.Contains(out, "mac/proj/gone") || strings.Contains(out, "vm/proj/maybe") {
		t.Errorf("orphaned judged wrongly:\n%s", out)
	}
}

// ls prints a pending task where its worktree row would be, with its
// state and detail, and hides the worktree row behind it; the dashboard
// gets the handoffs with the rows.
func TestMergedPendingRows(t *testing.T) {
	m := merged.New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts:     []protocol.HostStatus{{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Capabilities: []string{"status", "worktrees"}}},
		Worktrees: []protocol.Worktree{{ID: "venv/worktree//w/proj/fix", EnvironmentID: "venv", Repo: "proj", Branch: "fix", Root: "/w/proj/fix", Session: "proj/fix"}},
		Pendings: []protocol.Pending{{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "fix", Root: "/w/proj/fix",
			Taken: true, Reachable: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, Error: "not ready"}},
		Handoffs: []protocol.Handoff{{ID: "add-0", ReplacedBy: "venv/worktree//w/proj/old"}},
	})
	out := render(m.Status(""))
	if !strings.Contains(out, "! prompt not delivered") || !strings.Contains(out, "fix (vm)") || !strings.Contains(out, "not ready") || strings.Contains(out, "no agent") {
		t.Fatalf("render:\n%s", out)
	}
	v := &view.Model{}
	fill(v, m.Status(""))
	if len(v.Rows.Main) != 1 || v.Rows.Main[0].ID() != "add-1" || v.Handoffs["add-0"] != "venv/worktree//w/proj/old" {
		t.Fatalf("fill: %d rows, handoffs %v", len(v.Rows.Main), v.Handoffs)
	}
}

// tasks lists the pending records, then the tasks that handed over,
// named by their worktree when it is listed; handed-over tasks alone
// are listed too, and a record whose host is not in the stream says so.
func TestTaskReport(t *testing.T) {
	m := merged.New()
	if got := taskReport(m.Status("")); got != "no pending tasks\n" {
		t.Fatalf("empty: %q", got)
	}
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts:     []protocol.HostStatus{{Name: "vm", SSH: "vm", EnvironmentID: "venv"}},
		Worktrees: []protocol.Worktree{{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/w/a"}},
		Handoffs:  []protocol.Handoff{{ID: "t1", ReplacedBy: "venv/worktree//w/a"}, {ID: "t2", ReplacedBy: "venv/worktree//w/gone"}},
	})
	got := taskReport(m.Status(""))
	want := "t1  handed over to proj/a on vm; laatmux tasks show t1 prints its prompt, tasks dismiss drops it\n" +
		"t2  handed over to venv/worktree//w/gone; laatmux tasks show t2 prints its prompt, tasks dismiss drops it\n"
	if got != want {
		t.Fatalf("handed over only:\n%s\nwant\n%s", got, want)
	}
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, Pending: &protocol.Pending{ID: "p1", Host: "vm", Repo: "proj", Branch: "b", SubmittedAt: at.Add(time.Minute)}})
	m.Apply(protocol.Message{Type: protocol.TypeUpsert, Pending: &protocol.Pending{ID: "p0", Host: "old", Repo: "proj", Branch: "c", SubmittedAt: at}})
	got = taskReport(m.Status(""))
	lines := strings.SplitAfter(got, "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "p0  proj/c on old  "+at.Format(time.DateTime)+"  ") || !strings.HasPrefix(lines[1], "p1  proj/b on vm  ") || !strings.HasSuffix(got, want) {
		t.Fatalf("with pending records:\n%s", got)
	}
	if !strings.HasSuffix(lines[0], "  host removed; laatmux tasks dismiss p0 drops it\n") || !strings.HasSuffix(lines[1], "  submitted\n") {
		t.Fatalf("host configured or not:\n%s", got)
	}
}
