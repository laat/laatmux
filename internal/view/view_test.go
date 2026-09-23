package view

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/workspace"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fixture is a listing with one of everything: the three activities, a
// gone agent, a worktree without a session, one without an agent, a
// managed agent with no worktree, observed agents on the local and a
// remote default server, a host down, a settled and a stale workspace.
func fixture(now time.Time) rows.Rows { return rows.Build(fixtureInput(now)) }

func fixtureInput(now time.Time) rows.Input {
	return rows.Input{
		Hosts: []rows.Host{
			{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true},
			{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true},
			{Name: "box", EnvironmentID: "benv", Error: "ssh: connect to host box port 22: No route to host"},
		},
		Agents: []protocol.Agent{
			{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "laatmux/fix-ls", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now.Add(-2 * time.Minute), Liveness: protocol.Alive, Managed: true, Title: "Permission to run pnpm test in packages/api?"},
			{ID: "menv/laatmux/%2", EnvironmentID: "menv", Session: "proj/task", Agent: "codex", Activity: protocol.Working, ActivityAt: now.Add(-8 * time.Second), Liveness: protocol.Alive, Managed: true, Title: "Editing src/api.ts"},
			{ID: "venv/laatmux/%3", EnvironmentID: "venv", Session: "proj/other", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-time.Hour), Liveness: protocol.Alive, Managed: true, Title: "✳ Done. 日本語のタイトル that is long enough to be trimmed at the edge"},
			{ID: "venv/laatmux/%4", EnvironmentID: "venv", Session: "proj/dead", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-3 * time.Hour), Liveness: protocol.Gone, Managed: true},
			{ID: "menv/laatmux/%5", EnvironmentID: "menv", Session: "scratch", Agent: "", Activity: protocol.Idle, ActivityAt: now.Add(-40 * time.Second), Liveness: protocol.Alive, Managed: true, Title: "zsh"},
			{ID: "menv/default/%6", EnvironmentID: "menv", Server: "default", Session: "notes", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-5 * time.Minute), Liveness: protocol.Alive, Title: "notes"},
			{ID: "venv/default/%7", EnvironmentID: "venv", Server: "default", Session: "remote-notes", Agent: "codex", Activity: protocol.Working, ActivityAt: now.Add(-time.Second), Liveness: protocol.Alive, Title: "remote"},
			{ID: "benv/laatmux/%8", EnvironmentID: "benv", Session: "proj/down", Agent: "claude", Activity: protocol.Working, ActivityAt: now.Add(-10 * time.Minute), Liveness: protocol.Alive, Managed: true, Title: "last seen"},
			{ID: "menv/laatmux/%9", EnvironmentID: "menv", Session: "proj/done", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-26 * time.Hour), Liveness: protocol.Alive, Managed: true, Title: "finished"},
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
		Locals: []workspace.Local{
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
	return &Model{Rows: fixture(now), LocalHost: "mac", Now: now, Header: []string{"box  DOWN  ssh: connect to host box port 22: No route to host"}}
}

