package detect

// Rule engine ported from herdr src/detect/manifest.rs (Apache 2.0, see
// manifests/NOTICE). Manifests are parsed from TOML and evaluated as follows:
//
//   - `contains` needles are lowercased at load time and searched in a
//     lowercased copy of the region text; every needle must be present.
//   - `regex` patterns must all match the raw region text.
//   - `line_regex` patterns must each match at least one raw line.
//   - `all` gates must all match, at least one `any` gate must match (when
//     any are given), and no `not` gate may match.
//   - The highest-priority matching rule wins; the first rule wins ties.
//   - A known agent with no matching rule falls back to idle.
//
// Regex compatibility (Go RE2 vs Rust regex): every pattern in claude.toml
// and codex.toml compiles unchanged under RE2, so no pattern was rewritten
// and no rule was skipped. Known semantic differences that were accepted
// as-is: Go's \s, \d, \w and \b are ASCII-only while Rust's are Unicode
// (terminal chrome in these manifests is ASCII where it matters), and
// (?i) uses simple case folding in both engines.

import (
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed manifests/*.toml
var manifestFS embed.FS

type manifest struct {
	ID               string   `toml:"id"`
	Version          string   `toml:"version"`
	MinEngineVersion int      `toml:"min_engine_version"`
	UpdatedAt        string   `toml:"updated_at"`
	Aliases          []string `toml:"aliases"`
	Rules            []rule   `toml:"rules"`
}

type rule struct {
	ID              string `toml:"id"`
	State           string `toml:"state"`
	Priority        int    `toml:"priority"`
	Region          string `toml:"region"`
	VisibleIdle     bool   `toml:"visible_idle"`
	VisibleBlocker  bool   `toml:"visible_blocker"`
	VisibleWorking  bool   `toml:"visible_working"`
	SkipStateUpdate bool   `toml:"skip_state_update"`
	gate
}

type gate struct {
	All       []gate   `toml:"all"`
	Any       []gate   `toml:"any"`
	Not       []gate   `toml:"not"`
	Contains  []string `toml:"contains"`
	Regex     []string `toml:"regex"`
	LineRegex []string `toml:"line_regex"`
}

type compiledGate struct {
	all, any, not []compiledGate
	contains      []string // lowercased
	regex         []*regexp.Regexp
	lineRegex     []*regexp.Regexp
}

type compiledRule struct {
	rule
	state State
	gate  compiledGate
}

type loadedManifest struct {
	manifest
	rules []compiledRule
}

// manifests is keyed by id and by every alias.
var manifests = mustLoadManifests()

func mustLoadManifests() map[string]*loadedManifest {
	out := map[string]*loadedManifest{}
	entries, err := fs.ReadDir(manifestFS, "manifests")
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		raw, err := manifestFS.ReadFile("manifests/" + e.Name())
		if err != nil {
			panic(err)
		}
		lm, err := loadManifest(raw)
		if err != nil {
			panic(fmt.Sprintf("manifest %s: %v", e.Name(), err))
		}
		out[lm.ID] = lm
		for _, a := range lm.Aliases {
			out[a] = lm
		}
	}
	return out
}

func loadManifest(raw []byte) (*loadedManifest, error) {
	var m manifest
	md, err := toml.Decode(string(raw), &m)
	if err != nil {
		return nil, err
	}
	if u := md.Undecoded(); len(u) > 0 {
		return nil, fmt.Errorf("unknown keys: %v", u)
	}
	if m.ID == "" || len(m.Rules) == 0 {
		return nil, fmt.Errorf("manifest needs an id and at least one rule")
	}
	lm := &loadedManifest{manifest: m}
	for _, r := range m.Rules {
		if r.Region == "" {
			r.Region = "whole_recent"
		}
		cg, err := compileGate(r.gate)
		if err != nil {
			return nil, fmt.Errorf("rule %s: %w", r.ID, err)
		}
		lm.rules = append(lm.rules, compiledRule{rule: r, state: parseState(r.State), gate: cg})
	}
	return lm, nil
}

func parseState(s string) State {
	switch s {
	case "idle":
		return Idle
	case "working":
		return Working
	case "blocked":
		return Blocked
	default:
		return Unknown
	}
}

func compileGate(g gate) (compiledGate, error) {
	cg := compiledGate{}
	for _, n := range g.Contains {
		cg.contains = append(cg.contains, strings.ToLower(n))
	}
	for _, p := range g.Regex {
		re, err := regexp.Compile(p)
		if err != nil {
			return cg, fmt.Errorf("regex %q: %w", p, err)
		}
		cg.regex = append(cg.regex, re)
	}
	for _, p := range g.LineRegex {
		re, err := regexp.Compile(p)
		if err != nil {
			return cg, fmt.Errorf("line_regex %q: %w", p, err)
		}
		cg.lineRegex = append(cg.lineRegex, re)
	}
	var err error
	if cg.all, err = compileGates(g.All); err != nil {
		return cg, err
	}
	if cg.any, err = compileGates(g.Any); err != nil {
		return cg, err
	}
	if cg.not, err = compileGates(g.Not); err != nil {
		return cg, err
	}
	return cg, nil
}

func compileGates(gs []gate) ([]compiledGate, error) {
	var out []compiledGate
	for _, g := range gs {
		cg, err := compileGate(g)
		if err != nil {
			return nil, err
		}
		out = append(out, cg)
	}
	return out, nil
}

func (g *compiledGate) matches(text, lower string) bool {
	for _, n := range g.contains {
		if !strings.Contains(lower, n) {
			return false
		}
	}
	for _, re := range g.regex {
		if !re.MatchString(text) {
			return false
		}
	}
	for _, re := range g.lineRegex {
		if !anyLineMatches(re, text) {
			return false
		}
	}
	for i := range g.all {
		if !g.all[i].matches(text, lower) {
			return false
		}
	}
	if len(g.any) > 0 {
		hit := false
		for i := range g.any {
			if g.any[i].matches(text, lower) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for i := range g.not {
		if g.not[i].matches(text, lower) {
			return false
		}
	}
	return true
}

func anyLineMatches(re *regexp.Regexp, text string) bool {
	for _, line := range splitLines(text) {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// evaluation is the outcome of running one manifest against one Input.
type evaluation struct {
	screen  string
	regions map[string]regionInfo // by selector spec
	rules   []evaluatedRule
	winner  int // index into rules, or -1 when no rule matched
}

type regionInfo struct {
	text      string
	supported bool
}

type evaluatedRule struct {
	*compiledRule
	matched bool
}

func evaluate(lm *loadedManifest, in Input) evaluation {
	ev := evaluation{screen: strings.Join(in.Screen, "\n"), regions: map[string]regionInfo{}, winner: -1}
	for i := range lm.rules {
		r := &lm.rules[i]
		spec := strings.TrimSpace(r.Region)
		ri, seen := ev.regions[spec]
		if !seen {
			ri.text, ri.supported = regionText(in, ev.screen, spec)
			ev.regions[spec] = ri
		}
		matched := ri.supported && r.gate.matches(ri.text, strings.ToLower(ri.text))
		ev.rules = append(ev.rules, evaluatedRule{compiledRule: r, matched: matched})
		if matched && (ev.winner < 0 || r.Priority > lm.rules[ev.winner].Priority) {
			ev.winner = i
		}
	}
	return ev
}

func (ev evaluation) result() Result {
	if ev.winner < 0 {
		return Result{State: Idle, Reason: "default_known_agent_idle_fallback"}
	}
	w := ev.rules[ev.winner]
	return Result{
		State:          w.state,
		Rule:           w.ID,
		Reason:         fmt.Sprintf("rule %s (priority %d)", w.ID, w.Priority),
		VisibleIdle:    w.VisibleIdle && w.state == Idle,
		VisibleBlocker: w.VisibleBlocker && w.state == Blocked,
		VisibleWorking: w.VisibleWorking && w.state == Working,
		Skip:           w.SkipStateUpdate,
	}
}

func (ev evaluation) unsupportedRegions() []string {
	var out []string
	for spec, ri := range ev.regions {
		if !ri.supported {
			out = append(out, spec)
		}
	}
	sort.Strings(out)
	return out
}
