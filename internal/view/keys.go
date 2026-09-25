package view

import (
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/laat/laatmux/internal/rows"
)

// Key is one input event: a rune, a special key, a mouse event, or a
// bracketed paste with its text.
type Key struct {
	Rune  rune
	Kind  KeyKind
	X, Y  int    // mouse, 1-based cells
	Wheel int    // mouse: -1 up, +1 down, 0 for a click
	Text  string // paste: everything between the paste markers, line breaks as \n
	// At, on a click, is when its bytes were read: the screen clicked is
	// the last drawn before it. Zero when unknown.
	At time.Time
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
	// at is when the last byte arrived, for the bounds: a paste whose
	// end marker never comes is taken as it is once its bytes have
	// stopped for the grace, and a held start of a paste marker is
	// dropped after as long. now is the clock, for tests.
	at  time.Time
	now func() time.Time
	// stalled is a paste whose bytes stopped for the grace: its text
	// so far has been given out and the framing stays; stalledAt is
	// the arrival the stall was for, so nothing is looked at again
	// until more bytes come. A bare escape or Ctrl-C among the bytes
	// after a stall is the user's, and ends the paste.
	stalled   bool
	stalledAt time.Time
	// heldAt is when the first of the held bytes was read, for FeedAt:
	// a click whose bytes came in two reads is dated from the first.
	heldAt time.Time
}

// Bounds on a paste: the silence after which its text so far is given
// out, and the size past which a chunk is. Neither ends the framing,
// so a pasted line break after them is never Enter; a paste whose end
// marker is lost ends on the user's bare escape or Ctrl-C after a
// stall, bytes no paste sends alone, and that key only ends it.
const (
	pasteGrace = time.Second
	pasteMax   = 1 << 20
)

func (d *Decoder) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// Feed adds input and returns the keys complete so far. Pending reports
// whether bytes are held back; the caller flushes them after a short
// wait, since a bare escape looks like the start of a sequence.
func (d *Decoder) Feed(b []byte) []Key { return d.FeedAt(b, time.Time{}) }

// FeedAt is Feed for bytes read at the time given: each click it gives
// has At set to when its first byte was read, the held bytes' time for
// a click they begin and this read's for one begun in it, whatever the
// held bytes turned out to be.
func (d *Decoder) FeedAt(b []byte, at time.Time) []Key {
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
	// Offsets below are into the held bytes and this read's, joined:
	// those before h were held, and d.pending stays a suffix of them.
	h, held := len(d.pending), d.heldAt
	d.pending = append(d.pending, b...)
	total := len(d.pending)
	stamp := func(base int) func(int) time.Time {
		return func(off int) time.Time {
			if base+off < h {
				return held
			}
			return at
		}
	}
	defer func() {
		switch {
		case len(d.pending) == 0:
			d.heldAt = time.Time{}
		case total-len(d.pending) >= h:
			d.heldAt = at
		}
	}()
	d.at = d.clock()
	var keys []Key
	for {
		if d.pasting {
			// Everything up to the end marker is the paste; the marker
			// may be split across reads, so a prefix of it at the end is
			// held.
			if i := strings.Index(string(d.pending), pasteEnd); i >= 0 {
				d.paste = append(d.paste, d.pending[:i]...)
				d.pending = d.pending[i+len(pasteEnd):]
				// A paste whose text was given out at a stall ends
				// with nothing more to give.
				if text := pasteText(d.paste); text != "" || !d.stalled {
					keys = append(keys, Key{Kind: KeyPaste, Text: text})
				}
				d.paste, d.pasting, d.stalled = nil, false, false
				continue
			}
			keep := markerPrefix(d.pending, pasteEnd)
			d.paste = append(d.paste, d.pending[:len(d.pending)-keep]...)
			d.pending = append([]byte(nil), d.pending[len(d.pending)-keep:]...)
			if len(d.paste) > pasteMax {
				// Given out in chunks past the cap, an incomplete rune
				// and a trailing carriage return held for the next; the
				// framing stays.
				chunk, tail := splitTail(d.paste)
				keys = append(keys, Key{Kind: KeyPaste, Text: pasteText(chunk)})
				d.paste = tail
			}
			return keys
		}
		i := strings.Index(string(d.pending), pasteStart)
		if i < 0 {
			ks, rest := parse(d.pending, false, stamp(total-len(d.pending)))
			d.pending = rest
			return append(keys, ks...)
		}
		ks, rest := parse(d.pending[:i], true, stamp(total-len(d.pending)))
		keys = append(keys, ks...)
		_ = rest
		d.pending = d.pending[i+len(pasteStart):]
		d.pasting = true
	}
}

