package view

import (
	"strings"
	"time"

	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/term"
)

// Action is what a key asks of the view's host.
type Action struct {
	Kind ActionKind
	Key  term.Key // for ActionOther, the key the model did not handle
	// Row, on ActionJump, is the row a click or a digit named, which
	// is the selection unless the selection follows the viewer's own
	// row and goes on doing so; nil for Enter, which jumps to the
	// selection. Mouse is a jump by a click, which tmux's click binding
	// made the view's pane active for.
	Row   *rows.Row
	Mouse bool
}

type ActionKind int

const (
	ActionNone    ActionKind = iota
	ActionQuit               // q, Ctrl-C
	ActionJump               // Enter, a digit, a click: on Row
	ActionOther              // a key the model does not know; the host may
	ActionConfirm            // y on a Confirm; ConfirmTag says which
	ActionOverlay            // the overlay is Done; the host reads and clears it
	// ActionSettings is a change of the view's settings, the view,
	// layout, scope or a fold: the host may persist what it keeps.
	ActionSettings
)

// Handle applies one key to the model and says what the host should
// do. The message line clears on any key. With an overlay up the key
// is the overlay's, and its finishing is the action. A confirm line
// takes the next key: y confirms, anything else withdraws it.
func (m *Model) Handle(k term.Key) Action {
	m.Message = ""
	if k.Kind < 0 {
		return Action{}
	}
	if m.Overlay != nil {
		m.Overlay.Handle(k)
		if h, ok := m.Overlay.(*Help); ok && h.Done() {
			// The help is the model's own: closed here.
			m.Overlay = nil
			return Action{}
		}
		return m.Poll()
	}
	if m.Confirm != "" {
		m.Confirm = ""
		if k.Kind == term.KeyRune && (k.Rune == 'y' || k.Rune == 'Y') {
			if m.ConfirmTag == "quit" {
				m.ConfirmTag = ""
				return Action{Kind: ActionQuit}
			}
			return Action{Kind: ActionConfirm}
		}
		m.ConfirmTag = ""
		return Action{}
	}
	if m.Filtering {
		switch k.Kind {
		case term.KeyEsc:
			m.Filter, m.Filtering = "", false
		case term.KeyEnter, term.KeyNewline:
			m.Filtering = false
		case term.KeyBackspace, term.KeyRune, term.KeyPaste:
			m.Filter = edited(m.Filter, k)
		case term.KeyUp:
			m.move(-1)
		case term.KeyDown:
			m.move(1)
		case term.KeyCtrlC:
			return m.quit()
		}
		m.Selection()
		return Action{}
	}
	if m.Layout == Strip {
		if a, handled := m.stripKey(k); handled {
			return a
		}
	}
	switch k.Kind {
	case term.KeyUp:
		m.move(-1)
	case term.KeyDown:
		m.move(1)
	case term.KeyEnter, term.KeyNewline:
		// A newline is Enter on the list, as \n was before the form
		// told the two apart.
		return m.jump()
	case term.KeyEsc:
		m.Filter = ""
		m.Selection()
	case term.KeyCtrlC:
		return m.quit()
	case term.KeyTab:
		m.Switch()
	case term.KeyLeft:
		m.foldKey(false)
	case term.KeyRight:
		m.foldKey(true)
	case term.KeyMouse:
		if k.Wheel != 0 {
			m.move(k.Wheel)
			return Action{}
		}
		if m.Tabs && k.Y == 1 {
			// A click on the other tab switches; on the one shown, or
			// beside them, nothing.
			if v := tabAt(k.X); v != "" && v != m.View {
				m.Switch()
			}
			return Action{}
		}
		if i := m.hitRow(k.Y, k.At); i >= 0 {
			if r := m.Visible()[i].Row; r.Foldable() && (r.Kind == rows.KindRepo || r.Kind == rows.KindFold || k.X <= 2*r.Depth+2) {
				// A click on a repository or fold line, or on a
				// line's fold mark, folds.
				m.moveTo(i)
				m.toggleFold(r)
				return Action{}
			}
			a := m.jumpTo(i)
			a.Mouse = a.Kind == ActionJump
			return a
		}
	case term.KeyRune:
		switch k.Rune {
		case 'j':
			m.move(1)
		case 'k':
			m.move(-1)
		case 'g':
			m.moveTo(0)
		case 'G':
			m.moveTo(len(m.Visible()) - 1)
		case 'v':
			if m.Layout == Tiles {
				m.Layout = Compact
			} else {
				m.Layout = Tiles
			}
			m.settings, m.layoutSet = true, true
		case '/':
			m.Filtering = true
		case 'f':
			// The selection stays on its row, or, folded away, on the
			// line over it.
			id := ""
			if r := m.Selection(); r != nil && !m.Follow {
				id = r.ID()
			}
			m.foldAll()
			if id != "" && !m.Select(id) {
				m.selectAncestor(id)
			}
			m.Selection()
		case 'h':
			m.foldKey(false)
		case 'l':
			m.foldKey(true)
		case 's':
			if r := m.Selection(); r != nil {
				m.toggleFold(r)
				m.Selection()
			}
		case 'F':
			m.ToggleScope()
			m.Selection()
		case 'q':
			return m.quit()
		case '?':
			m.Overlay = NewHelp(m.HelpTitle, m.Layout == Strip, m.Help...)
		case '1', '2', '3', '4', '5', '6', '7', '8', '9':
			if i, ok := m.nth(int(k.Rune - '0')); ok {
				return m.jumpTo(i)
			}
		default:
			return Action{Kind: ActionOther, Key: k}
		}
	}
	return Action{}
}

