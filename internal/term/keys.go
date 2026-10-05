// Package term is the terminal: raw mode and the alternate screen, the
// frame drawn, and the bytes read decoded into keys, mouse events and
// pastes.
package term

import (
	"bytes"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/laat/laatmux/internal/palette"
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
	// osc is set when Flush dropped an incomplete OSC string, a late
	// answer to the background query cut by a pause: its rest is
	// swallowed through BEL or ST. oscEsc is an escape at the end of
	// the last bytes, the start of ST; oscLeft bounds the swallowing,
	// in bytes, and oscUntil in time, so an Alt-] the user typed does
	// not eat the keys after it.
	osc      bool
	oscEsc   bool
	oscLeft  int
	oscUntil time.Time
	// expectUntil is how long an answer to the background query may
	// still come: until then an OSC cut anywhere from its escape and
	// bracket on is armed against (one cut after the escape alone
	// reads as Esc and keys, as any other sequence would); after it only one whose
	// number and semicolon came, so an Alt-] and the keys after it are
	// the user's.
	expectUntil time.Time
	// oscBuf is the OSC string being swallowed so far, to tell the
	// answer from an echo when it ends.
	oscBuf []byte
	// paste is the text of a bracketed paste whose end has not arrived;
	// pasting is set from its start marker to its end. A paste is held
	// across reads and flushes however long it takes.
	paste   []byte
	pasting bool
	// at is when the last byte arrived, for the bounds: a paste whose
	// end marker never comes is taken as it is once its bytes have
	// stopped for the grace, and a held start of a paste marker is
	// dropped after as long.
	at time.Time
	// Now is the clock; nil is time.Now. Tests set it.
	Now func() time.Time
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

// Bounds on a paste: PasteGrace the silence after which its text so far
// is given out, pasteMax the size past which a chunk is. Neither ends
// the framing, so a pasted line break after them is never Enter; a paste whose end
// marker is lost ends on the user's bare escape or Ctrl-C after a
// stall, bytes no paste sends alone, and that key only ends it.
const (
	PasteGrace = time.Second
	pasteMax   = 1 << 20
)

func (d *Decoder) clock() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// FeedAt adds input read at the time given and returns the keys
// complete so far; bytes that may start a sequence are held back, and
// the caller flushes them after Wait, since a bare escape looks like
// the start of one. Each click it gives has At set to when its first
// byte was read, the held bytes' time for a click they begin and this
// read's for one begun in it, whatever the held bytes turned out to be.
func (d *Decoder) FeedAt(b []byte, at time.Time) []Key {
	if d.osc {
		b = d.swallowOSC(b)
		if b == nil {
			return nil
		}
	}
	if d.discard {
		// A CSI or SS3 sequence goes on through parameter and
		// intermediate bytes, 0x20..0x3f, and ends at a final byte in
		// 0x40..0x7e, dropped with it. Any other byte, a fresh escape,
		// Ctrl-C or Enter say, means the dropped sequence never
		// completed, as csi takes it: it is the user's and is parsed
		// from there.
		i := 0
		for i < len(b) && b[i] >= 0x20 && b[i] <= 0x3f {
			i++
		}
		if i == len(b) {
			return nil
		}
		if b[i] >= 0x40 && b[i] <= 0x7e {
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
			d.seeAnswer(d.pending)
			ks, rest := parse(d.pending, false, stamp(total-len(d.pending)))
			d.pending = rest
			return append(keys, ks...)
		}
		d.seeAnswer(d.pending[:i])
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
	// one still under way. One is over at its final byte, or at a byte
	// that cannot go on, which ends it as csi and pasteText end it.
	for i := cut - 1; i >= max(cut-32, 0); i-- {
		if b[i] != 0x1b {
			continue
		}
		if i+1 < cut && b[i+1] == '[' {
			done := false
			for j := i + 2; j < cut; j++ {
				if b[j] < 0x20 || b[j] > 0x3f {
					done = true
					break
				}
			}
			if !done {
				cut = i
			}
		} else if i+1 < cut && b[i+1] == 'O' {
			done := false
			for j := i + 2; j < cut; j++ {
				if b[j] < 0x20 || b[j] > 0x3f {
					done = true
					break
				}
			}
			if !done {
				cut = i
			}
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
			// is dropped alone. A sequence cut short, by a line break or
			// a letter outside the final bytes say, is dropped up to
			// that, which stays text, as csi ends it.
			final := func(j int) bool { return j < len(rs) && rs[j] >= 0x40 && rs[j] <= 0x7e }
			if i+1 < len(rs) && rs[i+1] == '[' {
				j := i + 2
				for j < len(rs) && rs[j] >= 0x20 && rs[j] <= 0x3f {
					j++
				}
				i = j - 1
				if final(j) {
					i = j
				}
			} else if i+1 < len(rs) && rs[i+1] == 'O' {
				j := i + 2
				for j < len(rs) && rs[j] >= 0x20 && rs[j] <= 0x3f {
					j++
				}
				i = j - 1
				if final(j) {
					i = j
				}
			}
		case r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// Pending reports whether bytes are held back: the start of a sequence
// waiting for its rest, or a paste under way, held until its end
// marker arrives.
func (d *Decoder) Pending() bool { return len(d.pending) > 0 || d.pasting }

// Parse reads one complete chunk of input as keys, flushing what is
// incomplete: for a caller that has the whole of it, a test say.
func Parse(b []byte) []Key {
	var d Decoder
	keys := d.FeedAt(b, time.Time{})
	return append(keys, d.Flush()...)
}

// Wait is how long until a Flush has something to do: the escape wait
// for held bytes, what is left of the grace for a paste or for a held
// escape and bracket, and nothing at all when nothing is held, a
// longer start of a marker is held, or a stalled paste has had no
// bytes since, so the caller sets no timer and draws nothing until
// input comes.
func (d *Decoder) Wait() time.Duration {
	grace := func() time.Duration { return max(PasteGrace-d.clock().Sub(d.at), time.Millisecond) }
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
		if d.clock().Sub(d.at) < PasteGrace {
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
		if len(d.pending) > 2 || d.clock().Sub(d.at) < PasteGrace {
			return nil
		}
		d.pending, d.heldAt = nil, time.Time{}
		return nil
	}
	if len(d.pending) >= 2 && d.pending[0] == 0x1b && (d.pending[1] == '[' || d.pending[1] == 'O') {
		d.discard = true
	}
	if len(d.pending) >= 2 && d.pending[0] == 0x1b && d.pending[1] == ']' {
		// A string whose number and semicolon came is armed against;
		// so, while the answer to the background query may still
		// come, is any start of it, `ESC ] 1 1 ;`. Otherwise an Alt-]
		// is the user's, and so are the keys after it.
		kind, _, body := oscScan(d.pending)
		expecting := d.clock().Before(d.expectUntil) && strings.HasPrefix(oscAnswer, string(d.pending))
		if kind == oscMore && (body || expecting) {
			d.osc, d.oscEsc = true, d.pending[len(d.pending)-1] == 0x1b
			d.oscLeft, d.oscUntil = oscMax-len(d.pending), d.clock().Add(oscWait)
			d.oscBuf = append([]byte(nil), d.pending...)
		}
	}
	heldAt := d.heldAt
	keys, _ := parse(d.pending, true, func(int) time.Time { return heldAt })
	d.pending, d.heldAt = nil, time.Time{}
	return keys
}

// The bounds on swallowing the rest of an OSC string: an answer to the
// background query is a few dozen bytes and comes in one go, so its rest
// comes quickly and is short.
const (
	oscMax  = 256
	oscWait = 500 * time.Millisecond
)

// swallowOSC drops the rest of an OSC string cut by a flush, through
// BEL or ST, and returns what follows it; nil when all of b was the
// string's. The string ends early at any other control byte, or an
// escape that does not begin ST, which are the user's; past the bounds
// in bytes and time everything is the user's again.
func (d *Decoder) swallowOSC(b []byte) []byte {
	if d.clock().After(d.oscUntil) {
		d.osc, d.oscEsc = false, false
		return b
	}
	if d.oscEsc {
		d.oscEsc = false
		if len(b) > 0 && b[0] == '\\' {
			d.osc = false
			d.seeAnswer(append(d.oscBuf, '\\'))
			return b[1:]
		}
		// The escape was not ST's: it is the start of what follows.
		d.osc = false
		return append([]byte{0x1b}, b...)
	}
	for i, c := range b {
		switch {
		case c == 0x07:
			d.osc = false
			d.seeAnswer(append(d.oscBuf, b[:i+1]...))
			return b[i+1:]
		case c == 0x1b && i+1 < len(b) && b[i+1] == '\\':
			d.osc = false
			d.seeAnswer(append(d.oscBuf, b[:i+2]...))
			return b[i+2:]
		case c == 0x1b && i+1 == len(b):
			d.oscEsc = true
			d.oscBuf = append(d.oscBuf, b[:i+1]...)
			return nil
		case c < 0x20:
			d.osc = false
			return b[i:]
		}
		d.oscLeft--
		if d.oscLeft <= 0 {
			d.osc = false
			return b[i+1:]
		}
	}
	d.oscBuf = append(d.oscBuf, b...)
	return nil
}

// seeAnswer ends the expectation of an answer when b holds one whole:
// an OSC 11 string with a colour. An echo of the query, or an answer
// still cut, does not end it.
func (d *Decoder) seeAnswer(b []byte) {
	if d.expectUntil.IsZero() {
		return
	}
	for i := 0; i < len(b); i++ {
		j := bytes.Index(b[i:], []byte(oscAnswer))
		if j < 0 {
			return
		}
		i += j
		if kind, n, _ := oscScan(b[i:]); kind == oscDone {
			if _, ok := palette.DarkBackground(string(b[i : i+n])); ok {
				d.expectUntil, d.oscBuf = time.Time{}, nil
				return
			}
		}
	}
}

// ExpectAnswer says the answer to the background query may still come
// until the time given.
func (d *Decoder) ExpectAnswer(until time.Time) { d.expectUntil = until }

// oscAnswer is how the answer to the background query begins.
const oscAnswer = "\x1b]11;"

// What oscScan finds at an escape and a right bracket.
const (
	oscMore = iota // cut short: more bytes may make it one
	oscNot         // not an OSC string: Alt-], dropped as the Alt chord it is
	oscDone        // a whole string, n bytes, through BEL or ST
	oscCut         // a string ended by a byte that is the user's, at n
)

// oscScan reads the OSC string b starts with, an escape, a right
// bracket, a number, a semicolon and the text, through BEL or ST, as a
// terminal answers the background query. body is that the number and
// its semicolon were seen, so a cut string is surely one.
func oscScan(b []byte) (kind, n int, body bool) {
	i := 2
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		i++
	}
	switch {
	case i == len(b):
		return oscMore, 0, false
	case i == 2 || b[i] != ';':
		return oscNot, 2, false
	}
	for j := i + 1; j < len(b); j++ {
		switch c := b[j]; {
		case c == 0x07:
			return oscDone, j + 1, true
		case c == 0x1b && j+1 == len(b):
			return oscMore, 0, true
		case c == 0x1b && b[j+1] == '\\':
			return oscDone, j + 2, true
		case c < 0x20:
			return oscCut, j, true
		}
	}
	return oscMore, 0, true
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
				// Parameter and intermediate bytes, as csi and the
				// discard after a flush take them: the old form of a
				// modified F1 to F4, ESC O 2 P, say. Then the final
				// byte.
				j := 2
				for j < len(b) && b[j] >= 0x20 && b[j] <= 0x3f {
					j++
				}
				if j == len(b) {
					if !flush {
						return keys, b
					}
					return keys, nil
				}
				// Not a final byte: Alt-O, or an Esc and O read
				// together, cut short by what follows, a click's
				// escape say, which is parsed afresh, as in csi.
				if b[j] < 0x40 || b[j] > 0x7e {
					b = b[j:]
					continue
				}
				// SS3 keys, sent in application cursor mode; the rest,
				// F1 to F4 and any with parameters, are dropped whole.
				if kind, ok := ss3Keys[b[j]]; ok && j == 2 {
					keys = append(keys, Key{Kind: kind})
				}
				b = b[j+1:]
				continue
			}
			if b[1] == ']' {
				// An OSC string, the terminal's late answer to the
				// background query say: dropped whole, up to BEL or ST,
				// so its digits never read as keys. Without its number
				// and semicolon it is Alt-], dropped alone as an Alt
				// chord; one ended by another control byte ends there,
				// the byte being the user's.
				switch kind, n, _ := oscScan(b); kind {
				case oscMore:
					if !flush {
						return keys, b
					}
					return keys, nil
				default:
					b = b[n:]
				}
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

// answerLate is how long an answer to the background query is expected
// after the query gave up on it.
const answerLate = 3 * time.Second

// escapeWait is how long a bare escape, or the start of a sequence, is
// held for the rest before it is read as the escape key. tmux writes a
// sequence in one go, so the wait is only ever paid for the escape key.
const escapeWait = 50 * time.Millisecond
