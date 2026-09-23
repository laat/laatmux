package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/worktree"
)

// addWorktree runs an add with no agent and returns the root.
func addWorktree(t *testing.T, pc *protocol.Conn, remote, branch string) string {
	t.Helper()
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "add-" + branch, Repo: remote, Branch: branch, Cmd: []string{"true"}})
	res, _ := result(t, pc, "add-"+branch)
	if !res.OK {
		t.Fatalf("add %s: %+v", branch, res)
	}
	return res.Root
}

// Progress is numbered from 1 in order; a follow replays from after
// the mark and then the result; a follow for an id the daemon does not
// know is refused with the unknown-command error rather than started.
func TestFollowReplaysFromMark(t *testing.T) {
	d, _, _, remote := newAddDaemon(t)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", Cmd: []string{"true"}})
	res, progress := result(t, pc, "c1")
	if !res.OK || len(progress) < 3 {
		t.Fatalf("add: %+v %d progress", res, len(progress))
	}
	for i, p := range progress {
		if p.N != uint64(i+1) {
			t.Fatalf("progress %d numbered %d", i, p.N)
		}
	}
	mark := uint64(len(progress) - 2)
	pc.Write(protocol.Message{Type: protocol.TypeFollow, ID: "c1", After: mark})
	res2, replay := result(t, pc, "c1")
	if !res2.OK || res2.Root != res.Root {
		t.Fatalf("follow result %+v", res2)
	}
	if len(replay) != 2 || replay[0].N != mark+1 || replay[1].N != mark+2 {
		t.Fatalf("replay after %d: %+v", mark, replay)
	}
	pc.Write(protocol.Message{Type: protocol.TypeFollow, ID: "nope", After: 0})
	if res, ps := result(t, pc, "nope"); res.OK || res.Error != protocol.ErrUnknownCommand || len(ps) != 0 {
		t.Fatalf("unknown follow: %+v %+v", res, ps)
	}
}

// A run streams each stream's lines with its fd and ends with the
// process's exit status; a refusal is a result with ok false.
func TestRunStreamsOutput(t *testing.T) {
	d, _, store, remote := newAddDaemon(t)
	pc := conn(t, d)
	root := addWorktree(t, pc, remote, "task")
	pc.Write(protocol.Message{Type: protocol.TypeRun, ID: "r1", Repo: remote, Branch: "task", Root: root,
		Cmd: []string{"sh", "-c", "echo one; echo two >&2; printf three; pwd >&2; exit 3"}})
	res, progress := result(t, pc, "r1")
	if !res.OK || res.Exit != 3 {
		t.Fatalf("result %+v", res)
	}
	var out, errs []string
	for _, p := range progress {
		switch {
		case p.State == protocol.StateStart:
			if p.Detail != root || p.N != 1 {
				t.Fatalf("start %+v", p)
			}
		case p.FD == 1:
			out = append(out, p.Detail)
		case p.FD == 2:
			errs = append(errs, p.Detail)
		}
	}
	if strings.Join(out, ",") != "one,three" || strings.Join(errs, ",") != "two,"+root {
		t.Fatalf("out %v errs %v", out, errs)
	}
	// A command that cannot start, a root that is no worktree, a
	// mismatched repository or branch, and a root outside the directory.
	for _, m := range []struct {
		m    protocol.Message
		want string
	}{
		{protocol.Message{Type: protocol.TypeRun, ID: "r2", Root: root, Cmd: []string{"/nonexistent/cmd"}}, "no such file"},
		{protocol.Message{Type: protocol.TypeRun, ID: "r3", Root: store.Dirs.Worktree("proj", "other"), Cmd: []string{"true"}}, "not a worktree"},
		{protocol.Message{Type: protocol.TypeRun, ID: "r4", Root: root, Branch: "other", Cmd: []string{"true"}}, "not other"},
		{protocol.Message{Type: protocol.TypeRun, ID: "r5", Root: root, Repo: "nope", Cmd: []string{"true"}}, "unknown repository"},
		{protocol.Message{Type: protocol.TypeRun, ID: "r6", Root: "/tmp", Cmd: []string{"true"}}, "not under the worktrees directory"},
		{protocol.Message{Type: protocol.TypeRun, ID: "r7", Root: root}, "needs a command"},
	} {
		pc.Write(m.m)
		if res, _ := result(t, pc, m.m.ID); res.OK || !strings.Contains(res.Error, m.want) {
			t.Fatalf("%s: %+v, want %q", m.m.ID, res, m.want)
		}
	}
}

