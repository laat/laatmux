package view

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// Debug is the lines with their attributes made visible, for golden
// tests: a flag column with S for the selection, D for a dim line, B
// for bold, then the text with dim spans between ‹ and ›, bold ones
// between « and », and coloured ones between ⟨ and ⟩ with the palette
// name first.
func Debug(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		flags := []byte("...")
		if l.Reverse {
			flags[0] = 'S'
		}
		if l.Dim {
			flags[1] = 'D'
		}
		if l.Bold {
			flags[2] = 'B'
		}
		b.Write(flags)
		b.WriteByte('|')
		for _, s := range l.Spans {
			t := s.Text
			if s.Fg != "" {
				t = "⟨" + s.Fg + ":" + t + "⟩"
			}
			if s.Bg != "" {
				t = "⟦" + s.Bg + ":" + t + "⟧"
			}
			if s.band {
				t = "⟪" + t + "⟫"
			}
			if s.Bold {
				t = "«" + t + "»"
			}
			if s.Dim {
				t = "‹" + t + "›"
			}
			b.WriteString(t)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// drawn is what a terminal draws of an ANSI line: the text in runs of
// one look, each run's look before it, so two encodings that draw the
// same cells compare equal. It knows the codes ANSI writes. Faint and
// bold are two attributes, in whatever order they are set, as tmux
// keeps them for every cell it draws on the outer terminal.
func drawn(t *testing.T, s string) string {
	t.Helper()
	type look struct {
		faint, bold, reverse bool
		fg, bg               string
	}
	var b strings.Builder
	var cur, last look
	first := true
	for s != "" {
		if strings.HasPrefix(s, "\x1b[") {
			end := strings.IndexByte(s, 'm')
			if end < 0 {
				t.Fatalf("an escape without its end: %q", s)
			}
			ps := strings.Split(s[2:end], ";")
			s = s[end+1:]
			for i := 0; i < len(ps); i++ {
				switch ps[i] {
				case "0":
					cur = look{}
				case "1":
					cur.bold = true
				case "2":
					cur.faint = true
				case "22":
					cur.faint, cur.bold = false, false
				case "7":
					cur.reverse = true
				case "38", "48":
					// 38;5;n or 38;2;r;g;b, the background's 48 alike.
					n := 2
					if i+1 < len(ps) && ps[i+1] == "2" {
						n = 4
					}
					if i+n >= len(ps) {
						t.Fatalf("a colour cut short: %q", ps)
					}
					c := strings.Join(ps[i+1:i+n+1], ";")
					if ps[i] == "38" {
						cur.fg = c
					} else {
						cur.bg = c
					}
					i += n
				default:
					t.Fatalf("a code ANSI does not write: %q", ps[i])
				}
			}
			continue
		}
		r, n := utf8.DecodeRuneInString(s)
		s = s[n:]
		if first || cur != last {
			fmt.Fprintf(&b, "⟨%+v⟩", cur)
			first, last = false, cur
		}
		b.WriteRune(r)
	}
	return b.String()
}
