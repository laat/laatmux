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
	if got := m.hitRow(1, time.Time{}); got < 0 || m.Visible()[got].Row.Name == "laatmux/fix-ls" {
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
	if a := m.Handle(Key{Kind: KeyNewline}); a.Kind != ActionJump {
		t.Errorf("newline = %+v", a)
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
		"j":              {{Rune: 'j'}},
		"\x1b[A\x1b[B":   {{Kind: KeyUp}, {Kind: KeyDown}},
		"\x1bOA":         {{Kind: KeyUp}},
		"\x1b":           {{Kind: KeyEsc}},
		"\r":             {{Kind: KeyEnter}},
		"\x7f":           {{Kind: KeyBackspace}},
		"\x03":           {{Kind: KeyCtrlC}},
		"\x1b[<0;12;5M":  {{Kind: KeyMouse, X: 12, Y: 5}},
		"\x1b[<0;12;5m":  {{Kind: -1}},
		"\x1b[<64;1;1M":  {{Kind: KeyMouse, X: 1, Y: 1, Wheel: -1}},
		"\x1b[<65;1;1M":  {{Kind: KeyMouse, X: 1, Y: 1, Wheel: 1}},
		"\x1b[<2;1;1M":   {{Kind: -1}},
		"\x1b[<32;1;1M":  {{Kind: -1}},
		"ø":              {{Rune: 'ø'}},
		"\x1b[1;5Cq":     {{Kind: -1}, {Rune: 'q'}},
		"\x1bj":          {},
		"\x1b[13;2u":     {{Kind: KeyNewline}},
		"\x1b[27;2;13~":  {{Kind: KeyNewline}},
		"\x1b[27;5;13~":  {{Kind: KeyNewline}},
		"\x1b[13u":       {{Kind: KeyEnter}},
		"\x1b[27;1;13~":  {{Kind: KeyEnter}},
		"\x1b[9;2u":      {{Kind: KeyShiftTab}},
		"\x1b[27u":       {{Kind: KeyEsc}},
		"\x1b[99;5u":     {{Kind: KeyCtrlC}},
		"\x1b[97;5u":     {{Kind: -1}},
		"\x1b[97u":       {{Rune: 'a'}},
		"\x1b[97:65;2u":  {{Rune: 'A'}},
		"\x1b[27;2;97~":  {{Rune: 'A'}},
		"\x1b[97;2u":     {{Rune: 'A'}},
		"\x1b[106;5u":    {{Kind: KeyNewline}},
		"\x1b[27;3;13~":  {{Kind: KeyNewline}},
		"\x1b[27;3;120~": {{Kind: -1}},
		"\x1b\x7f":       {},
		"\x1b\r":         {{Kind: KeyNewline}},
		"\x1bOP":         {},
		"\x1b[49;2u":     {{Kind: -1}},
		"\x1b\x1b":       {{Kind: KeyEsc}, {Kind: KeyEsc}},
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
	// Escape then a key that is no sequence, in one read, is an Alt
	// chord: neither the Esc that cancels nor the key.
	if got := d.Feed([]byte("\x1bj")); len(got) != 0 || d.Pending() {
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
	m.SetRows(rows.Build(in))
	// The row is gone: the selection is on none, not on the row that
	// took its index, so Enter jumps nowhere.
	if got := m.Selection(); got != nil || m.Selected != -1 {
		t.Errorf("after removal: %+v index %d", got, m.Selected)
	}
	if a := m.Handle(Key{Kind: KeyEnter}); a.Kind != ActionNone {
		t.Errorf("enter on no selection = %+v", a)
	}
	m.Render()
	if m.Selection() != nil {
		t.Error("a render put the selection back on a row")
	}
	// A key moves it onto a row again, the user's.
	m.Handle(Key{Rune: 'j'})
	if got := m.Selection(); got == nil || m.Selected != 0 {
		t.Errorf("j after removal: %+v index %d", got, m.Selected)
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
	// A digit jumps to the row it counts; the selection goes on
	// following, so it is on the viewer's own row when they are back.
	if a := m.Handle(Key{Rune: '2'}); a.Kind != ActionJump || a.Row == nil || a.Row.Name != m.Visible()[1].Row.Name || m.Selected != -1 || !m.Follow || a.Mouse {
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
	m.SetRows(rows.Build(in))
	// Filter to rows that are not the viewer's: rows remain, none is
	// selected, and Enter has nothing to act on.
	m.Handle(Key{Rune: '/'})
	for _, r := range "notes" {
		m.Handle(Key{Kind: KeyRune, Rune: r})
	}
	m.Handle(Key{Kind: KeyEnter}) // leaves the filter typing, keeps the filter
	if vis := m.Visible(); len(vis) == 0 || m.Filter != "notes" {
		t.Fatalf("filter %q left %d rows", m.Filter, len(vis))
	}
	if got := m.Selection(); got != nil || !m.Follow {
		t.Fatalf("filtered away: %+v follow=%v", got, m.Follow)
	}
	if a := m.Handle(Key{Kind: KeyEnter}); a.Kind != ActionNone {
		t.Fatalf("Enter with the viewer's row filtered away: %+v", a)
	}
	m.Handle(Key{Kind: KeyEsc})
	if got := m.Selection(); got == nil || got.Name != "proj/task" || !m.Follow {
		t.Fatalf("filter cleared: %+v follow=%v", got, m.Follow)
	}
	// The viewer's row settled: hidden in the collapsed group, shown
	// when the group is expanded.
	for i := range in.Locals {
		if in.Locals[i].Name == "mac/proj/task" {
			in.Locals[i].Settled = true
		}
	}
	m.SetRows(rows.Build(in))
	if got := m.Selection(); got != nil {
		t.Fatalf("settled and collapsed: %+v", got)
	}
	m.Handle(Key{Rune: 'f'})
	if got := m.Selection(); got == nil || got.Name != "proj/task" || !m.Follow {
		t.Fatalf("settled and expanded: %+v follow=%v", got, m.Follow)
	}
	m.Handle(Key{Rune: 'f'})
	if got := m.Selection(); got != nil || !m.Follow {
		t.Fatalf("collapsed again: %+v follow=%v", got, m.Follow)
	}
	// Moves that change nothing keep following: up from the first row,
	// onto the selected row, on an empty list.
	in = fixtureInput(now)
	in.Locals = append(in.Locals, workspace.Local{Name: "vm/laatmux/fix-ls", Key: "venv//r/fix-ls", Host: "vm"})
	in.Current = "vm/laatmux/fix-ls"
	m = model(now)
	m.Layout, m.Width, m.Height = Compact, 80, 30
	m.Follow = true
	m.SetRows(rows.Build(in))
	if m.Selected != 0 {
		t.Fatalf("current first: %d", m.Selected)
	}
	m.Handle(Key{Kind: KeyUp})
	m.Handle(Key{Rune: 'k'})
	m.Handle(Key{Rune: 'g'})
	m.Handle(Key{Kind: KeyMouse, Wheel: -1})
	if a := m.Handle(Key{Rune: '1'}); a.Kind != ActionJump || !m.Follow || m.Selected != 0 {
		t.Fatalf("moves that change nothing: %+v follow=%v at %d", a, m.Follow, m.Selected)
	}
	m.Handle(Key{Rune: 'j'})
	if m.Follow || m.Selected != 1 {
		t.Fatalf("a move that changes: follow=%v at %d", m.Follow, m.Selected)
	}
	m = model(now)
	m.Follow = true
	m.SetRows(rows.Build(rows.Input{}))
	m.Handle(Key{Rune: 'j'})
	m.Handle(Key{Rune: 'G'})
	if got := m.Selection(); got != nil || !m.Follow {
		t.Fatalf("empty list: %+v follow=%v", got, m.Follow)
	}
}

// A live working row's mark is the spinner frame for the clock, in
// colour; the frame advances every spinTick and wraps; a gone or dim
// working agent keeps the plain mark; Spinning says whether a tick is
// wanted at all.
func TestSpinner(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Compact, 80, 30
	in := fixtureInput(now)
	m.SetRows(rows.Build(in))
	m.Render()
	if !m.Spinning() {
		t.Fatal("working rows and no spinning")
	}
	frameOf := func(name string, at time.Time) Span {
		m.Now = at
		for _, it := range m.Visible() {
			if it.Row.Name == name {
				return m.mark(*it.Row)
			}
		}
		t.Fatalf("no row %s", name)
		return Span{}
	}
	first := frameOf("proj/task", now)
	if first.Fg != spinnerFg || first.Text != spinnerFrames[0] {
		t.Fatalf("frame at t0: %+v", first)
	}
	if next := frameOf("proj/task", now.Add(spinTick)); next.Text != spinnerFrames[1] || next.Fg != spinnerFg {
		t.Fatalf("frame at t0+tick: %+v", next)
	}
	if wrapped := frameOf("proj/task", now.Add(time.Duration(len(spinnerFrames))*spinTick)); wrapped.Text != spinnerFrames[0] {
		t.Fatalf("frame after a full cycle: %+v", wrapped)
	}
	// A zero Now, before the first draw, is a frame too, not a panic.
	if z := frameOf("proj/task", time.Time{}); z.Fg != spinnerFg || z.Text == "" {
		t.Fatalf("frame at the zero time: %+v", z)
	}
	// Working but dim, on the down host: the plain mark.
	if down := frameOf("proj/down", now); down.Text != "*" || down.Fg != 0 {
		t.Fatalf("dim working row: %+v", down)
	}
	if blocked := frameOf("laatmux/fix-ls", now); blocked.Text != "!" || blocked.Fg != 0 {
		t.Fatalf("blocked row: %+v", blocked)
	}
	// Every working agent gone: nothing spins.
	for i := range in.Agents {
		if in.Agents[i].Activity == protocol.Working {
			in.Agents[i].Liveness = protocol.Gone
		}
	}
	m.SetRows(rows.Build(in))
	m.Render()
	if m.Spinning() {
		t.Fatal("gone agents spin")
	}
	// The colour reaches the terminal and the plain text does not
	// carry it.
	l := Line{Spans: []Span{{Text: ">"}, {Text: "⠋", Fg: spinnerFg}, {Text: " x"}}}
	if got := ANSI(l); !strings.Contains(got, "\x1b[36m⠋\x1b[0m") || !strings.HasSuffix(got, " x\x1b[0m") {
		t.Errorf("ANSI: %q", got)
	}
	if got := Text([]Line{l}); got != ">⠋ x\n" {
		t.Errorf("Text: %q", got)
	}
	if got := Debug([]Line{l}); !strings.Contains(got, ">⟨⠋⟩ x") {
		t.Errorf("Debug: %q", got)
	}
}

// The spinner ticks only while a frame is on screen: working rows
// scrolled off, or filtered out, or behind an overlay, are not ticked
// for; and a narrow pane keeps the mark's colour in both layouts, down
// to the two cells the gutter and the mark take.
func TestSpinnerOnScreenAndNarrow(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Compact, 80, 6 // header lines, a few body lines, footer
	m.SetRows(rows.Build(fixtureInput(now)))
	m.Render()
	if !m.Spinning() {
		t.Fatal("working rows at the top and no spinning")
	}
	m.Handle(Key{Rune: 'G'})
	m.Render()
	if m.Spinning() {
		t.Fatal("working rows scrolled off and still spinning")
	}
	m.Handle(Key{Rune: 'g'})
	m.Render()
	if !m.Spinning() {
		t.Fatal("scrolled back and not spinning")
	}
	m.Filter = "spike"
	m.Render()
	if m.Spinning() {
		t.Fatal("working rows filtered out and still spinning")
	}
	m.Filter = ""
	m.Overlay = NewPrompt("branch", "", nil)
	m.Render()
	if m.Spinning() {
		t.Fatal("an overlay up and still spinning")
	}
	m.Overlay = nil
	// Tiles, the blocked tile selected at the top: the first working
	// tile's head line is the fifth body line, so five body lines show
	// its mark and four cut the tile off above it.
	m.Layout = Tiles
	m.Handle(Key{Rune: 'g'})
	m.Height = len(m.Header) + 5 + 1
	m.Render()
	if !m.Spinning() {
		t.Fatalf("tile head on screen and not spinning:\n%s", Debug(m.Render()))
	}
	m.Height = len(m.Header) + 4 + 1
	m.Render()
	if m.Spinning() {
		t.Fatalf("tile head clipped and still spinning:\n%s", Debug(m.Render()))
	}
	// Header lines that fill the terminal: the one body line the layout
	// keeps is cut off by the height, and does not count.
	m = model(now)
	m.Layout, m.Width = Compact, 80
	m.SetRows(rows.Build(fixtureInput(now)))
	m.Filter = "proj/task" // the one row is the live working one
	m.Header = []string{"one", "two"}
	m.Height = 4 // headers, the body line, footer: the mark is drawn
	if lines := m.Render(); len(lines) != 4 || !m.Spinning() {
		t.Fatalf("one working row under two headers: %d lines, spinning=%v", len(lines), m.Spinning())
	}
	m.Height = 2 // the headers alone: the body line is cut off
	if lines := m.Render(); len(lines) != 2 || m.Spinning() {
		t.Fatalf("headers filling the terminal: %d lines, spinning=%v", len(lines), m.Spinning())
	}
	// Narrow: the coloured mark survives the fallback in both layouts,
	// and no line is wider than the pane.
	for _, layout := range []Layout{Compact, Tiles} {
		for _, w := range []int{12, 6, 3, 2} {
			m = model(now)
			m.Layout, m.Width, m.Height = layout, w, 30
			m.SetRows(rows.Build(fixtureInput(now)))
			lines := m.Render()
			if out := Debug(lines); !strings.Contains(out, "⟨") {
				t.Errorf("layout %v width %d: no coloured mark:\n%s", layout, w, out)
			}
			for _, l := range lines {
				if width(strings.TrimSuffix(Text([]Line{l}), "\n")) > w {
					t.Errorf("layout %v width %d: line wider than the pane: %q", layout, w, Text([]Line{l}))
				}
			}
		}
		m = model(now)
		m.Layout, m.Width, m.Height = layout, 1, 30
		m.SetRows(rows.Build(fixtureInput(now)))
		m.Render() // one cell: the gutter alone, no panic
	}
}

// A newline is Enter outside the form's prompt: on the list, in the
// filter, a picker, a line prompt and a notice.
func TestNewlineIsEnter(t *testing.T) {
	m := &Model{Width: 80, Height: 24}
	m.Filtering = true
	m.Handle(Key{Kind: KeyNewline})
	if m.Filtering {
		t.Fatal("the filter did not close on a newline")
	}
	p := NewPicker("t", []Choice{{Label: "a"}}, 0)
	p.Handle(Key{Kind: KeyNewline})
	if !p.Done() || p.Chosen != 0 {
		t.Fatalf("picker: done %v chosen %d", p.Done(), p.Chosen)
	}
	pr := NewPrompt("t", "x", nil)
	pr.Handle(Key{Kind: KeyNewline})
	if !pr.Done() || pr.Cancelled {
		t.Fatalf("prompt: done %v cancelled %v", pr.Done(), pr.Cancelled)
	}
	n := NewNotice("t", []string{"l"}, "")
	n.Handle(Key{Kind: KeyNewline})
	if !n.Done() {
		t.Fatal("notice not dismissed by a newline")
	}
	f := NewForm("t", chips(), "")
	f.Handle(Key{Kind: KeyShiftTab})
	f.Handle(Key{Kind: KeyShiftTab}) // the agent chip
	f.Handle(Key{Kind: KeyNewline})
	if f.picker == nil {
		t.Fatal("a newline on a chip did not open the picker")
	}
}

// The anchor pair: a task selected before its root is known stays
// selected when the root arrives, when its worktree row takes over, and
// through the reorder; a task the view never saw hand over finds its
// worktree row through the handoffs; past them the selection is on
// none rather than on the row that took the index.
func TestAnchorFollowsTask(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	hosts := []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}}
	other := protocol.Worktree{ID: "venv/worktree//r/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/r/a"}
	task := protocol.Pending{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "task", Taken: true, Reachable: true, Stage: protocol.StageClone, SubmittedAt: now}
	wt := protocol.Worktree{ID: "venv/worktree//r/task", EnvironmentID: "venv", Repo: "proj", Branch: "task", Root: "/r/task", Session: "proj/task"}
	agent := protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Session: "proj/task", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true}
	build := func(ps []protocol.Pending, ws []protocol.Worktree, as []protocol.Agent) rows.Rows {
		return rows.Build(rows.Input{Hosts: hosts, Pendings: ps, Worktrees: ws, Agents: as})
	}
	m := &Model{Width: 60, Height: 20, Now: now}
	m.SetRows(build([]protocol.Pending{task}, []protocol.Worktree{other}, nil))
	m.Handle(Key{Rune: 'g'}) // the task, first
	if r := m.Selection(); r == nil || r.ID() != "add-1" {
		t.Fatalf("selected %+v", r)
	}
	// The root arrives: same id, now with its alias.
	task.Stage, task.Root = protocol.StageAgent, "/r/task"
	m.SetRows(build([]protocol.Pending{task}, []protocol.Worktree{other, wt}, []protocol.Agent{agent}))
	if r := m.Selection(); r == nil || r.ID() != "add-1" || m.alias != wt.ID {
		t.Fatalf("with the root: %+v alias %q", r, m.alias)
	}
	// Handed over: the worktree row, found by the alias, which sorts
	// first now its agent works.
	m.SetRows(build(nil, []protocol.Worktree{other, wt}, []protocol.Agent{agent}))
	if r := m.Selection(); r == nil || r.ID() != wt.ID {
		t.Fatalf("after the handover: %+v", r)
	}
	// A task for the same worktree again: the task that stands for it.
	again := task
	again.ID = "add-2"
	m.SetRows(build([]protocol.Pending{again}, []protocol.Worktree{other, wt}, []protocol.Agent{agent}))
	if r := m.Selection(); r == nil || r.ID() != "add-2" {
		t.Fatalf("a task standing for the worktree again: %+v", r)
	}

	// A view that selected a task during clone and missed every step
	// until the worktree row: the handoffs name it.
	m = &Model{Width: 60, Height: 20, Now: now}
	early := protocol.Pending{ID: "add-3", Host: "vm", Repo: "proj", Branch: "task", Taken: true, Reachable: true, Stage: protocol.StageClone, SubmittedAt: now}
	m.SetRows(build([]protocol.Pending{early}, []protocol.Worktree{other}, nil))
	m.Handle(Key{Rune: 'g'})
	if r := m.Selection(); r == nil || r.ID() != "add-3" || m.alias != "" {
		t.Fatalf("selected %+v alias %q", r, m.alias)
	}
	m.Handoffs = map[string]string{"add-3": wt.ID}
	m.SetRows(build(nil, []protocol.Worktree{other, wt}, []protocol.Agent{agent}))
	if r := m.Selection(); r == nil || r.ID() != wt.ID {
		t.Fatalf("through the handoffs: %+v", r)
	}
	// Without them the selection is on none, not on the other row.
	m = &Model{Width: 60, Height: 20, Now: now}
	m.SetRows(build([]protocol.Pending{early}, []protocol.Worktree{other}, nil))
	m.Handle(Key{Rune: 'g'})
	m.Selection()
	m.SetRows(build(nil, []protocol.Worktree{other, wt}, []protocol.Agent{agent}))
	if r := m.Selection(); r != nil {
		t.Fatalf("an unknown handoff left the selection on %q", r.ID())
	}
	// A following view goes back to the viewer's own row meanwhile.
	f := &Model{Width: 60, Height: 20, Now: now, Follow: true}
	f.SetRows(build([]protocol.Pending{early}, []protocol.Worktree{other}, nil))
	if f.Selection() != nil {
		t.Fatal("a following view selected a row that is not the viewer's")
	}
}

// Pending rows as drawn: the spinner and the state while the add runs,
// "!" and dim with the reason once it needs the user, first in the
// main group, in tiles and in compact with titles.
func TestRenderPending(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	in := rows.Input{
		Hosts:     []rows.Host{{Name: "mac", Local: true, EnvironmentID: "menv", Connected: true, Listed: true, Worktrees: true}, {Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}},
		Worktrees: []protocol.Worktree{{ID: "venv/worktree//r/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/r/a"}},
		Pendings: []protocol.Pending{
			{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "sidebar-follow", Taken: true, Reachable: true, Stage: protocol.StageClone, Detail: "cloning git@github.com:laat/proj.git", SubmittedAt: now},
			{ID: "add-2", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "fix-ls", Root: "/r/fix-ls", Session: "proj/fix-ls", Taken: true, Reachable: true,
				Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, Error: "the pane was not ready within a minute", SubmittedAt: now.Add(-time.Minute)},
		},
	}
	m := &Model{Rows: rows.Build(in), LocalHost: "mac", Now: now, Width: 40, Height: 12}
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
func TestAnchorStandIn(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	hosts := []rows.Host{{Name: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Worktrees: true}}
	wt := protocol.Worktree{ID: "venv/worktree//r/b", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b"}
	stuck := protocol.Pending{ID: "add-a", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, SubmittedAt: now.Add(-time.Hour)}
	next := protocol.Pending{ID: "add-b", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryDelivered, SubmittedAt: now}
	build := func(ps ...protocol.Pending) rows.Rows {
		return rows.Build(rows.Input{Hosts: hosts, Pendings: ps, Worktrees: []protocol.Worktree{wt}})
	}
	m := &Model{Width: 60, Height: 20, Now: now}
	m.SetRows(build(stuck, next))
	m.Handle(Key{Rune: 'g'})
	if r := m.Selection(); r == nil || r.ID() != "add-b" {
		t.Fatalf("selected %+v", r)
	}
	m.Handoffs = map[string]string{"add-b": wt.ID}
	m.SetRows(build(stuck))
	if r := m.Selection(); r == nil || r.ID() != "add-a" {
		t.Fatalf("after add-b handed over: %+v", r)
	}
	// Beside a task that failed or was gone at the root, which stands
	// for nothing, the hand-over lands on the worktree row itself.
	for _, bad := range []protocol.Pending{
		{ID: "add-f", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Taken: true, Done: true, Stage: protocol.StageAgent, Error: "failed at agent: x", SubmittedAt: now.Add(-time.Hour)},
		{ID: "add-g", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "b", Root: "/r/b", Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNone, Gone: true, SubmittedAt: now.Add(-time.Hour)},
	} {
		f := &Model{Width: 60, Height: 20, Now: now, Handoffs: map[string]string{"add-b": wt.ID}}
		f.SetRows(build(bad, next))
		f.Handle(Key{Rune: 'g'})
		if r := f.Selection(); r == nil || r.ID() != "add-b" {
			t.Fatalf("%s: selected %+v", bad.ID, r)
		}
		f.SetRows(build(bad))
		if r := f.Selection(); r == nil || r.ID() != wt.ID {
			t.Fatalf("%s: after add-b handed over: %+v", bad.ID, r)
		}
	}
	// Lost, then found again: a filter that hides every row and is
	// cleared puts the selection on the first row, as for any other.
	m.SetRows(build())
	m.SetRows(build(stuck))
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
	m.SetRows(rows.Build(in))
	own := m.Selected
	m.Render()
	// The first body line is the first row; the own row is elsewhere.
	y := 1 + len(m.Header)
	target := m.Visible()[m.hitRow(y, time.Time{})].Row.Name
	if m.hitRow(y, time.Time{}) == own {
		t.Fatal("the fixture's first row is the viewer's own")
	}
	a := m.Handle(Key{Kind: KeyMouse, Y: y})
	if a.Kind != ActionJump || a.Row == nil || a.Row.Name != target || !a.Mouse || !m.Follow || m.Selected != own {
		t.Fatalf("click while following: %+v selected %d follow %v", a, m.Selected, m.Follow)
	}
	// The user's own selection: j, then a click, moves it there.
	m.Handle(Key{Rune: 'j'})
	m.Render()
	a = m.Handle(Key{Kind: KeyMouse, Y: y})
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
	m.SetRows(rows.Build(in2))
	if a := m.Handle(Key{Kind: KeyMouse, Y: y}); a.Kind != ActionJump || a.Row.Name != drawn {
		t.Fatalf("after a reorder: jumped to %+v, drawn was %q", a.Row, drawn)
	}
	m.Filter = "zzz-nothing"
	if a := m.Handle(Key{Kind: KeyMouse, Y: y}); a.Kind != ActionNone {
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
			if r := m.Visible()[i].Row; r.Local != nil && !r.Settled {
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
	m.SetRows(rows.Build(in2))
	if i := m.hitRow(sy, time.Time{}); i != -1 {
		t.Fatalf("a row collapsed since resolved to %d", i)
	}
	if a := m.Handle(Key{Kind: KeyMouse, Y: sy}); a.Kind != ActionNone {
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
	m.SetRows(rows.Build(in))
	m.Render()
	y := 1 + len(m.Header)
	seen := m.Visible()[m.hitRow(y, time.Time{})].Row.ID()
	clicked := t0.Add(time.Second)
	// The rows reorder and are drawn again after the click was read.
	for i := range in.Agents {
		in.Agents[i].Activity = protocol.Idle
	}
	in.Agents[len(in.Agents)-1].Activity = protocol.Blocked
	m.SetRows(rows.Build(in))
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

// A click split across reads is stamped with when its first bytes
// came; a click in one read with that read's time.
func TestClickClock(t *testing.T) {
	t1, t2 := time.Unix(10, 0), time.Unix(20, 0)
	var dec Decoder
	var c clickClock
	if ks := c.feed(&dec, []byte("\x1b[<0;5;"), t1); len(ks) != 0 {
		t.Fatalf("half a click: %+v", ks)
	}
	ks := c.feed(&dec, []byte("3M"), t2)
	if len(ks) != 1 || ks[0].Kind != KeyMouse || !ks[0].At.Equal(t1) {
		t.Fatalf("split click: %+v", ks)
	}
	ks = c.feed(&dec, []byte("\x1b[<0;5;3M"), t2)
	if len(ks) != 1 || !ks[0].At.Equal(t2) {
		t.Fatalf("whole click: %+v", ks)
	}
	// The completion of a held click and a fresh one in the same read:
	// only the first is the held one's; a new partial suffix begins now.
	t3 := time.Unix(30, 0)
	c.feed(&dec, []byte("\x1b[<0;5;"), t1)
	ks = c.feed(&dec, []byte("3M\x1b[<0;6;4M\x1b[<0;7;"), t3)
	if len(ks) != 2 || !ks[0].At.Equal(t1) || !ks[1].At.Equal(t3) || !c.held.Equal(t3) {
		t.Fatalf("completion, fresh click and a new suffix: %+v held %v", ks, c.held)
	}
	dec.Flush()
	c.flushed(&dec)
	// A bare escape held, then flushed as the escape key: a click after
	// it has its own time.
	c.feed(&dec, []byte("\x1b"), t1)
	dec.Flush()
	c.flushed(&dec)
	ks = c.feed(&dec, []byte("\x1b[<0;5;3M"), t3)
	if len(ks) != 1 || !ks[0].At.Equal(t3) {
		t.Fatalf("a click after a flushed escape: %+v", ks)
	}
}
