package view

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/source"
	"github.com/laat/laatmux/internal/term"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fixture is a listing with one of everything: the three activities, a
// gone agent, a worktree without a session, one without an agent, a
// managed agent with no worktree, observed agents on the local and a
// remote default server, a host down, a settled and an orphaned workspace.
// It is the agent view as the dashboard builds it: the two worktrees with
// no agent and the orphaned workspace are tree lines, not tiles, and the
// settled workspace's agent is in the stale fold.
func fixture(now time.Time) rows.Rows {
	in := fixtureInput(now)
	return rows.Agents(in, rows.Tree(in))
}

func fixtureInput(now time.Time) rows.Input {
	return rows.Input{
		Hosts: []rows.Host{
			{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true},
			{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true},
			{Name: "box", EnvironmentID: "benv"},
		},
		Agents: []protocol.Agent{
			{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "laatmux/fix-ls", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now.Add(-2 * time.Minute), Liveness: protocol.Alive, Managed: true, Title: "Permission to run pnpm test in packages/api?"},
			{ID: "menv/laatmux/%2", EnvironmentID: "menv", Server: "laatmux", Session: "proj/task", Agent: "codex", Activity: protocol.Working, ActivityAt: now.Add(-8 * time.Second), Liveness: protocol.Alive, Managed: true, Title: "Editing src/api.ts"},
			{ID: "venv/laatmux/%3", EnvironmentID: "venv", Server: "laatmux", Session: "proj/other", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-time.Hour), Liveness: protocol.Alive, Managed: true, Title: "✳ Done. 日本語のタイトル that is long enough to be trimmed at the edge"},
			{ID: "venv/laatmux/%4", EnvironmentID: "venv", Server: "laatmux", Session: "proj/dead", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-3 * time.Hour), Liveness: protocol.Gone, Managed: true},
			{ID: "menv/laatmux/%5", EnvironmentID: "menv", Server: "laatmux", Session: "scratch", Agent: "", Activity: protocol.Idle, ActivityAt: now.Add(-40 * time.Second), Liveness: protocol.Alive, Managed: true, Title: "zsh"},
			{ID: "menv/default/%6", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-5 * time.Minute), Liveness: protocol.Alive, Title: "notes"},
			{ID: "venv/default/%7", EnvironmentID: "venv", Server: "default", Session: "remote-notes", Agent: "codex", Activity: protocol.Working, ActivityAt: now.Add(-time.Second), Liveness: protocol.Alive, Title: "remote"},
			{ID: "benv/laatmux/%8", EnvironmentID: "benv", Server: "laatmux", Session: "proj/down", Agent: "claude", Activity: protocol.Working, ActivityAt: now.Add(-10 * time.Minute), Liveness: protocol.Alive, Managed: true, Title: "last seen"},
			{ID: "menv/laatmux/%9", EnvironmentID: "menv", Server: "laatmux", Session: "proj/done", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-26 * time.Hour), Liveness: protocol.Alive, Managed: true, Title: "finished"},
		},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//r/fix-ls", EnvironmentID: "venv", Repo: "laatmux", Branch: "fix-ls", Root: "/r/fix-ls", Session: "laatmux/fix-ls"},
			{ID: "menv/worktree//w/task", EnvironmentID: "menv", Repo: "proj", Branch: "task", Root: "/w/task", Session: "proj/task"},
			{ID: "venv/worktree//r/other", EnvironmentID: "venv", Repo: "proj", Branch: "other", Root: "/r/other", Session: "proj/other"},
			{ID: "venv/worktree//r/dead", EnvironmentID: "venv", Repo: "proj", Branch: "dead", Root: "/r/dead", Session: "proj/dead"},
			{ID: "menv/worktree//w/spike", EnvironmentID: "menv", Repo: "proj", Branch: "spike", Root: "/w/spike"},
			{ID: "venv/worktree//r/shell", EnvironmentID: "venv", Repo: "proj", Branch: "shell", Root: "/r/shell", Session: "proj/shell"},
			{ID: "benv/worktree//r/down", EnvironmentID: "benv", Repo: "proj", Branch: "down", Root: "/r/down", Session: "proj/down"},
			{ID: "menv/worktree//w/done", EnvironmentID: "menv", Repo: "proj", Branch: "done", Root: "/w/done", Session: "proj/done"},
		},
		Locals: []protocol.Session{
			{Name: "vm/proj/other", Key: "venv//r/other", Host: "vm"},
			{Name: "mac/proj/task", Key: "menv//w/task", Host: "mac"},
			{Name: "mac/proj/done", Key: "menv//w/done", Host: "mac", Settled: true},
			{Name: "vm/proj/gone", Key: "venv//r/gone", Host: "vm"},
			{Name: "mac/scratch", Attach: "mac/scratch", Host: "mac"},
		},
		Current: "mac/proj/task",
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v; run with -update", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden (run with -update to accept):\n%s", name, got)
	}
}

func model(now time.Time) *Model {
	return &Model{Rows: fixture(now), LocalHost: "mac", Now: now, Header: []HeaderLine{{Text: "box  DOWN  ssh: connect to host box port 22: No route to host", Down: true}}}
}

// The tile layout at the sidebar's default width: each tile is the
// stripe and the icon in the status colour, the primary label and the
// time since the status changed; the secondary label and the host tag,
// dim for every host but the local one; and the cleaned title; the
// current row's label is bold in its own colour; a tile cut by the
// window's edge is drawn as far as it goes, and the rows below the
// window are counted, dim.
func TestRenderTiles(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Tiles, 35, 33
	m.Hint = "v layout  / filter  f all  q quit"
	golden(t, "tiles", Debug(m.Render()))
}

// The compact layout as the dashboard draws it, with titles under
// each row and the hidden groups expanded.
func TestRenderCompact(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Titles, m.Width, m.Height = Compact, true, 90, 30
	m.ShowHidden = true
	m.Selected = 2
	m.Hint = "enter jump  v layout  / filter  f settled  q quit"
	golden(t, "compact", Debug(m.Render()))
}

// The compact layout without titles at a narrow width still puts the
// host tag and age on the line and trims the name.
func TestRenderCompactNarrow(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Compact, 40, 8
	m.Filter = "proj"
	golden(t, "compact-narrow", Debug(m.Render()))
}

// A list taller than the screen scrolls to keep the selection visible,
// and the mouse map follows the scroll.
func TestRenderScroll(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Tiles, 35, 10
	m.Header = nil
	m.Selected = 4
	lines := m.Render()
	if len(lines) != 10 {
		t.Fatalf("%d lines, want 10", len(lines))
	}
	txt := Text(lines)
	if !strings.Contains(txt, "scratch") || strings.Contains(txt, "fix-ls") {
		t.Errorf("selection not scrolled to:\n%s", txt)
	}
	if got := m.hitRow(1, time.Time{}); got < 0 || m.Visible()[got].Row.Name == "laatmux/fix-ls" {
		t.Errorf("first line maps to row %d after scrolling", got)
	}
	// Back to the top: the scroll follows.
	m.Selected = 0
	txt = Text(m.Render())
	if !strings.HasPrefix(txt, "▌ 💬 fix-ls") {
		t.Errorf("did not scroll back:\n%s", txt)
	}
}

// Keys: movement clamps, digits pick within the group, v toggles the
// layout, / filters with Esc clearing, f expands the hidden groups, a
// click jumps to the row under it, the wheel moves the selection, and
// unknown keys are handed back.
func TestHandle(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Compact, 80, 30
	m.Render()
	n := len(m.Visible())
	m.Handle(term.Key{Rune: 'k'})
	if m.Selected != 0 {
		t.Error("k at the top moved")
	}
	m.Handle(term.Key{Rune: 'G'})
	m.Handle(term.Key{Rune: 'j'})
	if m.Selected != n-1 {
		t.Errorf("G then j = %d, want %d", m.Selected, n-1)
	}
	m.Handle(term.Key{Rune: 'g'})
	if a := m.Handle(term.Key{Rune: '3'}); a.Kind != ActionJump || m.Selection().Name != m.Visible()[2].Row.Name {
		t.Errorf("3 = %+v on %q", a, m.Selection().Name)
	}
	if a := m.Handle(term.Key{Kind: term.KeyEnter}); a.Kind != ActionJump {
		t.Errorf("enter = %+v", a)
	}
	if a := m.Handle(term.Key{Kind: term.KeyNewline}); a.Kind != ActionJump {
		t.Errorf("newline = %+v", a)
	}
	m.Handle(term.Key{Rune: 'v'})
	if m.Layout != Tiles {
		t.Error("v did not toggle")
	}
	for _, r := range "/dea" {
		m.Handle(term.Key{Rune: r})
	}
	if !m.Filtering || m.Filter != "dea" || len(m.Visible()) != 1 || m.Visible()[0].Row.Name != "proj/dead" {
		t.Errorf("filter: %q filtering=%v visible=%d", m.Filter, m.Filtering, len(m.Visible()))
	}
	m.Handle(term.Key{Kind: term.KeyBackspace})
	if m.Filter != "de" {
		t.Errorf("backspace: %q", m.Filter)
	}
	if a := m.Handle(term.Key{Kind: term.KeyEnter}); a.Kind != ActionNone || m.Filtering || m.Filter != "de" {
		t.Errorf("enter while filtering: %+v filtering=%v filter=%q", a, m.Filtering, m.Filter)
	}
	m.Handle(term.Key{Kind: term.KeyEsc})
	if m.Filter != "" || len(m.Visible()) != n {
		t.Errorf("esc did not clear the filter: %q", m.Filter)
	}
	m.Handle(term.Key{Rune: 'f'})
	// The stale fold holds one tile: the settled workspace's agent.
	if len(m.Visible()) != n+1 {
		t.Errorf("f showed %d rows, want %d", len(m.Visible()), n+1)
	}
	// The digits count the numbered rows whatever the fold: 1 is the
	// first tile from anywhere.
	m.Handle(term.Key{Rune: 'G'})
	if a := m.Handle(term.Key{Rune: '1'}); a.Kind != ActionJump || m.Selection().Name != "laatmux/fix-ls" {
		t.Errorf("1 = %+v on %q", a, m.Selection().Name)
	}
	m.Handle(term.Key{Rune: 'f'})
	m.Handle(term.Key{Rune: 'g'})
	m.Layout = Compact
	m.Render()
	if a := m.Handle(term.Key{Kind: term.KeyMouse, X: 3, Y: 1 + len(m.Header) + 2}); a.Kind != ActionJump || m.Selected != 2 {
		t.Errorf("click on the third row = %+v selected %d", a, m.Selected)
	}
	if a := m.Handle(term.Key{Kind: term.KeyMouse, X: 3, Y: 1 + len(m.Header) + n + 5}); a.Kind != ActionNone {
		t.Errorf("click below the list = %+v", a)
	}
	m.Handle(term.Key{Kind: term.KeyMouse, Wheel: 1})
	if m.Selected != 3 {
		t.Errorf("wheel down = %d", m.Selected)
	}
	if a := m.Handle(term.Key{Rune: 'x'}); a.Kind != ActionOther || a.Key.Rune != 'x' {
		t.Errorf("unknown key = %+v", a)
	}
	if a := m.Handle(term.Key{Rune: 'q'}); a.Kind != ActionQuit {
		t.Errorf("q = %+v", a)
	}
	m.Message = "hello"
	m.Handle(term.Key{Rune: 'j'})
	if m.Message != "" {
		t.Error("message survived a key")
	}
	m.Rows = rows.Rows{}
	if a := m.Handle(term.Key{Kind: term.KeyEnter}); a.Kind != ActionNone {
		t.Errorf("enter on an empty list = %+v", a)
	}
}

// A refresh that reorders or removes rows keeps the selection on the
// same row, not the same index.
func TestSetRowsKeepsSelection(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Compact, 80, 30
	m.Handle(term.Key{Rune: 'j'})
	m.Handle(term.Key{Rune: 'j'})
	was := m.Selection()
	if was.Name != "proj/task" || m.Selected != 2 {
		t.Fatalf("selected %q at %d", was.Name, m.Selected)
	}
	// The blocked agent goes idle and the other working agent goes
	// blocked: proj/task is now the first working row, index 1.
	in := fixtureInput(now)
	for i := range in.Agents {
		switch in.Agents[i].Session {
		case "laatmux/fix-ls":
			in.Agents[i].Activity = protocol.Idle
		case "remote-notes":
			in.Agents[i].Activity = protocol.Blocked
		}
	}
	m.SetRows(rows.Agents(rows.Input{}, rows.Tree(rows.Input{})))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	if got := m.Selection(); got.Name != "proj/task" || m.Selected != 1 {
		t.Errorf("after reorder: selected %q at %d, want proj/task at 1", got.Name, m.Selected)
	}
	// The row goes.
	for i := range in.Worktrees {
		if in.Worktrees[i].Branch == "task" {
			in.Worktrees = append(in.Worktrees[:i], in.Worktrees[i+1:]...)
			break
		}
	}
	for i := range in.Agents {
		if in.Agents[i].Session == "proj/task" {
			in.Agents = append(in.Agents[:i], in.Agents[i+1:]...)
			break
		}
	}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	// The row is gone: the selection is on none, not on the row that
	// took its index, so Enter jumps nowhere.
	if got := m.Selection(); got != nil || m.Selected != -1 {
		t.Errorf("after removal: %+v index %d", got, m.Selected)
	}
	if a := m.Handle(term.Key{Kind: term.KeyEnter}); a.Kind != ActionNone {
		t.Errorf("enter on no selection = %+v", a)
	}
	m.Render()
	if m.Selection() != nil {
		t.Error("a render put the selection back on a row")
	}
	// A key moves it onto a row again, the user's.
	m.Handle(term.Key{Rune: 'j'})
	if got := m.Selection(); got == nil || m.Selected != 0 {
		t.Errorf("j after removal: %+v index %d", got, m.Selected)
	}
}

