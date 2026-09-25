package view

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Key is one input event: a rune, a special key, a mouse event, or a
// bracketed paste with its text.
type Key struct {
	Rune  rune
	Kind  KeyKind
	X, Y  int    // mouse, 1-based cells
	Wheel int    // mouse: -1 up, +1 down, 0 for a click
	Text  string // paste: everything between the paste markers, line breaks as \n
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
	KeyTab
	KeyShiftTab
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyDelete
	// KeyNewline is Ctrl-J, a line feed: in raw mode Enter is \r, so
	// the two are told apart without any terminal extension.
	KeyNewline
	// KeyPaste is a bracketed paste, the terminal having been asked for
	// them: the text is inserted where the cursor is, never read as
	// keys, so a pasted line break is a newline and not a submit.
	KeyPaste
)

// Paste markers of bracketed paste mode.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// Decoder turns terminal input into keys across reads. Reads do not
// preserve write boundaries: an escape sequence or a multi-byte rune can
// arrive split, so bytes that could be the start of one are kept until
// the rest arrives or Flush says nothing more is coming, at which point
// a lone escape is the escape key and the rest are read as bytes.
type Decoder struct {
	pending []byte
	// discard is set when Flush dropped an incomplete escape sequence:
	// the rest of it may still arrive, and is swallowed through its
	// final byte rather than read as the keys its bytes spell.
	discard bool
	// paste is the text of a bracketed paste whose end has not arrived;
	// pasting is set from its start marker to its end. A paste is held
	// across reads and flushes however long it takes.
	paste   []byte
	pasting bool
}

// Feed adds input and returns the keys complete so far. Pending reports
// whether bytes are held back; the caller flushes them after a short
// wait, since a bare escape looks like the start of a sequence.
func (d *Decoder) Feed(b []byte) []Key {
	if d.discard {
		// A CSI or SS3 sequence ends at its first byte in 0x40..0x7e. A
		// fresh escape means the dropped sequence never completed; it
		// starts a new one and is parsed from there.
		i := 0
		for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) && b[i] != 0x1b {
			i++
		}
		if i == len(b) {
			return nil
		}
		if b[i] != 0x1b {
			i++
		}
		b = b[i:]
		d.discard = false
	}
	d.pending = append(d.pending, b...)
	var keys []Key
	for {
		if d.pasting {
			// Everything up to the end marker is the paste; the marker
			// may be split across reads, so a prefix of it at the end is
			// held.
			if i := strings.Index(string(d.pending), pasteEnd); i >= 0 {
				d.paste = append(d.paste, d.pending[:i]...)
				d.pending = d.pending[i+len(pasteEnd):]
				keys = append(keys, Key{Kind: KeyPaste, Text: pasteText(d.paste)})
				d.paste, d.pasting = nil, false
				continue
			}
			keep := markerPrefix(d.pending, pasteEnd)
			d.paste = append(d.paste, d.pending[:len(d.pending)-keep]...)
			d.pending = append([]byte(nil), d.pending[len(d.pending)-keep:]...)
			return keys
		}
		i := strings.Index(string(d.pending), pasteStart)
		if i < 0 {
			ks, rest := parse(d.pending, false)
			d.pending = rest
			return append(keys, ks...)
		}
		ks, rest := parse(d.pending[:i], true)
		keys = append(keys, ks...)
		_ = rest
		d.pending = d.pending[i+len(pasteStart):]
		d.pasting = true
	}
}

// markerPrefix is how many bytes at the end of b are a proper prefix
// of marker, held back for the rest of it.
func markerPrefix(b []byte, marker string) int {
	for n := min(len(b), len(marker)-1); n > 0; n-- {
		if string(b[len(b)-n:]) == marker[:n] {
			return n
		}
	}
	return 0
}

// pasteText is a paste's bytes as text: line breaks of any kind become
// newlines, escape sequences are dropped whole, and other control
// characters but tabs are dropped.
func pasteText(b []byte) string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var out strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == 0x1b:
			// CSI: through the final byte; SS3: one more; a lone escape
			// is dropped alone.
			if i+1 < len(rs) && rs[i+1] == '[' {
				i += 2
				for i < len(rs) && (rs[i] < 0x40 || rs[i] > 0x7e) {
					i++
				}
			} else if i+1 < len(rs) && rs[i+1] == 'O' {
				i += 2
			}
		case r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// Pending reports whether Feed held bytes back. A paste under way holds
// its text until its end marker arrives, so it is pending until then.
func (d *Decoder) Pending() bool { return len(d.pending) > 0 || d.pasting }

// Flush reads the held bytes as they are: a bare escape is the escape
// key; an incomplete sequence is dropped and its continuation, should
// it arrive, discarded; an incomplete rune is dropped. A paste under
// way is kept whole: its end is coming, however long it takes.
func (d *Decoder) Flush() []Key {
	if d.pasting {
		return nil
	}
	if len(d.pending) >= 2 && d.pending[0] == 0x1b && (d.pending[1] == '[' || d.pending[1] == 'O') {
		d.discard = true
	}
	keys, _ := parse(d.pending, true)
	d.pending = nil
	return keys
}