// A repository whose bare source equals another entry's label resolves
// by the record's own source first, so a run for it is not taken for
// the other entry; a source that is another repository's is refused.
func TestRunRepoBySourceFirst(t *testing.T) {
	d, _, store, remote := newAddDaemon(t)
	pc := conn(t, d)
	root := addWorktree(t, pc, remote, "task")
	store.Repos = append(store.Repos, worktree.Repo{Source: "/nowhere/other.git", Name: remote})
	pc.Write(protocol.Message{Type: protocol.TypeRun, ID: "r1", Repo: remote, Root: root, Cmd: []string{"true"}})
	if res, _ := result(t, pc, "r1"); !res.OK {
		t.Fatalf("run by source: %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypeRun, ID: "r2", Repo: "/nowhere/other.git", Root: root, Cmd: []string{"true"}})
	if res, _ := result(t, pc, "r2"); res.OK || !strings.Contains(res.Error, "is a worktree of proj, not") {
		t.Fatalf("run for another source: %+v", res)
	}
}

// A daemon shutting down while a run resolves its worktree ends it as
// cancelled, the outcome a cancel after the start has, not as git's
// context error.
func TestRunCancelledWhileResolving(t *testing.T) {
	d, _, _, remote := newAddDaemon(t)
	pc := conn(t, d)
	root := addWorktree(t, pc, remote, "task")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newCommand("r1")
	c.job = newRunJob()
	d.runRun(ctx, protocol.Message{Type: protocol.TypeRun, ID: "r1", Root: root, Cmd: []string{"true"}}, c)
	if c.result.OK || c.result.Error != protocol.ErrCancelled {
		t.Fatalf("result %+v", c.result)
	}
}

// cancel ends a run with a cancelled result: SIGTERM to the process
// group, and SIGKILL after the kill delay for a process that ignores
// it. A cancel for a finished run or an unknown id does nothing.
func TestRunCancel(t *testing.T) {
	d, _, _, remote := newAddDaemon(t)
	d.killDelay = 200 * time.Millisecond
	pc := conn(t, d)
	root := addWorktree(t, pc, remote, "task")
	for _, c := range []struct {
		id, script string
	}{
		{"r1", "sleep 30"},
		{"r2", "trap '' TERM; echo ready; sleep 30"},
	} {
		pc.Write(protocol.Message{Type: protocol.TypeRun, ID: c.id, Root: root, Cmd: []string{"sh", "-c", c.script}})
		// Wait for the start before cancelling, so the process is up.
		if m, err := pc.Read(); err != nil || m.State != protocol.StateStart {
			t.Fatalf("%s: first message %+v %v", c.id, m, err)
		}
		time.Sleep(100 * time.Millisecond)
		start := time.Now()
		pc.Write(protocol.Message{Type: protocol.TypeCancel, ID: c.id})
		res, _ := result(t, pc, c.id)
		if res.OK || res.Error != protocol.ErrCancelled {
			t.Fatalf("%s: %+v", c.id, res)
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("%s: cancel took %s", c.id, took)
		}
	}
	pc.Write(protocol.Message{Type: protocol.TypeCancel, ID: "r1"})
	pc.Write(protocol.Message{Type: protocol.TypeCancel, ID: "nope"})
	pc.Write(protocol.Message{Type: protocol.TypePing})
	if m, err := pc.Read(); err != nil || m.Type != protocol.TypePong {
		t.Fatalf("after stray cancels: %+v %v", m, err)
	}
	d.mu.Lock()
	n := len(d.runs)
	d.mu.Unlock()
	if n != 0 {
		t.Fatalf("runs still registered: %d", n)
	}
}

// rm cancels the runs in the root once git has removed the worktree and
// waits for them, so its ok means nothing of laatmux's is left there.
// A run that resolved before the removal is refused at registration,
// whether or not a worktree is back at the root.
func TestRmCancelsRuns(t *testing.T) {
	d, _, _, remote := newAddDaemon(t)
	pc := conn(t, d)
	root := addWorktree(t, pc, remote, "task")
	pc.Write(protocol.Message{Type: protocol.TypeRun, ID: "r1", Root: root, Cmd: []string{"sh", "-c", "sleep 30"}})
	if m, err := pc.Read(); err != nil || m.State != protocol.StateStart {
		t.Fatalf("first message %+v %v", m, err)
	}
	other := conn(t, d)
	other.Write(protocol.Message{Type: protocol.TypeRm, ID: "rm1", Repo: remote, Branch: "task", Root: root, Force: true})
	if res, _ := result(t, other, "rm1"); !res.OK {
		t.Fatalf("rm: %+v", res)
	}
	if res, _ := result(t, pc, "r1"); res.OK || res.Error != protocol.ErrCancelled {
		t.Fatalf("run after rm: %+v", res)
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("root still exists")
	}
	// The interlock: a run registered under the generation from before
	// the removal is refused, one that reads it afresh is not.
	stale := newRunJob()
	stale.root = root
	if err := d.registerRun(stale, 0); err == nil || !strings.Contains(err.Error(), "worktree removed") {
		t.Fatalf("stale registration: %v", err)
	}
	r := newRunJob()
	r.root = root
	if err := d.registerRun(r, d.runGen(root)); err != nil {
		t.Fatalf("fresh registration: %v", err)
	}
	d.unregisterRun(r)
}

// A cancel that lands after registration and before the start means
// the process never starts.
func TestRunCancelledBeforeStart(t *testing.T) {
	d, _, _, remote := newAddDaemon(t)
	pc := conn(t, d)
	root := addWorktree(t, pc, remote, "task")
	r := newRunJob()
	r.root = root
	r.requestCancel()
	marker := root + "/started"
	if _, err := d.runProcess(context.Background(), r, []string{"touch", marker}, func(int, string) {}); err == nil || err.Error() != protocol.ErrCancelled {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("process started after cancel")
	}
	// The daemon's own context ending before the start is the same.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.runProcess(ctx, newRunJob(), []string{"touch", marker}, func(int, string) {}); err == nil || err.Error() != protocol.ErrCancelled {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("process started after shutdown")
	}
	// A cancel sent on the heels of the run, before the daemon has
	// started anything, finds the job: the result is cancelled and no
	// process is left.
	pc.Write(protocol.Message{Type: protocol.TypeRun, ID: "r1", Root: root, Cmd: []string{"sh", "-c", "sleep 30; touch " + marker}})
	pc.Write(protocol.Message{Type: protocol.TypeCancel, ID: "r1"})
	if res, _ := result(t, pc, "r1"); res.OK || res.Error != protocol.ErrCancelled {
		t.Fatalf("run then cancel: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("process ran on after cancel")
	}
}

// A cancelled run is over only when its whole process group is: a
// parent that dies on SIGTERM leaves a child that ignores it, and the
// child gets SIGKILL after the delay, before the result.
func TestRunCancelKillsGroup(t *testing.T) {
	d, _, _, remote := newAddDaemon(t)
	d.killDelay = 300 * time.Millisecond
	pc := conn(t, d)
	root := addWorktree(t, pc, remote, "task")
	token := fmt.Sprintf("laatmux-run-test-%d", os.Getpid())
	// The token is the inner shell's $0, so it is on its command line.
	script := fmt.Sprintf(`sh -c 'trap "" TERM; echo ready; sleep 30' %s & wait`, token)
	pc.Write(protocol.Message{Type: protocol.TypeRun, ID: "r1", Root: root, Cmd: []string{"sh", "-c", script}})
	for {
		m, err := pc.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.State == protocol.StateOutput && m.Detail == "ready" {
			break
		}
	}
	pc.Write(protocol.Message{Type: protocol.TypeCancel, ID: "r1"})
	if res, _ := result(t, pc, "r1"); res.OK || res.Error != protocol.ErrCancelled {
		t.Fatalf("result %+v", res)
	}
	if out, _ := exec.Command("pgrep", "-f", token).Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("group member survived the cancel: pids %s", out)
	}
}

// A process that exits while a child of its holds the pipes ends the
// run with the process's status after the wait delay, not as a failure,
// and the child, laatmux's own, is stopped before the result rather
// than left for no rm or shutdown to find.
func TestRunExitWithPipesHeld(t *testing.T) {
	d, _, _, remote := newAddDaemon(t)
	d.killDelay = 300 * time.Millisecond
	pc := conn(t, d)
	root := addWorktree(t, pc, remote, "task")
	token := fmt.Sprintf("laatmux-run-bg-%d", os.Getpid())
	// The ":" keeps the inner shell from replacing itself with sleep,
	// so the token stays on a command line.
	script := fmt.Sprintf(`sh -c 'trap "" TERM; sleep 30; :' %s & echo bg; exit 2`, token)
	pc.Write(protocol.Message{Type: protocol.TypeRun, ID: "r1", Root: root, Cmd: []string{"sh", "-c", script}})
	res, progress := result(t, pc, "r1")
	if !res.OK || res.Exit != 2 || !hasProgress(progress, protocol.StageRun, protocol.StateOutput, "bg") {
		t.Fatalf("result %+v progress %+v", res, progress)
	}
	if out, _ := exec.Command("pgrep", "-f", token).Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("background child survived the result: pids %s", out)
	}
}