func TestWidth(t *testing.T) {
	if w := width("日本"); w != 4 {
		t.Errorf("wide = %d", w)
	}
	// An emoji is two cells; a skin tone and a joiner add none, so a
	// joined sequence measures as its base emoji; the emoji variation
	// selector makes a one-cell symbol two, as terminals draw ⚠️.
	if w := width("\U0001f600"); w != 2 {
		t.Errorf("emoji = %d", w)
	}
	if w := width("\U0001f44d\U0001f3fd"); w != 2 {
		t.Errorf("emoji with skin tone = %d", w)
	}
	if w := width("\u2764\ufe0f"); w != 2 {
		t.Errorf("heart with variation selector = %d", w)
	}
	if w := width("\u26a0\ufe0f x"); w != 4 {
		t.Errorf("warning sign with variation selector and text = %d", w)
	}
	if got := fit("a\u26a0\ufe0f", 2); got != "a" {
		t.Errorf("fit cut inside a two-cell symbol = %q", got)
	}
	if got := fit("ab日本c", 4); got != "ab日" {
		t.Errorf("fit = %q", got)
	}
	if got := fit("a\x1bb", 5); got != "ab" {
		t.Errorf("control dropped: %q", got)
	}
	if l, err := ParseLayout(""); err != nil || l != Tiles {
		t.Error("ParseLayout empty")
	}
	if l, err := ParseLayout("compact"); err != nil || l != Compact {
		t.Error("ParseLayout compact")
	}
	if _, err := ParseLayout("wide"); err == nil {
		t.Error("ParseLayout accepted wide")
	}
}

// With Follow the selection is the viewer's own row wherever the sort
// puts it, and nothing when no row is that session; the first key that
// moves the selection makes it the user's, anchored as before.
func TestFollowSelection(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Compact, 80, 30
	m.Follow = true
	in := fixtureInput(now)
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	if got := m.Selection(); got == nil || got.Name != "proj/task" || !got.Current || m.Selected != 2 {
		t.Fatalf("initial: %+v at %d", got, m.Selected)
	}
	// The sort moves the viewer's row: the selection follows.
	for i := range in.Agents {
		switch in.Agents[i].Session {
		case "laatmux/fix-ls":
			in.Agents[i].Activity = protocol.Idle
		case "remote-notes":
			in.Agents[i].Activity = protocol.Blocked
		}
	}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	if got := m.Selection(); got == nil || got.Name != "proj/task" || m.Selected != 1 {
		t.Fatalf("after reorder: %+v at %d", got, m.Selected)
	}
	reversed := 0
	for _, l := range m.Render() {
		if l.Reverse {
			reversed++
		}
	}
	if reversed == 0 {
		t.Fatal("followed row not drawn selected")
	}
	// No row is the viewer's session: nothing selected, nothing drawn
	// selected, Enter does nothing; a digit counts the main group.
	in.Current = ""
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	if got := m.Selection(); got != nil || m.Selected != -1 {
		t.Fatalf("no current row: %+v at %d", got, m.Selected)
	}
	for _, l := range m.Render() {
		if l.Reverse {
			t.Fatal("a row drawn selected with nothing selected")
		}
	}
	if a := m.Handle(term.Key{Kind: term.KeyEnter}); a.Kind != ActionNone {
		t.Fatalf("Enter with nothing selected: %+v", a)
	}
	// A digit jumps to the row it counts; the selection goes on
	// following, so it is on the viewer's own row when they are back.
	if a := m.Handle(term.Key{Rune: '2'}); a.Kind != ActionJump || a.Row == nil || a.Row.Name != m.Visible()[1].Row.Name || m.Selected != -1 || !m.Follow || a.Mouse {
		t.Fatalf("digit with nothing selected: %+v at %d follow=%v", a, m.Selected, m.Follow)
	}
	// Following again, then a key: the selection is the user's and a
	// reorder keeps it on the row it was on, not on the viewer's.
	m.Follow = true
	in.Current = "mac/proj/task"
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	if m.Selected != 1 {
		t.Fatalf("following again: %d", m.Selected)
	}
	m.Handle(term.Key{Rune: 'j'})
	taken := m.Selection()
	if m.Follow || taken == nil || taken.Name == "proj/task" || m.Selected != 2 {
		t.Fatalf("after j: %+v at %d follow=%v", taken, m.Selected, m.Follow)
	}
	for i := range in.Agents {
		if in.Agents[i].Session == "laatmux/fix-ls" {
			in.Agents[i].Activity = protocol.Blocked
		}
	}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	if got := m.Selection(); got == nil || got.Name != taken.Name {
		t.Fatalf("user's selection moved: %+v, was %s", got, taken.Name)
	}
	// Without Follow the model is as it was: the first row selected.
	m = model(now)
	m.SetRows(fixture(now))
	if m.Selected != 0 {
		t.Fatalf("without follow: %d", m.Selected)
	}
}

// Following is found afresh on every read: a filter that hides the
// viewer's row selects nothing rather than another row, clearing it
// brings the row back, and a collapsed group hides it the same way. A
// move that changes nothing, up from the first row, onto the selected
// row, or on an empty list, keeps following.
func TestFollowThroughFilterAndGroups(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Compact, 80, 30
	m.Follow = true
	in := fixtureInput(now)
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	// Filter to rows that are not the viewer's: rows remain, none is
	// selected, and Enter has nothing to act on.
	m.Handle(term.Key{Rune: '/'})
	for _, r := range "notes" {
		m.Handle(term.Key{Kind: term.KeyRune, Rune: r})
	}
	m.Handle(term.Key{Kind: term.KeyEnter}) // leaves the filter typing, keeps the filter
	if vis := m.Visible(); len(vis) == 0 || m.Filter != "notes" {
		t.Fatalf("filter %q left %d rows", m.Filter, len(vis))
	}
	if got := m.Selection(); got != nil || !m.Follow {
		t.Fatalf("filtered away: %+v follow=%v", got, m.Follow)
	}
	if a := m.Handle(term.Key{Kind: term.KeyEnter}); a.Kind != ActionNone {
		t.Fatalf("Enter with the viewer's row filtered away: %+v", a)
	}
	m.Handle(term.Key{Kind: term.KeyEsc})
	if got := m.Selection(); got == nil || got.Name != "proj/task" || !m.Follow {
		t.Fatalf("filter cleared: %+v follow=%v", got, m.Follow)
	}
	// The viewer's row settled: it stays in place, dim, still followed,
	// so z can unsettle it; the groups folding changes nothing.
	for i := range in.Locals {
		if in.Locals[i].Name == "mac/proj/task" {
			in.Locals[i].Settled = true
		}
	}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	if got := m.Selection(); got == nil || got.Name != "proj/task" || !got.Settled || !m.Follow {
		t.Fatalf("settled: %+v follow=%v", got, m.Follow)
	}
	m.Handle(term.Key{Rune: 'f'})
	m.Handle(term.Key{Rune: 'f'})
	if got := m.Selection(); got == nil || got.Name != "proj/task" || !m.Follow {
		t.Fatalf("after f twice: %+v follow=%v", got, m.Follow)
	}
	// Moves that change nothing keep following: up from the first row,
	// onto the selected row, on an empty list.
	in = fixtureInput(now)
	in.Locals = append(in.Locals, protocol.Session{Name: "vm/laatmux/fix-ls", Key: "venv//r/fix-ls", Host: "vm"})
	in.Current = "vm/laatmux/fix-ls"
	m = model(now)
	m.Layout, m.Width, m.Height = Compact, 80, 30
	m.Follow = true
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	if m.Selected != 0 {
		t.Fatalf("current first: %d", m.Selected)
	}
	m.Handle(term.Key{Kind: term.KeyUp})
	m.Handle(term.Key{Rune: 'k'})
	m.Handle(term.Key{Rune: 'g'})
	m.Handle(term.Key{Kind: term.KeyMouse, Wheel: -1})
	if a := m.Handle(term.Key{Rune: '1'}); a.Kind != ActionJump || !m.Follow || m.Selected != 0 {
		t.Fatalf("moves that change nothing: %+v follow=%v at %d", a, m.Follow, m.Selected)
	}
	m.Handle(term.Key{Rune: 'j'})
	if m.Follow || m.Selected != 1 {
		t.Fatalf("a move that changes: follow=%v at %d", m.Follow, m.Selected)
	}
	m = model(now)
	m.Follow = true
	m.SetRows(rows.Agents(rows.Input{}, rows.Tree(rows.Input{})))
	m.Handle(term.Key{Rune: 'j'})
	m.Handle(term.Key{Rune: 'G'})
	if got := m.Selection(); got != nil || !m.Follow {
		t.Fatalf("empty list: %+v follow=%v", got, m.Follow)
	}
}

// A live working row's icon is the spinner frame for the clock, in the
// info colour; the frame advances every spinTick and wraps; a dim
// working agent's spinner stands still; Spinning says whether a tick is
// wanted at all.
func TestSpinner(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Compact, 80, 30
	in := fixtureInput(now)
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Render()
	if !m.Spinning() {
		t.Fatal("working rows and no spinning")
	}
	frameOf := func(name string, at time.Time) Span {
		m.Now = at
		for _, it := range m.Visible() {
			if it.Row.Name == name {
				return m.iconSpan(*it.Row)
			}
		}
		t.Fatalf("no row %s", name)
		return Span{}
	}
	first := frameOf("proj/task", now)
	if first.Fg != palette.Info || first.Text != spinnerFrames[0] || !first.spin {
		t.Fatalf("frame at t0: %+v", first)
	}
	if next := frameOf("proj/task", now.Add(spinTick)); next.Text != spinnerFrames[1] || next.Fg != palette.Info {
		t.Fatalf("frame at t0+tick: %+v", next)
	}
	if wrapped := frameOf("proj/task", now.Add(time.Duration(len(spinnerFrames))*spinTick)); wrapped.Text != spinnerFrames[0] {
		t.Fatalf("frame after a full cycle: %+v", wrapped)
	}
	// A zero Now, before the first draw, is a frame too, not a panic.
	if z := frameOf("proj/task", time.Time{}); z.Fg != palette.Info || z.Text == "" {
		t.Fatalf("frame at the zero time: %+v", z)
	}
	// Working but dim, on the down host: the spinner stands still.
	if down := frameOf("proj/down", now); down.Text != spinnerFrames[0] || down.spin {
		t.Fatalf("dim working row: %+v", down)
	}
	if blocked := frameOf("laatmux/fix-ls", now); blocked.Text != "💬" || blocked.Fg != palette.Accent {
		t.Fatalf("blocked row: %+v", blocked)
	}
	// Every working agent gone: nothing spins.
	for i := range in.Agents {
		if in.Agents[i].Activity == protocol.Working {
			in.Agents[i].Liveness = protocol.Gone
		}
	}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Render()
	if m.Spinning() {
		t.Fatal("gone agents spin")
	}
	// The colour reaches the terminal and the plain text does not
	// carry it.
	th, _ := palette.New(true, nil)
	l := Line{Spans: []Span{{Text: ">"}, {Text: "⠋", Fg: palette.Info}, {Text: " x"}}}
	if got := ANSI(l, th); !strings.Contains(got, "\x1b[38;2;125;207;255m⠋\x1b[0m") || !strings.HasSuffix(got, " x\x1b[0m") {
		t.Errorf("ANSI: %q", got)
	}
	if got := ANSI(l, palette.Mono()); strings.Contains(got, "38;") {
		t.Errorf("ANSI without colours: %q", got)
	}
	if got := Text([]Line{l}); got != ">⠋ x\n" {
		t.Errorf("Text: %q", got)
	}
	if got := Debug([]Line{l}); !strings.Contains(got, ">⟨info:⠋⟩ x") {
		t.Errorf("Debug: %q", got)
	}
}

// The spinner ticks only while a frame is on screen: working rows
// scrolled off, or filtered out, or behind an overlay, are not ticked
// for; and a narrow pane keeps the stripe's and icon's colours in both
// layouts, down to a single cell.
func TestSpinnerOnScreenAndNarrow(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Compact, 80, 6 // header lines, a few body lines, footer
	m.SetRows(fixture(now))
	m.Render()
	if !m.Spinning() {
		t.Fatal("working rows at the top and no spinning")
	}
	m.Handle(term.Key{Rune: 'G'})
	m.Render()
	if m.Spinning() {
		t.Fatal("working rows scrolled off and still spinning")
	}
	m.Handle(term.Key{Rune: 'g'})
	m.Render()
	if !m.Spinning() {
		t.Fatal("scrolled back and not spinning")
	}
	// The idle scratch tile left, the working ones filtered out.
	m.Filter = "scratch"
	if m.Render(); len(m.Visible()) != 1 || m.Spinning() {
		t.Fatalf("working rows filtered out: %d rows, spinning=%v", len(m.Visible()), m.Spinning())
	}
	m.Filter = ""
	m.Overlay = NewLog("add")
	m.Render()
	if m.Spinning() {
		t.Fatal("an overlay up and still spinning")
	}
	m.Overlay = nil
	// Tiles, the blocked tile selected at the top: the first working
	// tile's head line is the fifth body line. Rows below take the
	// window's last line for their count, so six body lines show its
	// icon and five cut the tile off above it.
	m.Layout = Tiles
	m.Handle(term.Key{Rune: 'g'})
	m.Height = len(m.Header) + 6 + 1
	m.Render()
	if !m.Spinning() {
		t.Fatalf("tile head on screen and not spinning:\n%s", Debug(m.Render()))
	}
	m.Height = len(m.Header) + 5 + 1
	m.Render()
	if m.Spinning() {
		t.Fatalf("tile head clipped and still spinning:\n%s", Debug(m.Render()))
	}
	// Header lines that fill the terminal: the one body line the layout
	// keeps is cut off by the height, and does not count.
	m = model(now)
	m.Layout, m.Width = Compact, 80
	m.SetRows(fixture(now))
	m.Filter = "proj/task" // the one row is the live working one
	m.Header = []HeaderLine{{Text: "one"}, {Text: "two"}}
	m.Height = 4 // headers, the body line, footer: the icon is drawn
	if lines := m.Render(); len(lines) != 4 || !m.Spinning() {
		t.Fatalf("one working row under two headers: %d lines, spinning=%v", len(lines), m.Spinning())
	}
	m.Height = 2 // the headers alone: the body line is cut off
	if lines := m.Render(); len(lines) != 2 || m.Spinning() {
		t.Fatalf("headers filling the terminal: %d lines, spinning=%v", len(lines), m.Spinning())
	}
	// Narrow: the coloured stripe survives the fallback in both layouts,
	// and no line is wider than the pane.
	for _, layout := range []Layout{Compact, Tiles} {
		for _, w := range []int{12, 6, 3, 2} {
			m = model(now)
			m.Layout, m.Width, m.Height = layout, w, 30
			m.SetRows(fixture(now))
			lines := m.Render()
			if out := Debug(lines); !strings.Contains(out, "⟨") {
				t.Errorf("layout %v width %d: no colour:\n%s", layout, w, out)
			}
			for _, l := range lines {
				if width(strings.TrimSuffix(Text([]Line{l}), "\n")) > w {
					t.Errorf("layout %v width %d: line wider than the pane: %q", layout, w, Text([]Line{l}))
				}
			}
		}
		m = model(now)
		m.Layout, m.Width, m.Height = layout, 1, 30
		m.SetRows(fixture(now))
		m.Render() // one cell: the stripe alone, no panic
	}
}

