// Package procs identifies the agent process behind a tmux pane.
//
// tmux's pane_pid is the first process in the pane, usually the shell. A pane
// outlives several agent processes, so identity is the agent process itself:
// its pid plus start time. The agent is looked for among every process on
// the pane's tty, not only the foreground group, because a tool the agent
// runs takes the foreground while the agent is still there. A wrapper (a
// sandbox, a shell running -c) may name the agent in its own arguments;
// wrappers never win over their descendants.
package procs

import (
	"path/filepath"
	"strings"
	"time"
)

// Proc is one process on a tty.
type Proc struct {
	PID   int
	PPID  int
	PGID  int
	TPGID int // foreground process group of the controlling tty, as seen by this process
	Comm  string
	Start time.Time
	Argv  []string // best effort; empty when unreadable
	Env   []string // best effort; empty when unreadable
}

// Identity is a resolved agent instance in a pane.
type Identity struct {
	Agent     string // "claude", "codex", ...
	PID       int
	Start     time.Time
	Comm      string
	LeaderPID int // foreground process group leader at the time of identification

	// Tentative is set when the only evidence is an environment hint. A
	// tentative identity selects detection rules but is not proof that the
	// process is the agent, so the daemon keeps looking for a verified one
	// and replaces the tentative candidate when it appears.
	Tentative bool
}

// Same reports whether two identities refer to the same process instance.
func (a Identity) Same(b Identity) bool {
	return a.PID == b.PID && a.Start.Equal(b.Start)
}

// EnvHint is the environment variable a wrapper can set to name the agent it
// runs, for the case where neither comm nor argv reveals it. It selects
// detection rules; it is not evidence that the process is the agent rather
// than its wrapper. A hinted process is only a candidate when its own program
// is not a shell or known wrapper and nothing below it in the tree
// identifies as an agent, and even then the identity is tentative.
const EnvHint = "LAATMUX_AGENT"

var knownAgents = map[string]string{
	"claude":      "claude",
	"claude-code": "claude",
	"codex":       "codex",
}

// Programs that only ever wrap something else. Their command text is not
// evidence of an agent process: `bash -c 'claude; sleep 60'` is a shell
// before, between and after its children.
var wrappers = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
	"sandbox-exec": true, "safehouse": true, "safehouse-agent": true,
	"env": true, "sudo": true, "nice": true, "caffeinate": true, "login": true,
	"script": true, "tmux": true, "ssh": true,
}

// Interpreters host the agent in-process: node running Claude Code's cli.js
// is the agent, so the script path identifies it, by the npm package
// directory it lives in.
var interpreters = map[string]bool{
	"node": true, "bun": true, "deno": true,
}

var packagePaths = map[string]string{
	"@anthropic-ai/claude-code/": "claude",
	"@openai/codex/":             "codex",
}

func lookupScript(path string) (string, bool) {
	if ag, ok := LookupAgent(path); ok {
		return ag, true
	}
	for prefix, ag := range packagePaths {
		if strings.Contains(path, "node_modules/"+prefix) {
			return ag, true
		}
	}
	return "", false
}

// LookupAgent maps a process or executable name to a known agent id.
//
// Claude Code's native install runs a binary named by its version, for
// example ~/.local/share/claude/versions/2.1.278, so comm is "2.1.278". No
// other agent does that, so a bare semver name is taken as claude.
func LookupAgent(name string) (string, bool) {
	base := strings.TrimSuffix(filepath.Base(name), ".exe")
	if a, ok := knownAgents[base]; ok {
		return a, true
	}
	if isSemver(base) {
		return "claude", true
	}
	return "", false
}

func isSemver(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// Find looks for a known agent among the pane's processes. ok is false when
// none is present. Exists reports whether a previously identified instance
// is still there.
func Find(tty string) (Identity, bool, error) {
	procs, err := ListTTY(tty)
	if err != nil {
		return Identity{}, false, err
	}
	id, ok := find(procs)
	return id, ok, nil
}

// Exists reports whether the identity's process is still on the tty.
func Exists(tty string, id Identity) (bool, error) {
	procs, err := ListTTY(tty)
	if err != nil {
		return false, err
	}
	return exists(procs, id), nil
}

func exists(procs []Proc, id Identity) bool {
	for _, p := range procs {
		if p.PID == id.PID && p.Start.Equal(id.Start) {
			return true
		}
	}
	return false
}

// find picks the agent process. A process whose own program is a known
// agent, directly or as an interpreter's script, is a verified candidate.
// A process carrying only an env hint is a tentative candidate. Verified
// beats tentative; among equals the deepest process in the tree wins, so a
// wrapper never beats the agent it started.
func find(procs []Proc) (Identity, bool) {
	if len(procs) == 0 {
		return Identity{}, false
	}
	byPID := make(map[int]Proc, len(procs))
	hasChild := make(map[int]bool, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
	}
	for _, p := range procs {
		if _, ok := byPID[p.PPID]; ok {
			hasChild[p.PPID] = true
		}
	}
	depth := func(p Proc) int {
		d := 0
		for cur, ok := byPID[p.PPID]; ok && d < 64; cur, ok = byPID[cur.PPID] {
			d++
		}
		return d
	}
	var best Proc
	bestAgent := ""
	bestScore := -1
	bestDepth := -1
	for _, p := range procs {
		agent, score := classify(p)
		if score == 0 {
			continue
		}
		if score == 1 && hasChild[p.PID] {
			// A hinted process with a child on the tty is a wrapper.
			continue
		}
		d := depth(p)
		if score > bestScore || (score == bestScore && d > bestDepth) {
			best, bestAgent, bestScore, bestDepth = p, agent, score, d
		}
	}
	if bestAgent == "" {
		return Identity{}, false
	}
	return Identity{Agent: bestAgent, PID: best.PID, Start: best.Start, Comm: best.Comm,
		LeaderPID: leaderPID(procs), Tentative: bestScore == 1}, true
}

func leaderPID(procs []Proc) int {
	for _, p := range procs {
		if p.TPGID > 0 {
			return p.TPGID
		}
	}
	return 0
}

// classify scores one process: 2 when its own program is a known agent, 1
// when only an env hint names one, 0 otherwise. Arguments never count: a
// sandbox's target or a shell's -c text is not the process's own program.
func classify(p Proc) (agent string, score int) {
	prog := p.Comm
	if len(p.Argv) > 0 {
		prog = p.Argv[0]
	}
	if ag, ok := LookupAgent(prog); ok {
		return ag, 2
	}
	if len(p.Argv) == 0 || filepath.Base(p.Argv[0]) != p.Comm {
		// argv may be unreadable or rewritten; comm is the kernel's view.
		if ag, ok := LookupAgent(p.Comm); ok {
			return ag, 2
		}
	}
	if interpreters[filepath.Base(prog)] {
		for _, a := range p.Argv[1:] {
			if strings.HasPrefix(a, "-") {
				continue
			}
			if ag, ok := lookupScript(a); ok {
				return ag, 2
			}
			break
		}
	}
	if wrappers[filepath.Base(prog)] {
		return "", 0
	}
	for _, kv := range p.Env {
		if v, ok := strings.CutPrefix(kv, EnvHint+"="); ok {
			if ag, ok := LookupAgent(v); ok {
				return ag, 1
			}
		}
	}
	return "", 0
}

// FindIn and ExistsIn run the identification logic on a supplied process
// table. They exist for tests of callers; production code uses Find/Exists.
func FindIn(procs []Proc) (Identity, bool)    { return find(procs) }
func ExistsIn(procs []Proc, id Identity) bool { return exists(procs, id) }
