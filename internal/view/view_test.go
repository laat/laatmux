package view

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/workspace"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fixture is a listing with one of everything: the three activities, a
// gone agent, a worktree without a session, one without an agent, a
// managed agent with no worktree, observed agents on the local and a
// remote default server, a host down, a settled and an orphaned workspace.
// It is the agent view as the dashboard builds it: the two worktrees with
// no agent and the orphaned workspace are tree lines, not tiles, and the
// settled workspace's agent is in the stale fold.
func fixture(now time.Time) rows.Rows { return rows.Agents(fixtureInput(now)) }

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
	// The stale fold holds one tile: the settled workspace's agent.
	if len(m.Visible()) != n+1 {
		t.Errorf("f showed %d rows, want %d", len(m.Visible()), n+1)
	}
	// The digits count the numbered rows whatever the fold: 1 is the
	// first tile from anywhere.
	m.Handle(Key{Rune: 'G'})
	if a := m.Handle(Key{Rune: '1'}); a.Kind != ActionJump || m.Selection().Name != "laatmux/fix-ls" {
		t.Errorf("1 = %+v on %q", a, m.Selection().Name)
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
	m.SetRows(rows.Agents(rows.Input{}))
	m.SetRows(rows.Agents(in))
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
	m.SetRows(rows.Agents(in))
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
	m.SetRows(rows.Agents(in))
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
	m.SetRows(rows.Agents(in))
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
	m.SetRows(rows.Agents(in))
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
	m.SetRows(rows.Agents(in))
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
	m.SetRows(rows.Agents(in))
	if got := m.Selection(); got == nil || got.Name != taken.Name {
		t.Fatalf("user's selection moved: %+v, was %s", got, taken.Name)
	}
	// Without Follow the model is as it was: the first row selected.
	m = model(now)
	m.SetRows(rows.Agents(fixtureInput(now)))
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
	m.SetRows(rows.Agents(in))
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
	// The viewer's row settled: it stays in place, dim, still followed,
	// so z can unsettle it; the groups folding changes nothing.
	for i := range in.Locals {
		if in.Locals[i].Name == "mac/proj/task" {
			in.Locals[i].Settled = true
		}
	}
	m.SetRows(rows.Agents(in))
	if got := m.Selection(); got == nil || got.Name != "proj/task" || !got.Settled || !m.Follow {
		t.Fatalf("settled: %+v follow=%v", got, m.Follow)
	}
	m.Handle(Key{Rune: 'f'})
	m.Handle(Key{Rune: 'f'})
	if got := m.Selection(); got == nil || got.Name != "proj/task" || !m.Follow {
		t.Fatalf("after f twice: %+v follow=%v", got, m.Follow)
	}
	// Moves that change nothing keep following: up from the first row,
	// onto the selected row, on an empty list.
	in = fixtureInput(now)
	in.Locals = append(in.Locals, workspace.Local{Name: "vm/laatmux/fix-ls", Key: "venv//r/fix-ls", Host: "vm"})
	in.Current = "vm/laatmux/fix-ls"
	m = model(now)
	m.Layout, m.Width, m.Height = Compact, 80, 30
	m.Follow = true
	m.SetRows(rows.Agents(in))
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
	m.SetRows(rows.Agents(rows.Input{}))
	m.Handle(Key{Rune: 'j'})
	m.Handle(Key{Rune: 'G'})
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
	m.SetRows(rows.Agents(in))
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
	m.SetRows(rows.Agents(in))
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
	m.SetRows(rows.Agents(fixtureInput(now)))
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
	// The idle scratch tile left, the working ones filtered out.
	m.Filter = "scratch"
	if m.Render(); len(m.Visible()) != 1 || m.Spinning() {
		t.Fatalf("working rows filtered out: %d rows, spinning=%v", len(m.Visible()), m.Spinning())
	}
	m.Filter = ""
	m.Overlay = NewPrompt("branch", "", nil)
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
	m.Handle(Key{Rune: 'g'})
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
	m.SetRows(rows.Agents(fixtureInput(now)))
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
			m.SetRows(rows.Agents(fixtureInput(now)))
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
		m.SetRows(rows.Agents(fixtureInput(now)))
		m.Render() // one cell: the stripe alone, no panic
	}
}