// A newline is Enter outside the form's prompt: on the list, in the
// filter, a picker and a line prompt.
func TestNewlineIsEnter(t *testing.T) {
	m := &Model{Width: 80, Height: 24}
	m.Filtering = true
	m.Handle(term.Key{Kind: term.KeyNewline})
	if m.Filtering {
		t.Fatal("the filter did not close on a newline")
	}
	p := NewPicker("t", []Choice{{Label: "a"}}, 0)
	p.Handle(term.Key{Kind: term.KeyNewline})
	if !p.Done() || p.Chosen != 0 {
		t.Fatalf("picker: done %v chosen %d", p.Done(), p.Chosen)
	}
	f := NewForm("t", chips(), "")
	f.Handle(term.Key{Kind: term.KeyShiftTab})
	f.Handle(term.Key{Kind: term.KeyShiftTab}) // the agent chip
	f.Handle(term.Key{Kind: term.KeyNewline})
	if f.picker == nil {
		t.Fatal("a newline on a chip did not open the picker")
	}
}

// The anchor pair: a task selected before its root is known stays
// selected when the root arrives, when its worktree row takes over, and
// when another task stands for it again; a task the view never saw hand
// over finds its worktree row through the handoffs; past them the
// selection is on none rather than on the row that took the index. The
// worktree row is the tree's line: the agent view has none, and follows
// a handoff to the worktree's first agent's tile instead (TestTreeEdges,
// TestHandoffStanding). The model is filled as the dashboard fills it,
// the tree then the agent view.
func TestAnchorFollowsTask(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	hosts := []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}}
	other := protocol.Worktree{ID: "venv/worktree//r/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/r/a"}
	task := protocol.Pending{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "task", Taken: true, Reachable: true, Stage: protocol.StageClone, SubmittedAt: now}
	wt := protocol.Worktree{ID: "venv/worktree//r/task", EnvironmentID: "venv", Repo: "proj", Branch: "task", Root: "/r/task", Session: "proj/task"}
	agent := protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Server: "laatmux", Session: "proj/task", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true}
	set := func(m *Model, ps []protocol.Pending, ws []protocol.Worktree, as []protocol.Agent) {
		in := rows.Input{Hosts: hosts, Pendings: ps, Worktrees: ws, Agents: as}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
	}
	m := &Model{Width: 60, Height: 20, Now: now, View: ViewTree}
	set(m, []protocol.Pending{task}, []protocol.Worktree{other}, nil)
	if !m.Select("add-1") {
		t.Fatal("no task line")
	}
	if r := m.Selection(); r == nil || r.ID() != "add-1" {
		t.Fatalf("selected %+v", r)
	}
	// The root arrives: same id, now with its alias.
	task.Stage, task.Root = protocol.StageAgent, "/r/task"
	set(m, []protocol.Pending{task}, []protocol.Worktree{other, wt}, []protocol.Agent{agent})
	if r := m.Selection(); r == nil || r.ID() != "add-1" || m.alias != wt.ID {
		t.Fatalf("with the root: %+v alias %q", r, m.alias)
	}
	// Handed over: the worktree row, found by the alias.
	set(m, nil, []protocol.Worktree{other, wt}, []protocol.Agent{agent})
	if r := m.Selection(); r == nil || r.ID() != wt.ID {
		t.Fatalf("after the handover: %+v", r)
	}
	// A task for the same worktree again: the task that stands for it.
	again := task
	again.ID = "add-2"
	set(m, []protocol.Pending{again}, []protocol.Worktree{other, wt}, []protocol.Agent{agent})
	if r := m.Selection(); r == nil || r.ID() != "add-2" {
		t.Fatalf("a task standing for the worktree again: %+v", r)
	}

	// A view that selected a task during clone and missed every step
	// until the worktree row: the handoffs name it.
	m = &Model{Width: 60, Height: 20, Now: now, View: ViewTree}
	early := protocol.Pending{ID: "add-3", Host: "vm", Repo: "proj", Branch: "task", Taken: true, Reachable: true, Stage: protocol.StageClone, SubmittedAt: now}
	set(m, []protocol.Pending{early}, []protocol.Worktree{other}, nil)
	m.Select("add-3")
	if r := m.Selection(); r == nil || r.ID() != "add-3" || m.alias != "" {
		t.Fatalf("selected %+v alias %q", r, m.alias)
	}
	// Through Set, as the dashboard refreshes: the handoff is in place
	// before the rows that need it.
	in := rows.Input{Hosts: hosts, Worktrees: []protocol.Worktree{other, wt}, Agents: []protocol.Agent{agent}}
	tree := rows.Tree(in)
	m.Set(tree, rows.Agents(in, tree), map[string]string{"add-3": wt.ID})
	if r := m.Selection(); r == nil || r.ID() != wt.ID {
		t.Fatalf("through the handoffs: %+v", r)
	}
	// Without them the selection is on none, not on the other row.
	m = &Model{Width: 60, Height: 20, Now: now, View: ViewTree}
	set(m, []protocol.Pending{early}, []protocol.Worktree{other}, nil)
	m.Select("add-3")
	m.Selection()
	set(m, nil, []protocol.Worktree{other, wt}, []protocol.Agent{agent})
	if r := m.Selection(); r != nil {
		t.Fatalf("an unknown handoff left the selection on %q", r.ID())
	}
	// A following view goes back to the viewer's own row meanwhile.
	f := &Model{Width: 60, Height: 20, Now: now, Follow: true}
	set(f, []protocol.Pending{early}, []protocol.Worktree{other}, nil)
	if f.Selection() != nil {
		t.Fatal("a following view selected a row that is not the viewer's")
	}
}

// Pending rows as drawn: the spinner and the state while the add runs,
// "!" and dim with the reason once it needs the user, first in the
// main group, ahead of a blocked agent's tile, in tiles and in compact
// with titles.
func TestRenderPending(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	in := rows.Input{
		Hosts:     []rows.Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true}, {Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Worktrees: []protocol.Worktree{{ID: "venv/worktree//r/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/r/a", Session: "proj/a"}},
		// The agent view draws a worktree by its agents: a's, blocked,
		// the most pressing an agent can be, still sorts after the tasks.
		Agents: []protocol.Agent{{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "proj/a", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now.Add(-5 * time.Minute), Liveness: protocol.Alive, Managed: true, Title: "Allow?"}},
		Pendings: []protocol.Pending{
			{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "sidebar-follow", Taken: true, Reachable: true, Stage: protocol.StageClone, Detail: "cloning git@github.com:laat/proj.git", SubmittedAt: now},
			{ID: "add-2", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "fix-ls", Root: "/r/fix-ls", Session: "proj/fix-ls", Taken: true, Reachable: true,
				Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, Error: "the pane was not ready within a minute", SubmittedAt: now.Add(-time.Minute)},
		},
	}
	m := &Model{Rows: rows.Agents(in, rows.Tree(in)), LocalHost: "mac", Now: now, Width: 40, Height: 12}
	golden(t, "pending-tiles", Debug(m.Render()))
	m.Layout, m.Titles, m.Width = Compact, true, 72
	golden(t, "pending-compact", Debug(m.Render()))
	if !m.Spinning() {
		t.Error("a running task does not spin")
	}
}

// A task that hands over while another task for the same root stands
// for the worktree row: the selection goes to the task standing for
// it. A selection found again after it was lost is a plain one again.
// The model is filled as the dashboard fills it, the tree then the
// agent view.
func TestAnchorStandIn(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	hosts := []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}}
	wt := protocol.Worktree{ID: "venv/worktree//r/b", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b"}
	stuck := protocol.Pending{ID: "add-a", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, SubmittedAt: now.Add(-time.Hour)}
	next := protocol.Pending{ID: "add-b", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryDelivered, SubmittedAt: now}
	set := func(m *Model, ps ...protocol.Pending) {
		in := rows.Input{Hosts: hosts, Pendings: ps, Worktrees: []protocol.Worktree{wt}}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
	}
	m := &Model{Width: 60, Height: 20, Now: now}
	set(m, stuck, next)
	m.Handle(term.Key{Rune: 'g'})
	if r := m.Selection(); r == nil || r.ID() != "add-b" {
		t.Fatalf("selected %+v", r)
	}
	m.Handoffs = map[string]string{"add-b": wt.ID}
	set(m, stuck)
	if r := m.Selection(); r == nil || r.ID() != "add-a" {
		t.Fatalf("after add-b handed over: %+v", r)
	}
	// Beside a task that failed or was gone at the root, which stands
	// for nothing, the hand-over lands on the worktree row itself: the
	// tree's line, the worktree having no agent and so no tile.
	for _, bad := range []protocol.Pending{
		{ID: "add-f", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Taken: true, Done: true, Stage: protocol.StageAgent, Error: "failed at agent: x", SubmittedAt: now.Add(-time.Hour)},
		{ID: "add-g", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNone, Gone: true, SubmittedAt: now.Add(-time.Hour)},
	} {
		f := &Model{Width: 60, Height: 20, Now: now, View: ViewTree, Handoffs: map[string]string{"add-b": wt.ID}}
		set(f, bad, next)
		f.Select("add-b")
		if r := f.Selection(); r == nil || r.ID() != "add-b" {
			t.Fatalf("%s: selected %+v", bad.ID, r)
		}
		set(f, bad)
		if r := f.Selection(); r == nil || r.ID() != wt.ID {
			t.Fatalf("%s: after add-b handed over: %+v", bad.ID, r)
		}
	}
	// Gone before the stream carries its handoff: the task standing for
	// the same worktree, found by the alias alone.
	u := &Model{Width: 60, Height: 20, Now: now}
	set(u, stuck, next)
	u.Handle(term.Key{Rune: 'g'})
	u.Selection()
	set(u, stuck)
	if r := u.Selection(); r == nil || r.ID() != "add-a" {
		t.Fatalf("add-b gone with no handoff recorded: %+v", r)
	}
	// Lost, then found again: a filter that hides every row and is
	// cleared puts the selection on the first row, as for any other.
	set(m)
	set(m, stuck)
	if r := m.Selection(); r == nil || r.ID() != "add-a" || m.lost {
		t.Fatalf("found again: %+v lost %v", r, m.lost)
	}
	m.Filter = "zzz"
	m.Selection()
	m.Filter = ""
	if r := m.Selection(); r == nil {
		t.Fatal("a cleared filter left the selection on none")
	}
}

// A click jumps to the row clicked. While the selection follows the
// viewer's own row it goes on following and stays where it was; a
// selection the user moved moves to the row clicked, as a key would.
func TestClickJumpKeepsFollow(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	in := fixtureInput(now)
	in.Current = "mac/proj/task"
	m := &Model{Layout: Compact, Width: 80, Height: 30, Now: now, Follow: true}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	own := m.Selected
	m.Render()
	// The first body line is the first row; the own row is elsewhere.
	y := 1 + len(m.Header)
	target := m.Visible()[m.hitRow(y, time.Time{})].Row.Name
	if m.hitRow(y, time.Time{}) == own {
		t.Fatal("the fixture's first row is the viewer's own")
	}
	a := m.Handle(term.Key{Kind: term.KeyMouse, Y: y})
	if a.Kind != ActionJump || a.Row == nil || a.Row.Name != target || !a.Mouse || !m.Follow || m.Selected != own {
		t.Fatalf("click while following: %+v selected %d follow %v", a, m.Selected, m.Follow)
	}
	// The user's own selection: j, then a click, moves it there.
	m.Handle(term.Key{Rune: 'j'})
	m.Render()
	a = m.Handle(term.Key{Kind: term.KeyMouse, Y: y})
	if a.Kind != ActionJump || a.Row == nil || a.Row.Name != target || m.Follow || m.Selected != m.hitRow(y, time.Time{}) {
		t.Fatalf("click with the user's selection: %+v selected %d follow %v", a, m.Selected, m.Follow)
	}
	// What was clicked is what was drawn: rows that moved since the last
	// render, or a filter typed since, do not change the target; a row
	// that is gone is no target.
	m.Follow = true
	m.Render()
	drawn := m.Visible()[m.hitRow(y, time.Time{})].Row.Name
	in2 := fixtureInput(now)
	in2.Current = "mac/proj/task"
	for i := range in2.Agents {
		in2.Agents[i].Activity = protocol.Idle
	}
	m.SetRows(rows.Agents(in2, rows.Tree(in2)))
	if a := m.Handle(term.Key{Kind: term.KeyMouse, Y: y}); a.Kind != ActionJump || a.Row.Name != drawn {
		t.Fatalf("after a reorder: jumped to %+v, drawn was %q", a.Row, drawn)
	}
	m.Filter = "zzz-nothing"
	if a := m.Handle(term.Key{Kind: term.KeyMouse, Y: y}); a.Kind != ActionNone {
		t.Fatalf("a row filtered away since: %+v", a)
	}
	// A row that moved into a collapsed group since is no target either:
	// a main-group row with a local session is settled after the render.
	m.Filter = ""
	m.ShowHidden = false
	m.Render()
	sy, local := -1, ""
	for yy := 1; yy <= m.Height; yy++ {
		if i := m.hitRow(yy, time.Time{}); i >= 0 {
			if r := m.Visible()[i].Row; r.Local != nil && r.Local.Workspace() && !r.Settled && !r.Current {
				sy, local = yy, r.Local.Name
				break
			}
		}
	}
	if sy < 0 {
		t.Fatal("no row with a local session on screen")
	}
	for i := range in2.Locals {
		if in2.Locals[i].Name == local {
			in2.Locals[i].Settled = true
		}
	}
	m.SetRows(rows.Agents(in2, rows.Tree(in2)))
	if i := m.hitRow(sy, time.Time{}); i != -1 {
		t.Fatalf("a row collapsed since resolved to %d", i)
	}
	if a := m.Handle(term.Key{Kind: term.KeyMouse, Y: sy}); a.Kind != ActionNone {
		t.Fatalf("a click on a row collapsed since: %+v", a)
	}
}