// A follower whose connection ends while the command is quiet is let go
// at once, not held until the next event.
func TestFollowerReleasedOnDisconnect(t *testing.T) {
	d, _, _, remote := newAddDaemon(t)
	pc := conn(t, d)
	root := addWorktree(t, pc, remote, "task")
	pc.Write(protocol.Message{Type: protocol.TypeRun, ID: "r1", Root: root, Cmd: []string{"sh", "-c", "echo up; sleep 30"}})
	for {
		m, err := pc.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.State == protocol.StateOutput {
			break
		}
	}
	c, _ := d.lookup("r1")
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go d.HandleConn(ctx, server, func() { server.Close() })
	second := protocol.NewConn(client)
	second.Read() // hello
	second.Write(protocol.Message{Type: protocol.TypeFollow, ID: "r1", After: 0})
	if m, err := second.Read(); err != nil || m.State != protocol.StateStart {
		t.Fatalf("follow: %+v %v", m, err)
	}
	if m, err := second.Read(); err != nil || m.Detail != "up" {
		t.Fatalf("follow: %+v %v", m, err)
	}
	followers := func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.followers
	}
	// Two streams, each with its waker.
	if n := followers(); n != 4 {
		t.Fatalf("%d followers, want 4", n)
	}
	client.Close()
	cancel()
	for deadline := time.Now().Add(3 * time.Second); followers() != 2; {
		if time.Now().After(deadline) {
			t.Fatalf("%d followers after the disconnect, want 2", followers())
		}
		time.Sleep(10 * time.Millisecond)
	}
	pc.Write(protocol.Message{Type: protocol.TypeCancel, ID: "r1"})
	if res, _ := result(t, pc, "r1"); res.Error != protocol.ErrCancelled {
		t.Fatalf("result %+v", res)
	}
	// The result ends the stream and its waker, with the connection
	// still open: a connection that sends many commands holds nothing
	// of the finished ones.
	for deadline := time.Now().Add(3 * time.Second); followers() != 0; {
		if time.Now().After(deadline) {
			t.Fatalf("%d followers after the result, want 0", followers())
		}
		time.Sleep(10 * time.Millisecond)
	}
	pc.Write(protocol.Message{Type: protocol.TypePing})
	if m, err := pc.Read(); err != nil || m.Type != protocol.TypePong {
		t.Fatalf("connection after the result: %+v %v", m, err)
	}
}