// A newline is Enter outside the form's prompt: on the list, in the
// filter, a picker and a line prompt.
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
	agent := protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Session: "proj/task", Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true}
	set := func(m *Model, ps []protocol.Pending, ws []protocol.Worktree, as []protocol.Agent) {
		in := rows.Input{Hosts: hosts, Pendings: ps, Worktrees: ws, Agents: as}
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in))
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
	m.Handoffs = map[string]string{"add-3": wt.ID}
	set(m, nil, []protocol.Worktree{other, wt}, []protocol.Agent{agent})
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
		Agents: []protocol.Agent{{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/a", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now.Add(-5 * time.Minute), Liveness: protocol.Alive, Managed: true, Title: "Allow?"}},
		Pendings: []protocol.Pending{
			{ID: "add-1", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "sidebar-follow", Taken: true, Reachable: true, Stage: protocol.StageClone, Detail: "cloning git@github.com:laat/proj.git", SubmittedAt: now},
			{ID: "add-2", Host: "vm", EnvironmentID: "venv", Repo: "proj", Branch: "fix-ls", Root: "/r/fix-ls", Session: "proj/fix-ls", Taken: true, Reachable: true,
				Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered, Error: "the pane was not ready within a minute", SubmittedAt: now.Add(-time.Minute)},
		},
	}
	m := &Model{Rows: rows.Agents(in), LocalHost: "mac", Now: now, Width: 40, Height: 12}
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
		m.SetRows(rows.Agents(in))
	}
	m := &Model{Width: 60, Height: 20, Now: now}
	set(m, stuck, next)
	m.Handle(Key{Rune: 'g'})
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
	u.Handle(Key{Rune: 'g'})
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
	m.SetRows(rows.Agents(in))
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
	m.SetRows(rows.Agents(in2))
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
	m.SetRows(rows.Agents(in2))
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
	m.SetRows(rows.Agents(in))
	m.Render()
	y := 1 + len(m.Header)
	seen := m.Visible()[m.hitRow(y, time.Time{})].Row.ID()
	clicked := t0.Add(time.Second)
	// The rows reorder and are drawn again after the click was read.
	for i := range in.Agents {
		in.Agents[i].Activity = protocol.Idle
	}
	in.Agents[len(in.Agents)-1].Activity = protocol.Blocked
	m.SetRows(rows.Agents(in))
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

// A click split across reads is dated from when its first bytes came;
// a click begun in a read from that read, whatever the held bytes
// before it turned out to be.
func TestFeedAtDatesClicks(t *testing.T) {
	t1, t2, t3 := time.Unix(10, 0), time.Unix(20, 0), time.Unix(30, 0)
	click := func(ks []Key, want time.Time) bool {
		return len(ks) == 1 && ks[0].Kind == KeyMouse && ks[0].At.Equal(want)
	}
	var dec Decoder
	if ks := dec.FeedAt([]byte("\x1b[<0;5;"), t1); len(ks) != 0 {
		t.Fatalf("half a click: %+v", ks)
	}
	if ks := dec.FeedAt([]byte("3M"), t2); !click(ks, t1) {
		t.Fatalf("split click: %+v", ks)
	}
	if ks := dec.FeedAt([]byte("\x1b[<0;5;3M"), t2); !click(ks, t2) {
		t.Fatalf("whole click: %+v", ks)
	}
	// The completion of a held click and a fresh one in the same read,
	// then the rest of a click begun in that read.
	dec.FeedAt([]byte("\x1b[<0;5;"), t1)
	ks := dec.FeedAt([]byte("3M\x1b[<0;6;4M\x1b[<0;7;"), t2)
	if len(ks) != 2 || !ks[0].At.Equal(t1) || !ks[1].At.Equal(t2) {
		t.Fatalf("completion and a fresh click: %+v", ks)
	}
	if ks := dec.FeedAt([]byte("8M"), t3); !click(ks, t2) {
		t.Fatalf("a click begun after a completion: %+v", ks)
	}
	// Held bytes completed as a sequence that gives no key: a click
	// after them in the same read is that read's, whole or split.
	dec.FeedAt([]byte("\x1bO"), t1)
	if ks := dec.FeedAt([]byte("P\x1b[<0;5;3M"), t2); !click(ks, t2) {
		t.Fatalf("a click after an ignored completion: %+v", ks)
	}
	dec.FeedAt([]byte("\x1bO"), t1)
	if ks := dec.FeedAt([]byte("P\x1b[<0;5;"), t2); len(ks) != 0 {
		t.Fatalf("an ignored completion and half a click: %+v", ks)
	}
	if ks := dec.FeedAt([]byte("3M"), t3); !click(ks, t2) {
		t.Fatalf("a split click after an ignored completion: %+v", ks)
	}
	// A bare escape held, then flushed as the escape key: a click after
	// it has its own time.
	dec.FeedAt([]byte("\x1b"), t1)
	dec.Flush()
	if ks := dec.FeedAt([]byte("\x1b[<0;5;3M"), t3); !click(ks, t3) {
		t.Fatalf("a click after a flushed escape: %+v", ks)
	}
	// Feed, with no time, dates nothing.
	if ks := dec.Feed([]byte("\x1b[<0;5;3M")); !click(ks, time.Time{}) {
		t.Fatalf("a click fed with no time: %+v", ks)
	}
}

