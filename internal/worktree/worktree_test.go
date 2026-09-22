package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
)

// fixture is a bare "remote" with one commit on main, a .laatmux.yaml that
// copies .envrc and runs two setup commands, and a store whose repos and
// worktrees directories are empty.
type fixture struct {
	t      *testing.T
	remote string
	store  *Store
	repo   Repo
	ctx    context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	if runtime.GOOS == "darwin" {
		// /var is a symlink to /private/var; git registers real paths.
		if real, err := filepath.EvalSymlinks(base); err == nil {
			base = real
		}
	}
	remote := filepath.Join(base, "remote.git")
	seed := filepath.Join(base, "seed")
	run(t, base, "git", "init", "-q", "--bare", "--initial-branch=main", remote)
	run(t, base, "git", "init", "-q", "--initial-branch=main", seed)
	run(t, seed, "git", "config", "user.email", "t@example.com")
	run(t, seed, "git", "config", "user.name", "t")
	write(t, filepath.Join(seed, "README"), "hello\n")
	write(t, filepath.Join(seed, config.SetupFile), "copy: [.envrc, missing.txt]\nsetup: [\"echo one >> log\", \"echo two >> log\"]\n")
	run(t, seed, "git", "add", ".")
	run(t, seed, "git", "commit", "-q", "-m", "init")
	run(t, seed, "git", "push", "-q", remote, "main")
	dirs := config.Dirs{Repos: filepath.Join(base, "repos"), Worktrees: filepath.Join(base, "worktrees")}
	store := New(dirs, []config.Repo{{Source: remote, Name: "proj"}})
	return &fixture{t: t, remote: remote, store: store, repo: store.Repos[0], ctx: context.Background()}
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type step struct{ stage, state, detail string }

func (f *fixture) add(branch string) (Added, []step, error) {
	var steps []step
	a, err := f.store.Add(f.ctx, f.repo, branch, func(stage, state, detail string) {
		if state != protocol.StateOutput {
			steps = append(steps, step{stage, state, detail})
		}
	})
	return a, steps, err
}

func (f *fixture) checkout() string {
	c, ok, err := f.store.Checkout(f.ctx, f.repo)
	if err != nil || !ok {
		f.t.Fatalf("checkout: %v %v", ok, err)
	}
	return c
}

func hasStep(steps []step, stage, state, detailPrefix string) bool {
	for _, s := range steps {
		if s.stage == stage && s.state == state && strings.HasPrefix(s.detail, detailPrefix) {
			return true
		}
	}
	return false
}

func stageOf(t *testing.T, err error) string {
	t.Helper()
	var se *StageError
	if !errors.As(err, &se) {
		t.Fatalf("not a stage error: %v", err)
	}
	return se.Stage
}

func TestAddFromNothing(t *testing.T) {
	f := newFixture(t)
	// The main checkout is placed at <repos>/<name>, and the first add
	// clones it. .envrc lives only in the checkout, not in git.
	a, steps, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	if a.Checkout != f.store.Dirs.Checkout("proj") {
		t.Fatalf("checkout %s", a.Checkout)
	}
	if a.Root != f.store.Dirs.Worktree("proj", "task") {
		t.Fatalf("root %s", a.Root)
	}
	for _, want := range []step{
		{protocol.StageClone, protocol.StateDone, "cloned"},
		{protocol.StageFetch, protocol.StateDone, "fetched"},
		{protocol.StageWorktree, protocol.StateDone, "branch task from origin/HEAD"},
		{protocol.StageWorktree, protocol.StateDone, "worktree at " + a.Root},
		{protocol.StageCopy, protocol.StateSkip, ".envrc not in " + a.Checkout},
		{protocol.StageCopy, protocol.StateSkip, "missing.txt not in"},
		{protocol.StageSetup, protocol.StateDone, "echo one >> log"},
		{protocol.StageSetup, protocol.StateDone, "echo two >> log"},
	} {
		if !hasStep(steps, want.stage, want.state, want.detail) {
			t.Errorf("missing step %+v in %+v", want, steps)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(a.Root, "log")); string(b) != "one\ntwo\n" {
		t.Fatalf("setup log %q", b)
	}
	// The new branch has no upstream: push must not target main.
	if out, err := exec.Command("git", "-C", a.Root, "rev-parse", "--abbrev-ref", "task@{upstream}").CombinedOutput(); err == nil {
		t.Fatalf("task has upstream %s", out)
	}
	recs, err := f.store.List(f.ctx)
	if err != nil || len(recs) != 1 || recs[0].Branch != "task" || recs[0].Root != a.Root || recs[0].Repo != "proj" {
		t.Fatalf("list: %+v %v", recs, err)
	}
}

// A second add of the same branch skips every step: the note's "two adds
// for the same workspace" and "retry after a dropped bridge".
func TestAddAgainSkipsEverything(t *testing.T) {
	f := newFixture(t)
	first, _, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	write(f.t, filepath.Join(first.Checkout, ".envrc"), "export A=1\n")
	again, steps, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	if again.Root != first.Root {
		t.Fatalf("root changed: %s -> %s", first.Root, again.Root)
	}
	for _, want := range []step{
		{protocol.StageClone, protocol.StateSkip, "checkout exists"},
		{protocol.StageWorktree, protocol.StateSkip, "branch task exists"},
		{protocol.StageWorktree, protocol.StateSkip, "worktree registered at " + first.Root},
		{protocol.StageSetup, protocol.StateSkip, "echo one >> log (done before)"},
		{protocol.StageSetup, protocol.StateSkip, "echo two >> log (done before)"},
	} {
		if !hasStep(steps, want.stage, want.state, want.detail) {
			t.Errorf("missing step %+v in %+v", want, steps)
		}
	}
	// .envrc appeared in the checkout between the two adds: copied now,
	// since the target did not exist. Setup did not rerun.
	if b, _ := os.ReadFile(filepath.Join(again.Root, ".envrc")); string(b) != "export A=1\n" {
		t.Fatalf(".envrc %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(again.Root, "log")); string(b) != "one\ntwo\n" {
		t.Fatalf("setup reran: %q", b)
	}
}

// The checkout is found by origin, not by directory name: a clone made by
// hand under another name is used, and no second clone is made.
func TestCheckoutFoundByOrigin(t *testing.T) {
	f := newFixture(t)
	other := filepath.Join(f.store.Dirs.Repos, "elsewhere")
	run(t, "", "git", "clone", "-q", f.remote, other)
	// A directory with the label's name but a different origin must not be
	// mistaken for the checkout, and must not be cloned over.
	decoy := f.store.Dirs.Checkout("proj")
	run(t, f.store.Dirs.Repos, "git", "init", "-q", decoy)
	run(t, decoy, "git", "remote", "add", "origin", "https://example.com/x.git")
	a, steps, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	if a.Checkout != other {
		t.Fatalf("checkout %s, want %s", a.Checkout, other)
	}
	if !hasStep(steps, protocol.StageClone, protocol.StateSkip, "checkout exists") {
		t.Fatalf("steps %+v", steps)
	}
}

func TestCloneRefusesForeignDirectory(t *testing.T) {
	f := newFixture(t)
	decoy := f.store.Dirs.Checkout("proj")
	run(t, "", "git", "init", "-q", decoy)
	run(t, decoy, "git", "remote", "add", "origin", "https://example.com/x.git")
	_, _, err := f.add("task")
	if stageOf(t, err) != protocol.StageClone || !strings.Contains(err.Error(), "origin https://example.com/x.git") {
		t.Fatalf("err %v", err)
	}
	write(t, filepath.Join(decoy, "x"), "")
	os.RemoveAll(filepath.Join(decoy, ".git"))
	_, _, err = f.add("task")
	if stageOf(t, err) != protocol.StageClone || !strings.Contains(err.Error(), "not a checkout") {
		t.Fatalf("err %v", err)
	}
}

// A remote branch is tracked; a local branch made by hand is used as is; a
// branch checked out elsewhere, or in the main checkout, fails the stage.
func TestBranchCases(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.add("first"); err != nil {
		t.Fatal(err)
	}
	c := f.checkout()
	// remote branch
	run(t, c, "git", "push", "-q", "origin", "main:refs/heads/remote-only")
	a, steps, err := f.add("remote-only")
	if err != nil {
		t.Fatal(err)
	}
	if !hasStep(steps, protocol.StageWorktree, protocol.StateDone, "branch remote-only tracks origin/remote-only") {
		t.Fatalf("steps %+v", steps)
	}
	if up := strings.TrimSpace(run(t, a.Root, "git", "rev-parse", "--abbrev-ref", "remote-only@{upstream}")); up != "origin/remote-only" {
		t.Fatalf("upstream %s", up)
	}
	// local branch made by hand, from an earlier crashed attempt
	run(t, c, "git", "branch", "by-hand", "main")
	_, steps, err = f.add("by-hand")
	if err != nil {
		t.Fatal(err)
	}
	if !hasStep(steps, protocol.StageWorktree, protocol.StateSkip, "branch by-hand exists, used as is") {
		t.Fatalf("steps %+v", steps)
	}
	// checked out in the main checkout
	_, _, err = f.add("main")
	if stageOf(t, err) != protocol.StageWorktree || !strings.Contains(err.Error(), "main checkout") {
		t.Fatalf("err %v", err)
	}
	// checked out at a worktree outside the worktrees directory
	elsewhere := filepath.Join(filepath.Dir(f.store.Dirs.Repos), "elsewhere")
	run(t, c, "git", "worktree", "add", "-q", "-b", "outside", elsewhere, "main")
	_, _, err = f.add("outside")
	if stageOf(t, err) != protocol.StageWorktree || !strings.Contains(err.Error(), "checked out at "+elsewhere) {
		t.Fatalf("err %v", err)
	}
	// the root taken by a worktree on another branch
	run(t, c, "git", "worktree", "add", "-q", "-b", "squatter", f.store.Dirs.Worktree("proj", "wanted"), "main")
	_, _, err = f.add("wanted")
	if stageOf(t, err) != protocol.StageWorktree || !strings.Contains(err.Error(), "not wanted") {
		t.Fatalf("err %v", err)
	}
	// bad names never reach git
	for _, bad := range []string{"", "-x", "a..b", "x/", "a b"} {
		if _, _, err := f.add(bad); err == nil || stageOf(t, err) != protocol.StageResolve {
			t.Errorf("branch %q: %v", bad, err)
		}
	}
}

// A worktree whose directory was deleted outside git is prunable: not
// listed, and a repeat add prunes it and makes the directory again.
func TestPrunableWorktree(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(a.Root)
	if recs, err := f.store.List(f.ctx); err != nil || len(recs) != 0 {
		t.Fatalf("list after delete: %+v %v", recs, err)
	}
	again, steps, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	if again.Root != a.Root || !hasStep(steps, protocol.StageWorktree, protocol.StateDone, "worktree at "+a.Root) {
		t.Fatalf("root %s steps %+v", again.Root, steps)
	}
	if b, _ := os.ReadFile(filepath.Join(again.Root, "log")); string(b) != "one\ntwo\n" {
		t.Fatalf("setup after prune %q", b)
	}
}

// A crash halfway through a copy leaves the temporary file, not the
// target; the retry finishes the copy. A crash after a setup command's
// effects but before its marker reruns the command.
func TestCrashedCopyAndSetup(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(a.Checkout, ".envrc"), "export A=1\n")
	write(t, filepath.Join(a.Root, ".laatmux-copy-.envrc"), "export A=")
	markers, err := markerDir(f.ctx, a.Root)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(markers, "setup-1-"+hash("echo two >> log")))
	_, steps, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(a.Root, ".envrc")); string(b) != "export A=1\n" {
		t.Fatalf(".envrc %q", b)
	}
	if _, err := os.Stat(filepath.Join(a.Root, ".laatmux-copy-.envrc")); err == nil {
		t.Fatal("temporary copy left behind")
	}
	if !hasStep(steps, protocol.StageSetup, protocol.StateSkip, "echo one >> log") || !hasStep(steps, protocol.StageSetup, protocol.StateDone, "echo two >> log") {
		t.Fatalf("steps %+v", steps)
	}
	if b, _ := os.ReadFile(filepath.Join(a.Root, "log")); string(b) != "one\ntwo\ntwo\n" {
		t.Fatalf("log %q", b)
	}
}