// stall is the flush of a paste whose bytes have stopped for the
// grace. The text so far is given out and the framing kept: a resumed
// paste's line break is still no Enter. A paste with a lost end marker
// is ended by the user's Esc or Ctrl-C once it has stalled: the last
// bare escape, one at the end or followed by another escape, or
// Ctrl-C byte among the bytes after a stall ends the framing there,
// the text before it is the paste, the key itself is spent on that,
// so the form it goes to keeps its prompt, and the bytes after it are
// keys. Bytes from separate reads are joined here, so an Esc followed
// within the grace by another key reads as a chord and is paste: the
// user presses Esc and waits, which fails safe. A prefix of the end
// marker, an incomplete rune and a trailing carriage return are held
// for the next bytes.
func (d *Decoder) stall() []Key {
	held := markerPrefix(d.pending, pasteEnd)
	if held != len(d.pending) || held == 1 {
		// Not a marker prefix, or a bare escape alone, which is the
		// user's Esc once the bytes have stopped: a marker's second
		// byte would have come with it.
		held = 0
	}
	data := append(append([]byte(nil), d.paste...), d.pending[:len(d.pending)-held]...)
	d.pending = append([]byte(nil), d.pending[len(d.pending)-held:]...)
	if i, _ := lastRecovery(data); i >= 0 && d.stalled {
		var keys []Key
		if text := pasteText(data[:i]); text != "" {
			keys = append(keys, Key{Kind: KeyPaste, Text: text})
		}
		// The bytes after the key were joined across reads, so when a
		// click among them was read is not known, nor which screen it
		// was on: clicks there are dropped rather than resolved on the
		// screen drawn now.
		rest, _ := parse(data[i+1:], true, nil)
		for _, k := range rest {
			if k.Kind != KeyMouse {
				keys = append(keys, k)
			}
		}
		d.paste, d.pending, d.pasting, d.stalled = nil, nil, false, false
		return keys
	}
	d.stalled, d.stalledAt = true, d.at
	text, tail := splitTail(data)
	d.paste = tail
	if t := pasteText(text); t != "" {
		return []Key{{Kind: KeyPaste, Text: t}}
	}
	return nil
}

// lastRecovery is the index of the last byte that is the user's Esc or
// Ctrl-C in a stalled paste, and its key; -1 when there is none. The
// Esc is a bare escape, one at the end or followed by another: one
// followed by any other byte is a sequence or an Alt chord, as
// everywhere else.
func lastRecovery(b []byte) (int, KeyKind) {
	for i := len(b) - 1; i >= 0; i-- {
		switch b[i] {
		case 0x03:
			return i, KeyCtrlC
		case 0x1b:
			if i+1 == len(b) || b[i+1] == 0x1b {
				return i, KeyEsc
			}
		}
	}
	return -1, 0
}