// A click read before the last draw is on the screen drawn before it: a
// refresh that redrew with the rows reordered while the click waited
// does not change its target; one read before two draws is dropped.
func TestClickOnScreenItWasRead(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	in := fixtureInput(t0)
	m := &Model{Layout: Compact, Width: 80, Height: 30, Now: t0}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Render()
	y := 1 + len(m.Header)
	seen := m.Visible()[m.hitRow(y, time.Time{})].Row.ID()
	clicked := t0.Add(time.Second)
	// The rows reorder and are drawn again after the click was read.
	for i := range in.Agents {
		in.Agents[i].Activity = protocol.Idle
	}
	in.Agents[len(in.Agents)-1].Activity = protocol.Blocked
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Now = t0.Add(2 * time.Second)
	m.Render()
	if now := m.Visible()[m.hitRow(y, time.Time{})].Row.ID(); now == seen {
		t.Fatal("the fixture's reorder left the first line as it was")
	}
	if i := m.hitRow(y, clicked); i < 0 || m.Visible()[i].Row.ID() != seen {
		t.Fatalf("a click read before the redraw resolved to %d, want %q", i, seen)
	}
	// Drawn twice since: the screen clicked is gone, and the click with it.
	m.Now = t0.Add(3 * time.Second)
	m.Render()
	if i := m.hitRow(y, clicked); i != -1 {
		t.Fatalf("a click read before two redraws resolved to %d", i)
	}
}

// Done and stale: an agent that finished since the user last looked is
// ✅ and sorts after the blocked ones; one idle past the stale time is
// 💤, dim, in the stale group, collapsed with the settled ones; a settled
// workspace's blocked agent stays in place with its own icon.
func TestRenderAttention(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	in := fixtureInput(now)
	in.Agents = append(in.Agents,
		protocol.Agent{ID: "venv/laatmux/%10", EnvironmentID: "venv", Server: "laatmux", Session: "proj/old", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-2 * time.Hour), Liveness: protocol.Alive, Managed: true, Title: "long idle"},
		protocol.Agent{ID: "venv/laatmux/%11", EnvironmentID: "venv", Server: "laatmux", Session: "proj/asks", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now.Add(-3 * time.Hour), Liveness: protocol.Alive, Managed: true, Title: "Allow?"})
	in.Worktrees = append(in.Worktrees,
		protocol.Worktree{ID: "venv/worktree//r/old", EnvironmentID: "venv", Repo: "proj", Branch: "old", Root: "/r/old", Session: "proj/old"},
		protocol.Worktree{ID: "venv/worktree//r/asks", EnvironmentID: "venv", Repo: "proj", Branch: "asks", Root: "/r/asks", Session: "proj/asks"})
	in.Locals = append(in.Locals, protocol.Session{Name: "vm/proj/asks", Key: "venv//r/asks", Host: "vm", Settled: true})
	in.Attention = map[string]protocol.Attention{
		// notes finished after the last visit; other was seen since.
		"menv/default/%6": {AgentID: "menv/default/%6", FinishedAt: now.Add(-time.Minute)},
		"venv/laatmux/%3": {AgentID: "venv/laatmux/%3", FinishedAt: now.Add(-time.Hour), SeenAt: now.Add(-time.Minute)},
		// A done agent is never stale, however long ago it finished.
		"menv/laatmux/%9": {AgentID: "menv/laatmux/%9", FinishedAt: now.Add(-26 * time.Hour)},
	}
	in.Now, in.StaleAfter, in.DimStale, in.CollapseStale = now, time.Hour, true, true
	m := model(now)
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Layout, m.Titles, m.Width, m.Height = Compact, true, 90, 34
	m.ShowHidden = true
	golden(t, "attention", Debug(m.Render()))
	m.ShowHidden = false
	golden(t, "attention-folded", Debug(m.Render()))
}

// The diff stats on the tile's second line and before the time in the
// compact line: the rebase mark, the committed diff, ✎ and the
// uncommitted one; a narrow line drops the committed part, then all but
// the rebase mark; a stale object is dim. The stale object and the mark
// alone are on dead's and down's worktrees, which have agents and so
// tiles; spike and shell have neither.
func TestRenderGit(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	in := fixtureInput(now)
	yes := true
	stats := map[string]protocol.GitStatus{
		"venv/worktree//r/fix-ls": {Base: "origin/main", Committed: [2]int{46, 11}, Uncommitted: [2]int{28, 3}, Dirty: true, Ahead: 2},
		"menv/worktree//w/task":   {Base: "origin/main", Committed: [2]int{318, 87}, Rebasing: true, Conflict: &yes},
		"venv/worktree//r/other":  {Base: "origin/main", Uncommitted: [2]int{4, 1}, UncommittedPartial: true, Dirty: true},
		"venv/worktree//r/dead":   {Base: "origin/main", Committed: [2]int{153, 41}, Stale: true},
		// Dirty with no lines, a mode change say: the mark alone.
		"benv/worktree//r/down": {Base: "origin/main", Dirty: true},
	}
	for i := range in.Worktrees {
		if g, ok := stats[in.Worktrees[i].ID]; ok {
			in.Worktrees[i].Git = &g
		}
	}
	m := model(now)
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Layout, m.Width, m.Height = Tiles, 45, 40
	golden(t, "git-tiles", Debug(m.Render()))
	m.Width = 26
	golden(t, "git-narrow", Debug(m.Render()))
	m.Layout, m.Width, m.Height = Compact, 90, 20
	golden(t, "git-compact", Debug(m.Render()))
}

// The PR and checks on the third line: the PR number coloured by state,
// ✓, × with the counts, a spinner with the counts; a draft dim; a stale
// answer dim with ?; on main only failing checks; narrow, the counts go
// first. The worktree on main is down's, which has an agent and so a
// tile; spike, on main before, has none.
func TestRenderPR(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	in := fixtureInput(now)
	src := "git@github.com:laat/proj.git"
	for i := range in.Worktrees {
		in.Worktrees[i].Source = src
	}
	key := func(branch string) protocol.BranchKey {
		return protocol.BranchKey{Source: source.Key(src), Branch: branch}
	}
	in.Branches = map[protocol.BranchKey]protocol.BranchStatus{
		key("fix-ls"): {PR: &protocol.PullRequest{Number: 52, State: "open"}, Checks: &protocol.Checks{State: protocol.ChecksSuccess, Passed: 5, Total: 5}},
		key("task"):   {PR: &protocol.PullRequest{Number: 49, State: "open", Draft: true}, Checks: &protocol.Checks{State: protocol.ChecksFailure, Passed: 3, Total: 5}},
		key("other"):  {PR: &protocol.PullRequest{Number: 12, State: "merged"}, Checks: &protocol.Checks{State: protocol.ChecksPending, Passed: 1, Total: 4}},
		key("dead"):   {PR: &protocol.PullRequest{Number: 7, State: "closed"}, Checks: &protocol.Checks{State: protocol.ChecksSuccess}, Stale: true},
	}
	for i := range in.Worktrees {
		if in.Worktrees[i].Branch == "down" {
			in.Worktrees[i].Branch = "main"
			in.Branches[key("main")] = protocol.BranchStatus{PR: &protocol.PullRequest{Number: 1, State: "merged"}, Checks: &protocol.Checks{State: protocol.ChecksFailure, Passed: 2, Total: 3}}
		}
	}
	m := model(now)
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Layout, m.Width, m.Height = Tiles, 48, 44
	golden(t, "pr-tiles", Debug(m.Render()))
	m.Width = 22
	golden(t, "pr-narrow", Debug(m.Render()))
	m.Layout, m.Titles, m.Width, m.Height = Compact, true, 90, 30
	golden(t, "pr-compact", Debug(m.Render()))
	m.Icons = Icons{Set: IconsASCII}
	golden(t, "pr-ascii", Debug(m.Render()))
}

// treeInput is the rows package's tree fixture: two repositories on two
// hosts, a worktree with two agents, a shell and a run, one with no
// agent, one a task stands for, an orphaned session, and other sessions.
func treeInput(now time.Time) rows.Input {
	src := "git@github.com:laat/laatmux.git"
	agent := func(id, env, session, name string, act protocol.Activity, start int64, wt, title string) protocol.Agent {
		return protocol.Agent{ID: id, Server: "laatmux", EnvironmentID: env, Session: session, Agent: name, Activity: act, ActivityAt: now.Add(-time.Minute),
			Liveness: protocol.Alive, Managed: true, Identity: &protocol.Identity{PID: 1, StartUnix: start}, WorktreeID: wt, Title: title}
	}
	return rows.Input{
		Hosts: []rows.Host{
			{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
			{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
		},
		Agents: []protocol.Agent{
			agent("venv/laatmux/%2", "venv", "laatmux/agents-config", "codex", protocol.Idle, 20, "venv/worktree//r/agents-config", "Reading the config"),
			agent("venv/laatmux/%1", "venv", "laatmux/agents-config", "claude", protocol.Working, 10, "venv/worktree//r/agents-config", "✳ Adding per-agent config to the loader"),
			agent("menv/laatmux/%3", "menv", "proj/batch", "claude", protocol.Blocked, 30, "menv/worktree//w/batch", "Refactoring queue handling now"),
			agent("venv/laatmux/%8", "venv", "laatmux/auto-layout", "claude", protocol.Idle, 35, "venv/worktree//r/auto-layout", "done"),
			{ID: "venv/default/%5", EnvironmentID: "venv", Server: "default", Session: "scratch", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now, Liveness: protocol.Alive},
		},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//r/agents-config", EnvironmentID: "venv", Repo: "laatmux", Source: src, Branch: "agents-config", Root: "/r/agents-config", Session: "laatmux/agents-config",
				Git: &protocol.GitStatus{Committed: [2]int{46, 11}, Uncommitted: [2]int{28, 3}, Dirty: true}},
			{ID: "venv/worktree//r/auto-layout", EnvironmentID: "venv", Repo: "laatmux", Source: src, Branch: "auto-layout", Root: "/r/auto-layout", Session: "laatmux/auto-layout",
				Git: &protocol.GitStatus{Committed: [2]int{318, 87}}},
			{ID: "menv/worktree//w/fix-sidebar", EnvironmentID: "menv", Repo: "laatmux", Source: src, Branch: "fix-sidebar", Root: "/w/fix-sidebar", Git: &protocol.GitStatus{Uncommitted: [2]int{4, 1}, Dirty: true}},
			{ID: "menv/worktree//w/batch", EnvironmentID: "menv", Repo: "anki-llm", Source: "https://github.com/laat/anki-llm", Branch: "batch-processing", Root: "/w/batch", Session: "proj/batch"},
		},
		Panes: []protocol.Pane{{ID: "venv/pane/laatmux/%7", EnvironmentID: "venv", Session: "laatmux/agents-config", Window: 1, PaneID: "%7", Command: "zsh", WorktreeID: "venv/worktree//r/agents-config"}},
		Runs:  []protocol.Run{{ID: "venv/run/r1", EnvironmentID: "venv", Root: "/r/agents-config", WorktreeID: "venv/worktree//r/agents-config", Cmd: []string{"make", "test"}, StartedAt: now.Add(-42 * time.Second)}},
		Locals: []protocol.Session{
			{Name: "vm/laatmux/agents-config", Key: "venv//r/agents-config", Host: "vm"},
			{Name: "mac/anki-llm/batch-processing", Key: "menv//w/batch", Host: "mac"},
			{Name: "mac/laatmux/gone", Key: "menv//w/gone", Host: "mac", Source: src},
		},
		Branches: map[protocol.BranchKey]protocol.BranchStatus{
			{Source: source.Key(src), Branch: "agents-config"}: {PR: &protocol.PullRequest{Number: 52, State: "open"}, Checks: &protocol.Checks{State: protocol.ChecksSuccess, Passed: 5, Total: 5}},
			{Source: source.Key(src), Branch: "auto-layout"}:   {PR: &protocol.PullRequest{Number: 49, State: "open"}, Checks: &protocol.Checks{State: protocol.ChecksFailure, Passed: 3, Total: 5}},
		},
		Attention: map[string]protocol.Attention{"venv/laatmux/%8": {AgentID: "venv/laatmux/%8", FinishedAt: now.Add(-time.Minute)}},
		Current:   "vm/laatmux/agents-config",
		Now:       now, StaleAfter: time.Hour, DimStale: true, CollapseStale: true,
	}
}

// The tree view: repositories, worktree lines with the host, the git
// stats and the PR, their agents, panes and runs; a folded worktree
// shows its most pressing agent's icon; the orphaned session under its
// repository; other sessions last; the tab line on top.
func TestRenderTree(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	m := &Model{Now: now, LocalHost: "mac", View: ViewTree, Tabs: true, Width: 60, Height: 20, Follow: true}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.SetTree(rows.Tree(in))
	golden(t, "tree", Debug(m.Render()))
	// auto-layout, whose agent is done, starts open; fix-sidebar has no
	// children; folding agents-config shows the working spinner at the
	// right.
	m.Handle(term.Key{Rune: 'h'})
	golden(t, "tree-folded", Debug(m.Render()))
	m.Width = 30
	golden(t, "tree-narrow", Debug(m.Render()))
	m.Width = 60
	m.Handle(term.Key{Kind: term.KeyTab})
	golden(t, "tree-agents", Debug(m.Render()))
}

