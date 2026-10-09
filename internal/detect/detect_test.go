package detect

import (
	"os"
	"os/exec"
	"path/filepath"
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
// moment. claude-interrupted is from 2.1.284, captured the same way four
// seconds after Esc, and claude-working-2.1.284 seven seconds into a
// turn. codex-idle is from a local pane. Each case is a distinct rule,
// but for claude-interrupted, which checks that an interrupted turn
// reads as idle on its own, and claude-working-2.1.284, which checks
// that the footer rule carries working now that the title does not
// spin.
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
		{"claude-working-2.1.284", "claude", Working, "live_turn_working", false,
			"Claude Code 2.1.284 in auto mode seven seconds into a turn streaming an essay: the title is '✳ <topic>' with no spinner, so osc_title_working misses it and the footer's 'esc to interrupt' carries working"},
		{"claude-working-agents-panel", "claude", Working, "live_turn_working", false,
			"Claude Code 2.1.29x mid-turn with the agents panel open under the footer: six subagent lines and '↓ 1 more' push the spinner line ('· Kneading… (26s · ↓ 1.4k tokens)') to the 13th non-empty line from the bottom, past the old 12-line window; the footer has no 'esc to interrupt'"},
		{"claude-working-hook-frame", "claude", Working, "live_turn_working", false,
			"a user's PostToolUse hook running: '✽ Precipitating… (running PostToolUse hook · 29s · ↓ 1.4k tokens)', the timer after a note in the parentheses, no agents panel; the old pattern wanted a digit right after the ( and read the prompt box as idle"},
		{"claude-teammates-cursor", "claude", Working, "teammates_working", false,
			"the agents panel with the cursor on a teammate's row, '❯ ◯ iss357 …'"},
		{"claude-teammates-permission", "claude", Blocked, "bash_permission_prompt", false,
			"a permission prompt with the agents panel under it: the user's to answer, not working"},
		{"claude-teammates-running", "claude", Working, "teammates_working", false,
			"the same screen with main's turn over ('✻ Baked for 1m 12s · done'), its prompt empty, and the agents panel listing teammates with running timers: the session works until they finish"},
		{"claude-api-wait", "claude", Working, "live_turn_working", false,
			"Claude Code 2.1.29x waiting on the API: '✳ Waiting for API response · will retry in 3s' above the prompt box, no ellipsis on the line and no 'esc to interrupt' in the footer, the title '✳ <topic>' with no spinner"},
		{"claude-interrupted", "claude", Idle, "live_prompt_box", false,
			"Esc during a turn, Claude Code 2.1.284 in auto mode: '⎿ Interrupted · What should Claude do instead?' above an empty prompt box, no 'esc to interrupt'"},
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

// Aliases resolve to the same manifest.
func TestAgentAliases(t *testing.T) {
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

// A workflow syncs the manifests from herdr. A manifest that needs a newer
// herdr engine, or a region selector this port lacks, must fail here rather
// than load with rules that never match.
func TestManifestsFitThePort(t *testing.T) {
	const engineVersion = 3 // herdr's MANIFEST_ENGINE_VERSION the port matches
	for key, lm := range manifests {
		if key != lm.ID {
			continue // an alias
		}
		if lm.MinEngineVersion > engineVersion {
			t.Errorf("%s: min_engine_version %d, the port is engine %d", lm.ID, lm.MinEngineVersion, engineVersion)
		}
		for _, r := range lm.rules {
			spec := strings.TrimSpace(r.Region)
			if _, ok := regionText(Input{}, "", spec); !ok && spec != "osc_progress" {
				t.Errorf("%s: rule %s uses region %q, which the port lacks", lm.ID, r.ID, spec)
			}
		}
	}
}

// Each manifest the engine embeds is herdr's copy in manifests/upstream
// with manifests/patches/<name>.patch applied when there is one, and
// every upstream file has a manifest. The workflow that syncs upstream
// reapplies the patches; a patch that no longer applies, or a manifest
// edited without manifests/apply.sh refresh, fails here.
func TestManifestsPatched(t *testing.T) {
	if _, err := exec.LookPath("patch"); err != nil {
		t.Skip("no patch command")
	}
	ups, err := filepath.Glob("manifests/upstream/*.toml")
	if err != nil || len(ups) == 0 {
		t.Fatalf("upstream manifests: %v %v", ups, err)
	}
	dir := t.TempDir()
	for _, up := range ups {
		name := filepath.Base(up)
		raw, err := os.ReadFile(up)
		if err != nil {
			t.Fatal(err)
		}
		built := filepath.Join(dir, name)
		if err := os.WriteFile(built, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		patch := filepath.Join("manifests", "patches", name+".patch")
		if pf, err := os.Open(patch); err == nil {
			cmd := exec.Command("patch", "-p1", "-F0", "-s", "--no-backup-if-mismatch", built)
			cmd.Stdin = pf
			out, err := cmd.CombinedOutput()
			pf.Close()
			if err != nil {
				t.Errorf("%s does not apply to upstream/%s; rebase it (manifests/apply.sh in NOTICE): %v\n%s", patch, name, err, out)
				continue
			}
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		want, err := os.ReadFile(built)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join("manifests", name))
		if err != nil {
			t.Errorf("upstream/%s has no manifest: %v", name, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("manifests/%s is not upstream/%s with its patch applied; run manifests/apply.sh, or manifests/apply.sh refresh after editing it", name, name)
		}
	}
	tops, _ := filepath.Glob("manifests/*.toml")
	if len(tops) != len(ups) {
		t.Errorf("%d manifests, %d upstream files; every manifest is built from an upstream copy", len(tops), len(ups))
	}
}

// A response block marker after the last › prompt makes that prompt
// stale; one before it does not.
func TestCodexStalePrompt(t *testing.T) {
	for _, m := range []string{"•", "■", "✗", "✓"} {
		if _, ok := currentCodexPromptIndex([]string{"› hi", "", m + " answer"}); ok {
			t.Errorf("%s after the prompt left it current", m)
		}
	}
	if i, ok := currentCodexPromptIndex([]string{"• before", "› hi", "", "  text"}); !ok || i != 1 {
		t.Errorf("got %d %v, want the prompt at 1", i, ok)
	}
}
