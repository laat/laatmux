package view

import (
	"strings"
	"time"
)

// Feed is FeedAt for input whose read time does not matter.
func (d *Decoder) Feed(b []byte) []Key { return d.FeedAt(b, time.Time{}) }

// Pending reports whether Feed held bytes back. A paste under way holds
// its text until its end marker arrives, so it is pending until then.
func (d *Decoder) Pending() bool { return len(d.pending) > 0 || d.pasting }

// Parse reads one complete chunk of input as keys, flushing what is
// incomplete.
func Parse(b []byte) []Key {
	var d Decoder
	keys := d.Feed(b)
	return append(keys, d.Flush()...)
}

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
