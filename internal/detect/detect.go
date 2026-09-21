// Package detect classifies the state of a coding-agent TUI (Claude Code,
// Codex) from a captured tmux pane: its screen lines and OSC title. The
// rules live in TOML manifests copied from herdr (see manifests/NOTICE).
package detect

import (
	"fmt"
	"sort"
	"strings"
)

type State string

const (
	Working State = "working"
	Blocked State = "blocked"
	Idle    State = "idle"
	Unknown State = "unknown"
)

// Input is one observation of a pane.
type Input struct {
	Agent  string   // "claude", "codex", or "" when no known agent is identified
	Title  string   // the pane's current OSC title (tmux #{pane_title})
	Screen []string // captured screen lines, oldest first, as tmux capture-pane -p prints them
}

type Result struct {
	State          State
	Rule           string // matched rule id, or "" for fallback
	Reason         string // short human-readable explanation, e.g. "rule live_prompt_box (priority 950)" or "default_known_agent_idle_fallback"
	VisibleIdle    bool
	VisibleBlocker bool
	VisibleWorking bool
	Skip           bool // rule asked to skip this update (skip_state_update)
}

// Detect runs the agent's manifest against the observation. An empty or
// unknown Agent yields State Unknown with Reason "no_known_agent".
func Detect(in Input) Result {
	lm, ok := manifests[in.Agent]
	if !ok {
		return Result{State: Unknown, Reason: "no_known_agent"}
	}
	return evaluate(lm, in).result()
}

// Agents lists the available manifest ids, sorted.
func Agents() []string {
	seen := map[string]bool{}
	var ids []string
	for _, lm := range manifests {
		if !seen[lm.ID] {
			seen[lm.ID] = true
			ids = append(ids, lm.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// Explain reports, for debugging, which regions were extracted (first and
// last lines) and which rules matched.
func Explain(in Input) string {
	var b strings.Builder
	lm, ok := manifests[in.Agent]
	if !ok {
		fmt.Fprintf(&b, "agent %q: no manifest -> %s (no_known_agent)\n", in.Agent, Unknown)
		return b.String()
	}
	ev := evaluate(lm, in)
	res := ev.result()
	fmt.Fprintf(&b, "agent %s (manifest %s %s), %d screen lines, title %q\n", lm.ID, lm.ID, lm.Version, len(in.Screen), in.Title)
	fmt.Fprintf(&b, "result: %s", res.State)
	if res.Rule != "" {
		fmt.Fprintf(&b, " via %s", res.Rule)
	}
	fmt.Fprintf(&b, " (%s)", res.Reason)
	if res.VisibleIdle || res.VisibleBlocker || res.VisibleWorking || res.Skip {
		fmt.Fprintf(&b, " visible_idle=%t visible_blocker=%t visible_working=%t skip=%t",
			res.VisibleIdle, res.VisibleBlocker, res.VisibleWorking, res.Skip)
	}
	b.WriteString("\nregions:\n")
	specs := make([]string, 0, len(ev.regions))
	for spec := range ev.regions {
		specs = append(specs, spec)
	}
	sort.Strings(specs)
	for _, spec := range specs {
		ri := ev.regions[spec]
		if !ri.supported {
			fmt.Fprintf(&b, "  %s: UNSUPPORTED (rules using it never match)\n", spec)
			continue
		}
		lines := splitLines(strings.TrimSuffix(ri.text, "\n"))
		switch len(lines) {
		case 0:
			fmt.Fprintf(&b, "  %s: empty\n", spec)
		case 1:
			fmt.Fprintf(&b, "  %s: 1 line %q\n", spec, lines[0])
		default:
			fmt.Fprintf(&b, "  %s: %d lines, first %q, last %q\n", spec, len(lines), lines[0], lines[len(lines)-1])
		}
	}
	b.WriteString("rules:\n")
	for i, r := range ev.rules {
		mark := " "
		switch {
		case i == ev.winner:
			mark = "*"
		case r.matched:
			mark = "+"
		}
		fmt.Fprintf(&b, "  %s %s -> %s (priority %d, region %s)", mark, r.ID, r.state, r.Priority, strings.TrimSpace(r.Region))
		if !ev.regions[strings.TrimSpace(r.Region)].supported {
			b.WriteString(" [unsupported region]")
		}
		b.WriteString("\n")
	}
	if u := ev.unsupportedRegions(); len(u) > 0 {
		fmt.Fprintf(&b, "unsupported regions: %s\n", strings.Join(u, ", "))
	}
	return b.String()
}
