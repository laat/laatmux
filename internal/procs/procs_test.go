package procs

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
	id, ok := FindIn(procs)
	if !ok || id.Agent != "codex" || id.PID != 68164 || id.LeaderPID != 67090 {
		t.Fatalf("got %+v ok=%v", id, ok)
	}
}

// A wrapper whose arguments name the agent must not win
// over the agent it started.
func TestFindWrapperArgvDoesNotWin(t *testing.T) {
	procs := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 100, Comm: "zsh", Start: at(0)},
		{PID: 100, PPID: 10, PGID: 100, TPGID: 100, Comm: "safehouse", Start: at(1), Argv: []string{"safehouse", "--", "/opt/bin/claude", "--resume"}},
		{PID: 101, PPID: 100, PGID: 100, TPGID: 100, Comm: "claude", Start: at(2), Argv: []string{"/opt/bin/claude", "--resume"}},
	}
	id, ok := FindIn(procs)
	if !ok || id.PID != 101 {
		t.Fatalf("wrapper won: %+v", id)
	}
	// Claude exits, wrapper survives: no agent, not the wrapper.
	if id2, ok := FindIn(procs[:2]); ok {
		t.Fatalf("surviving wrapper reported as agent: %+v", id2)
	}
	// Claude restarts under the same wrapper: a new identity.
	procs[2] = Proc{PID: 102, PPID: 100, PGID: 100, TPGID: 100, Comm: "claude", Start: at(9), Argv: []string{"/opt/bin/claude"}}
	id3, ok := FindIn(procs)
	if !ok || id3.PID != 102 || id3.Same(id) {
		t.Fatalf("restart not seen as new instance: %+v", id3)
	}
}

// A shell's -c text is not evidence of an agent process.
// The child is the agent; the surviving shell alone is nothing.
func TestFindShellDashCIsNotTheAgent(t *testing.T) {
	procs := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 100, Comm: "zsh", Start: at(0)},
		{PID: 100, PPID: 10, PGID: 100, TPGID: 100, Comm: "bash", Start: at(1), Argv: []string{"bash", "-c", "claude --resume; sleep 60"}},
	}
	if id, ok := FindIn(procs); ok {
		t.Fatalf("shell -c reported as agent before child exists: %+v", id)
	}
	procs = append(procs, Proc{PID: 101, PPID: 100, PGID: 100, TPGID: 100, Comm: "2.1.278", Start: at(2)})
	id, ok := FindIn(procs)
	if !ok || id.PID != 101 || id.Agent != "claude" || id.Tentative {
		t.Fatalf("got %+v", id)
	}
	if id, ok := FindIn(procs[:2]); ok {
		t.Fatalf("shell -c reported as agent after child exit: %+v", id)
	}
}

// An interpreter hosting the agent is the agent process.
func TestFindInterpreterHosted(t *testing.T) {
	procs := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 100, Comm: "zsh", Start: at(0)},
		{PID: 100, PPID: 10, PGID: 100, TPGID: 100, Comm: "node", Start: at(1), Argv: []string{"node", "--no-warnings", "/opt/node_modules/@anthropic-ai/claude-code/cli.js"}},
	}
	id, ok := FindIn(procs)
	if !ok || id.PID != 100 || id.Agent != "claude" || id.Tentative {
		t.Fatalf("got %+v", id)
	}
}

// An interpreter whose argv cannot be read, a node exiting on the tty
// say, is no verified agent, and classifying it does not panic (it
// sliced a nil argv once, and the poll goroutine with it); an exec with
// no argv gives an empty, non-nil one on macOS, the same. argv[0] alone
// never panicked and is here to show nothing changed. An env hint still
// names the agent tentatively, as it does when comm and argv say
// nothing.
func TestClassifyInterpreterWithoutArgv(t *testing.T) {
	for _, comm := range []string{"node", "bun", "deno"} {
		for name, argv := range map[string][]string{"nil argv": nil, "empty argv": {}, "argv[0] alone": {comm}} {
			if ag, score := classify(Proc{Comm: comm, Argv: argv}); ag != "" || score != 0 {
				t.Fatalf("%s with %s classified as %q (%d)", comm, name, ag, score)
			}
		}
		if ag, score := classify(Proc{Comm: comm, Env: []string{EnvHint + "=claude"}}); ag != "claude" || score != 1 {
			t.Fatalf("%s without argv but with a hint classified as %q (%d), want claude tentatively", comm, ag, score)
		}
	}
	procs := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 10, Comm: "zsh", Start: at(0)},
		{PID: 100, PPID: 10, PGID: 100, TPGID: 100, Comm: "node", Start: at(1)},
	}
	if id, ok := FindIn(procs); ok {
		t.Fatalf("argv-less node reported as agent: %+v", id)
	}
}