// The tile layout at the sidebar's default width: each tile is the mark
// and name with the host tag right-aligned, dim for every host but the
// local one, the agent, activity and age, and the title trimmed to the
// width; rows without an agent say what they are instead; the current
// session is marked in the gutter; the settled and stale groups are
// collapsed to one line.
func TestRenderTiles(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Tiles, 35, 40
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
	if got := m.hit(1); got < 0 || m.Visible()[got].Row.Name == "laatmux/fix-ls" {
		t.Errorf("first line maps to row %d after scrolling", got)
	}
	// Back to the top: the scroll follows.
	m.Selected = 0
	txt = Text(m.Render())
	if !strings.HasPrefix(txt, " ! laatmux/fix-ls") {
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
	m.Handle(Key{Rune: 'k'})
	if m.Selected != 0 {
		t.Error("k at the top moved")
	}
	m.Handle(Key{Rune: 'G'})
	m.Handle(Key{Rune: 'j'})
	if m.Selected != n-1 {
		t.Errorf("G then j = %d, want %d", m.Selected, n-1)
	}
	m.Handle(Key{Rune: 'g'})
	if a := m.Handle(Key{Rune: '3'}); a.Kind != ActionJump || m.Selection().Name != m.Visible()[2].Row.Name {
		t.Errorf("3 = %+v on %q", a, m.Selection().Name)
	}
	if a := m.Handle(Key{Kind: KeyEnter}); a.Kind != ActionJump {
		t.Errorf("enter = %+v", a)
	}
	m.Handle(Key{Rune: 'v'})
	if m.Layout != Tiles {
		t.Error("v did not toggle")
	}
	for _, r := range "/dea" {
		m.Handle(Key{Rune: r})
	}
	if !m.Filtering || m.Filter != "dea" || len(m.Visible()) != 1 || m.Visible()[0].Row.Name != "proj/dead" {
		t.Errorf("filter: %q filtering=%v visible=%d", m.Filter, m.Filtering, len(m.Visible()))
	}
	m.Handle(Key{Kind: KeyBackspace})
	if m.Filter != "de" {
		t.Errorf("backspace: %q", m.Filter)
	}
	if a := m.Handle(Key{Kind: KeyEnter}); a.Kind != ActionNone || m.Filtering || m.Filter != "de" {
		t.Errorf("enter while filtering: %+v filtering=%v filter=%q", a, m.Filtering, m.Filter)
	}
	m.Handle(Key{Kind: KeyEsc})
	if m.Filter != "" || len(m.Visible()) != n {
		t.Errorf("esc did not clear the filter: %q", m.Filter)
	}
	m.Handle(Key{Rune: 'f'})
	if len(m.Visible()) != n+2 {
		t.Errorf("f showed %d rows, want %d", len(m.Visible()), n+2)
	}
	// The settled row is in its own group: 1 there is the settled row.
	m.Handle(Key{Rune: 'G'})
	m.Handle(Key{Rune: 'k'})
	if a := m.Handle(Key{Rune: '1'}); a.Kind != ActionJump || !m.Selection().Settled {
		t.Errorf("1 in the settled group = %+v on %q", a, m.Selection().Name)
	}
	if a := m.Handle(Key{Rune: '2'}); a.Kind != ActionNone {
		t.Errorf("2 past the end of the group = %+v", a)
	}
	m.Handle(Key{Rune: 'f'})
	m.Handle(Key{Rune: 'g'})
	m.Layout = Compact
	m.Render()
	if a := m.Handle(Key{Kind: KeyMouse, X: 3, Y: 1 + len(m.Header) + 2}); a.Kind != ActionJump || m.Selected != 2 {
		t.Errorf("click on the third row = %+v selected %d", a, m.Selected)
	}
	if a := m.Handle(Key{Kind: KeyMouse, X: 3, Y: 1 + len(m.Header) + n + 5}); a.Kind != ActionNone {
		t.Errorf("click below the list = %+v", a)
	}
	m.Handle(Key{Kind: KeyMouse, Wheel: 1})
	if m.Selected != 3 {
		t.Errorf("wheel down = %d", m.Selected)
	}
	if a := m.Handle(Key{Rune: 'x'}); a.Kind != ActionOther || a.Key.Rune != 'x' {
		t.Errorf("unknown key = %+v", a)
	}
	if a := m.Handle(Key{Rune: 'q'}); a.Kind != ActionQuit {
		t.Errorf("q = %+v", a)
	}
	m.Message = "hello"
	m.Handle(Key{Rune: 'j'})
	if m.Message != "" {
		t.Error("message survived a key")
	}
	m.Rows = rows.Rows{}
	if a := m.Handle(Key{Kind: KeyEnter}); a.Kind != ActionNone {
		t.Errorf("enter on an empty list = %+v", a)
	}
}

func TestParse(t *testing.T) {
	cases := map[string][]Key{
		"j":             {{Rune: 'j'}},
		"\x1b[A\x1b[B":  {{Kind: KeyUp}, {Kind: KeyDown}},
		"\x1bOA":        {{Kind: KeyUp}},
		"\x1b":          {{Kind: KeyEsc}},
		"\r":            {{Kind: KeyEnter}},
		"\x7f":          {{Kind: KeyBackspace}},
		"\x03":          {{Kind: KeyCtrlC}},
		"\x1b[<0;12;5M": {{Kind: KeyMouse, X: 12, Y: 5}},
		"\x1b[<0;12;5m": {{Kind: -1}},
		"\x1b[<64;1;1M": {{Kind: KeyMouse, X: 1, Y: 1, Wheel: -1}},
		"\x1b[<65;1;1M": {{Kind: KeyMouse, X: 1, Y: 1, Wheel: 1}},
		"\x1b[<2;1;1M":  {{Kind: -1}},
		"\x1b[<32;1;1M": {{Kind: -1}},
		"ø":             {{Rune: 'ø'}},
		"\x1b[1;5Cq":    {{Kind: -1}, {Rune: 'q'}},
		"\x1bj":         {{Kind: KeyEsc}, {Rune: 'j'}},
	}
	for in, want := range cases {
		got := Parse([]byte(in))
		if len(got) != len(want) {
			t.Errorf("Parse(%q) = %+v, want %+v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("Parse(%q)[%d] = %+v, want %+v", in, i, got[i], want[i])
			}
		}
	}
}

// Input split across reads: a multi-byte rune, an arrow and a mouse
// report arrive whole once their bytes are in; a bare escape is held
// until Flush, and an invalid byte is dropped, never a panic.
func TestDecoderSplit(t *testing.T) {
	for _, in := range []string{"ø", "\x1b[A", "\x1b[<0;12;5M", "j\x1b[Bk", "日本"} {
		want := Parse([]byte(in))
		for cut := 1; cut < len(in); cut++ {
			var d Decoder
			got := d.Feed([]byte(in[:cut]))
			got = append(got, d.Feed([]byte(in[cut:]))...)
			if d.Pending() {
				t.Errorf("%q split at %d: still pending", in, cut)
			}
			if len(got) != len(want) {
				t.Errorf("%q split at %d = %+v, want %+v", in, cut, got, want)
				continue
			}
			for i := range got {
				if got[i] != want[i] {
					t.Errorf("%q split at %d: key %d = %+v, want %+v", in, cut, i, got[i], want[i])
				}
			}
		}
	}
	var d Decoder
	if got := d.Feed([]byte{0x1b}); len(got) != 0 || !d.Pending() {
		t.Errorf("bare escape read at once: %+v", got)
	}
	if got := d.Flush(); len(got) != 1 || got[0].Kind != KeyEsc || d.Pending() {
		t.Errorf("flushed escape = %+v", got)
	}
	if got := d.Feed([]byte{0xc3}); len(got) != 0 || !d.Pending() {
		t.Errorf("partial rune read at once: %+v", got)
	}
	if got := d.Flush(); len(got) != 0 {
		t.Errorf("flushed partial rune = %+v", got)
	}
	// A mouse report cut short is dropped on flush, never read as an
	// escape and the digits 1 and 2, which would jump.
	if got := d.Feed([]byte("\x1b[<0;12;")); len(got) != 0 || !d.Pending() {
		t.Errorf("partial mouse report read at once: %+v", got)
	}
	if got := d.Flush(); len(got) != 0 {
		t.Errorf("flushed partial mouse report = %+v", got)
	}
	// Its tail arriving after the flush is swallowed through the final
	// byte; the key after it is read.
	if got := d.Feed([]byte("5M")); len(got) != 0 {
		t.Errorf("late tail of a mouse report = %+v", got)
	}
	if got := d.Feed([]byte("j")); len(got) != 1 || got[0].Rune != 'j' {
		t.Errorf("key after a swallowed tail = %+v", got)
	}
	if got := d.Feed([]byte("\x1b[<0;1")); len(got) != 0 {
		t.Errorf("second partial report = %+v", got)
	}
	d.Flush()
	if got := d.Feed([]byte("2;3Mk")); len(got) != 1 || got[0].Rune != 'k' {
		t.Errorf("tail and key in one read = %+v", got)
	}
	// A fresh report while discarding is a new sequence, read whole, not
	// a tail whose digits leak out.
	d.Feed([]byte("\x1b[<0;1"))
	d.Flush()
	if got := d.Feed([]byte("\x1b[<0;12;5M")); len(got) != 1 || got[0].Kind != KeyMouse || got[0].X != 12 {
		t.Errorf("fresh report during discard = %+v", got)
	}
	d.Feed([]byte("\x1bO"))
	d.Flush()
	if got := d.Feed([]byte("\x1b")); len(got) != 0 || !d.Pending() {
		t.Errorf("fresh escape during discard = %+v", got)
	}
	if got := d.Flush(); len(got) != 1 || got[0].Kind != KeyEsc {
		t.Errorf("flushed fresh escape = %+v", got)
	}
	if got := d.Feed([]byte("\x1bO")); len(got) != 0 {
		t.Errorf("partial SS3 read at once: %+v", got)
	}
	if got := d.Flush(); len(got) != 0 {
		t.Errorf("flushed partial SS3 = %+v", got)
	}
	// Escape then a key that is no sequence is both, at once.
	if got := d.Feed([]byte("\x1bj")); len(got) != 2 || got[0].Kind != KeyEsc || got[1].Rune != 'j' || d.Pending() {
		t.Errorf("escape then j = %+v", got)
	}
	if got := Parse([]byte{0xc3, 'j'}); len(got) != 1 || got[0].Rune != 'j' {
		t.Errorf("invalid byte then j = %+v", got)
	}
}

// A refresh that reorders or removes rows keeps the selection on the
// same row, not the same index.
func TestSetRowsKeepsSelection(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Compact, 80, 30
	m.Handle(Key{Rune: 'j'})
	m.Handle(Key{Rune: 'j'})
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
	m.SetRows(rows.Build(rows.Input{}))
	m.SetRows(rows.Build(in))
	if got := m.Selection(); got.Name != "proj/task" || m.Selected != 1 {
		t.Errorf("after reorder: selected %q at %d, want proj/task at 1", got.Name, m.Selected)
	}
	// The row is gone: the index stays, clamped.
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
	m.SetRows(rows.Build(in))
	if got := m.Selection(); got == nil || got.Name == "proj/task" || m.Selected != 1 {
		t.Errorf("after removal: %+v index %d", got, m.Selected)
	}
}

func TestWidth(t *testing.T) {
	if w := width("日本"); w != 4 {
		t.Errorf("wide = %d", w)
	}
	// An emoji is two cells; a variation selector, a skin tone and a
	// joiner add none, so a joined sequence measures as its base emoji.
	if w := width("\U0001f600"); w != 2 {
		t.Errorf("emoji = %d", w)
	}
	if w := width("\U0001f44d\U0001f3fd"); w != 2 {
		t.Errorf("emoji with skin tone = %d", w)
	}
	if w := width("\u2764\ufe0f"); w != 1 {
		t.Errorf("heart with variation selector = %d", w)
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
	m.SetRows(rows.Build(in))
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
	m.SetRows(rows.Build(in))
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
	m.SetRows(rows.Build(in))
	if got := m.Selection(); got != nil || m.Selected != -1 {
		t.Fatalf("no current row: %+v at %d", got, m.Selected)
	}
	for _, l := range m.Render() {
		if l.Reverse {
			t.Fatal("a row drawn selected with nothing selected")
		}
	}
	if a := m.Handle(Key{Kind: KeyEnter}); a.Kind != ActionNone {
		t.Fatalf("Enter with nothing selected: %+v", a)
	}
	if a := m.Handle(Key{Rune: '2'}); a.Kind != ActionJump || m.Selected != 1 || m.Follow {
		t.Fatalf("digit with nothing selected: %+v at %d follow=%v", a, m.Selected, m.Follow)
	}
	// Following again, then a key: the selection is the user's and a
	// reorder keeps it on the row it was on, not on the viewer's.
	m.Follow = true
	in.Current = "mac/proj/task"
	m.SetRows(rows.Build(in))
	if m.Selected != 1 {
		t.Fatalf("following again: %d", m.Selected)
	}
	m.Handle(Key{Rune: 'j'})
	taken := m.Selection()
	if m.Follow || taken == nil || taken.Name == "proj/task" || m.Selected != 2 {
		t.Fatalf("after j: %+v at %d follow=%v", taken, m.Selected, m.Follow)
	}
	for i := range in.Agents {
		if in.Agents[i].Session == "laatmux/fix-ls" {
			in.Agents[i].Activity = protocol.Blocked
		}
	}
	m.SetRows(rows.Build(in))
	if got := m.Selection(); got == nil || got.Name != taken.Name {
		t.Fatalf("user's selection moved: %+v, was %s", got, taken.Name)
	}
	// Without Follow the model is as it was: the first row selected.
	m = model(now)
	m.SetRows(rows.Build(fixtureInput(now)))
	if m.Selected != 0 {
		t.Fatalf("without follow: %d", m.Selected)
	}
}