// A failing setup command fails the stage with its output, leaves the
// worktree, and does not write its marker; a changed command has a new
// marker and runs.
func TestSetupFailureAndChange(t *testing.T) {
	f := newFixture(t)
	seedSetup := func(content string) {
		c := f.checkout()
		write(t, filepath.Join(c, config.SetupFile), content)
		run(t, c, "git", "add", config.SetupFile)
		run(t, c, "git", "commit", "-q", "-m", "setup")
		run(t, c, "git", "push", "-q", "origin", "main")
	}
	if _, _, err := f.add("first"); err != nil {
		t.Fatal(err)
	}
	seedSetup("setup: [\"echo ok >> log\", \"echo boom >&2; exit 3\"]\n")
	_, _, err := f.add("task")
	if stageOf(t, err) != protocol.StageSetup || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err %v", err)
	}
	root := f.store.Dirs.Worktree("proj", "task")
	if _, err := os.Stat(root); err != nil {
		t.Fatal("worktree removed after setup failure")
	}
	// Fix the command on the branch itself: the worktree's own file is read.
	write(t, filepath.Join(root, config.SetupFile), "setup: [\"echo ok >> log\", \"echo fixed >> log\"]\n")
	_, steps, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	if !hasStep(steps, protocol.StageSetup, protocol.StateSkip, "echo ok >> log") || !hasStep(steps, protocol.StageSetup, protocol.StateDone, "echo fixed >> log") {
		t.Fatalf("steps %+v", steps)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "log")); string(b) != "ok\nfixed\n" {
		t.Fatalf("log %q", b)
	}
}