// A tool taking the foreground must not replace the agent.
func TestFindSurvivesForegroundHandoff(t *testing.T) {
	procs := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 200, Comm: "zsh", Start: at(0)},
		{PID: 100, PPID: 10, PGID: 100, TPGID: 200, Comm: "claude", Start: at(1)},
		{PID: 200, PPID: 100, PGID: 200, TPGID: 200, Comm: "bash", Start: at(5), Argv: []string{"bash", "-c", "go test ./..."}},
		{PID: 201, PPID: 200, PGID: 200, TPGID: 200, Comm: "go", Start: at(5)},
	}
	id, ok := FindIn(procs)
	if !ok || id.PID != 100 {
		t.Fatalf("foreground tool replaced agent: %+v", id)
	}
	if id.LeaderPID != 200 {
		t.Fatalf("leader should be the foreground group: %+v", id)
	}
	if !ExistsIn(procs, id) {
		t.Fatal("exists false for live agent")
	}
}

// After the agent exits to the shell, nothing is found and the old identity
// no longer exists.
func TestFindAfterExit(t *testing.T) {
	old := Identity{PID: 100, Start: at(1)}
	procs := []Proc{{PID: 10, PPID: 1, PGID: 10, TPGID: 10, Comm: "zsh", Start: at(0)}}
	if _, ok := FindIn(procs); ok {
		t.Fatal("shell reported as agent")
	}
	if ExistsIn(procs, old) {
		t.Fatal("exited agent reported as existing")
	}
	// Same pid reused with a different start time is not the same instance.
	procs = append(procs, Proc{PID: 100, PPID: 10, PGID: 100, TPGID: 100, Comm: "claude", Start: at(50)})
	if ExistsIn(procs, old) {
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
	id, ok := FindIn(procs)
	if !ok || id.Agent != "claude" || id.PID != 300 {
		t.Fatalf("got %+v", id)
	}
	procs[1].Argv = nil
	if id, ok := FindIn(procs); !ok || id.Agent != "claude" {
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
	id, ok := FindIn(procs)
	if !ok || id.Agent != "claude" || id.PID != 300 || !id.Tentative {
		t.Fatalf("hint not honoured as tentative: %+v", id)
	}
	procs = append(procs, Proc{PID: 301, PPID: 300, PGID: 300, TPGID: 300, Comm: "helper", Start: at(2)})
	if id, ok := FindIn(procs); ok {
		t.Fatalf("hinted wrapper with a child reported as agent: %+v", id)
	}
	hintedShell := []Proc{
		{PID: 10, PPID: 1, PGID: 10, TPGID: 300, Comm: "zsh", Start: at(0)},
		{PID: 300, PPID: 10, PGID: 300, TPGID: 300, Comm: "bash", Start: at(1), Argv: []string{"bash"}, Env: []string{"LAATMUX_AGENT=claude"}},
	}
	if id, ok := FindIn(hintedShell); ok {
		t.Fatalf("hinted shell reported as agent: %+v", id)
	}
	// A verified candidate replaces a tentative one wherever it sits.
	procs = append(procs, Proc{PID: 302, PPID: 301, PGID: 300, TPGID: 300, Comm: "claude", Start: at(3)})
	if id, ok := FindIn(procs); !ok || id.PID != 302 || id.Tentative {
		t.Fatalf("verified did not beat tentative: %+v", id)
	}
}

// An agent that exits is gone from its tty at once, though its parent
// has not reaped it: the zombie, its comm still the agent's, kept the
// identity alive until the reap. The zombie's parent, a live session
// leader on a pty, stays listed; Lookup still finds the zombie, whose
// pid names it until the reap. The parent's argv, an empty argument in
// it, reads back whole: macOS's parse dropped the empty string and took
// the first env string into argv.
func TestListTTYDropsZombies(t *testing.T) {
	tru, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true:", err)
	}
	b, err := os.ReadFile(tru)
	if err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, b, 0o755); err != nil {
		t.Fatal(err)
	}
	_, tty := openPTY(t)
	slave, err := os.OpenFile(tty, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	pidR, pidW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pidR.Close()
	doneR, doneW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// The empty argument ends the test flags and is read back in its
	// place, and the env after it whole. An env hint of the host's would
	// make the parent a tentative agent once its child is off the tty.
	parent := exec.Command(os.Args[0], "-test.run=^TestZombieParent$", "", "end")
	hint := zombieEnv + "=" + claude
	parent.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, EnvHint+"=")
	}), hint)
	parent.Stdin = slave
	parent.ExtraFiles = []*os.File{pidW, doneR}
	parent.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	err = parent.Start()
	pidW.Close()
	doneR.Close()
	if err != nil {
		doneW.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The parent ends and reaps its child and exits when doneW
		// closes; failing that, its process group, the child in it, is
		// killed.
		doneW.Close()
		kill := time.AfterFunc(10*time.Second, func() { syscall.Kill(-parent.Process.Pid, syscall.SIGKILL) })
		parent.Wait()
		kill.Stop()
	})
	pidR.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := bufio.NewReader(pidR).ReadString('\n')
	if err != nil {
		t.Fatalf("no pid from the zombie's parent: %v", err)
	}
	zombie, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	// The child runs from the moment its pid is written, so the wait is
	// for it to exit; a zombie listed stays listed to the deadline.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		list, err := ListTTY(tty)
		if err != nil {
			t.Fatal(err)
		}
		listed, leader := false, false
		for _, p := range list {
			listed = listed || p.PID == zombie
			if p.PID == parent.Process.Pid {
				leader = true
				// Not printed: a wrong parse may take env strings into argv.
				if !slices.Equal(p.Argv, parent.Args) {
					t.Fatalf("the session leader's argv read as %d strings, not as %q", len(p.Argv), parent.Args)
				}
				if !slices.Contains(p.Env, hint) {
					t.Fatalf("the session leader's env read without %s", zombieEnv)
				}
			}
		}
		if !leader {
			t.Fatalf("the live session leader %d is not listed on %s: %s", parent.Process.Pid, tty, pidComms(list))
		}
		if !listed {
			if id, ok := FindIn(list); ok {
				t.Fatalf("an agent found on %s with its only one exited: %+v", tty, id)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still listed on %s 10s after its start: %s", zombie, tty, pidComms(list))
		}
	}
	// Off the listing as a zombie on the tty, not by leaving it.
	if !zombieOn(t, zombie, tty) {
		t.Fatalf("pid %d is not a zombie on %s", zombie, tty)
	}
	if p, ok := Lookup(zombie); !ok || p.Comm != "claude" {
		t.Fatalf("Lookup of the zombie %d: %+v ok=%v, want it found as claude", zombie, p, ok)
	}
}

