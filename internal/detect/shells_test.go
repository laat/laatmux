package detect

import "testing"

// The footer's count of background shells, read after the prompt box's
// lower rule; the prompt's own words and a tool's output do not count.
func TestShells(t *testing.T) {
	footer := func(f string) []string {
		return []string{
			"⏺ Started the dev server in the background: 2 shells now.",
			"",
			"───────────────────────────────────────────────────────────",
			"❯ how many shells · 3 shells · are there",
			"───────────────────────────────────────────────────────────",
			"  󰪡 Fable 5.1[1M] ctx 91% │ 5h 93% 7d 94% │ laatmux/ (main)",
			f,
		}
	}
	for _, c := range []struct {
		name   string
		agent  string
		screen []string
		want   int
	}{
		{"one", "claude", footer("  ⏵⏵ auto mode on · 1 shell · ← for agents"), 1},
		{"two", "claude", footer("  ⏵⏵ auto mode on · 2 shells · ← for agents"), 2},
		{"twelve last", "claude", footer("  ⏵⏵ accept edits on · 12 shells"), 12},
		{"none", "claude", footer("  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents"), 0},
		{"codex", "codex", footer("  ⏵⏵ auto mode on · 2 shells · ← for agents"), 0},
		{"only the prompt mentions them", "claude", footer("  ⏵⏵ auto mode on"), 0},
		{"no rule: the bottom lines", "claude", []string{"⏺ 4 shells in the output above.", "❯ 5 shells", "  ⏵⏵ auto mode on · 2 shells · ← for agents"}, 2},
		{"no rule, no footer", "claude", []string{"⏺ 4 shells in the output above.", "", "❯ "}, 0},
	} {
		if got := Shells(Input{Agent: c.agent, Screen: c.screen}); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}
