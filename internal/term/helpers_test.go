package term

import "time"

// Feed is FeedAt for input whose read time does not matter.
func (d *Decoder) Feed(b []byte) []Key { return d.FeedAt(b, time.Time{}) }
