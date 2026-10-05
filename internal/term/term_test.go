package term

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The terminal's late answer to the background query is dropped whole,
// whichever terminator ends it, and keys around it survive.
func TestOSCAnswerDropped(t *testing.T) {
	for _, in := range []string{"a\x1b]11;rgb:1a1a/1b1b/2626\x1b\\b", "a\x1b]11;rgb:ffff/ffff/ffff\x07b"} {
		keys := Parse([]byte(in))
		var got []rune
		for _, k := range keys {
			if k.Kind == KeyRune {
				got = append(got, k.Rune)
			}
		}
		if string(got) != "ab" || len(keys) != 2 {
			t.Errorf("%q: %+v", in, keys)
		}
	}
}

// Background reads the terminal's answer to OSC 11, and gives up on a
// terminal that does not answer.
func TestBackground(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	for answer, want := range map[string]bool{"\x1b]11;rgb:ffff/ffff/ffff\x1b\\": false, "\x1b]11;rgb:0000/0000/0000\x07": true} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		w.WriteString(answer)
		term := &Term{in: r, out: devnull}
		if dark, ok := term.Background(time.Second); !ok || dark != want {
			t.Errorf("%q: dark %v ok %v", answer, dark, ok)
		}
		r.Close()
		w.Close()
	}
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	start := time.Now()
	if _, ok := (&Term{in: r, out: devnull}).Background(50 * time.Millisecond); ok || time.Since(start) > time.Second {
		t.Errorf("no answer: ok %v after %v", ok, time.Since(start))
	}
}

// Keys that come while the terminal is asked for its background are
// kept for Input, and an answer cut by the deadline is waited on for its
// end.
func TestBackgroundKeepsInput(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	r, w, _ := os.Pipe()
	w.WriteString("j\x1b]11;rgb:0000/0000/0000\x07k")
	term := &Term{in: r, out: devnull}
	if dark, ok := term.Background(time.Second); !ok || !dark || string(term.pending) != "jk" {
		t.Errorf("dark %v ok %v pending %q", dark, ok, term.pending)
	}
	r.Close()
	w.Close()
	r, w, _ = os.Pipe()
	defer r.Close()
	defer w.Close()
	w.WriteString("\x1b]11;rgb:ffff/")
	go func() {
		time.Sleep(150 * time.Millisecond)
		w.WriteString("ffff/ffff\x1b\\")
	}()
	term = &Term{in: r, out: devnull}
	if dark, ok := term.Background(50 * time.Millisecond); !ok || dark || len(term.pending) != 0 {
		t.Errorf("split answer: dark %v ok %v pending %q", dark, ok, term.pending)
	}
}

