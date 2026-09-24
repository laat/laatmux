package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/worktree"
)

// DefaultKillDelay is how long a cancelled run gets between SIGTERM and
// SIGKILL.
const DefaultKillDelay = 5 * time.Second

// run is one run command: a process in a worktree root, registered by
// root so rm can stop it. It is cancellable from registration on; a
// cancel before the process has started means it never starts.
type runJob struct {
	root string
	mu   sync.Mutex // holds the start, so a cancel lands before or after it, never during
	// cancelled is set by the first cancel, under mu; cancel is closed
	// with it and wakes the goroutine waiting on the process.
	cancelled bool
	cancel    chan struct{}
	done      chan struct{} // closed when the result has been recorded
}

func newRunJob() *runJob {
	return &runJob{cancel: make(chan struct{}), done: make(chan struct{})}
}

// requestCancel marks the run cancelled and wakes it. Idempotent.
func (r *runJob) requestCancel() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelled {
		return
	}
	r.cancelled = true
	close(r.cancel)
}

// runGen is the removal generation of a root: bumped by rm once git has
// removed the worktree, so a run that resolved before the removal cannot
// register after it.
func (d *Daemon) runGen(root string) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rootGen[root]
}

// registerRun adds r to the root's runs when the generation it resolved
// under still holds. A bumped generation means the worktree the request
// named was removed meanwhile, whether or not another has been made at
// the same root since: the request was for the old one. Nothing
// registers once the daemon is stopping, so a run that resolved while
// StopRuns took its list cannot start after it.
func (d *Daemon) registerRun(r *runJob, gen uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping {
		return errors.New("daemon shutting down")
	}
	if d.rootGen[r.root] != gen {
		return errors.New("worktree removed; retry")
	}
	if d.runs[r.root] == nil {
		d.runs[r.root] = map[*runJob]struct{}{}
	}
	d.runs[r.root][r] = struct{}{}
	return nil
}

func (d *Daemon) unregisterRun(r *runJob) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rs := d.runs[r.root]; rs != nil {
		delete(rs, r)
		if len(rs) == 0 {
			delete(d.runs, r.root)
		}
	}
}

// cancelRunsIn bumps the root's generation and stops every run in it,
// waiting for each to finish. Called by rm once git has removed the
// worktree; the mutex is held only to take the list.
func (d *Daemon) cancelRunsIn(root string) {
	d.mu.Lock()
	d.rootGen[root]++
	var rs []*runJob
	for r := range d.runs[root] {
		rs = append(rs, r)
	}
	d.mu.Unlock()
	for _, r := range rs {
		r.requestCancel()
		<-r.done
	}
}

// cancelCommand stops the run under id, if there is one and it has not
// finished. A cancel for an add, an rm or an unknown id does nothing.
func (d *Daemon) cancelCommand(id string) {
	c, ok := d.lookup(id)
	if !ok || c.job == nil {
		return
	}
	c.job.requestCancel()
}

