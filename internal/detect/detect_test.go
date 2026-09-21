package detect

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func screen(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func title(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

// Real panes captured on a clean Debian VM with tmux capture-pane -p, 100x40,
// Claude Code 2.1.278 in manual mode; titles from #{pane_title} at the same
// moment. codex-idle is from a local pane. Each case is a distinct rule.
func TestRealFixtures(t *testing.T) {
	cases := []struct {
		name, agent string
		want        State
		rule        string
		skip        bool
		why         string
	}{
		{"claude-trust-dialog", "claude", Blocked, "live_blocked_form", false,
			"first screen: folder trust question with 'Enter to confirm · Esc to cancel'; title is still the terminal's"},
		{"claude-idle-fresh", "claude", Idle, "live_prompt_box", false,
			"empty ❯ prompt box with the placeholder, no turn yet"},
		{"claude-working", "claude", Working, "live_turn_working", false,
			"spinner line with a live timer and 'esc to interrupt' in the footer"},
		{"claude-idle-after-turn", "claude", Idle, "live_prompt_box", false,
			"'✻ Brewed for 12s · done' and an empty prompt box"},
		{"claude-idle-after-tool", "claude", Idle, "live_prompt_box", false,
			"a shell tool ran without a prompt in manual mode; back at the prompt box"},
		{"claude-permission-prompt", "claude", Blocked, "bash_permission_prompt", false,
			"'Do you want to proceed?' with numbered choices for rm -f outside the project"},
		{"claude-transcript", "claude", Unknown, "transcript_viewer", true,
			"ctrl+o transcript view: 'Showing detailed transcript'; the update must be skipped, not reclassified"},
		{"claude-model-picker", "claude", Unknown, "model_picker_menu", true,
			"/model menu open; skipped for the same reason"},
		{"codex-idle", "codex", Idle, "osc_title_idle", false,
			"last response ended with 'done', composer shows the placeholder, no spinner in the title"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := Input{Agent: c.agent, Title: title(t, c.name+".title"), Screen: screen(t, c.name+".screen")}
			got := Detect(in)
			if got.State != c.want || got.Rule != c.rule || got.Skip != c.skip {
				t.Fatalf("%s: got %s via %q skip=%v, want %s via %q skip=%v (%s)\n%s", c.name, got.State, got.Rule, got.Skip, c.want, c.rule, c.skip, c.why, Explain(in))
			}
			switch {
			case c.skip:
			case c.want == Idle && !got.VisibleIdle:
				t.Fatalf("%s: VisibleIdle false", c.name)
			case c.want == Working && !got.VisibleWorking:
				t.Fatalf("%s: VisibleWorking false", c.name)
			case c.want == Blocked && !got.VisibleBlocker:
				t.Fatalf("%s: VisibleBlocker false", c.name)
			}
		})
	}
}

// Without an identified agent the detector must not guess, whatever is on
// screen.
func TestNoAgentIsUnknown(t *testing.T) {
	for _, f := range []string{"claude-working.screen", "claude-permission-prompt.screen", "codex-idle.screen"} {
		got := Detect(Input{Agent: "", Title: "host", Screen: screen(t, f)})
		if got.State != Unknown || got.Reason != "no_known_agent" {
			t.Errorf("%s: got %+v, want Unknown/no_known_agent", f, got)
		}
	}
}

func TestUnknownAgent(t *testing.T) {
	for _, agent := range []string{"", "zsh", "gemini"} {
		got := Detect(Input{Agent: agent, Title: "⠋ busy", Screen: []string{"❯ "}})
		want := Result{State: Unknown, Reason: "no_known_agent"}
		if got != want {
			t.Errorf("agent %q: got %+v, want %+v", agent, got, want)
		}
	}
}

func TestClaudePermissionPrompt(t *testing.T) {
	in := Input{Agent: "claude", Title: "✳ Push branch", Screen: []string{
		"⏺ I'll push the branch now.",
		"",
		"⏺ Bash(git push -u origin feature)",
		"  ⎿  Running…",
		"",
		"───────────────────────────────────────────────────────────",
		" Bash command",
		"",
		"   git push -u origin feature",
		"   Push the feature branch",
		"",
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. Yes, and don't ask again for: git push",
		"   3. No",
		"",
		" Esc to cancel · Tab to amend",
	}}
	got := Detect(in)
	if got.State != Blocked || !got.VisibleBlocker || got.Rule != "bash_permission_prompt" {
		t.Fatalf("got %+v\n%s", got, Explain(in))
	}
}

func TestClaudeIdlePromptBox(t *testing.T) {
	in := Input{Agent: "claude", Title: "✳ Fix tests", Screen: []string{
		"⏺ All tests pass now.",
		"",
		"✻ Brewed for 16s · done 6:26 PM",
		"",
		"───────────────────────────────────────────────────────────",
		"❯ ",
		"───────────────────────────────────────────────────────────",
		"  F 5.1 laatmux (main) │ ctx 5%",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}}
	got := Detect(in)
	if got.State != Idle || !got.VisibleIdle || got.Rule != "live_prompt_box" {
		t.Fatalf("got %+v\n%s", got, Explain(in))
	}
	if got.Reason != "rule live_prompt_box (priority 950)" {
		t.Fatalf("reason %q", got.Reason)
	}
}

func TestClaudeTranscriptViewerSkips(t *testing.T) {
	in := Input{Agent: "claude", Title: "✳ Fix tests", Screen: []string{
		"⏺ Read(internal/detect/engine.go)",
		"  ⎿  Read 120 lines",
		"",
		"⏺ The loader compiles every gate up front.",
		"",
		"  Showing detailed transcript · ctrl+o to toggle",
	}}
	got := Detect(in)
	if !got.Skip || got.State != Unknown || got.Rule != "transcript_viewer" {
		t.Fatalf("got %+v\n%s", got, Explain(in))
	}
}

func TestClaudeTitleSpinnerOnly(t *testing.T) {
	got := Detect(Input{Agent: "claude", Title: "⠋ Thinking about tests"})
	if got.State != Working || !got.VisibleWorking || got.Rule != "osc_title_working" {
		t.Fatalf("got %+v", got)
	}
}

func TestKnownAgentFallsBackToIdle(t *testing.T) {
	got := Detect(Input{Agent: "claude", Title: "", Screen: []string{"nothing recognisable"}})
	want := Result{State: Idle, Reason: "default_known_agent_idle_fallback"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestAgents(t *testing.T) {
	if got := Agents(); !reflect.DeepEqual(got, []string{"claude", "codex"}) {
		t.Fatalf("Agents() = %v", got)
	}
	// Aliases resolve to the same manifest.
	if got := Detect(Input{Agent: "claude-code", Title: "⠋ busy"}); got.State != Working {
		t.Fatalf("alias claude-code: %+v", got)
	}
}

func TestExplainListsUnsupportedRegions(t *testing.T) {
	out := Explain(Input{Agent: "claude", Title: "✳ x", Screen: screen(t, "claude-idle-after-turn.screen")})
	for _, want := range []string{"osc_progress: UNSUPPORTED", "* live_prompt_box", "prompt_box_body: "} {
		if !strings.Contains(out, want) {
			t.Errorf("Explain output lacks %q:\n%s", want, out)
		}
	}
}
