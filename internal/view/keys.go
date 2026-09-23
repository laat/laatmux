package view

import (
	"strconv"
	"strings"
)

// Key is one input event: a rune, a special key, or a mouse event.
type Key struct {
	Rune  rune
	Kind  KeyKind
	X, Y  int // mouse, 1-based cells
	Wheel int // mouse: -1 up, +1 down, 0 for a click
}

type KeyKind int

const (
	KeyRune KeyKind = iota
	KeyUp
	KeyDown
	KeyEnter
	KeyEsc
	KeyBackspace
	KeyMouse // a left-button press or a wheel step
	KeyCtrlC
)

// Parse splits terminal input into keys. tmux writes each key's bytes
// in one go, so a chunk that ends in a bare escape is the escape key
// and a sequence is never split across chunks in practice; one that is
// split is read as its bytes.
func Parse(b []byte) []Key {
	var keys []Key
	for len(b) > 0 {
		c := b[0]
		switch {
		case c == 0x1b:
			if len(b) == 1 {
				return append(keys, Key{Kind: KeyEsc})
			}
			if b[1] == '[' {
				if k, n, ok := csi(b); ok {
					keys = append(keys, k)
					b = b[n:]
					continue
				}
			}
			if b[1] == 'O' && len(b) >= 3 {
				// SS3 arrows, sent in application cursor mode.
				switch b[2] {
				case 'A':
					keys = append(keys, Key{Kind: KeyUp})
					b = b[3:]
					continue
				case 'B':
					keys = append(keys, Key{Kind: KeyDown})
					b = b[3:]
					continue
				}
			}
			keys = append(keys, Key{Kind: KeyEsc})
			b = b[1:]
		case c == '\r' || c == '\n':
			keys = append(keys, Key{Kind: KeyEnter})
			b = b[1:]
		case c == 0x7f || c == 0x08:
			keys = append(keys, Key{Kind: KeyBackspace})
			b = b[1:]
		case c == 0x03:
			keys = append(keys, Key{Kind: KeyCtrlC})
			b = b[1:]
		case c < 0x20:
			b = b[1:]
		default:
			r, n := rune(c), 1
			if c >= 0x80 {
				r, n = decodeRune(b)
			}
			keys = append(keys, Key{Rune: r})
			b = b[n:]
		}
	}
	return keys
}

func decodeRune(b []byte) (rune, int) {
	for _, r := range string(b) {
		return r, len(string(r))
	}
	return 0, 1
}

// csi reads one CSI sequence at the start of b: arrows and SGR mouse
// reports. Anything else it recognises the shape of is dropped.
func csi(b []byte) (Key, int, bool) {
	i := 2
	for i < len(b) && (b[i] >= 0x30 && b[i] <= 0x3f) {
		i++
	}
	for i < len(b) && (b[i] >= 0x20 && b[i] <= 0x2f) {
		i++
	}
	if i >= len(b) {
		return Key{}, 0, false
	}
	final := b[i]
	params := string(b[2:i])
	n := i + 1
	switch final {
	case 'A':
		return Key{Kind: KeyUp}, n, true
	case 'B':
		return Key{Kind: KeyDown}, n, true
	case 'M', 'm':
		if strings.HasPrefix(params, "<") {
			f := strings.Split(params[1:], ";")
			if len(f) == 3 {
				btn, _ := strconv.Atoi(f[0])
				x, _ := strconv.Atoi(f[1])
				y, _ := strconv.Atoi(f[2])
				switch {
				case btn == 64:
					return Key{Kind: KeyMouse, X: x, Y: y, Wheel: -1}, n, true
				case btn == 65:
					return Key{Kind: KeyMouse, X: x, Y: y, Wheel: 1}, n, true
				case btn&(3|32|64|128) == 0 && final == 'M':
					// Left button press, with or without modifiers
					// (bits 4, 8, 16), not another button, a drag (bit
					// 32), a wheel step or a release.
					return Key{Kind: KeyMouse, X: x, Y: y}, n, true
				}
			}
		}
		return Key{Kind: -1}, n, true
	}
	return Key{Kind: -1}, n, true
}

// Action is what a key asks of the view's host.
type Action struct {
	Kind ActionKind
	Key  Key // for ActionOther, the key the model did not handle
}

type ActionKind int

const (
	ActionNone  ActionKind = iota
	ActionQuit             // q, Ctrl-C
	ActionJump             // Enter, a digit, a click: on Selection
	ActionOther            // a key the model does not know; the host may
)

// Handle applies one key to the model and says what the host should
// do. The message line clears on any key.
func (m *Model) Handle(k Key) Action {
	m.Message = ""
	if k.Kind < 0 {
		return Action{}
	}
	if m.Filtering {
		switch k.Kind {
		case KeyEsc:
			m.Filter, m.Filtering = "", false
		case KeyEnter:
			m.Filtering = false
		case KeyBackspace:
			if r := []rune(m.Filter); len(r) > 0 {
				m.Filter = string(r[:len(r)-1])
			}
		case KeyRune:
			m.Filter += string(k.Rune)
		case KeyUp:
			m.move(-1)
		case KeyDown:
			m.move(1)
		case KeyCtrlC:
			return Action{Kind: ActionQuit}
		}
		m.Selection()
		return Action{}
	}
	switch k.Kind {
	case KeyUp:
		m.move(-1)
	case KeyDown:
		m.move(1)
	case KeyEnter:
		return m.jump()
	case KeyEsc:
		m.Filter = ""
		m.Selection()
	case KeyCtrlC:
		return Action{Kind: ActionQuit}
	case KeyMouse:
		if k.Wheel != 0 {
			m.move(k.Wheel)
			return Action{}
		}
		if i := m.hit(k.Y); i >= 0 {
			m.Selected = i
			return m.jump()
		}
	case KeyRune:
		switch k.Rune {
		case 'j':
			m.move(1)
		case 'k':
			m.move(-1)
		case 'g':
			m.Selected = 0
		case 'G':
			m.Selected = len(m.Visible()) - 1
			m.Selection()
		case 'v':
			if m.Layout == Tiles {
				m.Layout = Compact
			} else {
				m.Layout = Tiles
			}
		case '/':
			m.Filtering = true
		case 'f':
			m.ShowHidden = !m.ShowHidden
			m.Selection()
		case 'q':
			return Action{Kind: ActionQuit}
		case '1', '2', '3', '4', '5', '6', '7', '8', '9':
			if i, ok := m.nth(int(k.Rune - '0')); ok {
				m.Selected = i
				return m.jump()
			}
		default:
			return Action{Kind: ActionOther, Key: k}
		}
	}
	return Action{}
}

func (m *Model) move(d int) {
	m.Selected += d
	m.Selection()
}

func (m *Model) jump() Action {
	if m.Selection() == nil {
		return Action{}
	}
	return Action{Kind: ActionJump}
}

// nth is the index of the nth row of the group the selection is in.
func (m *Model) nth(n int) (int, bool) {
	vis := m.Visible()
	m.clamp(len(vis))
	if len(vis) == 0 {
		return 0, false
	}
	g := vis[m.Selected].Group
	for _, it := range vis {
		if it.Group != g {
			continue
		}
		n--
		if n == 0 {
			return it.Index, true
		}
	}
	return 0, false
}

// hit is the row on screen line y (1-based), -1 for none. The body
// starts after the header lines.
func (m *Model) hit(y int) int {
	i := y - 1 - len(m.Header)
	if i < 0 || i >= len(m.hits) {
		return -1
	}
	return m.hits[i]
}