// A sequence cut short by a new escape, Alt-[ or an Esc and [ read
// together, is dropped up to it, and what follows is parsed afresh: a
// click right after is the click, not the digits of its report, which
// would jump. Held across reads, the click keeps its own read's time.
func TestBrokenSequenceBeforeClick(t *testing.T) {
	for in, want := range map[string][]Key{
		"\x1b[\x1b[<0;5;3M": {{Kind: -1}, {Kind: KeyMouse, X: 5, Y: 3}},
		"\x1b[1;\x1b[A":     {{Kind: -1}, {Kind: KeyUp}},
		"\x1b[\x03":         {{Kind: -1}, {Kind: KeyCtrlC}},
		// Alt-O the same way: SS3 cut short.
		"\x1bO\x1b[<0;5;3M": {{Kind: KeyMouse, X: 5, Y: 3}},
		"\x1bO\r":           {{Kind: KeyEnter}},
		// The old form of a modified F1 is dropped whole, not a digit.
		"\x1bO2P":              nil,
		"\x1bO1;2Pj":           {{Rune: 'j'}},
		"\x1bO2\x03":           {{Kind: KeyCtrlC}},
		"\x1bO 2Pj":            {{Rune: 'j'}},
		"\x1b[12\x1b[<64;1;1M": {{Kind: -1}, {Kind: KeyMouse, X: 1, Y: 1, Wheel: -1}},
	} {
		if got := Parse([]byte(in)); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %+v, want %+v", in, got, want)
		}
	}
	t1, t2 := time.Unix(10, 0), time.Unix(20, 0)
	var dec Decoder
	if ks := dec.FeedAt([]byte("\x1b["), t1); len(ks) != 0 || !dec.Pending() {
		t.Fatalf("Alt-[ held: %+v", ks)
	}
	ks := dec.FeedAt([]byte("\x1b[<0;5;3M"), t2)
	if len(ks) != 2 || ks[0].Kind != -1 || ks[1].Kind != KeyMouse || ks[1].X != 5 || !ks[1].At.Equal(t2) || dec.Pending() {
		t.Fatalf("a click after a held Alt-[: %+v pending %v", ks, dec.Pending())
	}
}

// A sequence flushed incomplete is discarded through its final byte
// when the rest comes, but a byte that cannot go on, Ctrl-C or Enter or
// a fresh escape, is the user's and comes through.
func TestDiscardStopsAtUserKeys(t *testing.T) {
	for in, want := range map[string][]Key{
		"2;5H":   nil,
		"\x03":   {{Kind: KeyCtrlC}},
		"\rq":    {{Kind: KeyEnter}, {Rune: 'q'}},
		"\x1b[A": {{Kind: KeyUp}},
		"1;\x03": {{Kind: KeyCtrlC}},
		"9~j":    {{Rune: 'j'}},
	} {
		var d Decoder
		d.Feed([]byte("\x1b[1"))
		if got := d.Flush(); len(got) != 0 {
			t.Fatalf("flush: %+v", got)
		}
		if got := d.Feed([]byte(in)); !reflect.DeepEqual(got, want) {
			t.Errorf("%q after a flushed ESC [1: %+v, want %+v", in, got, want)
		}
	}
}

