package procs

import (
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

// Observed on a real pane: safehouse runs the agent as a child of a bash
// wrapper that is the foreground group leader. Argv unreadable (sandbox).
func TestFindAgentUnderWrapperWithoutArgv(t *testing.T) {
	procs := []Proc{
		{PID: 66975, PPID: 31243, PGID: 66975, TPGID: 67090, Comm: "zsh", Start: at(0)},
		{PID: 67090, PPID: 66975, PGID: 67090, TPGID: 67090, Comm: "bash", Start: at(2)},
		{PID: 68164, PPID: 67090, PGID: 67090, TPGID: 67090, Comm: "codex", Start: at(3)},
		{PID: 68785, PPID: 68164, PGID: 67090, TPGID: 67090, Comm: "codex-code-mode-", Start: at(30)},
	}
	id, ok := find(procs)
	if !ok || id.Agent != "codex" || id.PID != 68164 || id.LeaderPID != 67090 {
		t.Fatalf("got %+v ok=%v", id, ok)
	}
}

// Review finding 1: a wrapper whose arguments name the agent must not win
// over the agent it started.
func TestFindWrapperArgvDoesNotWin(t *testing.T) {
	procs := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 100, Comm: "zsh", Start: at(0)},
		{PID: 100, PPID: 10, PGID: 100, TPGID: 100, Comm: "safehouse", Start: at(1), Argv: []string{"safehouse", "--", "/opt/bin/claude", "--resume"}},
		{PID: 101, PPID: 100, PGID: 100, TPGID: 100, Comm: "claude", Start: at(2), Argv: []string{"/opt/bin/claude", "--resume"}},
	}
	id, ok := find(procs)
	if !ok || id.PID != 101 {
		t.Fatalf("wrapper won: %+v", id)
	}
	// Claude exits, wrapper survives: no agent, not the wrapper.
	if id2, ok := find(procs[:2]); ok {
		t.Fatalf("surviving wrapper reported as agent: %+v", id2)
	}
	// Claude restarts under the same wrapper: a new identity.
	procs[2] = Proc{PID: 102, PPID: 100, PGID: 100, TPGID: 100, Comm: "claude", Start: at(9), Argv: []string{"/opt/bin/claude"}}
	id3, ok := find(procs)
	if !ok || id3.PID != 102 || id3.Same(id) {
		t.Fatalf("restart not seen as new instance: %+v", id3)
	}
}

// Review 2 finding 1: a shell's -c text is not evidence of an agent process.
// The child is the agent; the surviving shell alone is nothing.
func TestFindShellDashCIsNotTheAgent(t *testing.T) {
	procs := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 100, Comm: "zsh", Start: at(0)},
		{PID: 100, PPID: 10, PGID: 100, TPGID: 100, Comm: "bash", Start: at(1), Argv: []string{"bash", "-c", "claude --resume; sleep 60"}},
	}
	if id, ok := find(procs); ok {
		t.Fatalf("shell -c reported as agent before child exists: %+v", id)
	}
	procs = append(procs, Proc{PID: 101, PPID: 100, PGID: 100, TPGID: 100, Comm: "2.1.278", Start: at(2)})
	id, ok := find(procs)
	if !ok || id.PID != 101 || id.Agent != "claude" || id.Tentative {
		t.Fatalf("got %+v", id)
	}
	if id, ok := find(procs[:2]); ok {
		t.Fatalf("shell -c reported as agent after child exit: %+v", id)
	}
}

// An interpreter hosting the agent is the agent process.
func TestFindInterpreterHosted(t *testing.T) {
	procs := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 100, Comm: "zsh", Start: at(0)},
		{PID: 100, PPID: 10, PGID: 100, TPGID: 100, Comm: "node", Start: at(1), Argv: []string{"node", "--no-warnings", "/opt/node_modules/@anthropic-ai/claude-code/cli.js"}},
	}
	id, ok := find(procs)
	if !ok || id.PID != 100 || id.Agent != "claude" || id.Tentative {
		t.Fatalf("got %+v", id)
	}
}