// pidComms is a listing's pids and comms, for a failure message: the
// environment the processes carry stays out of the test's log.
func pidComms(list []Proc) string {
	s := make([]string, len(list))
	for i, p := range list {
		s[i] = strconv.Itoa(p.PID) + " " + p.Comm
	}
	return "[" + strings.Join(s, ", ") + "]"
}

const zombieEnv = "LAATMUX_PROCS_ZOMBIE"

// TestZombieParent is TestListTTYDropsZombies's session leader on the
// pty: it starts the program it is given and writes its pid to fd 3,
// and reaps it only when fd 4 reads EOF, the test done or gone; the
// zombie is not left to whatever reaps orphans on the host. The kill
// before the reap ends a child the test failed before it exited, held
// at its launch by a check of the new binary, say; on a zombie it does
// nothing.
func TestZombieParent(t *testing.T) {
	prog := os.Getenv(zombieEnv)
	if prog == "" {
		t.Skip("run by TestListTTYDropsZombies")
	}
	child := exec.Command(prog)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	out := os.NewFile(3, "pid")
	fmt.Fprintln(out, child.Process.Pid)
	out.Close()
	io.Copy(io.Discard, os.NewFile(4, "done"))
	child.Process.Kill()
	child.Wait()
}

func TestSameIdentity(t *testing.T) {
	a := Identity{PID: 1, Start: at(10)}
	b := Identity{PID: 1, Start: at(11)}
	if a.Same(b) {
		t.Fatal("same pid, different start must differ")
	}
}

// StartID's form is persisted by a daemon and compared by the next
// build's stop, so it is pinned to the kernel's values: darwin's
// microseconds since the epoch, Linux's boot id and the starttime
// field of /proc/<pid>/stat, in that form.
func TestStartIDForm(t *testing.T) {
	p, ok := Lookup(os.Getpid())
	if !ok {
		t.Fatal("no lookup of this process")
	}
	want := strconv.FormatInt(p.Start.UnixMicro(), 10)
	if runtime.GOOS == "linux" {
		id, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err != nil {
			t.Fatal(err)
		}
		stat, err := os.ReadFile("/proc/self/stat")
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+2:]))
		want = strings.TrimSpace(string(id)) + "/" + fields[19]
	}
	if p.StartID != want {
		t.Fatalf("StartID %q, the kernel's values give %q", p.StartID, want)
	}
}