// A sequence cut short inside pasted text is dropped up to the byte
// that ends it, which stays text; a whole one is dropped whole.
func TestPasteTextBrokenSequence(t *testing.T) {
	for in, want := range map[string]string{
		"a\x1b[31mb":       "ab",
		"a\x1b[\nbéc":      "a\nbéc",
		"x\x1b[ 1 2 3 日":   "x日",
		"\x1b[\x1b[31mred": "red",
		"a\x1bOPb":         "ab",
		"a\x1bO\nb":        "a\nb",
		"a\x1bO1;2Pb":      "ab",
		"a\x1bO 1Pb":       "ab",
	} {
		if got := pasteText([]byte(in)); got != want {
			t.Errorf("pasteText(%q) = %q, want %q", in, got, want)
		}
	}
	// A chunk is not cut before such a sequence's text: it has ended.
	if head, tail := splitTail([]byte("ok \x1b[\n12")); string(head) != "ok \x1b[\n12" || len(tail) != 0 {
		t.Errorf("splitTail: %q %q", head, tail)
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
		protocol.Agent{ID: "venv/laatmux/%10", EnvironmentID: "venv", Session: "proj/old", Agent: "claude", Activity: protocol.Idle, ActivityAt: now.Add(-2 * time.Hour), Liveness: protocol.Alive, Managed: true, Title: "long idle"},
		protocol.Agent{ID: "venv/laatmux/%11", EnvironmentID: "venv", Session: "proj/asks", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now.Add(-3 * time.Hour), Liveness: protocol.Alive, Managed: true, Title: "Allow?"})
	in.Worktrees = append(in.Worktrees,
		protocol.Worktree{ID: "venv/worktree//r/old", EnvironmentID: "venv", Repo: "proj", Branch: "old", Root: "/r/old", Session: "proj/old"},
		protocol.Worktree{ID: "venv/worktree//r/asks", EnvironmentID: "venv", Repo: "proj", Branch: "asks", Root: "/r/asks", Session: "proj/asks"})
	in.Locals = append(in.Locals, workspace.Local{Name: "vm/proj/asks", Key: "venv//r/asks", Host: "vm", Settled: true})
	in.Attention = map[string]protocol.Attention{
		// notes finished after the last visit; other was seen since.
		"menv/default/%6": {AgentID: "menv/default/%6", FinishedAt: now.Add(-time.Minute)},
		"venv/laatmux/%3": {AgentID: "venv/laatmux/%3", FinishedAt: now.Add(-time.Hour), SeenAt: now.Add(-time.Minute)},
		// A done agent is never stale, however long ago it finished.
		"menv/laatmux/%9": {AgentID: "menv/laatmux/%9", FinishedAt: now.Add(-26 * time.Hour)},
	}
	in.Now, in.StaleAfter, in.DimStale, in.CollapseStale = now, time.Hour, true, true
	m := model(now)
	m.SetRows(rows.Agents(in))
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
	m.SetRows(rows.Agents(in))
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
		return protocol.BranchKey{Source: config.SourceKey(src), Branch: branch}
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
	m.SetRows(rows.Agents(in))
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
		return protocol.Agent{ID: id, EnvironmentID: env, Session: session, Agent: name, Activity: act, ActivityAt: now.Add(-time.Minute),
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
		Locals: []workspace.Local{
			{Name: "vm/laatmux/agents-config", Key: "venv//r/agents-config", Host: "vm"},
			{Name: "mac/anki-llm/batch-processing", Key: "menv//w/batch", Host: "mac"},
			{Name: "mac/laatmux/gone", Key: "menv//w/gone", Host: "mac", Source: src},
		},
		Branches: map[protocol.BranchKey]protocol.BranchStatus{
			{Source: config.SourceKey(src), Branch: "agents-config"}: {PR: &protocol.PullRequest{Number: 52, State: "open"}, Checks: &protocol.Checks{State: protocol.ChecksSuccess, Passed: 5, Total: 5}},
			{Source: config.SourceKey(src), Branch: "auto-layout"}:   {PR: &protocol.PullRequest{Number: 49, State: "open"}, Checks: &protocol.Checks{State: protocol.ChecksFailure, Passed: 3, Total: 5}},
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
	m.SetRows(rows.Agents(in))
	m.SetTree(rows.Tree(in))
	golden(t, "tree", Debug(m.Render()))
	// auto-layout, whose agent is done, starts open; fix-sidebar has no
	// children; folding agents-config shows the working spinner at the
	// right.
	m.Handle(Key{Rune: 'h'})
	golden(t, "tree-folded", Debug(m.Render()))
	m.Width = 30
	golden(t, "tree-narrow", Debug(m.Render()))
	m.Width = 60
	m.Handle(Key{Kind: KeyTab})
	golden(t, "tree-agents", Debug(m.Render()))
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
	m.SetRows(rows.Agents(in))
	m.SetTree(rows.Tree(in))
	sel := func() string {
		r := m.Selection()
		if r == nil {
			return ""
		}
		return r.ID()
	}
	m.Select("venv/laatmux/%2") // codex under agents-config
	m.Handle(Key{Kind: KeyTab})
	if m.View != ViewAgents || sel() != "venv/laatmux/%2" {
		t.Errorf("agent to its tile: view %s, selection %q", m.View, sel())
	}
	m.Handle(Key{Kind: KeyTab})
	if m.View != ViewTree || sel() != "venv/laatmux/%2" {
		t.Errorf("tile to its node: view %s, selection %q", m.View, sel())
	}
	// The worktree line, then a pane under it: the first agent.
	for _, id := range []string{"venv/worktree//r/agents-config", "venv/pane/laatmux/%7"} {
		m.Select(id)
		m.Handle(Key{Kind: KeyTab})
		if sel() != "venv/laatmux/%1" {
			t.Errorf("from %s: %q", id, sel())
		}
		m.Handle(Key{Kind: KeyTab})
	}
	// A repository line: its first worktree's first agent.
	m.Select(rows.RepoNode("git@github.com:laat/laatmux.git"))
	m.Handle(Key{Kind: KeyTab})
	if sel() != "venv/laatmux/%1" {
		t.Errorf("from the repository: %q", sel())
	}
	m.Handle(Key{Kind: KeyTab})
	// A task: itself.
	m.Select("add-1")
	m.Handle(Key{Kind: KeyTab})
	if sel() != "add-1" {
		t.Errorf("from a task: %q", sel())
	}
	m.Handle(Key{Kind: KeyTab})
	// A worktree with no agent: no row.
	m.Select("menv/worktree//w/fix-sidebar")
	m.Handle(Key{Kind: KeyTab})
	if sel() != "" {
		t.Errorf("from an empty worktree: %q", sel())
	}
	// Folded away, then reached from the tile: opened.
	m.Handle(Key{Kind: KeyTab})
	m.Select("venv/worktree//r/agents-config")
	before := len(m.Visible())
	m.Handle(Key{Rune: 'h'})
	if len(m.Visible()) != before-4 {
		t.Fatalf("not folded: %d rows, %d before", len(m.Visible()), before)
	}
	m.Handle(Key{Kind: KeyTab})
	m.Select("venv/laatmux/%2")
	m.Handle(Key{Kind: KeyTab})
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
	m.SetRows(rows.Agents(in))
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
	m.Handle(Key{Rune: 'h'})
	if r := m.Selection(); r == nil || r.ID() != "venv/worktree//r/agents-config" {
		t.Errorf("h from a child: %+v", r)
	}
	m.Handle(Key{Rune: 'h'})
	if open("venv/worktree//r/agents-config") {
		t.Error("h on the line did not fold")
	}
	m.Handle(Key{Rune: 'l'})
	if !open("venv/worktree//r/agents-config") {
		t.Error("l did not unfold")
	}
	m.Handle(Key{Rune: 'f'})
	if !open("venv/worktree//r/auto-layout") {
		t.Error("f with a fold closed did not open every one")
	}
	m.Handle(Key{Rune: 'f'})
	if open("venv/worktree//r/agents-config") || open(rows.RepoNode("git@github.com:laat/laatmux.git")) {
		t.Error("f with every fold open did not close every one")
	}
	m.Handle(Key{Rune: 'f'})
	m.Select(rows.RepoNode("git@github.com:laat/laatmux.git"))
	if a := m.Handle(Key{Kind: KeyEnter}); a.Kind != ActionNone || open(rows.RepoNode("git@github.com:laat/laatmux.git")) {
		t.Errorf("Enter on a repository line: %+v", a)
	}
	m.Handle(Key{Kind: KeyEnter})
	if a := m.Handle(Key{Rune: '2'}); a.Kind != ActionJump || a.Row == nil || a.Row.ID() != "venv/worktree//r/agents-config" {
		t.Errorf("2: %+v", a)
	}
}

// Following in the tree: the viewer's worktree line, and in the agent
// view the first tile among its agents.
func TestFollowTree(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	m := &Model{Now: now, View: ViewTree, Width: 60, Height: 30, Follow: true}
	m.SetRows(rows.Agents(in))
	m.SetTree(rows.Tree(in))
	if r := m.Selection(); r == nil || r.ID() != "venv/worktree//r/agents-config" {
		t.Errorf("tree follows %+v", r)
	}
	m.Handle(Key{Kind: KeyTab})
	if r := m.Selection(); r == nil || r.ID() != "venv/laatmux/%1" || !m.Follow {
		t.Errorf("agent view follows %+v", r)
	}
}

// Review round 1: a task's first fold is by its agent's status; a task
// that handed over passes its fold to the node holding the children,
// and in the agent view the selection to the worktree's first tile;
// following survives a switch with the viewer's line folded away; f
// keeps the selection on its row.
func TestTreeEdges(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	src := "git@github.com:laat/laatmux.git"
	in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Session: "laatmux/new-one", Agent: "claude", Activity: protocol.Idle,
		ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/r/new-one", Identity: &protocol.Identity{PID: 9, StartUnix: 9}})
	in.Pendings = []protocol.Pending{{ID: "add-1", Host: "vm", EnvironmentID: "venv", Source: src, Repo: "laatmux", Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one", Taken: true, SubmittedAt: now}}
	m := &Model{Now: now, View: ViewTree, Width: 60, Height: 30}
	m.SetRows(rows.Agents(in))
	m.SetTree(rows.Tree(in))
	m.Render()
	if !m.closed(&m.Tree[m.indexOf("add-1")]) {
		t.Error("a task with an idle agent started open")
	}
	m.Select("add-1")
	m.Handle(Key{Rune: 'l'}) // the user's own fold: open
	// The host lists the worktree; the task hands over.
	in.Worktrees = append(in.Worktrees, protocol.Worktree{ID: "venv/worktree//r/new-one", EnvironmentID: "venv", Repo: "laatmux", Source: src, Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one"})
	in.Agents[len(in.Agents)-1].WorktreeID = "venv/worktree//r/new-one"
	in.Pendings = nil
	m.Handoffs = map[string]string{"add-1": "venv/worktree//r/new-one"}
	m.SetRows(rows.Agents(in))
	m.SetTree(rows.Tree(in))
	if m.closed(&m.Tree[m.indexOf("venv/worktree//r/new-one")]) {
		t.Error("the worktree line did not take the task's fold")
	}
	if r := m.Selection(); r == nil || r.ID() != "venv/worktree//r/new-one" {
		t.Errorf("selection after the handoff: %+v", r)
	}
	// In the agent view the handoff lands on the worktree's first tile.
	m.Handle(Key{Kind: KeyTab})
	m.Handle(Key{Kind: KeyTab})
	m.Select("add-1")
	m.View = ViewAgents
	m.anchor = "add-1"
	m.SetRows(rows.Agents(in))
	if r := m.Selection(); r == nil || r.ID() != "venv/laatmux/%9" {
		t.Errorf("handoff in the agent view: %+v", r)
	}
	// Following: every fold closed (all are open, so one f closes
	// them), a switch away and back.
	m.View, m.Follow = ViewTree, true
	m.Selection()
	m.Handle(Key{Rune: 'f'})
	if len(m.Visible()) > 6 {
		t.Fatalf("not folded: %d", len(m.Visible()))
	}
	m.Handle(Key{Kind: KeyTab})
	m.Handle(Key{Kind: KeyTab})
	if r := m.Selection(); r == nil || r.ID() != "venv/worktree//r/agents-config" || !m.Follow {
		t.Errorf("following after a switch with folds closed: %+v", r)
	}
	// f keeps a user's selection on its row, or on the line over it.
	m.Handle(Key{Rune: 'f'}) // every fold open again
	if !m.Select("venv/laatmux/%2") {
		t.Fatal("the child is not visible")
	}
	m.Handle(Key{Rune: 'f'}) // closes every fold: the child is hidden
	if r := m.Selection(); r == nil || r.ID() != rows.RepoNode(src) {
		t.Errorf("f with the selection on a child: %+v", r)
	}
	m.Handle(Key{Rune: 'f'})
	if r := m.Selection(); r == nil || r.ID() != rows.RepoNode(src) {
		t.Errorf("f opening every fold moved the selection: %+v", r)
	}
	// f under a filter goes by the folds shown: with a hidden line
	// folded and the shown one open, the first f closes the shown.
	m.Follow = false
	shown, hidden := "venv/worktree//r/agents-config", "venv/worktree//r/auto-layout"
	m.Select(hidden)
	m.Handle(Key{Rune: 'h'})
	m.Select(shown)
	m.Handle(Key{Rune: 'l'})
	if !m.closed(&m.Tree[m.indexOf(hidden)]) || m.closed(&m.Tree[m.indexOf(shown)]) {
		t.Fatal("the folds before the filter")
	}
	m.Filter = "agents-config"
	m.Render()
	if vis := m.Visible(); len(vis) != 6 || vis[1].Row.ID() != shown {
		t.Fatalf("the filtered tree: %d rows", len(vis))
	}
	m.Handle(Key{Rune: 'f'})
	if !m.closed(&m.Tree[m.indexOf(shown)]) {
		t.Error("f under a filter opened by a hidden fold")
	}
	m.Filter = ""
	// A click on the tab shown does nothing; on the other, a switch.
	m.Tabs = true
	m.Render()
	m.Handle(Key{Kind: KeyMouse, X: 12, Y: 1})
	if m.View != ViewTree {
		t.Error("a click on the shown tab switched")
	}
	m.Handle(Key{Kind: KeyMouse, X: 4, Y: 1})
	if m.View != ViewAgents {
		t.Error("a click on the other tab did not switch")
	}
	m.Handle(Key{Kind: KeyMouse, X: 9, Y: 1})
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
	m.SetRows(rows.Agents(in))
	m.SetTree(rows.Tree(in))
	m.Render()
	vis := m.Visible()
	m.Handle(Key{Rune: 'G'})
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
	walk := func(m *Model, key Key) {
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
			if m.Selected == 0 && key.Kind == KeyUp || m.Selected == len(m.Visible())-1 && key.Kind == KeyDown {
				break
			}
			m.Handle(key)
		}
	}
	for _, h := range []int{7, 8, 9} {
		m.Height = h
		m.Handle(Key{Rune: 'G'})
		walk(m, Key{Kind: KeyUp})
		m.Handle(Key{Rune: 'g'})
		walk(m, Key{Kind: KeyDown})
	}
	// Moving down to where the more line is needed pins the repository
	// in the same render.
	m.Height = 8
	m.Handle(Key{Rune: 'g'})
	m.Render()
	for i := 0; i < 5; i++ {
		m.Handle(Key{Kind: KeyDown})
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
	m.SetRows(rows.Agents(in))
	m.Handle(Key{Rune: 'G'})
	text = Debug(m.Render())
	if strings.Contains(text, "laatmux") {
		t.Errorf("a repository pinned over other sessions:\n%s", text)
	}
	walk(m, Key{Kind: KeyUp})
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
		m.SetRows(rows.Agents(in))
		m.Render()
		m.Handle(Key{Rune: 'G'})
		text := Debug(m.Render())
		if !strings.Contains(text, "\nS") || strings.Contains(text, "↓ 0 more") {
			t.Errorf("height %d: the selection is not shown:\n%s", h, text)
		}
		if again := Debug(m.Render()); again != text {
			t.Errorf("height %d: the next render differs:\n%s\nthen:\n%s", h, text, again)
		}
		for m.Selected > 0 {
			m.Handle(Key{Kind: KeyUp})
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
				in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%" + strconv.Itoa(n), EnvironmentID: "venv", Session: w.Session, Agent: "claude", Activity: protocol.Working, ActivityAt: now, Liveness: protocol.Alive, Managed: true, WorktreeID: w.ID, Identity: &protocol.Identity{PID: n, StartUnix: int64(n)}})
			}
		}
	}
	m := &Model{Now: now, View: ViewTree, Width: 60, Height: 11}
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in))
	m.Render()
	m.Handle(Key{Rune: 'G'})
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
	in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Session: "laatmux/new-one", Agent: "claude", Activity: protocol.Idle,
		ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/r/new-one", Identity: &protocol.Identity{PID: 9, StartUnix: 9}})
	task := func(id string, at time.Time) protocol.Pending {
		return protocol.Pending{ID: id, Host: "vm", EnvironmentID: "venv", Source: src, Repo: "laatmux", Branch: "new-one", Root: "/r/new-one", Session: "laatmux/new-one", Taken: true, SubmittedAt: at}
	}
	in.Pendings = []protocol.Pending{task("add-1", now.Add(-time.Minute)), task("add-2", now)}
	set := func(m *Model) {
		m.SetTree(rows.Tree(in))
		m.SetRows(rows.Agents(in))
	}
	m := &Model{Now: now, View: ViewTree, Width: 60, Height: 30}
	set(m)
	m.Render()
	if m.closed(&m.Tree[m.indexOf("add-2")]) != true {
		t.Fatal("the owner with an idle agent started open")
	}
	m.Select("add-2")
	m.Handle(Key{Rune: 'l'}) // opened by the user
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
	m.Handle(Key{Rune: 'h'})
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
	in.Agents = append(in.Agents, protocol.Agent{ID: "venv/laatmux/%9", EnvironmentID: "venv", Session: "laatmux/new-one", Agent: "claude", Activity: protocol.Idle,
		ActivityAt: now, Liveness: protocol.Alive, Managed: true, Cwd: "/r/new-one", Identity: &protocol.Identity{PID: 9, StartUnix: 9}})
	in.Pendings = []protocol.Pending{task("add-1", now.Add(-time.Minute)), task("add-2", now)}
	m = &Model{Now: now, View: ViewTree, Width: 60, Height: 30}
	set(m)
	m.Render()
	if m.successor("venv/worktree//r/new-one") != "add-2" || m.Tree[m.indexOf("add-2")].Children != 1 {
		t.Fatalf("the newest loose task does not hold the agent: %q", m.successor("venv/worktree//r/new-one"))
	}
	m.Select("add-2")
	m.Handle(Key{Rune: 'l'})
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
	m.SetRows(rows.Agents(in))
	m.SetTree(rows.Tree(in))
	m.Render()
	if len(m.Rows.Stale) != 1 {
		t.Fatalf("stale tiles: %d", len(m.Rows.Stale))
	}
	m.Handle(Key{Rune: 'f'}) // every fold open
	if !m.Select("venv/laatmux/%8") {
		t.Fatal("the stale agent is not visible in the tree")
	}
	m.Handle(Key{Kind: KeyTab})
	if r := m.Selection(); r == nil || r.ID() != "venv/laatmux/%8" || !m.ShowHidden {
		t.Errorf("switch to a stale tile: %+v shown %v", r, m.ShowHidden)
	}
}