func TestListAndFindAndRemove(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("feature/x")
	if err != nil {
		t.Fatal(err)
	}
	c := f.checkout()
	// A detached worktree made by hand under the worktrees directory is
	// listed with an empty branch; one outside the directory is not.
	detached := f.store.Dirs.Worktree("proj", "detached")
	run(t, c, "git", "worktree", "add", "-q", "--detach", detached)
	run(t, c, "git", "worktree", "add", "-q", "--detach", filepath.Join(filepath.Dir(f.store.Dirs.Repos), "outside"))
	recs, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Root != detached || recs[0].Branch != "" || recs[1].Root != a.Root || recs[1].Branch != "feature/x" {
		t.Fatalf("list %+v", recs)
	}
	rec, checkout, ok, err := f.store.Find(f.ctx, detached)
	if err != nil || !ok || checkout != c || rec.Repo != "proj" {
		t.Fatalf("find: %+v %s %v %v", rec, checkout, ok, err)
	}
	if _, _, ok, _ := f.store.Find(f.ctx, c); ok {
		t.Fatal("main checkout found as a worktree")
	}
	// Dirty: refused without force, with git's message; removed with it.
	write(t, filepath.Join(a.Root, "untracked"), "x")
	if removed, err := Remove(f.ctx, c, a.Root, false); err == nil || removed {
		t.Fatalf("dirty remove: %v %v", removed, err)
	}
	if removed, err := Remove(f.ctx, c, a.Root, true); err != nil || !removed {
		t.Fatalf("forced remove: %v %v", removed, err)
	}
	if _, err := os.Stat(a.Root); err == nil {
		t.Fatal("root still exists")
	}
	// Gone: a retry skips.
	if removed, err := Remove(f.ctx, c, a.Root, false); err != nil || removed {
		t.Fatalf("repeat remove: %v %v", removed, err)
	}
	// The branch is left alone.
	run(t, c, "git", "rev-parse", "--verify", "refs/heads/feature/x")
}

func TestParseWorktrees(t *testing.T) {
	out := "worktree /r/main\nHEAD abc\nbranch refs/heads/main\n\nworktree /r/w1\nHEAD abc\nbranch refs/heads/feature/x\n\nworktree /r/w2\nHEAD abc\ndetached\nprunable gitdir file points to non-existent location\n\nworktree /r/b\nbare\n"
	got := parseWorktrees(out)
	want := []Entry{
		{Root: "/r/main", Branch: "main"},
		{Root: "/r/w1", Branch: "feature/x"},
		{Root: "/r/w2", Detached: true, Prunable: true},
		{Root: "/r/b", Bare: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}