// Once StopRuns has begun, a run that resolved before it cannot
// register, so nothing starts after the shutdown took its list.
func TestStopRunsClosesRegistry(t *testing.T) {
	d, _, _, _ := newAddDaemon(t)
	d.StopRuns(context.Background())
	r := newRunJob()
	r.root = "/r"
	if err := d.registerRun(r, 0); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("registration after stop: %v", err)
	}
}

// Empty and short lines are charged for the message around them, so a
// run of them is bounded like long lines are.
func TestRunRingBoundsEmptyLines(t *testing.T) {
	c := newCommand("x")
	c.ring = true
	for i := 0; i < 4*maxOutput/eventCost; i++ {
		c.emit(protocol.Message{Type: protocol.TypeProgress, Stage: protocol.StageRun, State: protocol.StateOutput, FD: 1})
	}
	if len(c.events) > maxOutput/eventCost || c.outBytes > maxOutput {
		t.Fatalf("retained %d events, %d bytes", len(c.events), c.outBytes)
	}
}

// StopRuns, what a clean shutdown does, cancels every run and returns
// when they have finished.
func TestStopRuns(t *testing.T) {
	d, _, _, remote := newAddDaemon(t)
	pc := conn(t, d)
	root := addWorktree(t, pc, remote, "task")
	pc.Write(protocol.Message{Type: protocol.TypeRun, ID: "r1", Root: root, Cmd: []string{"sleep", "30"}})
	if m, err := pc.Read(); err != nil || m.State != protocol.StateStart {
		t.Fatalf("first message %+v %v", m, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d.StopRuns(ctx)
	if ctx.Err() != nil {
		t.Fatal("StopRuns timed out")
	}
	if res, _ := result(t, pc, "r1"); res.OK || res.Error != protocol.ErrCancelled {
		t.Fatalf("run after stop: %+v", res)
	}
}

// A run retains a bounded tail for replay and forgets its oldest lines:
// a follower behind the tail gets one gap numbered as the last dropped
// line, then the tail; a live follower gets every line as it comes.
func TestRunRingReplay(t *testing.T) {
	c := newCommand("x")
	c.ring = true
	server, client := net.Pipe()
	live := protocol.NewConn(client)
	go c.stream(protocol.NewConn(server), 0, nil)
	line := strings.Repeat("x", 1024)
	total := 2*maxOutput/len(line) + 1
	// The live follower, reading as fast as the lines come, sees every
	// one in order and never a gap.
	for i := 1; i <= total; i++ {
		m := protocol.Message{Type: protocol.TypeProgress, Stage: protocol.StageRun, State: protocol.StateOutput, FD: 1, Detail: line}
		if i == 1 {
			m = protocol.Message{Type: protocol.TypeProgress, Stage: protocol.StageRun, State: protocol.StateStart, Detail: "/r"}
		}
		c.emit(m)
		got, err := live.Read()
		if err != nil || got.N != uint64(i) || got.State == protocol.StateGap {
			t.Fatalf("live follower got %+v %v as event %d", got, err, i)
		}
	}
	c.emit(protocol.Message{Type: protocol.TypeResult, OK: true})
	if m, err := live.Read(); err != nil || m.Type != protocol.TypeResult {
		t.Fatalf("live result %+v %v", m, err)
	}
	client.Close()
	// The retained tail is under the budget and the start is gone.
	if len(c.events) >= total || c.events[0].State != protocol.StateOutput || c.outBytes > maxOutput {
		t.Fatalf("retained %d events, first %s, %d bytes", len(c.events), c.events[0].State, c.outBytes)
	}
	base := c.events[0].N
	// A follow from before the tail: a gap for base-1-after lines, then
	// the tail, then the result.
	for _, after := range []uint64{0, base - 5} {
		server, client := net.Pipe()
		go c.stream(protocol.NewConn(server), after, nil)
		pc := protocol.NewConn(client)
		m, _ := pc.Read()
		if m.State != protocol.StateGap || m.N != base-1 || m.Detail != fmt.Sprintf("%d lines dropped", base-1-after) || m.ID != "x" {
			t.Fatalf("after %d: gap %+v", after, m)
		}
		for want := base; ; want++ {
			m, err := pc.Read()
			if err != nil {
				t.Fatal(err)
			}
			if m.Type == protocol.TypeResult {
				if want != c.events[len(c.events)-1].N+1 {
					t.Fatalf("after %d: result at %d", after, want)
				}
				break
			}
			if m.N != want {
				t.Fatalf("after %d: got %d want %d", after, m.N, want)
			}
		}
		client.Close()
	}
	// A follow from inside the tail gets no gap.
	server, client = net.Pipe()
	go c.stream(protocol.NewConn(server), base+2, nil)
	pc := protocol.NewConn(client)
	if m, _ := pc.Read(); m.N != base+3 || m.State != protocol.StateOutput {
		t.Fatalf("inside the tail: %+v", m)
	}
	client.Close()
}
