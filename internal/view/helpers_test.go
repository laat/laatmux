package view

import "time"

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