// Poll is the action an overlay's finishing is, when it has: a host
// whose overlay ends on its own, a command's log say, asks after every
// change signal.
func (m *Model) Poll() Action {
	if m.Overlay != nil && m.Overlay.Done() {
		return Action{Kind: ActionOverlay}
	}
	return Action{}
}

// Ask puts a question in the footer for the next key to answer.
func (m *Model) Ask(question, tag string) {
	m.Confirm, m.ConfirmTag = question, tag
}

// quit is q or Ctrl-C: the end, or in a sidebar pane, where a key meant
// for another pane is common, a question first.
func (m *Model) quit() Action {
	if m.AskQuit {
		m.Ask("Quit sidebar? y/n", "quit")
		return Action{}
	}
	return Action{Kind: ActionQuit}
}

func (m *Model) move(d int) { m.moveTo(m.Selected + d) }

// moveTo puts the selection on the row at i, clamped. A move that puts
// it on another row than it was on makes the selection the user's: it
// stops following the viewer's own row and stays where the user put it.
// A move that changes nothing, up from the first row or onto the row
// already selected, leaves the following as it was.
func (m *Model) moveTo(i int) {
	n := len(m.Visible())
	target := min(max(i, 0), n-1) // -1 on an empty list
	if target != m.Selected {
		m.Follow = false
	}
	m.Selected, m.lost = target, false
	m.Selection()
}

func (m *Model) jump() Action {
	r := m.Selection()
	if r == nil {
		return Action{}
	}
	if r.Kind == rows.KindRepo || r.Kind == rows.KindFold {
		// Enter folds a repository line and the stale fold; a worktree
		// line jumps, its fold being h, l and s.
		m.toggleFold(r)
		m.Selection()
		return Action{}
	}
	return Action{Kind: ActionJump}
}

// foldKey is h and l, Left and Right: h folds the selected line, or
// from a child goes to its worktree or task line; l unfolds.
func (m *Model) foldKey(open bool) {
	r := m.Selection()
	if r == nil {
		return
	}
	switch {
	case open && r.Foldable() && m.closed(r):
		m.toggleFold(r)
	case !open && r.Foldable() && !m.closed(r):
		m.toggleFold(r)
	case !open:
		if p := m.parentOf(); p >= 0 {
			m.moveTo(p)
		}
	}
	m.Selection()
}

// Select puts the selection on the visible row with the id, as a key
// would, which makes it the user's: the host's answer to a click that
// jumped nowhere, so the row clicked is the one the next key acts on.
// False when no visible row has the id.
func (m *Model) Select(id string) bool {
	for _, it := range m.Visible() {
		if it.Row.ID() == id {
			// Following ends even when the row is the one it was on: it
			// is the user's from here, and a later refresh must not move
			// the selection off it.
			m.moveTo(it.Index)
			m.Follow = false
			return true
		}
	}
	return false
}

// jumpTo is a jump to the visible row at i, by a click or a digit. A
// selection that follows the viewer's own row goes on following it: the
// jump takes the viewer to that row's session, and when they are back
// here the selection is on their own row again rather than on the one
// they clicked. A selection that is the user's moves to the row, as a
// key would move it.
func (m *Model) jumpTo(i int) Action {
	vis := m.Visible()
	if i < 0 || i >= len(vis) {
		return Action{}
	}
	if !m.Follow {
		m.moveTo(i)
	}
	return Action{Kind: ActionJump, Row: vis[i].Row}
}

// nth is the index of the nth numbered row: the nth tile of the agent
// view, the nth worktree or task line of the tree; folds, groups and
// repository lines are skipped, and so are a worktree's children.
func (m *Model) nth(n int) (int, bool) {
	for _, it := range m.Visible() {
		if !it.Row.Numbered() {
			continue
		}
		n--
		if n == 0 {
			return it.Index, true
		}
	}
	return 0, false
}

// hitRow is the visible row now that the screen clicked drew on line y
// (1-based), found by its id, -1 when that line drew no row or the row
// is no longer visible: what was clicked is what was on screen, whatever
// a refresh or a key since has done to the indexes. The screen clicked
// is the last drawn before the click was read, at: the previous render
// when one was drawn after it, the click dropped when two were; the
// last render when at is zero.
func (m *Model) hitRow(y int, at time.Time) int {
	ids, top := m.hitIDs, m.hitTop
	if !at.IsZero() && at.Before(m.hitAt) {
		if m.hitPrevAt.IsZero() || at.Before(m.hitPrevAt) {
			return -1
		}
		ids, top = m.hitPrevIDs, m.hitPrevTop
	}
	i := y - 1 - top
	if i < 0 || i >= len(ids) || ids[i] == "" {
		return -1
	}
	for _, it := range m.Visible() {
		if it.Row.ID() == ids[i] {
			return it.Index
		}
	}
	return -1
}

// edited is one line of text after a key: a rune or a paste's text, as
// one line, appended; backspace with the last rune gone.
func edited(s string, k term.Key) string {
	switch k.Kind {
	case term.KeyRune:
		return s + string(k.Rune)
	case term.KeyPaste:
		return s + pasteLine(k.Text)
	case term.KeyBackspace:
		if r := []rune(s); len(r) > 0 {
			return string(r[:len(r)-1])
		}
	}
	return s
}

// pasteLine is a paste as one line of text, for a filter or a name:
// line breaks and tabs become spaces.
func pasteLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, s)
}
