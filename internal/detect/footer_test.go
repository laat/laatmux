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

// The footer's count of monitors, an item of the same line: Claude
// Code 2.1.306's `· 1 monitor`, beside the shells; the turn's closing
// line above the prompt box, which says the monitor is still running,
// is not counted.
func TestMonitors(t *testing.T) {
	screen := func(f string) []string {
		return []string{
			"⏺ Monitor event: PostgreSQL (./internal/apiv2/...): success 388s · Test run 39677707767 job completions on PR #460",
			"✻ Sautéed for 3s · done 11:25 PM · 3 monitors still running",
			"───────────────────────────────────────────────────────────",
			"❯ ",
			"───────────────────────────────────────────────────────────",
			"  󰪣 Opus 5.5[1M] ctx 81% │ 5h 100% │ plattform-lock-search/ (ci/parallel-tests)",
			f,
		}
	}
	for _, c := range []struct {
		name   string
		agent  string
		screen []string
		want   int
	}{
		{"one", "claude", screen("  ⏵⏵ bypass permissions on · PR #460 · 1 monitor"), 1},
		{"two beside shells", "claude", screen("  ⏵⏵ auto mode on · 2 shells · 2 monitors · ← for agents"), 2},
		{"none", "claude", screen("  ⏵⏵ bypass permissions on · PR #460"), 0},
		{"codex", "codex", screen("  ⏵⏵ bypass permissions on · 1 monitor"), 0},
	} {
		if got := Monitors(Input{Agent: c.agent, Screen: c.screen}); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	if got := Shells(Input{Agent: "claude", Screen: screen("  ⏵⏵ auto mode on · 2 shells · 2 monitors · ← for agents")}); got != 2 {
		t.Errorf("the shells beside the monitors: %d", got)
	}
}

// The agents panel's rows that are not idle, the folded ones counted,
// after the `⏺ main` header; a panel under a dialog, with no rule
// before it, counts too.
func TestSubagents(t *testing.T) {
	panel := func(rows ...string) []string {
		return append([]string{
			"⏺ Both agents are running; I'll put the results together when they finish.",
			"",
			"───────────────────────────────────────────────────────────",
			"❯ ",
			"───────────────────────────────────────────────────────────",
			"  󰪣 Opus 5.5[1M] ctx 89% │ 5h 90% │ pin-delta-auth/ (main)",
			"  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents",
		}, rows...)
	}
	for _, c := range []struct {
		name   string
		agent  string
		screen []string
		want   int
	}{
		{"none", "claude", panel(), 0},
		{"two running", "claude", panel("  ⏺ main",
			"  ◯ general-purpose  Checking token grant pipelines for IP use      7m 6s · ↓ 235.5k tokens",
			"  ◯ general-purpose  Reading AKS module variables.tf defaults        7m 6s · ↓ 115.4k tokens"), 2},
		{"one idle", "claude", panel("  ⏺ main",
			"  ◯ footer-probe  Hold a subagent for the footer                                     idle"), 0},
		{"idle ones folded", "claude", panel("  ⏺ main",
			"  ◯ probe-1  Idle probe one                                                        idle",
			"  ◯ probe-2  Idle probe two                                             12s · ↓ 1.2k tokens",
			"  ◯ 4 idle agents"), 1},
		{"cursor row, time clipped", "claude", panel("  ↑/↓ to select", "❯ ⏺ main",
			"❯ ◯ iss357         Checking flushAttentionLocked in attention.go",
			"  ◯ iss339         Waiting on opus-1.md review report               4",
			"  ◯ done           Opus review of PR 362 round 1                 idle",
			"  ↓ 2 more"), 4},
		{"under a dialog", "claude", []string{"───", " Bash command", "   git push", " Do you want to proceed?", " ❯ 1. Yes", "   2. No", " Esc to cancel · Tab to amend",
			"  ⏺ main", "  ◯ iss357  Checking attention.go   43m 8s · ↓"}, 1},
		{"no header", "claude", panel("  ◯ not a panel row  1s · ↓"), 0},
		{"codex", "codex", panel("  ⏺ main", "  ◯ x  y  1s · ↓"), 0},
	} {
		if got := Subagents(Input{Agent: c.agent, Screen: c.screen}); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	for _, c := range []struct {
		fixture string
		want    int
	}{{"claude-working-agents-panel", 5}, {"claude-teammates-running", 5}, {"claude-teammates-permission", 5}, {"claude-teammates-cursor", 1}, {"claude-idle-fresh", 0}} {
		if got := Subagents(Input{Agent: "claude", Screen: screen(t, c.fixture+".screen")}); got != c.want {
			t.Errorf("%s: %d, want %d", c.fixture, got, c.want)
		}
	}
}