// A line in other sessions shows the host as the {host} token draws it
// on the agent's tile: the server after it for an agent observed off
// the managed server, none for a managed agent in no worktree, ? for a
// host no record claims; dim off this machine. On a narrow line the
// server gives way to the icon, cut with …, down to the host alone.
func TestOtherSessionsHost(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	in.Agents = append(in.Agents,
		protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Server: "laatmux", Session: "loose", Agent: "codex", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Managed: true},
		protocol.Agent{ID: "xenv/work/%4", EnvironmentID: "xenv", Server: "work", Session: "stray", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive},
		protocol.Agent{ID: "menv/default/%6", EnvironmentID: "menv", Server: "default", Session: "here", Agent: "codex", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive})
	m := &Model{Now: now, LocalHost: "mac", View: ViewTree, Width: 80, Height: 30}
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	host, _ := ParseTemplate("{host}")
	for _, c := range []struct{ id, line, tile string }{
		{"venv/default/%5", "...|    scratch‹ (vm/default)›  ⟨accent:💬⟩ claude\n", "...|‹vm/default›\n"},
		{"venv/laatmux/%9", "...|    loose‹ (vm)›  ⟨border:  ⟩ codex\n", "...|‹vm›\n"},
		{"xenv/work/%4", ".D.|    stray‹ (?/work)›  ⟨border:  ⟩ claude\n", "...|‹?/work›\n"},
		{"menv/default/%6", "...|    here (mac/default)  ⟨border:  ⟩ codex\n", "...|mac/default\n"},
	} {
		i := m.indexOf(c.id)
		if i < 0 || m.Tree[i].Depth != 1 {
			t.Fatalf("%s not in other sessions", c.id)
		}
		if got := Debug(m.treeLine(m.Tree[i], 0)); got != c.line {
			t.Errorf("%s's line: %q, want %q", c.id, got, c.line)
		}
		tile := false
		for _, r := range m.Rows.Main {
			if r.ID() == c.id {
				tile = true
				if got := Debug([]Line{{Spans: m.line(Compiled{Template: host}, r, 80, 0)}}); got != c.tile {
					t.Errorf("%s's tile host: %q, want %q", c.id, got, c.tile)
				}
			}
		}
		if !tile {
			t.Errorf("%s has no tile", c.id)
		}
	}
	// Narrow: at 28 the host whole, just; at 25, the sidebar's least
	// width, the icon kept and the server cut; at 20 the host alone, as
	// the line read before it named the server; dim throughout.
	scratch := m.Tree[m.indexOf("venv/default/%5")]
	for _, c := range []struct {
		w    int
		want string
	}{
		{34, "...|    scratch‹ (vm/default)›  ⟨accent:💬⟩ claud\n"},
		{28, "...|    scratch‹ (vm/default)›  ⟨accent:💬⟩\n"},
		{25, "...|    scratch‹ (vm/def…)›  ⟨accent:💬⟩\n"},
		{21, "...|    scratch‹ (vm…)›  ⟨accent:💬⟩\n"},
		{20, "...|    scratch‹ (vm)›  ⟨accent:💬⟩\n"},
	} {
		m.Width = c.w
		if got := Debug(m.treeLine(scratch, 0)); got != c.want {
			t.Errorf("at %d: %q, want %q", c.w, got, c.want)
		}
	}
	// Down to the host alone, a host no record claims is ?, not nothing.
	m.Width = 17
	if got, want := Debug(m.treeLine(m.Tree[m.indexOf("xenv/work/%4")], 0)), ".D.|    stray‹ (?)›  ⟨border:  ⟩\n"; got != want {
		t.Errorf("stray at 17: %q, want %q", got, want)
	}
}

// An agent observed on this machine's default server in a window of a
// settled workspace session, in other sessions as its worktree is on
// another host: its line is dim with 💤, and its tile, dim with 💤 as
// well, folds with the stale ones. Unsettled, neither.
func TestOtherSessionsSettled(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	const id = "menv/default/%6"
	for _, c := range []struct {
		settled    bool
		line, tile string
		stale      bool
	}{
		{true, ".D.|    vm/laatmux/agents-config (mac/default)  ⟨dimmed:💤⟩ codex\n",
			".D.|⟨dimmed:▌⟩ ⟨dimmed:💤⟩ vm/laatmux/agents-config @mac/default                                  0:00\n", true},
		{false, "...|    vm/laatmux/agents-config (mac/default)  ⟨border:  ⟩ codex\n",
			"...|⟨border:▌⟩ ⟨border:  ⟩ vm/laatmux/agents-config @mac/default                                  0:00\n", false},
	} {
		in := treeInput(now)
		in.Agents = append(in.Agents, protocol.Agent{ID: id, EnvironmentID: "menv", Server: "default", Session: "vm/laatmux/agents-config", Agent: "codex", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive, Cwd: "/Users/u"})
		in.Locals[0].Settled = c.settled
		in.Current = ""
		m := &Model{Now: now, LocalHost: "mac", View: ViewTree, Width: 80, Height: 30}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		i := m.indexOf(id)
		if i < 0 || m.Tree[i].Depth != 1 {
			t.Fatalf("settled %v: %s not in other sessions", c.settled, id)
		}
		if got := Debug(m.treeLine(m.Tree[i], 0)); got != c.line {
			t.Errorf("settled %v: the line %q, want %q", c.settled, got, c.line)
		}
		var tile *rows.Row
		stale := false
		for fold, g := range [][]rows.Row{m.Rows.Main, m.Rows.Stale} {
			for k := range g {
				if g[k].ID() == id {
					tile, stale = &g[k], fold == 1
				}
			}
		}
		if tile == nil || stale != c.stale {
			t.Fatalf("settled %v: the tile %v in the Stale fold %v", c.settled, tile != nil, stale)
		}
		if got := Debug(m.compact(*tile, 0)); got != c.tile {
			t.Errorf("settled %v: the tile %q, want %q", c.settled, got, c.tile)
		}
	}
}

// Switching: the selection follows across Tab, from an agent to its
// node and back, from a worktree line or a pane to the worktree's first
// agent, from a repository line to its first worktree's, from a task to
// itself; a target in a folded worktree opens it; a worktree with no
// agent leaves the selection on no row.
func TestSwitch(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	in.Pendings = []protocol.Pending{{ID: "add-1", Host: "vm", EnvironmentID: "venv", Source: "git@github.com:laat/laatmux.git", Repo: "laatmux", Branch: "new-one", Root: "/r/new-one", Taken: true, SubmittedAt: now}}
	m := &Model{Now: now, View: ViewTree, Width: 60, Height: 30}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.SetTree(rows.Tree(in))
	sel := func() string {
		r := m.Selection()
		if r == nil {
			return ""
		}
		return r.ID()
	}
	m.Select("venv/laatmux/%2") // codex under agents-config
	m.Handle(term.Key{Kind: term.KeyTab})
	if m.View != ViewAgents || sel() != "venv/laatmux/%2" {
		t.Errorf("agent to its tile: view %s, selection %q", m.View, sel())
	}
	m.Handle(term.Key{Kind: term.KeyTab})
	if m.View != ViewTree || sel() != "venv/laatmux/%2" {
		t.Errorf("tile to its node: view %s, selection %q", m.View, sel())
	}
	// The worktree line, then a pane under it: the first agent.
	for _, id := range []string{"venv/worktree//r/agents-config", "venv/pane/laatmux/%7"} {
		m.Select(id)
		m.Handle(term.Key{Kind: term.KeyTab})
		if sel() != "venv/laatmux/%1" {
			t.Errorf("from %s: %q", id, sel())
		}
		m.Handle(term.Key{Kind: term.KeyTab})
	}
	// A repository line: its first worktree's first agent.
	m.Select(rows.RepoNode("git@github.com:laat/laatmux.git"))
	m.Handle(term.Key{Kind: term.KeyTab})
	if sel() != "venv/laatmux/%1" {
		t.Errorf("from the repository: %q", sel())
	}
	m.Handle(term.Key{Kind: term.KeyTab})
	// A task: itself.
	m.Select("add-1")
	m.Handle(term.Key{Kind: term.KeyTab})
	if sel() != "add-1" {
		t.Errorf("from a task: %q", sel())
	}
	m.Handle(term.Key{Kind: term.KeyTab})
	// A worktree with no agent: no row.
	m.Select("menv/worktree//w/fix-sidebar")
	m.Handle(term.Key{Kind: term.KeyTab})
	if sel() != "" {
		t.Errorf("from an empty worktree: %q", sel())
	}
	// Folded away, then reached from the tile: opened.
	m.Handle(term.Key{Kind: term.KeyTab})
	m.Select("venv/worktree//r/agents-config")
	before := len(m.Visible())
	m.Handle(term.Key{Rune: 'h'})
	if len(m.Visible()) != before-4 {
		t.Fatalf("not folded: %d rows, %d before", len(m.Visible()), before)
	}
	m.Handle(term.Key{Kind: term.KeyTab})
	m.Select("venv/laatmux/%2")
	m.Handle(term.Key{Kind: term.KeyTab})
	if sel() != "venv/laatmux/%2" || m.closed(&m.Tree[m.indexOf("venv/worktree//r/agents-config")]) {
		t.Errorf("a target in a folded worktree: %q, still folded", sel())
	}
}

// Folds: a worktree with a working agent starts open and one whose
// agent is idle folded, then stays as the user set it; h from a child
// goes to its line; f opens every fold when any is closed, else closes
// every one; Enter folds a repository line; the numbers count the
// worktree lines.
func TestFolds(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	in.Agents[3].Activity = protocol.Idle // auto-layout's agent: idle, and seen
	in.Attention = nil
	m := &Model{Now: now, View: ViewTree, Width: 60, Height: 30}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.SetTree(rows.Tree(in))
	m.Render()
	open := func(id string) bool { return !m.closed(&m.Tree[m.indexOf(id)]) }
	if !open("venv/worktree//r/agents-config") || open("venv/worktree//r/auto-layout") {
		t.Errorf("first folds: agents-config open %v, auto-layout open %v", open("venv/worktree//r/agents-config"), open("venv/worktree//r/auto-layout"))
	}
	// A working agent appears in auto-layout: the fold stays.
	in.Agents[3].Activity = protocol.Working
	m.SetTree(rows.Tree(in))
	if open("venv/worktree//r/auto-layout") {
		t.Error("a fold changed as the agent worked")
	}
	m.Select("venv/laatmux/%2")
	m.Handle(term.Key{Rune: 'h'})
	if r := m.Selection(); r == nil || r.ID() != "venv/worktree//r/agents-config" {
		t.Errorf("h from a child: %+v", r)
	}
	m.Handle(term.Key{Rune: 'h'})
	if open("venv/worktree//r/agents-config") {
		t.Error("h on the line did not fold")
	}
	m.Handle(term.Key{Rune: 'l'})
	if !open("venv/worktree//r/agents-config") {
		t.Error("l did not unfold")
	}
	m.Handle(term.Key{Rune: 'f'})
	if !open("venv/worktree//r/auto-layout") {
		t.Error("f with a fold closed did not open every one")
	}
	m.Handle(term.Key{Rune: 'f'})
	if open("venv/worktree//r/agents-config") || open(rows.RepoNode("git@github.com:laat/laatmux.git")) {
		t.Error("f with every fold open did not close every one")
	}
	m.Handle(term.Key{Rune: 'f'})
	m.Select(rows.RepoNode("git@github.com:laat/laatmux.git"))
	if a := m.Handle(term.Key{Kind: term.KeyEnter}); a.Kind != ActionNone || open(rows.RepoNode("git@github.com:laat/laatmux.git")) {
		t.Errorf("Enter on a repository line: %+v", a)
	}
	m.Handle(term.Key{Kind: term.KeyEnter})
	if a := m.Handle(term.Key{Rune: '2'}); a.Kind != ActionJump || a.Row == nil || a.Row.ID() != "venv/worktree//r/agents-config" {
		t.Errorf("2: %+v", a)
	}
}

// Following in the tree: the viewer's worktree line, and in the agent
// view the first tile among its agents.
func TestFollowTree(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	m := &Model{Now: now, View: ViewTree, Width: 60, Height: 30, Follow: true}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.SetTree(rows.Tree(in))
	if r := m.Selection(); r == nil || r.ID() != "venv/worktree//r/agents-config" {
		t.Errorf("tree follows %+v", r)
	}
	m.Handle(term.Key{Kind: term.KeyTab})
	if r := m.Selection(); r == nil || r.ID() != "venv/laatmux/%1" || !m.Follow {
		t.Errorf("agent view follows %+v", r)
	}
}