// splitTail takes an incomplete trailing rune, an unfinished escape
// sequence and a trailing carriage return off b, to be held for the
// bytes that follow, so a chunk never ends inside a character or a
// sequence, whose rest would read as text, or between a \r and its \n.
func splitTail(b []byte) (head, tail []byte) {
	cut := len(b)
	for i := max(len(b)-4, 0); i < len(b); i++ {
		if r, _ := utf8.DecodeRune(b[i:]); r == utf8.RuneError && !utf8.FullRune(b[i:]) {
			cut = i
			break
		}
	}
	// A sequence is bounded: an escape further back than this is not
	// one still under way.
	for i := cut - 1; i >= max(cut-32, 0); i-- {
		if b[i] != 0x1b {
			continue
		}
		if i+1 < cut && b[i+1] == '[' {
			done := false
			for j := i + 2; j < cut; j++ {
				if b[j] >= 0x40 && b[j] <= 0x7e {
					done = true
					break
				}
			}
			if !done {
				cut = i
			}
		} else if i+1 < cut && b[i+1] == 'O' && i+2 >= cut {
			cut = i
		}
		break
	}
	if cut > 0 && cut == len(b) && b[cut-1] == '\r' {
		cut--
	}
	return b[:cut], append([]byte(nil), b[cut:]...)
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

// Wait is how long until a Flush has something to do: the escape wait
// for held bytes, what is left of the grace for a paste or for a held
// escape and bracket, and nothing at all when nothing is held, a
// longer start of a marker is held, or a stalled paste has had no
// bytes since, so the caller sets no timer and draws nothing until
// input comes.
func (d *Decoder) Wait() time.Duration {
	grace := func() time.Duration { return max(pasteGrace-d.clock().Sub(d.at), time.Millisecond) }
	switch {
	case d.pasting:
		if d.stalled && d.stalledAt.Equal(d.at) {
			return 0
		}
		return grace()
	case len(d.pending) == 0:
		return 0
	case d.startHeld():
		if len(d.pending) > 2 {
			return 0
		}
		return grace()
	}
	return escapeWait
}

// startHeld is a held prefix of the paste start marker, and nothing
// else, in the pending bytes.
func (d *Decoder) startHeld() bool {
	n := markerPrefix(d.pending, pasteStart)
	return n == len(d.pending) && n >= 2
}

// Flush reads the held bytes as they are: a bare escape is the escape
// key; an incomplete sequence is dropped and its continuation, should
// it arrive, discarded; an incomplete rune is dropped. A paste under
// way is kept whole: its end is coming, however long it takes.
func (d *Decoder) Flush() []Key {
	if d.pasting {
		if d.clock().Sub(d.at) < pasteGrace {
			return nil
		}
		return d.stall()
	}
	// The start of a paste marker, split by a slow read, is held too,
	// however long: dropped, the paste's text would be read as keys,
	// its line breaks as Enter. An escape and bracket alone are as
	// likely Alt-[, and are dropped whole after the grace rather than
	// with the next key.
	if d.startHeld() {
		if len(d.pending) > 2 || d.clock().Sub(d.at) < pasteGrace {
			return nil
		}
		d.pending, d.heldAt = nil, time.Time{}
		return nil
	}
	if len(d.pending) >= 2 && d.pending[0] == 0x1b && (d.pending[1] == '[' || d.pending[1] == 'O') {
		d.discard = true
	}
	heldAt := d.heldAt
	keys, _ := parse(d.pending, true, func(int) time.Time { return heldAt })
	d.pending, d.heldAt = nil, time.Time{}
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
// read as an escape and digits, and digits jump. stamp, when set, dates
// a click by the offset of its first byte in b.
func parse(b []byte, flush bool, stamp func(off int) time.Time) (keys []Key, rest []byte) {
	whole := len(b)
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
					if k.Kind == KeyMouse && stamp != nil {
						k.At = stamp(whole - len(b))
					}
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
				}
				// SS3 keys, sent in application cursor mode; the rest,
				// F1 to F4 say, are dropped whole.
				if kind, ok := ss3Keys[b[2]]; ok {
					keys = append(keys, Key{Kind: kind})
				}
				b = b[3:]
				continue
			}
			if b[1] == '\r' || b[1] == '\n' {
				// Alt-Enter, and what a terminal bound to send it for
				// Shift-Enter sends: a newline, never a submit.
				keys = append(keys, Key{Kind: KeyNewline})
				b = b[2:]
				continue
			}
			if b[1] != 0x1b {
				// Escape then another byte in one read is an Alt chord,
				// Alt-Backspace or Alt-b say, the terminal sending the
				// key with the escape in front; it is not the Esc that
				// cancels a form, and is dropped whole.
				b = b[2:]
				continue
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

// extended is the key an extended-key report names: the code is the
// key's unmodified character, alt its shifted one when the report has
// it, and mod one more than the modifier bits (shift 1, alt 2, ctrl
// 4). Enter with any modifier is a newline, so Shift-Enter and
// Ctrl-Enter break a line as Ctrl-J does; Shift-Tab is itself; the
// plain keys are themselves; a modified letter or other chord is
// dropped, as an unknown sequence is.
func extended(code, alt, mod int) Key {
	if mod < 1 {
		mod = 1
	}
	plain, shift := mod == 1, mod == 2
	switch code {
	case 13:
		if plain {
			return Key{Kind: KeyEnter}
		}
		return Key{Kind: KeyNewline}
	case 10:
		return Key{Kind: KeyNewline}
	case 9:
		if plain {
			return Key{Kind: KeyTab}
		}
		if shift {
			return Key{Kind: KeyShiftTab}
		}
	case 27:
		if plain {
			return Key{Kind: KeyEsc}
		}
	case 127, 8:
		if plain || shift {
			return Key{Kind: KeyBackspace}
		}
	case 3:
		return Key{Kind: KeyCtrlC}
	}
	if mod == 5 {
		// Control chords at xterm's second level, where every modified
		// key is a sequence: the ones the view reads are their bytes.
		switch code {
		case 'c':
			return Key{Kind: KeyCtrlC}
		case 'j':
			return Key{Kind: KeyNewline}
		case 'm':
			return Key{Kind: KeyEnter}
		case 'i':
			return Key{Kind: KeyTab}
		case 'h':
			return Key{Kind: KeyBackspace}
		case '[':
			return Key{Kind: KeyEsc}
		}
	}
	switch {
	case code < 0x20 || code == 0x7f:
	case plain:
		return Key{Rune: rune(code)}
	case shift && alt >= 0x20:
		return Key{Rune: rune(alt)}
	case shift && unicode.IsLetter(rune(code)):
		// tmux reports a shifted letter by its unshifted code; another
		// shifted key's character is not known from its code, and is
		// dropped rather than inserted wrong.
		return Key{Rune: unicode.ToUpper(rune(code))}
	}
	return Key{Kind: -1}
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
	if final < 0x40 || final > 0x7e {
		// Not a final byte: the sequence was cut short, as Alt-[ or an
		// Esc and [ in one read are, and a new escape may start here.
		// The broken part is dropped and the rest parsed afresh, so a
		// click after it is a click and not its digits, which jump.
		return Key{Kind: -1}, i, true
	}
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
	case 'u':
		// CSI u, an extended key: the code, a shifted alternative
		// after a colon, and the modifier after a semicolon.
		f := strings.SplitN(params, ";", 2)
		codes := strings.Split(f[0], ":")
		code, err := strconv.Atoi(codes[0])
		if err != nil {
			return Key{Kind: -1}, n, true
		}
		mod := 1
		if len(f) == 2 {
			mod, _ = strconv.Atoi(strings.Split(f[1], ":")[0])
		}
		alt := 0
		if len(codes) > 1 {
			alt, _ = strconv.Atoi(codes[1])
		}
		return extended(code, alt, mod), n, true
	case '~':
		if strings.HasPrefix(params, "27;") {
			// The xterm form of an extended key: 27, the modifier,
			// the code.
			f := strings.Split(params, ";")
			if len(f) == 3 {
				mod, _ := strconv.Atoi(f[1])
				code, err := strconv.Atoi(f[2])
				if err == nil {
					return extended(code, 0, mod), n, true
				}
			}
			return Key{Kind: -1}, n, true
		}
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
		case KeyEnter, KeyNewline:
			m.Filtering = false
		case KeyBackspace:
			if r := []rune(m.Filter); len(r) > 0 {
				m.Filter = string(r[:len(r)-1])
			}
		case KeyRune:
			m.Filter += string(k.Rune)
		case KeyPaste:
			m.Filter += pasteLine(k.Text)
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
	case KeyEnter, KeyNewline:
		// A newline is Enter on the list, as \n was before the form
		// told the two apart.
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
		if i := m.hitRow(k.Y, k.At); i >= 0 {
			a := m.jumpTo(i)
			a.Mouse = a.Kind == ActionJump
			return a
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
	if m.Selection() == nil {
		return Action{}
	}
	return Action{Kind: ActionJump}
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