// Parse reads one complete chunk of input as keys, flushing what is
// incomplete.
func Parse(b []byte) []Key {
	var d Decoder
	keys := d.Feed(b)
	return append(keys, d.Flush()...)
}

// parse splits b into keys. With flush false, bytes that may be the
// start of an escape sequence or a rune are returned as rest instead;
// with flush true a bare escape is the escape key and an incomplete
// sequence is dropped, since a mouse report cut short would otherwise
// read as an escape and digits, and digits jump.
func parse(b []byte, flush bool) (keys []Key, rest []byte) {
	for len(b) > 0 {
		c := b[0]
		switch {
		case c == 0x1b:
			if len(b) == 1 {
				if !flush {
					return keys, b
				}
				return append(keys, Key{Kind: KeyEsc}), nil
			}
			if b[1] == '[' {
				k, n, ok := csi(b)
				if ok {
					keys = append(keys, k)
					b = b[n:]
					continue
				}
				if !flush {
					return keys, b
				}
				return keys, nil
			}
			if b[1] == 'O' {
				if len(b) < 3 {
					if !flush {
						return keys, b
					}
					return keys, nil
				} else {
					// SS3 keys, sent in application cursor mode.
					if kind, ok := ss3Keys[b[2]]; ok {
						keys = append(keys, Key{Kind: kind})
						b = b[3:]
						continue
					}
				}
			}
			keys = append(keys, Key{Kind: KeyEsc})
			b = b[1:]
		case c == '\r':
			keys = append(keys, Key{Kind: KeyEnter})
			b = b[1:]
		case c == '\n':
			keys = append(keys, Key{Kind: KeyNewline})
			b = b[1:]
		case c == '\t':
			keys = append(keys, Key{Kind: KeyTab})
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
			if !utf8.FullRune(b) {
				if !flush {
					return keys, b
				}
				// Never completed: drop the bytes rather than read them
				// as anything.
				return keys, nil
			}
			r, n := utf8.DecodeRune(b)
			if r != utf8.RuneError {
				keys = append(keys, Key{Rune: r})
			}
			b = b[n:]
		}
	}
	return keys, nil
}

// ss3Keys are the keys sent as ESC O x in application cursor mode.
var ss3Keys = map[byte]KeyKind{'A': KeyUp, 'B': KeyDown, 'C': KeyRight, 'D': KeyLeft, 'H': KeyHome, 'F': KeyEnd}

// csi reads one CSI sequence at the start of b: arrows, Home and End,
// Shift-Tab, the tilde keys and SGR mouse reports. Anything else it
// recognises the shape of is dropped.
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
	// A modified arrow, Ctrl-Right say, is not the plain key and is
	// dropped as before.
	plain := params == "" || params == "1"
	switch final {
	case 'A', 'B', 'C', 'D', 'H', 'F':
		if !plain {
			return Key{Kind: -1}, n, true
		}
		return Key{Kind: map[byte]KeyKind{'A': KeyUp, 'B': KeyDown, 'C': KeyRight, 'D': KeyLeft, 'H': KeyHome, 'F': KeyEnd}[final]}, n, true
	case 'Z':
		return Key{Kind: KeyShiftTab}, n, true
	case '~':
		switch params {
		case "1", "7":
			return Key{Kind: KeyHome}, n, true
		case "4", "8":
			return Key{Kind: KeyEnd}, n, true
		case "3":
			return Key{Kind: KeyDelete}, n, true
		}
		return Key{Kind: -1}, n, true
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
	ActionNone    ActionKind = iota
	ActionQuit               // q, Ctrl-C
	ActionJump               // Enter, a digit, a click: on Selection
	ActionOther              // a key the model does not know; the host may
	ActionConfirm            // y on a Confirm; ConfirmTag says which
	ActionOverlay            // the overlay is Done; the host reads and clears it
)

// Handle applies one key to the model and says what the host should
// do. The message line clears on any key. With an overlay up the key
// is the overlay's, and its finishing is the action. A confirm line
// takes the next key: y confirms, anything else withdraws it.
func (m *Model) Handle(k Key) Action {
	m.Message = ""
	if k.Kind < 0 {
		return Action{}
	}
	if m.Overlay != nil {
		m.Overlay.Handle(k)
		return m.Poll()
	}
	if m.Confirm != "" {
		m.Confirm = ""
		if k.Kind == KeyRune && (k.Rune == 'y' || k.Rune == 'Y') {
			return Action{Kind: ActionConfirm}
		}
		m.ConfirmTag = ""
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
			m.moveTo(i)
			return m.jump()
		}
	case KeyRune:
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
		case '/':
			m.Filtering = true
		case 'f':
			m.ShowHidden = !m.ShowHidden
			m.Selection()
		case 'q':
			return Action{Kind: ActionQuit}
		case '1', '2', '3', '4', '5', '6', '7', '8', '9':
			if i, ok := m.nth(int(k.Rune - '0')); ok {
				m.moveTo(i)
				return m.jump()
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
	m.Selected = target
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
	// With nothing selected the digits count the main group.
	g := GroupMain
	if m.Selected >= 0 {
		g = vis[m.Selected].Group
	}
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