// Review finding 2: a tool taking the foreground must not replace the agent.
func TestFindSurvivesForegroundHandoff(t *testing.T) {
	procs := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 200, Comm: "zsh", Start: at(0)},
		{PID: 100, PPID: 10, PGID: 100, TPGID: 200, Comm: "claude", Start: at(1)},
		{PID: 200, PPID: 100, PGID: 200, TPGID: 200, Comm: "bash", Start: at(5), Argv: []string{"bash", "-c", "go test ./..."}},
		{PID: 201, PPID: 200, PGID: 200, TPGID: 200, Comm: "go", Start: at(5)},
	}
	id, ok := find(procs)
	if !ok || id.PID != 100 {
		t.Fatalf("foreground tool replaced agent: %+v", id)
	}
	if id.LeaderPID != 200 {
		t.Fatalf("leader should be the foreground group: %+v", id)
	}
	if !exists(procs, id) {
		t.Fatal("exists false for live agent")
	}
}

// After the agent exits to the shell, nothing is found and the old identity
// no longer exists.
func TestFindAfterExit(t *testing.T) {
	old := Identity{PID: 100, Start: at(1)}
	procs := []Proc{{PID: 10, PPID: 1, PGID: 10, TPGID: 10, Comm: "zsh", Start: at(0)}}
	if _, ok := find(procs); ok {
		t.Fatal("shell reported as agent")
	}
	if exists(procs, old) {
		t.Fatal("exited agent reported as existing")
	}
	// Same pid reused with a different start time is not the same instance.
	procs = append(procs, Proc{PID: 100, PPID: 10, PGID: 100, TPGID: 100, Comm: "claude", Start: at(50)})
	if exists(procs, old) {
		t.Fatal("pid reuse mistaken for the old instance")
	}
}

// A version-named Claude binary: comm alone carries the signal, and a path
// argument on a sandbox does not.
func TestFindVersionNamedBinary(t *testing.T) {
	procs := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 300, Comm: "zsh", Start: at(0)},
		{PID: 300, PPID: 10, PGID: 300, TPGID: 300, Comm: "2.1.278", Start: at(1),
			Argv: []string{"/Users/x/.local/share/claude/versions/2.1.278", "--resume"}},
	}
	id, ok := find(procs)
	if !ok || id.Agent != "claude" || id.PID != 300 {
		t.Fatalf("got %+v", id)
	}
	procs[1].Argv = nil
	if id, ok := find(procs); !ok || id.Agent != "claude" {
		t.Fatalf("comm semver not recognised: %+v", id)
	}
}

// The env hint yields a tentative identity on a non-wrapper process only. A
// hinted shell, or a hinted process with a child, is never the agent.
func TestFindEnvHintIsTentative(t *testing.T) {
	procs := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 300, Comm: "zsh", Start: at(0)},
		{PID: 300, PPID: 10, PGID: 300, TPGID: 300, Comm: "agentd", Start: at(1), Env: []string{"LAATMUX_AGENT=claude"}},
	}
	id, ok := find(procs)
	if !ok || id.Agent != "claude" || id.PID != 300 || !id.Tentative {
		t.Fatalf("hint not honoured as tentative: %+v", id)
	}
	procs = append(procs, Proc{PID: 301, PPID: 300, PGID: 300, TPGID: 300, Comm: "helper", Start: at(2)})
	if id, ok := find(procs); ok {
		t.Fatalf("hinted wrapper with a child reported as agent: %+v", id)
	}
	hintedShell := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 300, Comm: "zsh", Start: at(0)},
		{PID: 300, PPID: 10, PGID: 300, TPGID: 300, Comm: "bash", Start: at(1), Argv: []string{"bash"}, Env: []string{"LAATMUX_AGENT=claude"}},
	}
	if id, ok := find(hintedShell); ok {
		t.Fatalf("hinted shell reported as agent: %+v", id)
	}
	// A verified candidate replaces a tentative one wherever it sits.
	procs = append(procs, Proc{PID: 302, PPID: 301, PGID: 300, TPGID: 300, Comm: "claude", Start: at(3)})
	if id, ok := find(procs); !ok || id.PID != 302 || id.Tentative {
		t.Fatalf("verified did not beat tentative: %+v", id)
	}
}

func TestSameIdentity(t *testing.T) {
	a := Identity{PID: 1, Start: at(10)}
	b := Identity{PID: 1, Start: at(11)}
	if a.Same(b) {
		t.Fatal("same pid, different start must differ")
	}
}