// An OSC answer cut by a flush is swallowed through BEL or ST when its
// rest comes, whether the ST is split or not; past the bound in time
// the bytes are keys again.
func TestOSCAcrossFlush(t *testing.T) {
	runes := func(ks []Key) string {
		var b strings.Builder
		for _, k := range ks {
			if k.Kind == KeyRune {
				b.WriteRune(k.Rune)
			}
		}
		return b.String()
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	for _, parts := range [][]string{
		{"\x1b]11;rgb:", "1111/2222/3333\x07j"},
		{"\x1b]11;rgb:0/0/0\x1b", "\\j"},
		{"\x1b]11;rgb:0/0", "/0\x1b", "\\j"},
	} {
		d := Decoder{Now: clock}
		var got string
		for _, p := range parts {
			got += runes(d.Feed([]byte(p)))
			got += runes(d.Flush())
		}
		if got != "j" {
			t.Errorf("%q: %q", parts, got)
		}
	}
	// Alt-] and then, a while later, keys: they are the user's.
	d := Decoder{Now: clock}
	d.Feed([]byte("\x1b]"))
	d.Flush()
	now = now.Add(oscWait + time.Millisecond)
	if got := runes(d.Feed([]byte("jk"))); got != "jk" {
		t.Errorf("keys after the bound: %q", got)
	}
}

// Alt-] is the user's: dropped alone as the Alt chord it is, the key or
// the click after it read, flushed or not.
func TestAltBracket(t *testing.T) {
	keys := Parse([]byte("\x1b]j\x1b[<0;5;3M"))
	if len(keys) != 2 || keys[0].Kind != KeyRune || keys[0].Rune != 'j' || keys[1].Kind != KeyMouse {
		t.Errorf("Alt-] then a key and a click: %+v", keys)
	}
	var d Decoder
	d.Feed([]byte("\x1b]"))
	d.Flush()
	if ks := d.Feed([]byte("j")); len(ks) != 1 || ks[0].Rune != 'j' {
		t.Errorf("a key after a flushed Alt-]: %+v", ks)
	}
}

// An answer cut anywhere in its header, `ESC ] 1 1 ;`, is swallowed
// when its rest comes; a bare Alt-] still is not armed against.
func TestOSCCutInHeader(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	full := "\x1b]11;rgb:1a1a/1b1b/2626\x1b\\"
	for cut := 2; cut < len(full); cut++ {
		d := Decoder{Now: clock}
		d.ExpectAnswer(now.Add(time.Second))
		var ks []Key
		ks = append(ks, d.Feed([]byte(full[:cut]))...)
		ks = append(ks, d.Flush()...)
		ks = append(ks, d.Feed([]byte(full[cut:]+"j"))...)
		ks = append(ks, d.Flush()...)
		if len(ks) != 1 || ks[0].Rune != 'j' {
			t.Errorf("cut at %d: %+v", cut, ks)
		}
	}
}

// The query finds the answer past an echo of itself and past an Alt-]
// typed before it, and keeps the Alt-] and the keys for Input.
func TestBackgroundPastEchoAndAlt(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	for _, in := range []string{
		"\x1b]11;?\x1b\\\x1b]11;rgb:ffff/ffff/ffff\x07j",
		"\x1b]\x1b]11;rgb:ffff/ffff/ffff\x07j",
	} {
		r, w, _ := os.Pipe()
		w.WriteString(in)
		term := &Term{in: r, out: devnull}
		start := time.Now()
		dark, ok := term.Background(150 * time.Millisecond)
		if !ok || dark || time.Since(start) > 100*time.Millisecond || !strings.HasSuffix(string(term.pending), "j") {
			t.Errorf("%q: dark %v ok %v pending %q after %v", in, dark, ok, term.pending, time.Since(start))
		}
		r.Close()
		w.Close()
	}
}

// Past the time an answer is expected, the keys typed after an Alt-]
// are the user's, whatever a flush cuts; a string whose number and
// semicolon came is still dropped.
func TestOSCNotExpected(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	d := Decoder{Now: clock}
	d.ExpectAnswer(now.Add(-time.Second))
	var ks []Key
	ks = append(ks, d.Feed([]byte("\x1b]"))...)
	ks = append(ks, d.Flush()...)
	ks = append(ks, d.Feed([]byte("1j"))...)
	ks = append(ks, d.Flush()...)
	if len(ks) != 2 || ks[0].Rune != '1' || ks[1].Rune != 'j' {
		t.Errorf("Alt-] then 1j: %+v", ks)
	}
	ks = append(d.Feed([]byte("\x1b]11;rgb:1a")), d.Flush()...)
	ks = append(ks, d.Feed([]byte("1a/1b1b/2626\x07j"))...)
	ks = append(ks, d.Flush()...)
	if len(ks) != 1 || ks[0].Rune != 'j' {
		t.Errorf("a cut answer: %+v", ks)
	}
}

// The answer arriving ends the expectation.
func TestOSCAnswerEndsExpectation(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	d := Decoder{Now: func() time.Time { return now }}
	d.ExpectAnswer(now.Add(time.Second))
	d.Feed([]byte("\x1b]11;rgb:1a1a/1b1b/2626\x07"))
	if !d.expectUntil.IsZero() {
		t.Errorf("still expecting until %v", d.expectUntil)
	}

	// An answer across reads, then across a flush, ends it too.
	for _, parts := range [][]string{
		{"\x1b]1", "1;rgb:1a1a/1b1b/2626\x07"},
		{"\x1b]11", "", ";rgb:1a1a/1b1b/2626\x1b", "\\"},
	} {
		d := Decoder{Now: func() time.Time { return now }}
		d.ExpectAnswer(now.Add(time.Second))
		var ks []Key
		for _, p := range parts {
			if p == "" {
				ks = append(ks, d.Flush()...)
				continue
			}
			ks = append(ks, d.Feed([]byte(p))...)
		}
		ks = append(ks, d.Feed([]byte("\x1b]"))...)
		ks = append(ks, d.Flush()...)
		ks = append(ks, d.Feed([]byte("1j"))...)
		ks = append(ks, d.Flush()...)
		if !d.expectUntil.IsZero() || len(ks) != 2 {
			t.Errorf("%q: expecting until %v, keys %+v", parts, d.expectUntil, ks)
		}
	}

	// An echo of the query does not: the answer after it, cut after
	// its escape and bracket, is still dropped.
	d = Decoder{Now: func() time.Time { return now }}
	d.ExpectAnswer(now.Add(time.Second))
	ks := d.Feed([]byte("\x1b]11;?\x1b\\\x1b]"))
	ks = append(ks, d.Flush()...)
	ks = append(ks, d.Feed([]byte("11;rgb:1a1a/1b1b/2626\x07j"))...)
	ks = append(ks, d.Flush()...)
	if len(ks) != 1 || ks[0].Rune != 'j' || !d.expectUntil.IsZero() {
		t.Errorf("echo, then a cut answer: %+v, expecting until %v", ks, d.expectUntil)
	}
}

// The decoder reads the editing keys, Shift-Tab, a bracketed paste
// whole, even split across reads, with its line breaks as newlines,
// and does not flush a paste under way.
func TestDecoderPasteAndKeys(t *testing.T) {
	cases := map[string][]Key{
		"\t":                           {{Kind: KeyTab}},
		"\x1b[Z":                       {{Kind: KeyShiftTab}},
		"\n":                           {{Kind: KeyNewline}},
		"\x1b[C\x1b[D":                 {{Kind: KeyRight}, {Kind: KeyLeft}},
		"\x1b[H\x1b[F":                 {{Kind: KeyHome}, {Kind: KeyEnd}},
		"\x1b[1~\x1b[4~":               {{Kind: KeyHome}, {Kind: KeyEnd}},
		"\x1b[3~":                      {{Kind: KeyDelete}},
		"\x1bOC\x1bOH":                 {{Kind: KeyRight}, {Kind: KeyHome}},
		"\x1b[200~a\r\nb\x1b[201~":     {{Kind: KeyPaste, Text: "a\nb"}},
		"x\x1b[200~\t\x1b[A\x1b[201~y": {{Rune: 'x'}, {Kind: KeyPaste, Text: "\t"}, {Rune: 'y'}},
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
	in := "j\x1b[200~line one\nline two\x1b[201~k"
	want := Parse([]byte(in))
	for cut := 1; cut < len(in); cut++ {
		var d Decoder
		got := d.Feed([]byte(in[:cut]))
		got = append(got, d.Feed([]byte(in[cut:]))...)
		got = append(got, d.Flush()...)
		if len(got) != len(want) {
			t.Fatalf("split at %d = %+v, want %+v", cut, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("split at %d: key %d = %+v, want %+v", cut, i, got[i], want[i])
			}
		}
	}
	// The start of a paste marker split by a slow read survives the
	// flush too: dropped, the text's line breaks would be Enter.
	var split Decoder
	if got := split.Feed([]byte("\x1b[200")); len(got) != 0 || !split.Pending() {
		t.Fatalf("marker prefix: %+v", got)
	}
	if got := split.Flush(); len(got) != 0 || !split.Pending() {
		t.Fatalf("flush on a marker prefix: %+v", got)
	}
	// And past the grace: only an escape and bracket alone are let go.
	split.Now = func() time.Time { return time.Unix(0, 0).Add(2 * PasteGrace) }
	if got := split.Flush(); len(got) != 0 || !split.Pending() || split.Wait() != 0 {
		t.Fatalf("flush on a marker prefix past the grace: %+v pending %v", got, split.Pending())
	}
	got := split.Feed([]byte("~fix\rmore\x1b[201~"))
	if len(got) != 1 || got[0].Kind != KeyPaste || got[0].Text != "fix\nmore" {
		t.Fatalf("paste after the split marker: %+v", got)
	}
	// A paste under way survives a flush: the escape wait must not cut
	// a long paste short.
	var d Decoder
	if got := d.Feed([]byte("\x1b[200~abc")); len(got) != 0 || !d.Pending() {
		t.Fatalf("paste start: %+v", got)
	}
	if got := d.Flush(); len(got) != 0 || !d.Pending() {
		t.Fatalf("flush during a paste: %+v pending %v", got, d.Pending())
	}
	if got := d.Feed([]byte("def\x1b[201~")); len(got) != 1 || got[0].Text != "abcdef" || d.Pending() {
		t.Fatalf("paste end: %+v", got)
	}
}

// A paste keeps its framing past the bounds: a stalled paste gives out
// its text so far and a resumed one with a line break in it is still
// a paste, as is a chunk past the size cap; a bare escape alone after a
// stall is the user's Esc, which ends a paste whose end is lost.
func TestDecoderPasteBounded(t *testing.T) {
	now := time.Unix(1000, 0)
	d := Decoder{Now: func() time.Time { return now }}
	if got := d.Feed([]byte("\x1b[200~lost")); len(got) != 0 || !d.Pending() {
		t.Fatalf("start: %+v", got)
	}
	if got := d.Flush(); len(got) != 0 || !d.Pending() {
		t.Fatalf("flush within the grace: %+v", got)
	}
	now = now.Add(PasteGrace + time.Millisecond)
	got := d.Flush()
	if len(got) != 1 || got[0].Kind != KeyPaste || got[0].Text != "lost" || !d.Pending() {
		t.Fatalf("flush past the grace: %+v pending %v", got, d.Pending())
	}
	// The paste resumes with a carriage return and a tab and then ends:
	// still one paste, never Enter.
	got = d.Feed([]byte("\rmore\t\x1b[201~"))
	if len(got) != 1 || got[0].Kind != KeyPaste || got[0].Text != "\nmore\t" || d.Pending() {
		t.Fatalf("resumed paste: %+v pending %v", got, d.Pending())
	}
	// A lost end marker: a stall, then the user's Esc ends it, pressed
	// once or twice, after typing, or as Ctrl-C; the key is spent on
	// ending it, and never reaches the form.
	for _, c := range []struct {
		in   string
		text string
		rest int
	}{
		{"\x1b", "more", 0},
		{"\x1b\x1b", "more", 0},
		{"q\x03\x1b", "moreq", 0},
		{"\x03", "more", 0},
		{"\x03j", "more", 1},
		// A click after the key: when it was read is not known, so
		// it is dropped rather than resolved on the screen drawn now.
		{"\x03\x1b[<0;5;3Mj", "more", 1},
	} {
		d := Decoder{Now: func() time.Time { return now }}
		d.Feed([]byte("\x1b[200~gone"))
		now = now.Add(PasteGrace + time.Millisecond)
		if got := d.Flush(); len(got) != 1 || got[0].Text != "gone" {
			t.Fatalf("%q stalled: %+v", c.in, got)
		}
		if d.Wait() != 0 {
			t.Fatalf("%q: a stalled paste with no new bytes waits %v", c.in, d.Wait())
		}
		d.Feed([]byte("more" + c.in))
		if w := d.Wait(); w <= 0 || w > PasteGrace {
			t.Fatalf("%q: waits %v for the grace", c.in, w)
		}
		now = now.Add(PasteGrace + time.Millisecond)
		got := d.Flush()
		want := 1 + c.rest
		if len(got) != want || got[0].Kind != KeyPaste || got[0].Text != c.text || d.Pending() {
			t.Fatalf("%q after a lost end: %+v pending %v", c.in, got, d.Pending())
		}
		for _, k := range got {
			if k.Kind == KeyEsc || k.Kind == KeyCtrlC {
				t.Fatalf("%q: the recovery key came through: %+v", c.in, got)
			}
		}
	}
	// An escape followed by another byte after a stall is a chord or a
	// sequence, as everywhere, not the user's Esc: the paste goes on.
	chord := Decoder{Now: func() time.Time { return now }}
	chord.Feed([]byte("\x1b[200~gone"))
	now = now.Add(PasteGrace + time.Millisecond)
	chord.Flush()
	chord.Feed([]byte("more\x1bj"))
	now = now.Add(PasteGrace + time.Millisecond)
	if got := chord.Flush(); len(got) != 1 || got[0].Kind != KeyPaste || got[0].Text != "morej" || !chord.Pending() {
		t.Fatalf("a chord after a stall: %+v pending %v", got, chord.Pending())
	}
	// A bare escape in a slow paste's first chunk, the output of tput
	// say, is paste and not the user's: only after a stall does one
	// end the framing.
	slow := Decoder{Now: func() time.Time { return now }}
	slow.Feed([]byte("\x1b[200~echo $(tput sgr0)\x1b(Bdone\r"))
	now = now.Add(PasteGrace + time.Millisecond)
	if got := slow.Flush(); len(got) != 1 || got[0].Kind != KeyPaste || !slow.Pending() {
		t.Fatalf("escape before any stall: %+v pending %v", got, slow.Pending())
	}
	// The first chunk's trailing carriage return was held, and leads.
	if got := slow.Feed([]byte("second line\r\x1b[201~")); len(got) != 1 || got[0].Kind != KeyPaste || got[0].Text != "\nsecond line\n" || slow.Pending() {
		t.Fatalf("the rest of a slow paste: %+v", got)
	}
	// An end marker split across the stall still ends the paste, and a
	// trailing carriage return is held so \r\n split by it is one
	// newline.
	e := Decoder{Now: func() time.Time { return now }}
	e.Feed([]byte("\x1b[200~abc\r"))
	now = now.Add(PasteGrace + time.Millisecond)
	if got := e.Flush(); len(got) != 1 || got[0].Text != "abc" {
		t.Fatalf("stalled before the marker: %+v", got)
	}
	e.Feed([]byte("\nd\x1b[20"))
	now = now.Add(PasteGrace + time.Millisecond)
	if got := e.Flush(); len(got) != 1 || got[0].Text != "\nd" || !e.Pending() {
		t.Fatalf("stalled inside the marker: %+v pending %v", got, e.Pending())
	}
	if got := e.Feed([]byte("1~")); len(got) != 0 || e.Pending() {
		t.Fatalf("marker completed: %+v pending %v", got, e.Pending())
	}
	// Past the size cap the text comes in chunks and the framing stays.
	var big Decoder
	big.Feed([]byte("\x1b[200~"))
	got = big.Feed([]byte(strings.Repeat("a", pasteMax+1)))
	if len(got) != 1 || got[0].Kind != KeyPaste || len(got[0].Text) != pasteMax+1 || !big.Pending() {
		t.Fatalf("size cap: %d keys pending %v", len(got), big.Pending())
	}
	got = big.Feed([]byte("b\rc\x1b[201~"))
	if len(got) != 1 || got[0].Kind != KeyPaste || got[0].Text != "b\nc" || big.Pending() {
		t.Fatalf("after the cap: %+v pending %v", got, big.Pending())
	}
	// A sequence cut by the cap is held whole, so its rest is not text.
	var seq Decoder
	seq.Feed([]byte("\x1b[200~"))
	got = seq.Feed([]byte(strings.Repeat("a", pasteMax) + "\x1b[3"))
	got = append(got, seq.Feed([]byte("1mb\x1b[201~"))...)
	if len(got) != 2 || strings.Contains(got[0].Text, "[3") || got[1].Text != "b" {
		t.Fatalf("sequence at the cap: %d keys, last %q", len(got), got[len(got)-1].Text)
	}
	// A rune split at the cap is held whole.
	var split Decoder
	split.Feed([]byte("\x1b[200~"))
	got = split.Feed(append([]byte(strings.Repeat("a", pasteMax)), 0xc3))
	got = append(got, split.Feed([]byte{0xb8, 'x', 0x1b, '[', '2', '0', '1', '~'})...)
	if len(got) != 2 || !strings.HasSuffix(got[1].Text, "øx") || strings.Contains(got[0].Text, "\uFFFD") {
		t.Fatalf("rune at the cap: %d keys, last %q", len(got), got[len(got)-1].Text)
	}
}

// The start of a paste marker held alone is dropped after the grace,
// and the key after it is read; a held escape waits the escape wait;
// nothing held waits nothing.
func TestDecoderHeldStart(t *testing.T) {
	now := time.Unix(0, 0)
	d := Decoder{Now: func() time.Time { return now }}
	if d.Wait() != 0 {
		t.Fatalf("nothing held waits %v", d.Wait())
	}
	d.Feed([]byte("\x1b"))
	if d.Wait() != escapeWait {
		t.Fatalf("a held escape waits %v", d.Wait())
	}
	d.Flush()
	d.Feed([]byte("\x1b["))
	if w := d.Wait(); w <= 0 || w > PasteGrace {
		t.Fatalf("a held marker start waits %v", w)
	}
	if got := d.Flush(); len(got) != 0 || !d.Pending() {
		t.Fatalf("flushed within the grace: %+v pending %v", got, d.Pending())
	}
	now = now.Add(PasteGrace + time.Millisecond)
	if got := d.Flush(); len(got) != 0 || d.Pending() || d.Wait() != 0 {
		t.Fatalf("flushed past the grace: %+v pending %v", got, d.Pending())
	}
	if got := d.Feed([]byte("a")); len(got) != 1 || got[0].Rune != 'a' {
		t.Fatalf("the key after: %+v", got)
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

// Input as the loop drives it: Start gives what Background read, the
// held escape is flushed when due, Rearm sets no timer with nothing
// held, a late answer to the background query is swallowed, and Reads
// closes when the read fails.
func TestInput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := (&Term{in: r, pending: []byte("j\x1b"), unanswered: true}).Input(ctx)
	if ks := in.Start(); len(ks) != 1 || ks[0].Rune != 'j' {
		t.Fatalf("Start = %+v", ks)
	}
	in.Rearm()
	select {
	case <-in.Flush:
	case <-time.After(time.Second):
		t.Fatal("no flush for the held escape")
	}
	if ks := in.Flushed(); len(ks) != 1 || ks[0].Kind != KeyEsc {
		t.Fatalf("Flushed = %+v", ks)
	}
	if in.Rearm(); in.Flush != nil {
		t.Fatal("timer armed with nothing held")
	}
	w.Write([]byte("\x1b]11;rgb:0000/0000/0000\x1b\\x"))
	if ks := in.Decode(<-in.Reads); len(ks) != 1 || ks[0].Rune != 'x' {
		t.Fatalf("late answer not swallowed: %+v", ks)
	}
	w.Close()
	if _, ok := <-in.Reads; ok {
		t.Fatal("Reads open after the read failed")
	}
}