// Following prefers the viewer's worktree's tile to one of no worktree
// in other sessions. vm lists proj/z homed in proj/z, and the viewer is
// in vm/proj/z. The root agent is idle, last active ten minutes ago.
// Working, active a minute ago, is either a `cd ~ && claude` in a split
// of proj/z, a managed agent of no worktree, or claude observed in a
// window of vm/proj/z on mac's default server. Both tiles are the
// viewer's, and the other agent's sorts first in every order and is in
// every scope, yet the agent view follows the root agent's, through a
// refresh and a switch from the tree, as the tree follows proj/z's
// line. With the root agent no longer listed the other's is the
// viewer's only tile and is followed.
func TestFollowWorktreeTile(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	src := "git@github.com:u/proj.git"
	wt := "venv/worktree//w/proj/z"
	root := protocol.Agent{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "proj/z", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-10 * time.Minute), Liveness: protocol.Alive, Managed: true, Cwd: "/w/proj/z", WorktreeID: wt}
	split := protocol.Agent{ID: "venv/laatmux/%5", EnvironmentID: "venv", Server: "laatmux", Session: "proj/z", Agent: "claude", Activity: protocol.Working, ActivityAt: now.Add(-time.Minute), Liveness: protocol.Alive, Managed: true, Cwd: "/home/u"}
	observed := protocol.Agent{ID: "menv/default/%6", EnvironmentID: "menv", Server: "default", Session: "vm/proj/z", Agent: "claude", Activity: protocol.Working, ActivityAt: now.Add(-time.Minute), Liveness: protocol.Alive, Cwd: "/Users/u"}
	input := func(order string, agents ...protocol.Agent) rows.Input {
		return rows.Input{
			Hosts: []rows.Host{
				{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
				{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
			},
			Agents:    agents,
			Worktrees: []protocol.Worktree{{ID: wt, EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "z", Root: "/w/proj/z", Session: "proj/z"}},
			Locals:    []protocol.Session{{Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm"}},
			Current:   "vm/proj/z",
			Now:       now,
			Sort:      order,
		}
	}
	model := func(in rows.Input, scope Scope) *Model {
		m := &Model{Now: now, LocalHost: "mac", View: ViewAgents, Width: 80, Height: 30, Follow: true, Scope: scope}
		m.Set(rows.Tree(in), rows.Agents(in, rows.Tree(in)), nil)
		return m
	}
	for _, other := range []protocol.Agent{split, observed} {
		for _, scope := range []Scope{ScopeAll, ScopeSession, ScopeProject} {
			for _, order := range []string{rows.SortPriority, rows.SortRecency, rows.SortWindow} {
				name := other.ID + " " + string(scope) + " " + order
				m := model(input(order, root, other), scope)
				if vis := m.Visible(); len(vis) != 2 || vis[0].Row.ID() != other.ID || !vis[0].Row.Current || !vis[1].Row.Current {
					t.Fatalf("%s: the other agent's tile is not first, both the viewer's: %s", name, ids(m))
				}
				if r := m.Selection(); r == nil || r.ID() != root.ID {
					t.Errorf("%s: agent view follows %+v", name, r)
				}
				if id := m.followedID(); id != root.ID {
					t.Errorf("%s: followedID %q", name, id)
				}
				m.Handle(term.Key{Kind: term.KeyTab})
				if r := m.Selection(); r == nil || r.ID() != wt || !m.Follow {
					t.Errorf("%s: tree follows %+v", name, r)
				}
				m.Handle(term.Key{Kind: term.KeyTab})
				if r := m.Selection(); r == nil || r.ID() != root.ID || !m.Follow {
					t.Errorf("%s: agent view after a switch follows %+v", name, r)
				}
			}
			// The root agent no longer listed: the other agent's tile is
			// the viewer's only one.
			name := other.ID + " " + string(scope)
			m := model(input("", other), scope)
			if r := m.Selection(); r == nil || r.ID() != other.ID {
				t.Errorf("%s: only the other agent's tile: agent view follows %+v", name, r)
			}
			if id := m.followedID(); id != other.ID {
				t.Errorf("%s: only the other agent's tile: followedID %q", name, id)
			}
			m.Handle(term.Key{Kind: term.KeyTab})
			if r := m.Selection(); r == nil || r.ID() != wt {
				t.Errorf("%s: only the other agent's tile: tree follows %+v", name, r)
			}
		}
	}
}

// The agent view follows what the tree follows, whatever the tiles'
// sort, and the tree follows a line the viewer is on by its own session
// over one an agent of it visiting the viewer's session marks. With the
// viewer in vm/scratch, the attachment to a `new` session on vm:
//   - claude there at ~ and an idle claude started in a split of it in
//     proj/q's directory: the tree follows the session's own agent in
//     other sessions, not proj/q's line, which sorts first and the
//     visiting agent marks, and so does the agent view;
//   - an idle claude there and a working one observed in a window of
//     vm/scratch on mac's default server, both in other sessions: the
//     tree follows the managed one, first there, and so does the agent
//     view, though the observed one sorts first.
//
// With the viewer in vm/proj/z:
//   - its root agent working, and an idle claude observed in a window
//     of vm/proj/z on mac's default server in mac's worktree proj/a,
//     beside proj/a's own agent: the tree follows proj/z's line, not
//     proj/a's, which sorts first and the visiting agent marks, and the
//     agent view proj/z's root agent's tile;
//   - its root agent idle, and an idle claude started in a split of it
//     in proj/y's directory, beside proj/y's own working agent in
//     vm/proj/y: the same, though proj/y's line sorts first and its
//     agent's tile first among the viewer's (#253, #254);
//   - its root agent gone but on record and a working `cd ~ && claude`
//     in a split: the tree follows proj/z's line and the agent view the
//     gone root agent's tile.
//
// With the viewer in the untagged orphaned session vm/proj/old, and an
// agent observed in a window of it, which other sessions list before
// the orphaned line: the tree follows the orphaned line, and the agent
// view, which has no tile of it, the agent's (#273).
//
// With the viewer in the workspace session of a task before the
// listing: the task's line and its tile, and with the filter hiding the
// task's tile its add agent's. Each time the scope's worktree is the
// followed tile's.
func TestFollowWhatTheTreeFollows(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	src := "git@github.com:u/proj.git"
	hosts := []rows.Host{
		{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
		{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true},
	}
	agent := func(id, session, cwd, wt string, activity protocol.Activity, ago time.Duration) protocol.Agent {
		return protocol.Agent{ID: id, EnvironmentID: "venv", Server: "laatmux", Session: session, Agent: "claude", Activity: activity, ActivityAt: now.Add(-ago), Liveness: protocol.Alive, Managed: true, Cwd: cwd, WorktreeID: wt}
	}
	follows := func(name string, in rows.Input, tile, node string) {
		t.Helper()
		in.Hosts, in.Now = hosts, now
		for _, scope := range []Scope{ScopeAll, ScopeSession, ScopeProject} {
			m := &Model{Now: now, LocalHost: "mac", View: ViewAgents, Width: 80, Height: 30, Follow: true, Scope: scope}
			m.Set(rows.Tree(in), rows.Agents(in, rows.Tree(in)), nil)
			if r := m.Selection(); r == nil || r.ID() != tile {
				t.Errorf("%s, %s: agent view follows %+v, want %s; tiles:\n%s", name, scope, r, tile, ids(m))
			} else if w, _, _ := m.viewerWorktree(); m.tileWorktree(r) != w {
				t.Errorf("%s, %s: the followed tile's worktree %q, the scope's %q", name, scope, m.tileWorktree(r), w)
			}
			m.Handle(term.Key{Kind: term.KeyTab})
			if r := m.Selection(); r == nil || r.ID() != node {
				t.Errorf("%s, %s: tree follows %+v, want %s", name, scope, r, node)
			}
			m.Handle(term.Key{Kind: term.KeyTab})
			if r := m.Selection(); r == nil || r.ID() != tile {
				t.Errorf("%s, %s: agent view after a switch follows %+v, want %s", name, scope, r, tile)
			}
		}
	}
	scratch := []protocol.Session{{Name: "vm/scratch", Attach: "vm/scratch", Host: "vm"}}
	q := protocol.Worktree{ID: "venv/worktree//w/proj/q", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "q", Root: "/w/proj/q", Session: "proj/q"}
	visitor := agent("venv/laatmux/%4", "scratch", "/w/proj/q", q.ID, protocol.Idle, 10*time.Minute)
	visitor.Managed = false
	follows("a visiting agent", rows.Input{
		Agents:    []protocol.Agent{agent("venv/laatmux/%3", "scratch", "/home/u", "", protocol.Working, time.Minute), visitor},
		Worktrees: []protocol.Worktree{q},
		Locals:    scratch,
		Current:   "vm/scratch",
	}, "venv/laatmux/%3", "venv/laatmux/%3")
	observed := protocol.Agent{ID: "menv/default/%6", EnvironmentID: "menv", Server: "default", Session: "vm/scratch", Agent: "claude", Activity: protocol.Working, ActivityAt: now.Add(-time.Minute), Liveness: protocol.Alive, Cwd: "/Users/u"}
	follows("two in other sessions", rows.Input{
		Agents:  []protocol.Agent{agent("venv/laatmux/%3", "scratch", "/home/u", "", protocol.Idle, 10*time.Minute), observed},
		Locals:  scratch,
		Current: "vm/scratch",
	}, "venv/laatmux/%3", "venv/laatmux/%3")
	z := protocol.Worktree{ID: "venv/worktree//w/proj/z", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "z", Root: "/w/proj/z", Session: "proj/z"}
	zLocal := protocol.Session{Name: "vm/proj/z", Key: "venv//w/proj/z", Host: "vm"}
	a := protocol.Worktree{ID: "menv/worktree//m/proj/a", EnvironmentID: "menv", Repo: "proj", Source: src, Branch: "a", Root: "/m/proj/a", Session: "proj/a"}
	visiting := protocol.Agent{ID: "menv/default/%10", EnvironmentID: "menv", Server: "default", Session: "vm/proj/z", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-10 * time.Minute), Liveness: protocol.Alive, Cwd: a.Root, WorktreeID: a.ID}
	home := protocol.Agent{ID: "menv/laatmux/%4", EnvironmentID: "menv", Server: "laatmux", Session: "proj/a", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-20 * time.Minute), Liveness: protocol.Alive, Managed: true, Cwd: a.Root, WorktreeID: a.ID}
	follows("two worktrees", rows.Input{
		Agents:    []protocol.Agent{agent("venv/laatmux/%1", "proj/z", "/w/proj/z", z.ID, protocol.Working, time.Minute), visiting, home},
		Worktrees: []protocol.Worktree{z, a},
		Locals:    []protocol.Session{zLocal, {Name: "mac/proj/a", Key: "menv//m/proj/a", Host: "mac"}},
		Current:   "vm/proj/z",
	}, "venv/laatmux/%1", z.ID)
	y := protocol.Worktree{ID: "venv/worktree//w/proj/y", EnvironmentID: "venv", Repo: "proj", Source: src, Branch: "y", Root: "/w/proj/y", Session: "proj/y"}
	split := agent("venv/laatmux/%3", "proj/z", y.Root, y.ID, protocol.Idle, 10*time.Minute)
	split.Managed = false
	follows("a split's visitor", rows.Input{
		Agents:    []protocol.Agent{agent("venv/laatmux/%1", "proj/z", "/w/proj/z", z.ID, protocol.Idle, 20*time.Minute), agent("venv/laatmux/%2", "proj/y", y.Root, y.ID, protocol.Working, time.Minute), split},
		Worktrees: []protocol.Worktree{z, y},
		Locals:    []protocol.Session{zLocal, {Name: "vm/proj/y", Key: "venv//w/proj/y", Host: "vm"}},
		Current:   "vm/proj/z",
	}, "venv/laatmux/%1", z.ID)
	gone := agent("venv/laatmux/%1", "proj/z", "/w/proj/z", z.ID, protocol.Idle, 10*time.Minute)
	gone.Liveness = protocol.Gone
	follows("a gone root agent", rows.Input{
		Agents:    []protocol.Agent{gone, agent("venv/laatmux/%5", "proj/z", "/home/u", "", protocol.Working, time.Minute)},
		Worktrees: []protocol.Worktree{z},
		Locals:    []protocol.Session{zLocal},
		Current:   "vm/proj/z",
	}, gone.ID, z.ID)
	follows("an orphaned session", rows.Input{
		Agents:  []protocol.Agent{{ID: "menv/default/%6", EnvironmentID: "menv", Server: "default", Session: "vm/proj/old", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Cwd: "/Users/u"}},
		Locals:  []protocol.Session{{Name: "vm/proj/old", Key: "venv//w/proj/old", Host: "vm"}},
		Current: "vm/proj/old",
	}, "menv/default/%6", "session/vm/proj/old")
	follows("a task before the listing", rows.Input{
		Agents:   []protocol.Agent{agent("venv/laatmux/%7", "proj/n", "/w/proj/n", "", protocol.Working, 0)},
		Pendings: []protocol.Pending{{ID: "add-1", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "proj", Branch: "n", Root: "/w/proj/n", Session: "proj/n", SubmittedAt: now.Add(-time.Minute), Taken: true, Sent: true}},
		Locals:   []protocol.Session{{Name: "vm/proj/n", Key: "venv//w/proj/n", Host: "vm"}},
		Current:  "vm/proj/n",
	}, "add-1", "add-1")
	// The viewer in vm/proj/z, which has no agent on record, with a
	// visitor there: claude started in a split of it in proj/y's
	// directory, beside proj/y's own working agent in vm/proj/y; or
	// claude observed in a window of it on mac's default server in mac's
	// worktree proj/a, beside proj/a's own working agent. The tree
	// follows proj/z's line; the agent view, with no tile of proj/z,
	// follows the visitor's tile, the one in the viewer's session, not
	// the other worktree's own agent, which its line being the viewer's
	// makes the viewer's too and sorts first. A blocked `cd ~ && claude`
	// in a split of proj/z, in other sessions, is in the viewer's
	// session through proj/z's workspace session as much as an idle
	// claude observed in a window of it, and sorts first.
	busy := home
	busy.Activity, busy.ActivityAt = protocol.Working, now.Add(-time.Minute)
	stray := agent("venv/laatmux/%5", "proj/z", "/home/u", "", protocol.Blocked, time.Minute)
	idle := protocol.Agent{ID: "menv/default/%6", EnvironmentID: "menv", Server: "default", Session: "vm/proj/z", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-10 * time.Minute), Liveness: protocol.Alive, Cwd: "/Users/u"}
	for _, c := range []struct {
		visitor protocol.Agent
		in      rows.Input
	}{
		{stray, rows.Input{
			Agents:    []protocol.Agent{stray, idle},
			Worktrees: []protocol.Worktree{z},
			Locals:    []protocol.Session{zLocal},
		}},
		{split, rows.Input{
			Agents:    []protocol.Agent{agent("venv/laatmux/%2", "proj/y", y.Root, y.ID, protocol.Working, time.Minute), split},
			Worktrees: []protocol.Worktree{z, y},
			Locals:    []protocol.Session{zLocal, {Name: "vm/proj/y", Key: "venv//w/proj/y", Host: "vm"}},
		}},
		{visiting, rows.Input{
			Agents:    []protocol.Agent{visiting, busy},
			Worktrees: []protocol.Worktree{z, a},
			Locals:    []protocol.Session{zLocal, {Name: "mac/proj/a", Key: "menv//m/proj/a", Host: "mac"}},
		}},
	} {
		in := c.in
		in.Hosts, in.Now, in.Current = hosts, now, "vm/proj/z"
		for _, scope := range []Scope{ScopeAll, ScopeSession, ScopeProject} {
			m := &Model{Now: now, LocalHost: "mac", View: ViewAgents, Width: 80, Height: 30, Follow: true, Scope: scope}
			m.Set(rows.Tree(in), rows.Agents(in, rows.Tree(in)), nil)
			if r := m.Selection(); r == nil || r.ID() != c.visitor.ID {
				t.Errorf("%s, %s, no agent of proj/z: agent view follows %+v; tiles:\n%s", c.visitor.ID, scope, r, ids(m))
			}
			m.Handle(term.Key{Kind: term.KeyTab})
			if r := m.Selection(); r == nil || r.ID() != z.ID {
				t.Errorf("%s, %s, no agent of proj/z: tree follows %+v", c.visitor.ID, scope, r)
			}
		}
	}
	// The viewer in vm/proj/z, its root agent idle there and another of
	// its agents working in the managed session scratch, which has a plain
	// attachment: the viewer's own line is followed, and in the agent view
	// its first tile in sort order, the one in scratch, whichever is in
	// the viewer's session.
	follows("the viewer's worktree in two sessions", rows.Input{
		Agents:    []protocol.Agent{agent("venv/laatmux/%1", "proj/z", "/w/proj/z", z.ID, protocol.Idle, 10*time.Minute), agent("venv/laatmux/%9", "scratch", "/w/proj/z/sub", z.ID, protocol.Working, time.Minute)},
		Worktrees: []protocol.Worktree{z},
		Locals:    []protocol.Session{zLocal, {Name: "vm/scratch", Attach: "vm/scratch", Host: "vm"}},
		Current:   "vm/proj/z",
	}, "venv/laatmux/%9", z.ID)
	// The viewer in the plain session notes on mac's default server, with
	// claude observed there in mac's worktree proj/a, beside proj/a's own
	// working agent in its home: nothing is the viewer's by its own
	// session, so the tree follows proj/a's line, marked through the
	// visitor, and the agent view the visitor's tile, in the viewer's
	// session, not proj/a's own agent, which sorts first.
	inNotes := visiting
	inNotes.Session = "notes"
	follows("a visitor in a plain session", rows.Input{
		Agents:    []protocol.Agent{inNotes, busy},
		Worktrees: []protocol.Worktree{a},
		Locals:    []protocol.Session{{Name: "notes"}, {Name: "mac/proj/a", Key: "menv//m/proj/a", Host: "mac"}},
		Current:   "notes",
	}, inNotes.ID, a.ID)
	// The viewer in a plain attachment to the task's session, with and
	// without its workspace session, and a working `cd ~ && claude` in a
	// split of it, in other sessions in the viewer's session: the task's
	// line is the viewer's by the attachment to its home, and followed.
	for _, ws := range [][]protocol.Session{nil, {{Name: "vm/proj/n", Key: "venv//w/proj/n", Host: "vm"}}} {
		follows("a task's attachment", rows.Input{
			Agents:   []protocol.Agent{agent("venv/laatmux/%7", "proj/n", "/w/proj/n", "", protocol.Idle, 10*time.Minute), agent("venv/laatmux/%8", "proj/n", "/home/u", "", protocol.Working, time.Minute)},
			Pendings: []protocol.Pending{{ID: "add-1", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "proj", Branch: "n", Root: "/w/proj/n", Session: "proj/n", SubmittedAt: now.Add(-time.Minute), Taken: true, Sent: true}},
			Locals:   append([]protocol.Session{{Name: "vm/proj/n-1", Attach: "vm/proj/n", Host: "vm"}}, ws...),
			Current:  "vm/proj/n-1",
		}, "add-1", "add-1")
	}
	// The task's managed session other-n, and a working claude observed
	// in a window of its workspace session vm/other-n: with the filter
	// leaving the task's add agent and the observed one, not the task,
	// the agent view follows the add agent, of the task's worktree.
	in := rows.Input{
		Hosts:    hosts,
		Agents:   []protocol.Agent{agent("venv/laatmux/%7", "other-n", "/w/proj/n", "", protocol.Idle, 10*time.Minute), {ID: "menv/default/%6", EnvironmentID: "menv", Server: "default", Session: "vm/other-n", Agent: "claude", Activity: protocol.Working, ActivityAt: now.Add(-time.Minute), Liveness: protocol.Alive, Cwd: "/Users/u"}},
		Pendings: []protocol.Pending{{ID: "add-1", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "proj", Branch: "n", Root: "/w/proj/n", Session: "other-n", SubmittedAt: now.Add(-time.Minute), Taken: true, Sent: true}},
		Locals:   []protocol.Session{{Name: "vm/other-n", Key: "venv//w/proj/n", Host: "vm"}},
		Current:  "vm/other-n",
		Now:      now,
	}
	m := &Model{Now: now, LocalHost: "mac", View: ViewAgents, Width: 80, Height: 30, Follow: true, Filter: "other"}
	m.Set(rows.Tree(in), rows.Agents(in, rows.Tree(in)), nil)
	if r := m.Selection(); r == nil || r.ID() != "venv/laatmux/%7" {
		t.Errorf("the task's tile filtered away: follows %+v; tiles:\n%s", r, ids(m))
	} else if w, _, _ := m.viewerWorktree(); m.tileWorktree(r) != w {
		t.Errorf("the task's tile filtered away: the followed tile's worktree %q, the scope's %q", m.tileWorktree(r), w)
	}
}

// A task's first fold is by its agent's status; a task
// that handed over passes its fold to the node holding the children,
// and in the agent view the selection to the worktree's first tile;
// following survives a switch with the viewer's line folded away; f
// keeps the selection on its row.
func TestTreeEdges(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	src := "git@github.com:laat/laatmux.git"
	in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Server: "laatmux", Session: "laatmux/new-one", Agent: "claude", Activity: protocol.Idle,
		ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/r/new-one", Identity: &protocol.Identity{PID: 9, StartUnix: 9}})
	in.Pendings = []protocol.Pending{{ID: "add-1", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "laatmux", Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one", Taken: true, SubmittedAt: now}}
	m := &Model{Now: now, View: ViewTree, Width: 60, Height: 30}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.SetTree(rows.Tree(in))
	m.Render()
	if !m.closed(&m.Tree[m.indexOf("add-1")]) {
		t.Error("a task with an idle agent started open")
	}
	m.Select("add-1")
	m.Handle(term.Key{Rune: 'l'}) // the user's own fold: open
	// The host lists the worktree; the task hands over.
	in.Worktrees = append(in.Worktrees, protocol.Worktree{ID: "venv/worktree//r/new-one", EnvironmentID: "venv", Repo: "laatmux", Source: src, Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one"})
	in.Agents[len(in.Agents)-1].WorktreeID = "venv/worktree//r/new-one"
	in.Pendings = nil
	m.Handoffs = map[string]string{"add-1": "venv/worktree//r/new-one"}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.SetTree(rows.Tree(in))
	if m.closed(&m.Tree[m.indexOf("venv/worktree//r/new-one")]) {
		t.Error("the worktree line did not take the task's fold")
	}
	if r := m.Selection(); r == nil || r.ID() != "venv/worktree//r/new-one" {
		t.Errorf("selection after the handoff: %+v", r)
	}
	// In the agent view the handoff lands on the worktree's first tile.
	m.Handle(term.Key{Kind: term.KeyTab})
	m.Handle(term.Key{Kind: term.KeyTab})
	m.Select("add-1")
	m.View = ViewAgents
	m.anchor = "add-1"
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	if r := m.Selection(); r == nil || r.ID() != "venv/laatmux/%9" {
		t.Errorf("handoff in the agent view: %+v", r)
	}
	// Following: every fold closed (all are open, so one f closes
	// them), a switch away and back.
	m.View, m.Follow = ViewTree, true
	m.Selection()
	m.Handle(term.Key{Rune: 'f'})
	if len(m.Visible()) > 6 {
		t.Fatalf("not folded: %d", len(m.Visible()))
	}
	m.Handle(term.Key{Kind: term.KeyTab})
	m.Handle(term.Key{Kind: term.KeyTab})
	if r := m.Selection(); r == nil || r.ID() != "venv/worktree//r/agents-config" || !m.Follow {
		t.Errorf("following after a switch with folds closed: %+v", r)
	}
	// f keeps a user's selection on its row, or on the line over it.
	m.Handle(term.Key{Rune: 'f'}) // every fold open again
	if !m.Select("venv/laatmux/%2") {
		t.Fatal("the child is not visible")
	}
	m.Handle(term.Key{Rune: 'f'}) // closes every fold: the child is hidden
	if r := m.Selection(); r == nil || r.ID() != rows.RepoNode(src) {
		t.Errorf("f with the selection on a child: %+v", r)
	}
	m.Handle(term.Key{Rune: 'f'})
	if r := m.Selection(); r == nil || r.ID() != rows.RepoNode(src) {
		t.Errorf("f opening every fold moved the selection: %+v", r)
	}
	// f under a filter goes by the folds shown: with a hidden line
	// folded and the shown one open, the first f closes the shown.
	m.Follow = false
	shown, hidden := "venv/worktree//r/agents-config", "venv/worktree//r/auto-layout"
	m.Select(hidden)
	m.Handle(term.Key{Rune: 'h'})
	m.Select(shown)
	m.Handle(term.Key{Rune: 'l'})
	if !m.closed(&m.Tree[m.indexOf(hidden)]) || m.closed(&m.Tree[m.indexOf(shown)]) {
		t.Fatal("the folds before the filter")
	}
	m.Filter = "agents-config"
	m.Render()
	if vis := m.Visible(); len(vis) != 6 || vis[1].Row.ID() != shown {
		t.Fatalf("the filtered tree: %d rows", len(vis))
	}
	m.Handle(term.Key{Rune: 'f'})
	if !m.closed(&m.Tree[m.indexOf(shown)]) {
		t.Error("f under a filter opened by a hidden fold")
	}
	m.Filter = ""
	// A click on the tab shown does nothing; on the other, a switch.
	m.Tabs = true
	m.Render()
	m.Handle(term.Key{Kind: term.KeyMouse, X: 12, Y: 1})
	if m.View != ViewTree {
		t.Error("a click on the shown tab switched")
	}
	m.Handle(term.Key{Kind: term.KeyMouse, X: 4, Y: 1})
	if m.View != ViewAgents {
		t.Error("a click on the other tab did not switch")
	}
	m.Handle(term.Key{Kind: term.KeyMouse, X: 9, Y: 1})
	if m.View != ViewAgents {
		t.Error("a click between the tabs switched")
	}
}

// The pinned repository line takes a line of its own above the window,
// so the last row is still shown when the list is scrolled to its end.
func TestTreePinned(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	m := &Model{Now: now, LocalHost: "mac", View: ViewTree, Tabs: true, Width: 60, Height: 8}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.SetTree(rows.Tree(in))
	m.Render()
	vis := m.Visible()
	m.Handle(term.Key{Rune: 'G'})
	out := m.Render()
	last := vis[len(vis)-1].Row
	if r := m.Selection(); r == nil || r.ID() != last.ID() {
		t.Fatalf("G selected %+v", r)
	}
	text := Debug(out)
	if !strings.Contains(text, last.Name) {
		t.Errorf("the last row is not shown:\n%s", text)
	}
	if !strings.Contains(out[1].Spans[0].Text, "laatmux") || !out[1].Spans[0].Bold {
		t.Errorf("no pinned repository line:\n%s", text)
	}
	if got := m.hitIDs[0]; got != rows.RepoNode("git@github.com:laat/laatmux.git") {
		t.Errorf("the pinned line is %q in the hit map", got)
	}
	// Walking up to the top: the selection shown on every step, no row
	// under the more line, never "0 more", and a repository line at the
	// top of the window has no pin over it.
	walk := func(m *Model, key term.Key) {
		t.Helper()
		for step := 0; ; step++ {
			out := m.Render()
			text := Debug(out)
			// Stable: the render after a key is the one the next tick
			// draws.
			if again := Debug(m.Render()); again != text {
				t.Errorf("step %d: the next render differs:\n%s\nthen:\n%s", step, text, again)
			}
			if m.Selection() == nil || !strings.Contains(text, "\nS") {
				t.Errorf("step %d: the selection is not shown:\n%s", step, text)
			}
			if strings.Contains(text, "↓ 0 more") {
				t.Errorf("step %d: 0 more:\n%s", step, text)
			}
			for i, l := range out {
				if strings.HasPrefix(l.Spans[0].Text, "↓ ") && i < len(out)-2 {
					t.Errorf("step %d: a line under the more line:\n%s", step, text)
				}
			}
			if m.scroll > 0 && strings.Contains(out[1].Spans[0].Text, "laatmux") && out[1].Spans[0].Bold && out[2].Spans[0].Bold {
				t.Errorf("step %d: a repository pinned over itself:\n%s", step, text)
			}
			if m.Selected == 0 && key.Kind == term.KeyUp || m.Selected == len(m.Visible())-1 && key.Kind == term.KeyDown {
				break
			}
			m.Handle(key)
		}
	}
	for _, h := range []int{7, 8, 9} {
		m.Height = h
		m.Handle(term.Key{Rune: 'G'})
		walk(m, term.Key{Kind: term.KeyUp})
		m.Handle(term.Key{Rune: 'g'})
		walk(m, term.Key{Kind: term.KeyDown})
	}
	// Moving down to where the more line is needed pins the repository
	// in the same render.
	m.Height = 8
	m.Handle(term.Key{Rune: 'g'})
	m.Render()
	for i := 0; i < 5; i++ {
		m.Handle(term.Key{Kind: term.KeyDown})
	}
	if out := m.Render(); m.scroll == 0 || !out[1].Spans[0].Bold || m.hitIDs[0] != rows.RepoNode("git@github.com:laat/anki-llm") && m.hitIDs[0] != rows.RepoNode("git@github.com:laat/laatmux.git") {
		t.Errorf("scroll %d without a pin:\n%s", m.scroll, Debug(out))
	}
	// Rows in other sessions have no repository pinned over them.
	for i := 2; i <= 6; i++ {
		in.Agents = append(in.Agents, protocol.Agent{ID: "venv/default/%" + string(rune('0'+i)), EnvironmentID: "venv", Server: "default", Session: "s" + string(rune('0'+i)), Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive})
	}
	m.Height = 8
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Handle(term.Key{Rune: 'G'})
	text = Debug(m.Render())
	if strings.Contains(text, "laatmux") {
		t.Errorf("a repository pinned over other sessions:\n%s", text)
	}
	walk(m, term.Key{Kind: term.KeyUp})
}

// Two repositories with an empty worktree each and one observed agent
// in other sessions, in a five-line body: the pin and the more line
// settle on a layout that shows the selection at the end, blank lines
// left over or not.
func TestTreePinnedSmall(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := rows.Input{
		Hosts:  []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}},
		Agents: []protocol.Agent{{ID: "venv/default/%5", EnvironmentID: "venv", Server: "default", Session: "scratch", Agent: "claude", Activity: protocol.Idle, ActivityAt: now, Liveness: protocol.Alive}},
		Worktrees: []protocol.Worktree{
			{ID: "venv/worktree//r/a", EnvironmentID: "venv", Repo: "alpha", Source: "git@github.com:laat/alpha.git", Branch: "a", Root: "/r/a"},
			{ID: "venv/worktree//r/b", EnvironmentID: "venv", Repo: "beta", Source: "git@github.com:laat/beta.git", Branch: "b", Root: "/r/b"},
		},
		Now: now,
	}
	for h := 6; h <= 9; h++ {
		m := &Model{Now: now, View: ViewTree, Tabs: true, Width: 60, Height: h}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
		m.Render()
		m.Handle(term.Key{Rune: 'G'})
		text := Debug(m.Render())
		if !strings.Contains(text, "\nS") || strings.Contains(text, "↓ 0 more") {
			t.Errorf("height %d: the selection is not shown:\n%s", h, text)
		}
		if again := Debug(m.Render()); again != text {
			t.Errorf("height %d: the next render differs:\n%s\nthen:\n%s", h, text, again)
		}
		for m.Selected > 0 {
			m.Handle(term.Key{Kind: term.KeyUp})
			if text := Debug(m.Render()); !strings.Contains(text, "\nS") {
				t.Errorf("height %d: the selection is not shown:\n%s", h, text)
			}
		}
	}
}

// Three repositories, the last with an empty worktree, in a ten-line
// body: G shows the selection, a blank line left over rather than a
// pin and a more line fighting over the scroll.
func TestTreePinnedEnd(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := rows.Input{Hosts: []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true, Attribution: true}}, Now: now}
	n := 0
	for r, counts := range map[string][]int{"r0": {2, 3}, "r1": {3, 1}, "r2": {0}} {
		for i, agents := range counts {
			w := protocol.Worktree{ID: "venv/worktree//" + r + "/w" + string(rune('0'+i)), EnvironmentID: "venv", Repo: r, Source: "git@github.com:laat/" + r + ".git", Branch: "w" + string(rune('0'+i)), Root: "/" + r + "/w" + string(rune('0'+i)), Session: r + "/w" + string(rune('0'+i))}
			in.Worktrees = append(in.Worktrees, w)
			for k := 0; k < agents; k++ {
				n++
				in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%" + strconv.Itoa(n), EnvironmentID: "venv", Server: "laatmux", Session: w.Session, Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: w.ID, Identity: &protocol.Identity{PID: n, StartUnix: int64(n)}})
			}
		}
	}
	m := &Model{Now: now, View: ViewTree, Width: 60, Height: 11}
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Render()
	m.Handle(term.Key{Rune: 'G'})
	out := m.Render()
	text := Debug(out)
	if !strings.Contains(text, "\nS") || strings.Contains(text, "↓") {
		t.Errorf("G at the end:\n%s", text)
	}
	// The window starts on the second repository's line, which needs
	// no pin.
	if !out[0].Spans[0].Bold || m.hitIDs[0] != rows.RepoNode("git@github.com:laat/r1.git") || m.scroll != 8 {
		t.Errorf("the top of the window at scroll %d:\n%s", m.scroll, text)
	}
	if again := Debug(m.Render()); again != text {
		t.Errorf("the next render differs:\n%s\nthen:\n%s", text, again)
	}
}

// A task handing over while another stands for its worktree: the
// selection follows to the worktree's line in the tree and to its first
// agent's tile in the agent view, not to the other task; the owner's
// fold goes with it. An owner that fails after the worktree is made
// passes its fold to the next task, with no handoff.
func TestHandoffStanding(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	src := "git@github.com:laat/laatmux.git"
	in := treeInput(now)
	in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Server: "laatmux", Session: "laatmux/new-one", Agent: "claude", Activity: protocol.Idle,
		ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/r/new-one", Identity: &protocol.Identity{PID: 9, StartUnix: 9}})
	task := func(id string, at time.Time) protocol.Pending {
		return protocol.Pending{ID: id, Host: "vm", EnvironmentID: "venv", Source: src, Repo: "laatmux", Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one", Taken: true, SubmittedAt: at}
	}
	in.Pendings = []protocol.Pending{task("add-1", now.Add(-time.Minute)), task("add-2", now)}
	set := func(m *Model) {
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in, rows.Tree(in)))
	}
	m := &Model{Now: now, View: ViewTree, Width: 60, Height: 30}
	set(m)
	m.Render()
	if m.closed(&m.Tree[m.indexOf("add-2")]) != true {
		t.Fatal("the owner with an idle agent started open")
	}
	m.Select("add-2")
	m.Handle(term.Key{Rune: 'l'}) // opened by the user
	// add-2 hands over; add-1 stands still.
	in.Worktrees = append(in.Worktrees, protocol.Worktree{ID: "venv/worktree//r/new-one", EnvironmentID: "venv", Repo: "laatmux", Source: src, Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one"})
	in.Agents[len(in.Agents)-1].WorktreeID = "venv/worktree//r/new-one"
	in.Pendings = in.Pendings[:1]
	m.Handoffs = map[string]string{"add-2": "venv/worktree//r/new-one"}
	set(m)
	owner := m.successor("venv/worktree//r/new-one")
	if owner != "add-1" {
		t.Fatalf("the standing task does not own the children: %q", owner)
	}
	if r := m.Selection(); r == nil || r.ID() != "add-1" {
		t.Errorf("selection after the handoff with a task standing: %+v", r)
	}
	if m.closed(&m.Tree[m.indexOf("add-1")]) {
		t.Error("the next owner did not take the fold")
	}
	// The same in the agent view: the first agent's tile, not add-1's.
	a := &Model{Now: now, View: ViewAgents, Width: 60, Height: 30}
	in.Pendings = []protocol.Pending{task("add-1", now.Add(-time.Minute)), task("add-2", now)}
	in.Worktrees = in.Worktrees[:len(in.Worktrees)-1]
	in.Agents[len(in.Agents)-1].WorktreeID = ""
	set(a)
	a.Render()
	a.Select("add-2")
	in.Worktrees = append(in.Worktrees, protocol.Worktree{ID: "venv/worktree//r/new-one", EnvironmentID: "venv", Repo: "laatmux", Source: src, Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one"})
	in.Agents[len(in.Agents)-1].WorktreeID = "venv/worktree//r/new-one"
	in.Pendings = in.Pendings[:1]
	a.Handoffs = map[string]string{"add-2": "venv/worktree//r/new-one"}
	set(a)
	if r := a.Selection(); r == nil || r.ID() != "venv/laatmux/%9" {
		t.Errorf("selection in the agent view: %+v", r)
	}
	// add-1 owns the children now, open by the carried fold; the user
	// folds it, then it fails: the worktree line takes the children and
	// the closed fold, with no handoff.
	m.Handoffs = nil
	m.Select("add-1")
	m.Handle(term.Key{Rune: 'h'})
	if !m.closed(&m.Tree[m.indexOf("add-1")]) {
		t.Fatal("h did not fold the owner")
	}
	in.Pendings = append([]protocol.Pending(nil), in.Pendings...)
	in.Pendings[0].Done, in.Pendings[0].OK, in.Pendings[0].Error = true, false, "failed at agent: boom"
	set(m)
	if owner := m.successor("venv/worktree//r/new-one"); owner != "venv/worktree//r/new-one" {
		t.Fatalf("the worktree line does not own the children: %q", owner)
	}
	if !m.closed(&m.Tree[m.indexOf("venv/worktree//r/new-one")]) {
		t.Error("the worktree line did not take the failed owner's fold")
	}
	if _, ok := m.folds["add-1"]; ok {
		t.Error("the failed task's fold was not consumed")
	}
	// Two tasks at one root before the listing: the newest holds the
	// add's agent; opened by the user, then failing, it passes the fold
	// to the older, with no handoff and no worktree record.
	in = treeInput(now)
	in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Server: "laatmux", Session: "laatmux/new-one", Agent: "claude", Activity: protocol.Idle,
		ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/r/new-one", Identity: &protocol.Identity{PID: 9, StartUnix: 9}})
	in.Pendings = []protocol.Pending{task("add-1", now.Add(-time.Minute)), task("add-2", now)}
	m = &Model{Now: now, View: ViewTree, Width: 60, Height: 30}
	set(m)
	m.Render()
	if m.successor("venv/worktree//r/new-one") != "add-2" || m.Tree[m.indexOf("add-2")].Children != 1 {
		t.Fatalf("the newest loose task does not hold the agent: %q", m.successor("venv/worktree//r/new-one"))
	}
	m.Select("add-2")
	m.Handle(term.Key{Rune: 'l'})
	// A fresh record: the tree's rows point into the input's.
	in.Pendings = append([]protocol.Pending(nil), in.Pendings...)
	in.Pendings[1].Done, in.Pendings[1].OK, in.Pendings[1].Error = true, false, "failed at agent: boom"
	set(m)
	if m.successor("venv/worktree//r/new-one") != "add-1" {
		t.Fatalf("the older task does not take the agent: %q", m.successor("venv/worktree//r/new-one"))
	}
	if m.closed(&m.Tree[m.indexOf("add-1")]) {
		t.Errorf("the older task did not take the failed owner's open fold: folds %v toggled %v", m.folds, m.toggled)
	}
}

// A switch to the agent view whose target is a stale tile opens the
// stale fold and lands on it.
func TestSwitchToStale(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	// auto-layout's agent, no longer done and two hours idle.
	in.Attention = nil
	for i := range in.Agents {
		if in.Agents[i].ID == "venv/laatmux/%8" {
			in.Agents[i].ActivityAt = now.Add(-2 * time.Hour)
		}
	}
	m := &Model{Now: now, LocalHost: "mac", View: ViewTree, Tabs: true, Width: 60, Height: 30}
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.SetTree(rows.Tree(in))
	m.Render()
	if len(m.Rows.Stale) != 1 {
		t.Fatalf("stale tiles: %d", len(m.Rows.Stale))
	}
	m.Handle(term.Key{Rune: 'f'}) // every fold open
	if !m.Select("venv/laatmux/%8") {
		t.Fatal("the stale agent is not visible in the tree")
	}
	m.Handle(term.Key{Kind: term.KeyTab})
	if r := m.Selection(); r == nil || r.ID() != "venv/laatmux/%8" || !m.ShowHidden {
		t.Errorf("switch to a stale tile: %+v shown %v", r, m.ShowHidden)
	}
}

// The modes are settled by order: an overlay over a confirm line over a
// filter over the list, and each key goes to the top one.
func TestMode(t *testing.T) {
	var m Model
	if m.mode() != modeList {
		t.Fatal("an empty model is not the list")
	}
	m.Filtering = true
	if m.mode() != modeFilter {
		t.Fatal("filtering is not the filter's")
	}
	m.Confirm = "sure?"
	if m.mode() != modeConfirm {
		t.Fatal("a confirm line over the filter is not the confirm's")
	}
	m.Overlay = NewLog("add")
	if m.mode() != modeOverlay {
		t.Fatal("an overlay over the confirm line is not the overlay's")
	}
	// The key goes to the overlay: the confirm line and the filter are
	// untouched for after it.
	m.Handle(term.Key{Rune: 'n'})
	if m.Confirm != "sure?" || !m.Filtering {
		t.Fatalf("the overlay's key reached the confirm line or the filter: %q %v", m.Confirm, m.Filtering)
	}
}

// The row's number reaches the tiles and compact layouts: {idx} in every
// slot counts the numbered rows, the title line included.
func TestIdxLayouts(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for _, lay := range []Layout{Tiles, Compact} {
		m := model(now)
		m.Layout, m.Titles, m.Width, m.Height = lay, true, 60, 60
		m.SetTemplates(CompileTemplates([]string{"<{idx}>{primary}", "", "[{idx}]"}, "<{idx}>{primary}", nil, "", "", "", "", ""))
		text := Text(m.Render())
		n := 0
		for _, it := range m.Visible() {
			if it.Row.Numbered() {
				n++
				for _, want := range []string{"<" + strconv.Itoa(n) + ">", "[" + strconv.Itoa(n) + "]"} {
					if !strings.Contains(text, want) {
						t.Errorf("%v: row %d without %s:\n%s", lay, n, want, text)
					}
				}
			}
		}
		if n < 2 {
			t.Fatalf("%v: %d numbered rows", lay, n)
		}
	}
}

// Set's order: the tree before the tiles. A task tile handed over to a
// worktree that gained a first agent lands on that agent in the new
// tree's order; the tiles reselected against the old tree would stay on
// the agent the task had.
func TestSetFollowsTheNewTree(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	hosts := []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}}
	other := protocol.Worktree{ID: "venv/worktree//r/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/r/a"}
	wt := protocol.Worktree{ID: "venv/worktree//r/task", EnvironmentID: "venv", Repo: "proj", Branch: "task", Root: "/r/task", Session: "proj/task"}
	task := protocol.Pending{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "task", Root: "/r/task", Session: "proj/task", Taken: true, Reachable: true, Stage: protocol.StageAgent, SubmittedAt: now}
	a9 := protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Server: "laatmux", Session: "proj/task", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true}
	a8 := a9
	a8.ID = "venv/laatmux/%8"
	m := &Model{Width: 60, Height: 20, Now: now}
	old := rows.Input{Hosts: hosts, Pendings: []protocol.Pending{task}, Worktrees: []protocol.Worktree{other, wt}, Agents: []protocol.Agent{a9}}
	ot := rows.Tree(old)
	m.Set(ot, rows.Agents(old, ot), nil)
	if !m.Select("add-1") {
		t.Fatal("no task tile")
	}
	in := rows.Input{Hosts: hosts, Worktrees: []protocol.Worktree{other, wt}, Agents: []protocol.Agent{a9, a8}}
	tree := rows.Tree(in)
	m.Set(tree, rows.Agents(in, tree), map[string]string{"add-1": wt.ID})
	if r := m.Selection(); r == nil || r.ID() != a8.ID {
		t.Fatalf("not on the worktree's first agent in the new tree's order: %+v", r)
	}
}