// StopRuns closes the registry, cancels every run and waits for them,
// and waits for every paste in flight, all bounded by ctx: what a
// clean shutdown does, so a restart for an upgrade leaves no orphan
// and no prompt in a buffer on the server.
func (d *Daemon) StopRuns(ctx context.Context) {
	d.mu.Lock()
	d.stopping = true
	var rs []*runJob
	for _, m := range d.runs {
		for r := range m {
			rs = append(rs, r)
		}
	}
	d.mu.Unlock()
	for _, r := range rs {
		r.requestCancel()
	}
	for _, r := range rs {
		select {
		case <-r.done:
		case <-ctx.Done():
			return
		}
	}
	for {
		d.mu.Lock()
		n := d.pasting
		d.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// runRun runs a command in a worktree root and records the result on c.
// The root must be a registered worktree of a known repository under the
// worktrees directory, and the repository and branch, when given, must be
// the root's. The process runs in its own process group, with stdin at
// /dev/null and no tty, and its output is streamed one line per message
// with the stream it came from. A run takes no repository lock: it does
// not touch the main checkout, and a long one must not block add.
func (d *Daemon) runRun(ctx context.Context, m protocol.Message, c *command) {
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	r := c.job
	defer close(r.done)
	err := func() error {
		if len(m.Cmd) == 0 {
			return errors.New("run needs a command")
		}
		if m.Root == "" {
			return errors.New("run needs the worktree root")
		}
		root := filepath.Clean(m.Root)
		if !d.cfg.Store.Owns(root) {
			return fmt.Errorf("%s is not under the worktrees directory %s", root, d.cfg.Store.Dirs.Worktrees)
		}
		r.root = root
		// The generation is read before git is asked, so a removal
		// between the two is seen at registration.
		gen := d.runGen(root)
		rec, _, found, err := d.cfg.Store.Find(ctx, root)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%s is not a worktree of a known repository", root)
		}
		if m.Repo != "" && rec.Source != m.Repo {
			// The client sends the source; a source that is not the
			// record's may still be a label, resolved as add resolves
			// it. A bare source equal to another entry's label is the
			// record's own source first, so it is never taken for the
			// other entry.
			repo, ok := d.cfg.Store.Repo(m.Repo)
			if !ok {
				return fmt.Errorf("unknown repository %q: not in this host's config", m.Repo)
			}
			if rec.Source != repo.Source {
				return fmt.Errorf("%s is a worktree of %s, not %s", root, rec.Repo, repo.Name)
			}
		}
		if m.Branch != "" && rec.Branch != m.Branch {
			return fmt.Errorf("%s is the worktree for %s, not %s", root, branchOrDetached(rec.Branch), m.Branch)
		}
		if err := d.registerRun(r, gen); err != nil {
			return err
		}
		defer d.unregisterRun(r)
		c.emit(protocol.Message{Type: protocol.TypeProgress, ID: m.ID, Stage: protocol.StageRun, State: protocol.StateStart, Detail: root})
		exit, err := d.runProcess(ctx, r, m.Cmd, func(fd int, line string) {
			c.emit(protocol.Message{Type: protocol.TypeProgress, ID: m.ID, Stage: protocol.StageRun, State: protocol.StateOutput, FD: fd, Detail: line})
		})
		if err != nil {
			return err
		}
		res.Exit = exit
		return nil
	}()
	switch {
	case err != nil && ctx.Err() != nil:
		// The daemon shut down while the run was resolving: git's
		// context error is the same outcome as a cancel after the start.
		res.Error = protocol.ErrCancelled
	case err != nil:
		res.Error = err.Error()
	default:
		res.OK = true
	}
	c.emit(res)
	d.evict(res.ID, c)
}

// runProcess starts the command in the run's root and waits for it,
// streaming each output line to out with its fd. A cancel, from the
// client, from rm or from the daemon shutting down, sends SIGTERM to the
// process group, SIGKILL after the kill delay, and makes the outcome
// cancelled whatever the process then exits with. The exit status is the
// process's; a process killed by a signal reports 128 plus the signal, as
// a shell would.
func (d *Daemon) runProcess(ctx context.Context, r *runJob, argv []string, out func(fd int, line string)) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = r.root
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A child that exits but leaves a grandchild holding the pipes must
	// not hold the result; past the delay the pipes are closed.
	cmd.WaitDelay = time.Second
	pipes := [2]*io.PipeReader{}
	for i := range pipes {
		pr, pw := io.Pipe()
		pipes[i] = pr
		if i == 0 {
			cmd.Stdout = pw
		} else {
			cmd.Stderr = pw
		}
	}
	r.mu.Lock()
	if r.cancelled || ctx.Err() != nil {
		r.mu.Unlock()
		return 0, errors.New(protocol.ErrCancelled)
	}
	err := cmd.Start()
	r.mu.Unlock()
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for i, pr := range pipes {
		wg.Add(1)
		go func(fd int, pr *io.PipeReader) {
			defer wg.Done()
			streamLines(pr, func(line string) { out(fd, line) })
		}(i+1, pr)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	pgid := cmd.Process.Pid
	var werr error
	cancelled := false
	select {
	case werr = <-waited:
		// The process is done; what it left in its group is laatmux's
		// own, in a root rm may remove next, and goes with it. Its exit
		// status stands.
		werr = d.terminate(pgid, waited, werr, true)
	case <-r.cancel:
		cancelled = true
		werr = d.terminate(pgid, waited, nil, false)
	case <-ctx.Done():
		cancelled = true
		r.requestCancel()
		werr = d.terminate(pgid, waited, nil, false)
	}
	// Wait closes the pipe writers once the copying is done or the
	// delay has passed, which ends the readers.
	for _, pr := range pipes {
		pr.Close()
	}
	wg.Wait()
	if cancelled {
		return 0, errors.New(protocol.ErrCancelled)
	}
	if werr != nil {
		// The process exited but a child of its kept the pipes past the
		// delay: the exit status is the process's own.
		var ee *exec.ExitError
		if !errors.As(werr, &ee) && !errors.Is(werr, exec.ErrWaitDelay) {
			return 0, werr
		}
	}
	return exitStatus(cmd.ProcessState), nil
}

// terminate empties the process group: SIGTERM, then SIGKILL after the
// kill delay unless every member has gone by then. The leader exiting
// does not end the group, since a descendant that ignores the signal
// survives its parent, so the group is watched, not the child, and the
// return waits for the group to be empty, bounded, so a result and an
// rm that waited for it mean nothing is left. exited says whether Wait
// has returned already, with werr; otherwise it is read from waited.
// Returns what Wait said. A group with nothing left in it returns at
// once.
func (d *Daemon) terminate(pgid int, waited <-chan error, werr error, exited bool) error {
	if exited && !groupAlive(pgid) {
		return werr
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(d.killDelay)
	for (!exited || groupAlive(pgid)) && time.Now().Before(deadline) {
		if exited {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		select {
		case werr = <-waited:
			exited = true
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !exited || groupAlive(pgid) {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		if !exited {
			werr = <-waited
		}
		for i := 0; i < 100 && groupAlive(pgid); i++ {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return werr
}

// groupAlive reports whether any process is left in the group. A group
// the daemon may not signal still exists.
func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// exitStatus is the process's exit code, or 128 plus the signal that
// killed it.
func exitStatus(ps *os.ProcessState) int {
	if ps == nil {
		return 0
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// streamLines is the worktree package's line splitter, shared so a run's
// output is bounded per line the same way setup output is.
func streamLines(r io.Reader, fn func(string)) { worktree.StreamLines(r, fn) }
