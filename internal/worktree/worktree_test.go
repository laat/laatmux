package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
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
	"unicode"
	"unicode/utf8"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/gittest"
	"github.com/laat/laatmux/internal/protocol"
)

// fixture is a bare "remote" with one commit on main, a .laatmux.yaml that
// copies .envrc and runs two setup commands, and a store whose repos and
// worktrees directories are empty.
type fixture struct {
	t      testing.TB
	remote string
	store  *Store
	repo   Repo
	ctx    context.Context
}

func newFixture(t testing.TB) *fixture {
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
	dirs := config.Dirs{Repos: []string{filepath.Join(base, "repos")}, Worktrees: filepath.Join(base, "worktrees")}
	store := New(dirs, []config.Repo{{Source: remote, Name: "proj"}})
	return &fixture{t: t, remote: remote, store: store, repo: store.Repos()[0], ctx: context.Background()}
}

func run(t testing.TB, dir string, name string, args ...string) string {
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

func write(t testing.TB, path, content string) {
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
	other := filepath.Join(f.store.Dirs.Repos[0], "elsewhere")
	run(t, "", "git", "clone", "-q", f.remote, other)
	// A directory with the label's name but a different origin must not be
	// mistaken for the checkout, and must not be cloned over.
	decoy := f.store.Dirs.Checkout("proj")
	run(t, f.store.Dirs.Repos[0], "git", "init", "-q", decoy)
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
	elsewhere := filepath.Join(filepath.Dir(f.store.Dirs.Repos[0]), "elsewhere")
	run(t, c, "git", "worktree", "add", "-q", "-b", "outside", elsewhere, "main")
	_, _, err = f.add("outside")
	if stageOf(t, err) != protocol.StageWorktree || !strings.Contains(err.Error(), "checked out at "+elsewhere) {
		t.Fatalf("err %v", err)
	}
	// ByBranch does not hand that worktree to rm either.
	if rec, co, found, err := f.store.ByBranch(f.ctx, f.repo, "outside"); err != nil || found || co != c {
		t.Fatalf("ByBranch outside: %+v %s %v %v", rec, co, found, err)
	}
	if rec, _, found, err := f.store.ByBranch(f.ctx, f.repo, "by-hand"); err != nil || !found || rec.Root != f.store.Dirs.Worktree("proj", "by-hand") {
		t.Fatalf("ByBranch by-hand: %+v %v %v", rec, found, err)
	}
	// the root taken by a worktree on another branch
	run(t, c, "git", "worktree", "add", "-q", "-b", "squatter", f.store.Dirs.Worktree("proj", "wanted"), "main")
	_, _, err = f.add("wanted")
	if stageOf(t, err) != protocol.StageWorktree || !strings.Contains(err.Error(), "not wanted") {
		t.Fatalf("err %v", err)
	}
	// bad names never reach git
	for _, bad := range []string{"", "-x", "a..b", "x/", "a b", "a\xffb"} {
		if _, _, err := f.add(bad); err == nil || stageOf(t, err) != protocol.StageResolve {
			t.Errorf("branch %q: %v", bad, err)
		}
	}
}

// A branch that is not valid UTF-8, or has U+FFFD, is refused, with
// the reason: laatmux's connection would carry another name (#304), and
// on macOS neither its ref nor its root can be made (#305). git takes
// each of them, so the refusal is CheckBranch's own; a name in UTF-8
// that is not ASCII is a name like any other.
func TestCheckBranchNotCarried(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	for _, c := range []struct{ branch, want string }{
		{"a\xffb", `branch "a\xffb" is not valid UTF-8; laatmux cannot carry it`},
		{"\x9b", `branch "\x9b" is not valid UTF-8; laatmux cannot carry it`},
		{"bl\xc3", `branch "bl\xc3" is not valid UTF-8; laatmux cannot carry it`},
		{"a\ufffdb", "branch \"a\ufffdb\" has U+FFFD, which a byte that is not UTF-8 becomes"},
	} {
		if _, err := git(ctx, "", "check-ref-format", "--branch", c.branch); err != nil {
			t.Fatalf("git refuses %q itself: %v", c.branch, err)
		}
		if err := CheckBranch(ctx, c.branch); err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("CheckBranch %q: %v", c.branch, err)
		}
		if err := CheckWire(c.branch); err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("CheckWire %q: %v", c.branch, err)
		}
	}
	for _, b := range []string{"blåbær", "feature/日本", "task"} {
		if err := CheckBranch(ctx, b); err != nil {
			t.Errorf("CheckBranch %q: %v", b, err)
		}
	}
}

// A worktree checked out by hand on a branch laatmux cannot carry is
// listed with the branch as git has it, found by its root and removed:
// the daemon decides how to show it, and rm --root reaches it.
func TestHandMadeBranchNotCarried(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.add("first"); err != nil {
		t.Fatal(err)
	}
	c := f.checkout()
	root := filepath.Join(f.store.Dirs.Worktrees, "proj", "hand")
	gittest.HandMadeWorktree(t, c, root, "a\xffb")
	recs, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, r := range recs {
		listed = listed || r.Root == root && r.Branch == "a\xffb"
	}
	if !listed {
		t.Fatalf("records %+v", recs)
	}
	rec, co, found, err := f.store.Find(f.ctx, root)
	if err != nil || !found || co != c || rec.Branch != "a\xffb" {
		t.Fatalf("find: %+v %s %v %v", rec, co, found, err)
	}
	// ByBranch finds it by the name and by the form a listing shows,
	// which is what a client has; the U+FFFD form an older client sent
	// is neither.
	for b, want := range map[string]bool{"a\xffb": true, strconv.Quote("a\xffb"): true, "a\ufffdb": false} {
		if rec, _, found, err := f.store.ByBranch(f.ctx, f.repo, b); err != nil || found != want || found && rec.Root != root {
			t.Errorf("by branch %q: %+v %v %v", b, rec, found, err)
		}
	}
	if removed, err := Remove(f.ctx, c, root, false); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("root after remove: %v", err)
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
	write(t, filepath.Join(a.Root, ".laatmux-copy-.envrc.123456"), "export A=")
	// Files of the repository's own that merely resemble the temporary
	// name must survive the copy: the bare name, a non-numeric suffix,
	// and a directory.
	write(t, filepath.Join(a.Root, ".laatmux-copy-.envrc"), "mine")
	write(t, filepath.Join(a.Root, ".laatmux-copy-.envrc.backup"), "backup")
	os.Mkdir(filepath.Join(a.Root, ".laatmux-copy-.envrc.7"), 0o755)
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
	if _, err := os.Stat(filepath.Join(a.Root, ".laatmux-copy-.envrc.123456")); err == nil {
		t.Fatal("stale temporary copy left behind")
	}
	for name, want := range map[string]string{".laatmux-copy-.envrc": "mine", ".laatmux-copy-.envrc.backup": "backup"} {
		if b, _ := os.ReadFile(filepath.Join(a.Root, name)); string(b) != want {
			t.Fatalf("%s clobbered: %q", name, b)
		}
	}
	if fi, err := os.Stat(filepath.Join(a.Root, ".laatmux-copy-.envrc.7")); err != nil || !fi.IsDir() {
		t.Fatal("directory resembling a temporary removed")
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
		run(t, c, "git", "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "setup")
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
	run(t, c, "git", "worktree", "add", "-q", "--detach", filepath.Join(filepath.Dir(f.store.Dirs.Repos[0]), "outside"))
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
	if _, _, ok, _ := f.store.Find(f.ctx, filepath.Join(filepath.Dir(f.store.Dirs.Repos[0]), "outside")); ok {
		t.Fatal("worktree outside the worktrees directory found")
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
	out := "worktree /r/main\x00HEAD abc\x00branch refs/heads/main\x00\x00worktree /r/w1\x00HEAD abc\x00branch refs/heads/feature/x\x00\x00worktree /r/w2\x00HEAD abc\x00detached\x00prunable gitdir file points to non-existent location\x00\x00worktree /r/b\x00bare\x00\x00worktree /r/odd\nname\x00HEAD abc\x00branch refs/heads/nl\x00\x00"
	got := parseWorktrees(out)
	want := []Entry{
		{Root: "/r/main", Branch: "main"},
		{Root: "/r/w1", Branch: "feature/x"},
		{Root: "/r/w2", Detached: true, Prunable: true},
		{Root: "/r/b", Bare: true},
		{Root: "/r/odd\nname", Branch: "nl"},
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

// A name lookup wins over a source lookup, as in config, so a bare local
// source equal to another entry's label does not hijack it.
func TestRepoLookupNameFirst(t *testing.T) {
	s := New(config.Dirs{}, []config.Repo{{Source: "proj", Name: "other"}, {Source: "git@x:a/proj.git", Name: "proj"}})
	if r, ok := s.Repo("proj"); !ok || r.Source != "git@x:a/proj.git" {
		t.Fatalf("Repo(proj) = %+v %v", r, ok)
	}
	if r, ok := s.Repo("git@x:a/proj.git"); !ok || r.Name != "proj" {
		t.Fatalf("Repo(source) = %+v %v", r, ok)
	}
}

// An output line longer than a Scanner's limit must neither hang the
// stage nor be lost: it is truncated, the rest of the output still
// arrives, and the command's exit status is what is reported.
func TestRunStreamingLongLine(t *testing.T) {
	var lines []string
	report := func(_, state, detail string) {
		if state == protocol.StateOutput {
			lines = append(lines, detail)
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- runStreaming(context.Background(), t.TempDir(), report, "setup", os.Environ(),
			"sh", "-c", "head -c 2097152 /dev/zero | tr '\\0' x; echo; echo tail; exit 3")
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exit status 3") {
			t.Fatalf("err %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runStreaming hung on a long line")
	}
	if len(lines) != 2 || len(lines[0]) != maxLine+3 || !strings.HasSuffix(lines[0], "...") || lines[1] != "tail" {
		t.Fatalf("lines: %d, first %d bytes, last %q", len(lines), len(lines[0]), lines[len(lines)-1])
	}
}

// A root or checkout with a tab and an ESC in it, here from the repos
// and worktrees directories' names, is in add's steps and errors, in
// git's command line and in a failed read of a .git as tmux.Printable
// shows it: raw, the tab would break the line and the ESC reach the
// terminal of the client that prints it.
func TestRootWithControlBytesQuoted(t *testing.T) {
	f := newFixture(t)
	base := filepath.Dir(f.store.Dirs.Repos[0])
	f.store = New(config.Dirs{Repos: []string{filepath.Join(base, "re\tpos\x1b[32m")}, Worktrees: filepath.Join(base, "work\ttrees\x1b[31m")}, []config.Repo{{Source: f.remote, Name: "proj"}})
	f.repo = f.store.Repos()[0]
	quoted := func(err error, want string) bool {
		return err != nil && strings.Contains(err.Error(), want) && !strings.ContainsAny(err.Error(), "\t\x1b")
	}
	// A copy pattern, which a repository's .laatmux.yaml may spell with
	// any byte, is named quoted when it matches nothing.
	pattern := "no\tsuch*\x1b.env"
	if err := config.CheckCopy(pattern); err != nil {
		t.Fatalf("the pattern is not one a config may have: %v", err)
	}
	f.repo.Copy = []string{pattern}
	a, steps, err := f.add("first")
	if err != nil {
		t.Fatal(err)
	}
	c := f.checkout()
	q, qc := strconv.Quote(a.Root), strconv.Quote(c)
	for _, want := range []step{
		{protocol.StageResolve, protocol.StateDone, "checkout " + qc},
		{protocol.StageClone, protocol.StateStart, "git clone " + f.remote + " " + qc},
		{protocol.StageWorktree, protocol.StateStart, "git worktree add " + q + " first"},
		{protocol.StageWorktree, protocol.StateDone, "worktree at " + q},
		{protocol.StageCopy, protocol.StateSkip, "missing.txt not in " + qc},
		{protocol.StageCopy, protocol.StateSkip, strconv.Quote(pattern) + " matches nothing in " + qc},
	} {
		if !hasStep(steps, want.stage, want.state, want.detail) {
			t.Errorf("missing %q in %+v", want, steps)
		}
	}
	for _, s := range steps {
		if strings.ContainsAny(s.detail, "\t\x1b") {
			t.Errorf("step with a raw control byte: %q", s.detail)
		}
	}
	// A file the main checkout has, named with them (no [, which would
	// make the entry a glob), is copied and named quoted.
	file := "lit\tx\x1b.env"
	if err := config.CheckCopy(file); err != nil {
		t.Fatalf("the entry is not one a config may have: %v", err)
	}
	write(t, filepath.Join(c, file), "x")
	f.repo.Copy = []string{file}
	if _, steps, err := f.add("copied"); err != nil || !hasStep(steps, protocol.StageCopy, protocol.StateDone, strconv.Quote(file)) {
		t.Errorf("copy of %q: %v %+v", file, err, steps)
	}
	// Again, with the file there and an entry the checkout lacks; then
	// a directory, a link out of the checkout, and a directory of the
	// worktree that leads out of it, each of which fails the stage.
	outside := filepath.Join(base, "outside")
	write(t, filepath.Join(outside, "f"), "x")
	copied := f.store.Dirs.Worktree("proj", "copied")
	missing, dir, link, wdir := "gone\tx\x1b.env", "dir\tx\x1b", "out\tlink\x1b", "wdir\tx\x1b"
	if err := os.Mkdir(filepath.Join(c, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(c, wdir, "f"), "x")
	for _, l := range [][2]string{{outside, filepath.Join(c, link)}, {outside, filepath.Join(copied, wdir)}} {
		if err := os.Symlink(l[0], l[1]); err != nil {
			t.Fatal(err)
		}
	}
	f.repo.Copy = []string{file, missing}
	_, steps, err = f.add("copied")
	if err != nil || !hasStep(steps, protocol.StageCopy, protocol.StateSkip, strconv.Quote(file)+" exists") || !hasStep(steps, protocol.StageCopy, protocol.StateSkip, strconv.Quote(missing)+" not in "+qc) {
		t.Errorf("copy again: %v %+v", err, steps)
	}
	for _, tc := range [][2]string{
		{dir, strconv.Quote(filepath.Join(c, dir)) + ": not a regular file"},
		{link + "/f", strconv.Quote(link+"/f") + " resolves outside the checkout " + qc},
		{wdir + "/f", strconv.Quote(wdir+"/f") + ": its directory resolves outside the worktree " + strconv.Quote(copied)},
	} {
		f.repo.Copy = []string{tc[0]}
		if _, _, err := f.add("copied"); !quoted(err, tc[1]) {
			t.Errorf("copy of %q: %v", tc[0], err)
		}
	}
	f.repo.Copy = nil

	// A checkout's place taken by a directory that is no checkout of the
	// repository, or one of another origin.
	squat := f.store.Dirs.Checkout("squat")
	other := Repo{Source: "/nowhere/squat.git", Name: "squat"}
	if err := os.MkdirAll(squat, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Prepare(f.ctx, other, nil); !quoted(err, strconv.Quote(squat)+" exists and is not a checkout of /nowhere/squat.git") {
		t.Errorf("prepare in a plain directory: %v", err)
	}
	run(t, squat, "git", "init", "-q")
	run(t, squat, "git", "remote", "add", "origin", "/nowhere/else.git")
	if _, err := f.store.Prepare(f.ctx, other, nil); !quoted(err, strconv.Quote(squat)+" exists with origin /nowhere/else.git, not /nowhere/squat.git") {
		t.Errorf("prepare in another checkout: %v", err)
	}
	if err := os.RemoveAll(squat); err != nil {
		t.Fatal(err)
	}
	// A checkout whose config git cannot read; git's message after the
	// quoted checkout is git's, and names the file relative to the
	// checkout it ran in.
	cfgFile := filepath.Join(c, ".git", "config")
	cfg, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfgFile, "[core\n")
	f.store.origins = map[string]originEntry{}
	if _, _, err := f.store.Checkout(f.ctx, f.repo); !quoted(err, qc+": git config --get remote.origin.url: fatal: bad config line 1 in file .git/config") {
		t.Errorf("checkout with a bad config: %v", err)
	}
	write(t, cfgFile, string(cfg))
	f.store.origins = map[string]originEntry{}

	if _, _, err := f.add("main"); !quoted(err, "branch main is checked out in the main checkout "+qc+"; a worktree needs another branch: a new name, or a prompt to propose one") {
		t.Errorf("add main: %v", err)
	}
	elsewhere := filepath.Join(base, "else\twhere\x1b[31m")
	run(t, c, "git", "worktree", "add", "-q", "-b", "outside", elsewhere, "main")
	if _, _, err := f.add("outside"); !quoted(err, "branch outside is checked out at "+strconv.Quote(elsewhere)) {
		t.Errorf("add outside: %v", err)
	}
	wanted := f.store.Dirs.Worktree("proj", "wanted")
	run(t, c, "git", "worktree", "add", "-q", "-b", "squatter", wanted, "main")
	if _, _, err := f.add("wanted"); !quoted(err, strconv.Quote(wanted)+" is a worktree on branch squatter, not wanted") {
		t.Errorf("add wanted: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(f.store.Dirs.Worktrees, "proj", "sym")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.add("sym/x"); !quoted(err, strconv.Quote(f.store.Dirs.Worktree("proj", "sym/x"))+" resolves outside the worktrees directory "+strconv.Quote(f.store.Dirs.Worktrees)) {
		t.Errorf("add sym/x: %v", err)
	}
	// git refuses to remove a worktree with an untracked file in it, and
	// the error names the command with the root quoted; git's own
	// message after it names the root too, and its line is quoted. git
	// itself turns the ESC into ?, and keeps the tab.
	write(t, filepath.Join(elsewhere, "dirt"), "x")
	if _, err := Remove(f.ctx, c, elsewhere, false); !quoted(err, "git worktree remove "+strconv.Quote(elsewhere)+": "+openQuote("fatal: '"+filepath.Join(base, "else\twhere"))) || !strings.Contains(err.Error(), "contains modified or untracked files") || strings.Contains(err.Error(), "\n") {
		t.Errorf("remove: %v", err)
	}
	// A root under the worktrees directory whose .git is not a worktree's.
	dotgit := filepath.Join(a.Root, ".git")
	saved, err := os.ReadFile(dotgit)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dotgit, "not a gitdir line\n")
	if _, _, _, err := f.store.Find(f.ctx, a.Root); !quoted(err, strconv.Quote(dotgit)+" is not a worktree's .git file") {
		t.Errorf("find with a bad .git: %v", err)
	}
	write(t, dotgit, string(saved))

	if os.Getuid() == 0 {
		return // root reads what it likes whatever the mode
	}
	if err := os.Chmod(dotgit, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dotgit, 0o644) })
	if _, _, _, err := f.store.Find(f.ctx, a.Root); !quoted(err, strconv.Quote(dotgit)+": open "+strconv.Quote(dotgit)+": ") || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("find with an unreadable .git: %v", err)
	}
	gitDir := filepath.Join(c, ".git")
	if err := os.Chmod(gitDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(gitDir, 0o755) })
	f.store.origins = map[string]originEntry{}
	if _, _, err := f.store.Checkout(f.ctx, f.repo); !quoted(err, qc+": stat "+strconv.Quote(cfgFile)+": ") || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("checkout with an unreadable .git: %v", err)
	}
}

// openQuote is s as strconv.Quote quotes it without the closing quote:
// how a quoted line that starts with s starts.
func openQuote(s string) string {
	q := strconv.Quote(s)
	return q[:len(q)-1]
}

// Errors whose text is git's or os's name a root, the checkout or the
// directories as tmux.Printable shows them: git's message line by line,
// its lines kept, and an os error with its path quoted.
func TestGitAndOSErrorsQuoted(t *testing.T) {
	f := newFixture(t)
	base := filepath.Dir(f.store.Dirs.Repos[0])
	f.store = New(config.Dirs{Repos: []string{filepath.Join(base, "re\tpos\x1b[32m")}, Worktrees: filepath.Join(base, "work\ttrees\x1b[31m")}, []config.Repo{{Source: f.remote, Name: "proj"}})
	f.repo = f.store.Repos()[0]
	a, _, err := f.add("first")
	if err != nil {
		t.Fatal(err)
	}
	c := f.checkout()
	quoted := func(err error, want string) bool {
		return err != nil && strings.Contains(err.Error(), want) && !strings.ContainsAny(err.Error(), "\t\x1b")
	}

	// git's message of two lines, the first with a tab: that line is
	// quoted, the second is as git wrote it, and the newline stays.
	run(t, c, "git", "worktree", "lock", "--reason", "lo\tck", a.Root)
	_, err = Remove(f.ctx, c, a.Root, false)
	lines := strings.Split(fmt.Sprint(err), "\n")
	if !quoted(err, "git worktree remove "+strconv.Quote(a.Root)+": "+openQuote("fatal: ")) || len(lines) != 2 || !strings.Contains(lines[0], `lock reason: lo\tck`) || !strings.Contains(lines[1], "remove -f -f") || strings.HasPrefix(lines[1], `"`) {
		t.Errorf("remove of a locked worktree: %v", err)
	}
	run(t, c, "git", "worktree", "unlock", a.Root)

	// A directory that is gone: git cannot be run in it, and os's error
	// names it.
	gone := filepath.Join(base, "go\tne\x1b[33m")
	for _, dir := range []string{gone, filepath.Join(base, "new\nline")} {
		if _, err := git(f.ctx, dir, "status"); err == nil || err.Error() != "git status: chdir "+strconv.Quote(dir)+": no such file or directory" {
			t.Errorf("git in a directory gone: %v", err)
		}
	}
	var out []string
	report := func(_, state, detail string) {
		if state == protocol.StateOutput {
			out = append(out, detail)
		}
	}
	if err := runStreaming(f.ctx, gone, report, protocol.StageSetup, os.Environ(), "true"); !quoted(err, "chdir "+strconv.Quote(gone)+": ") {
		t.Errorf("a command in a directory gone: %v", err)
	}
	// A failed command's last lines are each quoted in its error, and
	// reported as the command wrote them.
	line := "Cloning into '" + a.Root + "'..."
	err = runStreaming(f.ctx, a.Root, report, protocol.StageSetup, os.Environ(), "sh", "-c", `printf '%s\nplain\n' "$1"; exit 3`, "sh", line)
	if !quoted(err, "exit status 3: "+strconv.Quote(line)+" | plain") || len(out) != 2 || out[0] != line || out[1] != "plain" {
		t.Errorf("a failed command's lines: %v %q", err, out)
	}
	// The checkout or the worktree gone when a file is copied.
	for _, dirs := range [][2]string{{gone, a.Root}, {c, gone}} {
		if err := copyFile(f.ctx, dirs[0], dirs[1], ".envrc", report); !quoted(err, "open "+strconv.Quote(gone)+": ") || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("copy from %q to %q: %v", dirs[0], dirs[1], err)
		}
	}
	// The file's directory in the worktree a link to itself: os.Root's
	// MkdirAll nests the failed stat's error, which names it too.
	loop := "lo\top\x1b[36m"
	write(t, filepath.Join(c, loop, "f"), "x")
	if err := os.Symlink(loop, filepath.Join(a.Root, loop)); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(f.ctx, c, a.Root, loop+"/f", report); !quoted(err, strconv.Quote(loop)) || !errors.Is(err, syscall.ELOOP) {
		t.Errorf("copy into a directory that links to itself: %v", err)
	}

	// A new worktree's directory that cannot be made, a file in the way.
	in := filepath.Join(f.store.Dirs.Worktrees, "proj", "in")
	write(t, in, "")
	if _, _, err := f.add("in/file"); !quoted(err, "mkdir "+strconv.Quote(in)+": ") {
		t.Errorf("add under a file: %v", err)
	}
	// A file git lists under a directory that a file has replaced since:
	// the lookup's error is not "gone", and fails the copy.
	write(t, filepath.Join(c, "sub", "f"), "x")
	run(t, c, "git", "add", "sub/f")
	if err := os.RemoveAll(filepath.Join(c, "sub")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(c, "sub"), "")
	f.repo.Copy = []string{"sub/*"}
	if _, _, err := f.add("first"); !quoted(err, "lstat "+strconv.Quote(filepath.Join(c, "sub", "f"))+": ") || stageOf(t, err) != protocol.StageCopy {
		t.Errorf("copy of a file under a file: %v", err)
	}
	f.repo.Copy = nil
	// The setup markers' directory, under the checkout's .git, with a
	// file in its way.
	markers := filepath.Join(strings.TrimSpace(run(t, a.Root, "git", "rev-parse", "--absolute-git-dir")), "laatmux")
	if err := os.RemoveAll(markers); err != nil {
		t.Fatal(err)
	}
	write(t, markers, "")
	if _, _, err := f.add("first"); !quoted(err, "mkdir "+strconv.Quote(markers)+": ") || stageOf(t, err) != protocol.StageSetup {
		t.Errorf("add with the markers' directory a file: %v", err)
	}
	if err := os.Remove(markers); err != nil {
		t.Fatal(err)
	}
	// A checkout whose .git/worktrees cannot be resolved, its name too
	// long for the file system.
	long := filepath.Join(base, "lo\tng\x1b"+strings.Repeat("x", 300))
	if _, err := pointsBack(a.Root, long); !quoted(err, "lstat "+strconv.Quote(long)+": ") || !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Errorf("points back to a checkout whose name is too long: %v", err)
	}
	// A repos directory that cannot be read, a file.
	file := filepath.Join(base, "fi\tle\x1b[34m")
	write(t, file, "")
	if _, err := New(config.Dirs{Repos: []string{file}, Worktrees: f.store.Dirs.Worktrees}, nil).List(f.ctx); !quoted(err, strconv.Quote(file)+": ") {
		t.Errorf("list with the repos directory a file: %v", err)
	}

	if os.Getuid() == 0 {
		return // root writes where it likes whatever the mode
	}
	// A repos directory that cannot be made, its parent read-only.
	ro := filepath.Join(base, "read\tonly\x1b[35m")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	repos := filepath.Join(ro, "repos")
	if _, err := New(config.Dirs{Repos: []string{repos}, Worktrees: f.store.Dirs.Worktrees}, nil).Prepare(f.ctx, f.repo, nil); !quoted(err, "mkdir "+strconv.Quote(repos)+": ") || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("prepare with a repos directory that cannot be made: %v", err)
	}
	// A setup marker that cannot be written, its directory read-only.
	if err := os.Mkdir(markers, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(markers, 0o755) })
	if _, _, err := f.add("first"); !quoted(err, "open "+openQuote(markers+"/setup-")) || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("add with a marker that cannot be written: %v", err)
	}
}

// rawByte is whether s has a control character, C1 among them, or a
// byte that is not UTF-8: what tmux.Printable quotes.
func rawByte(s string) bool {
	return !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl)
}

// A branch with a C1 control character, which git takes, is named in
// add's progress and refusals and in ByBranch's and Allocate's errors as
// tmux.Printable shows it: raw, U+009B is a CSI to a terminal that acts
// on C1. A lone 0x9b byte, which is not UTF-8, is no branch add takes
// (TestCheckBranchNotCarried).
func TestBranchWithC1Quoted(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.add("first"); err != nil {
		t.Fatal(err)
	}
	c := f.checkout()
	q := strconv.Quote
	csi := "\u009b31m"
	fresh, pushed, hand := "new"+csi, "remote"+csi, "hand"+csi
	run(t, c, "git", "push", "-q", "origin", "main:refs/heads/"+pushed)
	run(t, c, "git", "branch", hand, "main")
	cases := []struct {
		branch string
		want   []step
	}{
		{fresh, []step{
			{protocol.StageWorktree, protocol.StateStart, "git branch --no-track " + q(fresh) + " origin/HEAD"},
			{protocol.StageWorktree, protocol.StateDone, "branch " + q(fresh) + " from origin/HEAD"},
			{protocol.StageWorktree, protocol.StateStart, "git worktree add " + q(f.store.Dirs.Worktree("proj", fresh)) + " " + q(fresh)},
		}},
		{pushed, []step{
			{protocol.StageWorktree, protocol.StateStart, "git branch --track " + q(pushed) + " " + q("origin/"+pushed)},
			{protocol.StageWorktree, protocol.StateDone, "branch " + q(pushed) + " tracks " + q("origin/"+pushed)},
		}},
		{hand, []step{{protocol.StageWorktree, protocol.StateSkip, "branch " + q(hand) + " exists, used as is"}}},
	}
	for _, tc := range cases {
		_, steps, err := f.add(tc.branch)
		if err != nil {
			t.Fatalf("add %q: %v", tc.branch, err)
		}
		for _, want := range tc.want {
			if !hasStep(steps, want.stage, want.state, want.detail) {
				t.Errorf("missing %q in %+v", want, steps)
			}
		}
		for _, s := range steps {
			if rawByte(s.detail) {
				t.Errorf("step with a raw byte: %q", s.detail)
			}
		}
	}

	quoted := func(err error, want string) bool {
		return err != nil && strings.Contains(err.Error(), want) && !rawByte(err.Error())
	}
	// The branch checked out at a worktree outside the worktrees
	// directory, and the root of the branch's worktree taken by one on
	// another branch.
	out, squatter, wanted := "out"+csi, "squat"+csi, "wanted"+csi
	elsewhere := filepath.Join(filepath.Dir(f.store.Dirs.Repos[0]), "elsewhere")
	run(t, c, "git", "worktree", "add", "-q", "-b", out, elsewhere, "main")
	if _, _, err := f.add(out); !quoted(err, "branch "+q(out)+" is checked out at "+elsewhere) {
		t.Errorf("add %q: %v", out, err)
	}
	wantedRoot := f.store.Dirs.Worktree("proj", wanted)
	run(t, c, "git", "worktree", "add", "-q", "-b", squatter, wantedRoot, "main")
	if _, _, err := f.add(wanted); !quoted(err, q(wantedRoot)+" is a worktree on branch "+q(squatter)+", not "+q(wanted)) {
		t.Errorf("add %q: %v", wanted, err)
	}
	// The branch in two clones of the repository.
	second := filepath.Join(f.store.Dirs.Repos[0], "proj2")
	run(t, filepath.Dir(f.store.Dirs.Repos[0]), "git", "clone", "-q", f.remote, second)
	run(t, second, "git", "worktree", "add", "-q", "-b", fresh, f.store.Dirs.Worktree("proj2", fresh))
	if _, _, _, err := f.store.ByBranch(f.ctx, f.repo, fresh); !quoted(err, "branch "+q(fresh)+" of proj has worktrees at ") {
		t.Errorf("by branch in two clones: %v", err)
	}
	if _, err := Allocate(fresh, func(string) bool { return true }); !quoted(err, "no free name for "+q(fresh)+":") {
		t.Errorf("allocate: %v", err)
	}
	// The branch checked out in the main checkout.
	inMain := "main" + csi
	run(t, c, "git", "checkout", "-q", "-b", inMain)
	if _, _, err := f.add(inMain); !quoted(err, "branch "+q(inMain)+" is checked out in the main checkout "+c) {
		t.Errorf("add %q: %v", inMain, err)
	}
}

// A checkout whose .git/config cannot be read is an error, not a
// repository without a checkout: polling must not drop its records and rm
// must not take its worktrees as already gone.
func TestCheckoutStatErrorPropagates(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores permissions")
	}
	f := newFixture(t)
	a, _, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(a.Checkout, ".git")
	if err := os.Chmod(gitDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(gitDir, 0o755) })
	f.store.origins = map[string]originEntry{}
	if _, _, err := f.store.Checkout(f.ctx, f.repo); err == nil {
		t.Fatal("unreadable checkout taken as absent")
	}
	if _, err := f.store.List(f.ctx); err == nil {
		t.Fatal("List hid the unreadable checkout")
	}
	if _, _, _, err := f.store.Find(f.ctx, a.Root); err == nil {
		t.Fatal("Find took the unreadable checkout as absent")
	}
}

func TestOwns(t *testing.T) {
	s := New(config.Dirs{Repos: []string{"/r"}, Worktrees: "/w/trees/"}, nil)
	cases := map[string]bool{
		"/w/trees/proj/task":        true,
		"/w/trees/proj/a/../b":      true,
		"/w/trees":                  false,
		"/w/trees/":                 false,
		"/w/trees/..":               false,
		"/w/trees/../outside":       false,
		"/w/trees/proj/../../x":     false,
		"/w/treesX/proj":            false,
		"/w/trees/..hidden":         true,
		"relative/w/trees/proj":     false,
		"/other/w/trees/proj":       false,
		"/w/trees/proj/../../trees": false,
	}
	for root, want := range cases {
		if got := s.Owns(root); got != want {
			t.Errorf("Owns(%q) = %v, want %v", root, got, want)
		}
	}
}

// Cleanup reads the destination directory literally: a directory whose
// name is a glob pattern must not reach into its siblings.
func TestRemoveStaleTempsLiteralDir(t *testing.T) {
	base := t.TempDir()
	sib := filepath.Join(base, "a")
	pat := filepath.Join(base, "[ab]")
	write(t, filepath.Join(sib, ".laatmux-copy-.envrc.123456"), "other worktree's copy")
	write(t, filepath.Join(pat, ".laatmux-copy-.envrc.654321"), "stale")
	write(t, filepath.Join(pat, ".laatmux-copy-.envrc.backup"), "kept")
	wt, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Close()
	removeStaleTemps(wt, "[ab]", ".envrc")
	if _, err := os.Stat(filepath.Join(sib, ".laatmux-copy-.envrc.123456")); err != nil {
		t.Fatal("sibling directory's file removed")
	}
	if _, err := os.Stat(filepath.Join(pat, ".laatmux-copy-.envrc.654321")); err == nil {
		t.Fatal("stale temporary kept")
	}
	if _, err := os.Stat(filepath.Join(pat, ".laatmux-copy-.envrc.backup")); err != nil {
		t.Fatal("backup removed")
	}
}

// Symlinks are resolved on both sides: a link under the worktrees
// directory that leaves it is not owned, a link into it is, an alias of
// the directory itself is, and a deleted worktree still resolves through
// the links above it.
func TestOwnsResolvesSymlinks(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wt, outside := filepath.Join(base, "wt"), filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(wt, "real"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.Symlink(outside, filepath.Join(wt, "escape"))
	os.Symlink(filepath.Join(wt, "real"), filepath.Join(wt, "inward"))
	os.Symlink(wt, filepath.Join(base, "alias"))
	os.Symlink(filepath.Join(wt, "missing"), filepath.Join(wt, "dangling"))
	os.Symlink(filepath.Join(wt, "loop2"), filepath.Join(wt, "loop1"))
	os.Symlink(filepath.Join(wt, "loop1"), filepath.Join(wt, "loop2"))
	s := New(config.Dirs{Repos: []string{base}, Worktrees: filepath.Join(base, "alias")}, nil)
	cases := map[string]bool{
		// A prefix that exists but cannot be resolved fails closed.
		filepath.Join(wt, "dangling", "task"):        false,
		filepath.Join(wt, "loop1", "task"):           false,
		filepath.Join(wt, "escape", "scratch"):       false,
		filepath.Join(wt, "escape"):                  false,
		filepath.Join(wt, "inward", "task"):          true,
		filepath.Join(wt, "real", "task"):            true,
		filepath.Join(base, "alias", "proj", "task"): true,
		filepath.Join(wt, "gone", "deleted", "deep"): true,
		filepath.Join(base, "alias", "escape", "x"):  false,
		outside:                      false,
		filepath.Join(base, "alias"): false,
	}
	if os.Getuid() != 0 {
		noperm := filepath.Join(wt, "noperm")
		os.Mkdir(noperm, 0)
		t.Cleanup(func() { os.Chmod(noperm, 0o755) })
		cases[filepath.Join(noperm, "task")] = false
	}
	for root, want := range cases {
		if got := s.Owns(root); got != want {
			t.Errorf("Owns(%q) = %v, want %v", root, got, want)
		}
	}
}

// A symlink already sitting at <worktrees>/<name> would carry a new
// worktree outside the directory; add refuses at the worktree stage
// before creating anything.
func TestAddRefusesSymlinkedRepoDir(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.add("first"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(f.store.Dirs.Repos[0]), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(f.store.Dirs.Worktrees, "proj"))
	if err := os.Symlink(outside, filepath.Join(f.store.Dirs.Worktrees, "proj")); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.add("task")
	if stageOf(t, err) != protocol.StageWorktree || !strings.Contains(err.Error(), "outside the worktrees directory") {
		t.Fatalf("err %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("worktree created outside: %v", entries)
	}
}

// A worktree whose path contains a newline is listed whole, root intact.
func TestListWorktreeWithNewlineInPath(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("first")
	if err != nil {
		t.Fatal(err)
	}
	odd := filepath.Join(f.store.Dirs.Worktrees, "proj", "odd\nname")
	run(t, a.Checkout, "git", "worktree", "add", "-q", "-b", "nl", odd, "main")
	recs, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range recs {
		if r.Root == odd && r.Branch == "nl" {
			found = true
		}
		if strings.HasPrefix(odd, r.Root) && r.Root != odd {
			t.Fatalf("truncated root %q", r.Root)
		}
	}
	if !found {
		t.Fatalf("worktree with newline not listed: %+v", recs)
	}
}

// An invalid setup entry fails the setup stage, an invalid copy entry the
// copy stage, though one read of the file serves both.
func TestSetupFileErrorsNameTheirStage(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.add("first"); err != nil {
		t.Fatal(err)
	}
	c := f.checkout()
	for _, tc := range []struct{ branch, content, stage string }{
		{"bad-setup", "setup: [\" \"]\n", protocol.StageSetup},
		{"bad-copy", "copy: [../x]\n", protocol.StageCopy},
		{"bad-yaml", "copy: [\n", protocol.StageCopy},
	} {
		run(t, c, "git", "checkout", "-q", "-b", tc.branch, "main")
		write(t, filepath.Join(c, config.SetupFile), tc.content)
		run(t, c, "git", "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qam", tc.branch)
		run(t, c, "git", "push", "-q", "origin", tc.branch)
		run(t, c, "git", "checkout", "-q", "main")
		_, _, err := f.add(tc.branch)
		if got := stageOf(t, err); got != tc.stage {
			t.Errorf("%s: stage %s, want %s (%v)", tc.branch, got, tc.stage, err)
		}
	}
}

// ListAll has a record of every main checkout it scans, labelled as its
// worktrees are, its branch read from HEAD, "" when detached, on the
// reftable backend too; whether the config lists its repository, and
// whether a worktree of it is listed, which the daemon publishes it by.
func TestListAllMainCheckouts(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(f.store.Dirs.Repos[0])
	clone := func(name, branch string) string {
		dir := filepath.Join(f.store.Dirs.Repos[0], name)
		run(t, base, "git", "clone", "-q", f.remote, dir)
		run(t, dir, "git", "remote", "set-url", "origin", filepath.Join(base, name+".git"))
		if branch == "" {
			run(t, dir, "git", "checkout", "-q", "--detach")
		} else {
			run(t, dir, "git", "checkout", "-q", "-b", branch)
		}
		return dir
	}
	other, detached := clone("other", "feature"), clone("detached", "")
	want := []Record{
		{Repo: "detached", Source: filepath.Join(base, "detached.git"), Root: detached, Main: true},
		{Repo: "other", Source: filepath.Join(base, "other.git"), Branch: "feature", Root: other, Main: true},
		{Repo: "proj", Source: f.remote, Branch: "main", Root: f.checkout(), Main: true, Configured: true, Linked: true},
	}
	if err := exec.Command("git", "init", "-q", "--ref-format=reftable", filepath.Join(base, "probe")).Run(); err == nil {
		dir := filepath.Join(f.store.Dirs.Repos[0], "table")
		run(t, base, "git", "clone", "-q", "--ref-format=reftable", f.remote, dir)
		run(t, dir, "git", "checkout", "-q", "-b", "tabled")
		want = append(want, Record{Repo: "proj", Source: f.remote, Branch: "tabled", Root: dir, Main: true, Configured: true})
	}
	recs, checkouts, err := f.store.ListAll(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Root != a.Root || recs[0].Main {
		t.Fatalf("worktrees %+v", recs)
	}
	if !slices.Equal(checkouts, want) {
		t.Fatalf("checkouts\n%+v\nwant\n%+v", checkouts, want)
	}
	// List is the worktrees alone.
	if recs, err := f.store.List(f.ctx); err != nil || len(recs) != 1 || recs[0].Root != a.Root {
		t.Fatalf("list %+v %v", recs, err)
	}
}

// A branch ending in U+0085 or U+00A0, which git takes, is read from
// HEAD as it is, on either backend; a checkout whose HEAD cannot be read
// is marked unread and fails nothing, the worktrees still listed, when
// it has none itself and the store has its origin.
func TestListAllHeadEdges(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(f.store.Dirs.Repos[0])
	want := map[string]string{}
	for i, branch := range []string{"nel\u0085", "nbsp\u00a0"} {
		dir := filepath.Join(f.store.Dirs.Repos[0], "c"+strconv.Itoa(i))
		run(t, base, "git", "clone", "-q", f.remote, dir)
		run(t, dir, "git", "checkout", "-q", "-b", branch)
		want[dir] = branch
	}
	if err := exec.Command("git", "init", "-q", "--ref-format=reftable", filepath.Join(base, "probe")).Run(); err == nil {
		dir := filepath.Join(f.store.Dirs.Repos[0], "table")
		run(t, base, "git", "clone", "-q", "--ref-format=reftable", f.remote, dir)
		run(t, dir, "git", "checkout", "-q", "-b", "tab\u0085")
		want[dir] = "tab\u0085"
	}
	_, checkouts, err := f.store.ListAll(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checkouts {
		if b, ok := want[c.Root]; ok && c.Branch != b {
			t.Errorf("%s: branch %q, want %q", c.Root, c.Branch, b)
		}
	}
	// A clone with no worktree: a checkout with one fails the listing
	// all the same, as git worktree list cannot find its repository.
	unread := filepath.Join(f.store.Dirs.Repos[0], "c0")
	head := filepath.Join(unread, ".git", "HEAD")
	if err := os.Chmod(head, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(head, 0o644) })
	if _, err := os.ReadFile(head); err == nil {
		t.Skip("HEAD readable without permission (root)")
	}
	var logged strings.Builder
	f.store.Log = log.New(&logged, "", 0)
	recs, checkouts, err := f.store.ListAll(f.ctx)
	if err != nil || len(recs) != 1 || recs[0].Root != a.Root {
		t.Fatalf("with a HEAD unreadable: %+v %v", recs, err)
	}
	if !slices.ContainsFunc(checkouts, func(c Record) bool { return c.Root == unread && c.Unread && c.Branch == "" }) || !strings.Contains(logged.String(), "HEAD") {
		t.Fatalf("the checkout whose HEAD is unreadable: %+v, log %q", checkouts, logged.String())
	}
	// Logged once while it fails, and once more after it has read again.
	count := func() int { return strings.Count(logged.String(), "HEAD") }
	f.store.ListAll(f.ctx)
	if count() != 1 {
		t.Fatalf("logged again while it fails: %q", logged.String())
	}
	os.Chmod(head, 0o644)
	f.store.ListAll(f.ctx)
	os.Chmod(head, 0)
	f.store.ListAll(f.ctx)
	if count() != 2 {
		t.Fatalf("not logged after it read again: %q", logged.String())
	}
}

// Where the repos directory is reached through a symlink into the
// worktrees directory, git lists the main worktree by its real path,
// which Owns takes: it is the main checkout's record, not a worktree.
func TestListAllSymlinkedReposUnderWorktrees(t *testing.T) {
	f := newFixture(t)
	base := filepath.Dir(f.store.Dirs.Repos[0])
	real := filepath.Join(f.store.Dirs.Worktrees, "checkouts")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	f.store.Dirs.Repos = []string{link}
	a, _, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	recs, checkouts, err := f.store.ListAll(f.ctx)
	if err != nil || len(recs) != 1 || recs[0].Root != a.Root {
		t.Fatalf("worktrees %+v %v", recs, err)
	}
	if len(checkouts) != 1 || checkouts[0].Root != filepath.Join(link, "proj") {
		t.Fatalf("checkouts %+v", checkouts)
	}
}

// With the repos directory under the worktrees one, the main checkout
// satisfies Owns but is not a worktree: it is not listed.
func TestMainCheckoutNotListed(t *testing.T) {
	f := newFixture(t)
	f.store.Dirs.Repos = []string{filepath.Join(f.store.Dirs.Worktrees, "checkouts")}
	a, _, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	recs, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Root != a.Root {
		t.Fatalf("list %+v", recs)
	}
}

// One scan of the repos directory serves every repository: with many
// known repositories a poll stats each checkout once, not once per repo.
func TestCheckoutsScannedOnce(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.add("task"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		f.store.SetRepos(append(f.store.Repos(), Repo{Source: fmt.Sprintf("/nowhere/%d.git", i), Name: fmt.Sprintf("r%d", i)}))
	}
	checkouts, err := f.store.Checkouts(f.ctx)
	if err != nil || len(checkouts) != 1 {
		t.Fatalf("checkouts %v %v", checkouts, err)
	}
	if recs, err := f.store.List(f.ctx); err != nil || len(recs) != 1 {
		t.Fatalf("list %+v %v", recs, err)
	}
}

// A copy glob matches slash-separated paths segment by segment, ** for
// any number of segments; * and ? stay within a segment.
func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"**/.envrc.cache.enc", ".envrc.cache.enc", true},
		{"**/.envrc.cache.enc", "apps/web/.envrc.cache.enc", true},
		{"**/.envrc.cache.enc", "apps/web/.envrc", false},
		{"*.enc", ".envrc.cache.enc", true},
		{"*.enc", "apps/.envrc.cache.enc", false},
		{"apps/*/.envrc", "apps/web/.envrc", true},
		{"apps/*/.envrc", "apps/web/x/.envrc", false},
		{"apps/**", "apps/web/x/.envrc", true},
		{"apps/**", "apps", true},
		{"config/*.local", "config/db.local", true},
		{"config/*.local", "config/db.local.bak", false},
		{"[ab].txt", "a.txt", true},
		{"**", "anything/at/all", true},
	}
	for _, c := range cases {
		if got := MatchGlob(c.pattern, c.path); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v", c.pattern, c.path, got)
		}
	}
}

// The store's copy rules and a repository's own copy and setup run after
// the committed ones: a glob finds the ignored files of the main
// checkout wherever they sit, but nothing inside an ignored directory,
// and a literal path is copied as before; a personal setup command runs
// after the committed commands, once.
func TestAddPersonalCopyAndSetup(t *testing.T) {
	f := newFixture(t)
	checkout, _, _ := f.store.Checkout(f.ctx, f.repo)
	if checkout == "" {
		a, err := f.store.Add(f.ctx, f.repo, "first", nil)
		if err != nil {
			t.Fatal(err)
		}
		checkout = a.Checkout
	}
	write(t, filepath.Join(checkout, ".gitignore"), "*.enc\nnode_modules/\n")
	write(t, filepath.Join(checkout, ".envrc.cache.enc"), "root secret")
	write(t, filepath.Join(checkout, "apps", "web", ".envrc.cache.enc"), "web secret")
	write(t, filepath.Join(checkout, "node_modules", "dep", ".envrc.cache.enc"), "never")
	write(t, filepath.Join(checkout, "notes.txt"), "untracked")
	write(t, filepath.Join(checkout, "config", "db.local"), "local")
	f.store.Copy = []string{"**/.envrc.cache.enc", "nothing/*.here"}
	repos := f.store.Repos()
	repos[0].Copy = []string{"notes.txt", "config/*.local"}
	repos[0].Setup = []string{"echo personal >> log"}
	f.store.SetRepos(repos)
	repo := repos[0]
	var reports []string
	a, err := f.store.Add(f.ctx, repo, "task", func(stage, state, detail string) {
		reports = append(reports, stage+" "+state+" "+detail)
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{".envrc.cache.enc": "root secret", "apps/web/.envrc.cache.enc": "web secret", "notes.txt": "untracked", "config/db.local": "local"} {
		if b, err := os.ReadFile(filepath.Join(a.Root, rel)); err != nil || string(b) != want {
			t.Errorf("%s: %q %v", rel, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(a.Root, "node_modules", "dep", ".envrc.cache.enc")); err == nil {
		t.Error("a file inside an ignored directory was copied")
	}
	if b, _ := os.ReadFile(filepath.Join(a.Root, "log")); string(b) != "one\ntwo\npersonal\n" {
		t.Errorf("setup order: %q", b)
	}
	joined := strings.Join(reports, "\n")
	if !strings.Contains(joined, "copy skip nothing/*.here matches nothing") || !strings.Contains(joined, "setup done echo personal >> log") {
		t.Errorf("reports:\n%s", joined)
	}
	// A retry copies nothing again and reruns no command.
	reports = nil
	if _, err := f.store.Add(f.ctx, repo, "task", func(stage, state, detail string) {
		reports = append(reports, stage+" "+state+" "+detail)
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range reports {
		if strings.HasPrefix(r, "copy done") || strings.HasPrefix(r, "setup start") {
			t.Errorf("retry redid: %s", r)
		}
	}
	// The committed list growing does not move the personal command
	// onto another marker: it is still done, and the new committed
	// command runs.
	write(t, filepath.Join(a.Root, config.SetupFile), "copy: [.envrc, missing.txt]\nsetup: [\"echo one >> log\", \"echo two >> log\", \"echo three >> log\"]\n")
	reports = nil
	if _, err := f.store.Add(f.ctx, repo, "task", func(stage, state, detail string) {
		reports = append(reports, stage+" "+state+" "+detail)
	}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(a.Root, "log")); string(b) != "one\ntwo\npersonal\nthree\n" {
		t.Errorf("markers after the committed list grew: %q", b)
	}
	// A personal command changed at the same index runs; the one that
	// moved does not run again.
	repos[0].Setup = []string{"echo first >> log", "echo personal >> log"}
	f.store.SetRepos(repos)
	repo = repos[0]
	if _, err := f.store.Add(f.ctx, repo, "task", nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(a.Root, "log")); string(b) != "one\ntwo\npersonal\nthree\nfirst\npersonal\n" {
		t.Errorf("personal list changed: %q", b)
	}
}

// A glob passes over what git lists that is not a file: a symlink, a
// directory, a submodule's gitlink. A literal entry that is a symlink
// to a file outside the checkout is refused, and a target whose
// directory is a symlink out of the worktree is refused, so nothing is
// read from or written to outside the two roots.
func TestCopyStaysInsideRoots(t *testing.T) {
	f := newFixture(t)
	a, err := f.store.Add(f.ctx, f.repo, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	checkout := a.Checkout
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret"), "outside")
	write(t, filepath.Join(checkout, ".gitignore"), "*.enc\nlinks/\n")
	write(t, filepath.Join(checkout, "real.enc"), "real")
	os.MkdirAll(filepath.Join(checkout, "links"), 0o755)
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(checkout, "links", "link.enc"))
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(checkout, "direct.enc"))
	// A submodule: a gitlink git lists without a trailing slash.
	sub := filepath.Join(t.TempDir(), "sub")
	run(t, filepath.Dir(sub), "git", "init", "-q", "--initial-branch=main", sub)
	run(t, sub, "git", "config", "user.email", "t@example.com")
	run(t, sub, "git", "config", "user.name", "t")
	write(t, filepath.Join(sub, "f"), "x")
	run(t, sub, "git", "add", ".")
	run(t, sub, "git", "commit", "-q", "-m", "sub")
	run(t, checkout, "git", "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "vendor/lib")
	f.store.Copy = []string{"**/*.enc", "vendor/**"}
	var reports []string
	b, err := f.store.Add(f.ctx, f.repo, "second", func(stage, state, detail string) { reports = append(reports, stage+" "+state+" "+detail) })
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(reports, "\n"))
	}
	if got, _ := os.ReadFile(filepath.Join(b.Root, "real.enc")); string(got) != "real" {
		t.Errorf("real.enc: %q", got)
	}
	for _, rel := range []string{"links/link.enc", "direct.enc"} {
		if _, err := os.Lstat(filepath.Join(b.Root, rel)); err == nil {
			t.Errorf("%s: a symlink out of the checkout was copied by a glob", rel)
		}
	}
	// A literal entry that is a symlink out of the checkout is refused.
	f.store.Copy = []string{"direct.enc"}
	if _, err := f.store.Add(f.ctx, f.repo, "third", nil); err == nil || !strings.Contains(err.Error(), "outside the checkout") {
		t.Errorf("literal symlink out of the checkout: %v", err)
	}
	// A target directory that is a symlink out of the worktree is
	// refused: nothing lands outside.
	f.store.Copy = []string{"real.enc"}
	c, err := f.store.Add(f.ctx, f.repo, "fourth", nil)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(c.Root, "real.enc"))
	f.store.Copy = []string{"esc/new/nested/real.enc"}
	write(t, filepath.Join(checkout, "esc", "new", "nested", "real.enc"), "real")
	os.Symlink(outside, filepath.Join(c.Root, "esc"))
	if _, err := f.store.Add(f.ctx, f.repo, "fourth", nil); err == nil || !strings.Contains(err.Error(), "outside the worktree") {
		t.Errorf("target directory out of the worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Error("a directory was made outside the worktree")
	}
	if _, err := os.Stat(filepath.Join(outside, "new", "nested", "real.enc")); err == nil {
		t.Error("a file was written outside the worktree")
	}
	// A literal entry naming a pipe with no writer is refused at once,
	// not waited on.
	fifo := filepath.Join(checkout, "pipe.enc")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	f.store.Copy = []string{"pipe.enc"}
	done := make(chan error, 1)
	go func() { _, err := f.store.Add(f.ctx, f.repo, "sixth", nil); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("pipe as a source: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("opening a pipe with no writer blocked the copy")
	}
	os.Remove(fifo)
	// A glob candidate whose lookup fails for a reason other than being
	// gone fails the stage rather than being passed over in silence.
	if os.Getuid() != 0 {
		locked := filepath.Join(checkout, "locked")
		write(t, filepath.Join(locked, "x.enc"), "x")
		run(t, checkout, "git", "add", "-f", "locked/x.enc")
		os.Chmod(locked, 0)
		t.Cleanup(func() { os.Chmod(locked, 0o755) })
		f.store.Copy = []string{"**/*.enc"}
		if _, err := f.store.Add(f.ctx, f.repo, "fifth", nil); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("unreadable candidate: %v", err)
		}
		os.Chmod(locked, 0o755)
	}
}

// ProposeBranch turns the first words of a prompt into a name git
// accepts, cut on a word boundary; Allocate is the first free numbered
// form; Branches lists local and remote names.
func TestProposeAndAllocate(t *testing.T) {
	cases := map[string]string{
		"Make the sidebar follow the current row when the sort moves it": "make-the-sidebar-follow-the-current-row",
		"  Fix: tests!! (again)  ":                                       "fix-tests-again",
		"ÆØÅ æøå 42":                                                     "æøå-æøå-42",
		"!!!":                                                            "",
		"a-very-long-single-word-that-goes-past-forty-characters": "a-very-long-single-word-that-goes-past",
		"short": "short",
	}
	for prompt, want := range cases {
		if got := ProposeBranch(prompt); got != want {
			t.Errorf("%q: got %q, want %q", prompt, got, want)
		}
		if want != "" {
			if err := CheckBranch(context.Background(), want); err != nil {
				t.Errorf("%q: %v", want, err)
			}
		}
	}
	taken := map[string]bool{"task": true, "task-2": true}
	if got, err := Allocate("task", func(n string) bool { return taken[n] }); got != "task-3" || err != nil {
		t.Fatalf("allocate %s %v", got, err)
	}
	// A name no numbering frees, an occupied ancestor, is an error, not
	// a search that never ends.
	if got, err := Allocate("feature/task", func(n string) bool { return RefConflict("feature", n) }); err == nil {
		t.Fatalf("allocate under an occupied ancestor gave %s", got)
	}
	// Long names are cut on characters, never inside one.
	cjk := strings.Repeat("修复侧边栏排序", 10)
	if got := ProposeBranch(cjk); len([]rune(got)) != 40 || !utf8.ValidString(got) {
		t.Fatalf("cjk proposal %q (%d runes)", got, len([]rune(got)))
	}
	for _, c := range []struct {
		existing, candidate string
		want                bool
	}{{"task", "task", true}, {"task/sub", "task", true}, {"task", "task/sub", true}, {"task-2", "task", false}, {"tasks", "task", false}} {
		if got := RefConflict(c.existing, c.candidate); got != c.want {
			t.Errorf("RefConflict(%q, %q) = %v", c.existing, c.candidate, got)
		}
	}
	if got, err := Allocate("free", func(n string) bool { return taken[n] }); got != "free" || err != nil {
		t.Fatalf("allocate %s %v", got, err)
	}
	f := newFixture(t)
	if _, _, err := f.add("feature"); err != nil {
		t.Fatal(err)
	}
	local, rem, err := Branches(f.ctx, f.checkout())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(local, ",") != "feature,main" || strings.Join(rem, ",") != "main" {
		t.Fatalf("local %v remote %v", local, rem)
	}
	// Prepare, Place and Materialize are the parts of Add: the root is
	// placed at the label's place, or where git has the branch.
	p, err := f.store.Prepare(f.ctx, f.repo, nil)
	if err != nil || !p.Found || p.Checkout != f.checkout() {
		t.Fatalf("prepare %+v %v", p, err)
	}
	if root, err := f.store.Place(f.ctx, p, f.repo, "feature"); err != nil || root != f.store.Dirs.Worktree("proj", "feature") {
		t.Fatalf("place %s %v", root, err)
	}
	if root, err := f.store.Place(f.ctx, p, f.repo, "new"); err != nil || root != f.store.Dirs.Worktree("proj", "new") {
		t.Fatalf("place new %s %v", root, err)
	}
}

// A checkout the config does not list is listed too, labelled by its
// directory with its origin as the source, and found by rm's lookups:
// by root, by its origin, and by that label.
func TestUnlistedCheckoutListed(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.store.Dirs.Repos[0], "other")
	run(t, f.store.Dirs.Repos[0], "git", "clone", "-q", f.remote, other)
	run(t, other, "git", "remote", "set-url", "origin", "/elsewhere/other.git")
	root := f.store.Dirs.Worktree("other", "side")
	run(t, other, "git", "worktree", "add", "-q", "-b", "side", root)
	recs, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Record{
		a.Root: {Repo: "proj", Source: f.remote, Branch: "task", Root: a.Root},
		root:   {Repo: "other", Source: "/elsewhere/other.git", Branch: "side", Root: root},
	}
	if len(recs) != 2 || recs[0] != want[recs[0].Root] || recs[1] != want[recs[1].Root] {
		t.Fatalf("list %+v", recs)
	}
	rec, co, ok, err := f.store.Find(f.ctx, root)
	if err != nil || !ok || co != other || rec != want[root] {
		t.Fatalf("find: %+v %s %v %v", rec, co, ok, err)
	}
	for _, name := range []string{"/elsewhere/other.git", "other"} {
		r, ok, err := f.store.Known(f.ctx, name)
		if err != nil || !ok || r.Name != "other" || r.Source != "/elsewhere/other.git" {
			t.Fatalf("known %s: %+v %v %v", name, r, ok, err)
		}
		if _, ok := f.store.Repo(name); ok {
			t.Fatalf("%s taken for a listed repository", name)
		}
	}
	if r, ok, err := f.store.Known(f.ctx, "/nowhere.git"); err != nil || ok {
		t.Fatalf("known nowhere: %+v %v %v", r, ok, err)
	}
	// A listed repository keeps the config's label and source.
	if r, ok, err := f.store.Known(f.ctx, "proj"); err != nil || !ok || r.Source != f.remote {
		t.Fatalf("known proj: %+v %v %v", r, ok, err)
	}
}

// A directory's name that is a label is the label; any other becomes
// one, each character the rule does not take a _.
func TestDirLabel(t *testing.T) {
	for name, want := range map[string]string{
		"proj":    "proj",
		"a-b_C9":  "a-b_C9",
		"x--":     "x--",
		"next.js": "next_js",
		"a:b":     "a_b",
		"a\tb":    "a_b",
		"\xff":    "_",
		"å":       "_",
		"-x":      "_x",
		"--":      "_-",
		".":       "_",
	} {
		got := dirLabel(name)
		if got != want || !config.ValidLabel(got) {
			t.Errorf("dirLabel(%q) = %q, want %q", name, got, want)
		}
	}
}

// A checkout cloned into the repos directory by hand under a name that
// is not a label, a tab and an ESC in it, is labelled by dirLabel in
// what ls lists and in rm's and run's lookups, which name it in their
// refusals: no raw byte of the directory's name reaches them. The
// label finds it; the directory's name does not.
func TestUnlistedCheckoutNotALabel(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.add("task"); err != nil {
		t.Fatal(err)
	}
	name := "hand\tmade\x1b[31m"
	const label = "hand_made__31m"
	hand := filepath.Join(f.store.Dirs.Repos[0], name)
	run(t, f.store.Dirs.Repos[0], "git", "clone", "-q", f.remote, hand)
	run(t, hand, "git", "remote", "set-url", "origin", "/elsewhere/hand.git")
	root := f.store.Dirs.Worktree("hand", "side")
	run(t, hand, "git", "worktree", "add", "-q", "-b", "side", root)
	want := Record{Repo: label, Source: "/elsewhere/hand.git", Branch: "side", Root: root}
	recs, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || (recs[0] != want && recs[1] != want) {
		t.Fatalf("list %+v", recs)
	}
	rec, co, ok, err := f.store.Find(f.ctx, root)
	if err != nil || !ok || co != hand || rec != want {
		t.Fatalf("find: %+v %q %v %v", rec, co, ok, err)
	}
	for _, by := range []string{label, "/elsewhere/hand.git"} {
		r, ok, err := f.store.Known(f.ctx, by)
		if err != nil || !ok || r.Name != label || r.Source != "/elsewhere/hand.git" {
			t.Fatalf("known %q: %+v %v %v", by, r, ok, err)
		}
		rec, _, ok, err := f.store.ByBranch(f.ctx, r, "side")
		if err != nil || !ok || rec != want {
			t.Fatalf("by branch of %q: %+v %v %v", by, rec, ok, err)
		}
	}
	if r, ok, err := f.store.Known(f.ctx, name); err != nil || ok {
		t.Fatalf("known by the directory's name: %+v %v %v", r, ok, err)
	}
}

// Two checkouts the config does not list whose names make one label,
// of two repositories, do not share it: the first in directory order
// keeps it, or the one whose directory is named so, and the other's
// label has a hash of its origin after it, logged once, its directory
// quoted. Nothing is left out of the listing, so the worktrees of a
// checkout a collision appears next to stay listed. Clones of one
// repository share a label, plain or hashed. A made label that is the
// config's name for another repository is hashed too.
func TestUnlistedLabelCollision(t *testing.T) {
	f := newFixture(t)
	var logged bytes.Buffer
	f.store.Log = log.New(&logged, "", 0)
	clone := func(name, origin, branch string) (dir, root string) {
		t.Helper()
		dir = filepath.Join(f.store.Dirs.Repos[0], name)
		run(t, filepath.Dir(f.store.Dirs.Repos[0]), "git", "clone", "-q", f.remote, dir)
		run(t, dir, "git", "remote", "set-url", "origin", origin)
		if branch != "" {
			root = f.store.Dirs.Worktree(branch, "w")
			run(t, dir, "git", "worktree", "add", "-q", "-b", branch, root)
		}
		return dir, root
	}
	list := func(want ...Record) {
		t.Helper()
		recs, err := f.store.List(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(recs, want) {
			t.Fatalf("list %+v, want %+v", recs, want)
		}
	}
	known := func(by, name, src string) {
		t.Helper()
		if r, ok, err := f.store.Known(f.ctx, by); err != nil || !ok || r.Name != name || r.Source != src {
			t.Fatalf("known %s: %+v %v %v, want %s of %s", by, r, ok, err, name, src)
		}
	}
	lines := func() int { return strings.Count(logged.String(), "\n") }
	dot, one := clone("a.b", "/elsewhere/one.git", "one")
	del, two := clone("a\x7fb", "/elsewhere/two.git", "two")
	oneRec := Record{Repo: "a_b", Source: "/elsewhere/one.git", Branch: "one", Root: one}
	twoRec := Record{Repo: "a_b-f16526", Source: "/elsewhere/two.git", Branch: "two", Root: two}
	for range 2 {
		list(oneRec, twoRec)
	}
	line := strconv.Quote(del) + " is labelled a_b-f16526: the label its name makes is " + dot + "'s"
	if lines() != 1 || !strings.Contains(logged.String(), line) || !strings.Contains(logged.String(), "name") || strings.Contains(logged.String(), "\x7f") {
		t.Fatalf("logged %q, want once %q", logged.String(), line)
	}
	known("a_b", "a_b", "/elsewhere/one.git")
	known("a_b-f16526", "a_b-f16526", "/elsewhere/two.git")
	known("/elsewhere/two.git", "a_b-f16526", "/elsewhere/two.git")
	if rec, co, ok, err := f.store.Find(f.ctx, two); err != nil || !ok || co != del || rec != twoRec {
		t.Fatalf("find the second checkout's worktree: %+v %q %v %v", rec, co, ok, err)
	}
	// Another clone of the second repository that loses the label gets
	// the same hash, which is of the origin.
	_, again := clone("a;b", "/elsewhere/two.git", "again")
	againRec := Record{Repo: "a_b-f16526", Source: "/elsewhere/two.git", Branch: "again", Root: again}
	list(againRec, oneRec, twoRec)
	if lines() != 2 {
		t.Fatalf("logged %q", logged.String())
	}

	// A directory named a_b, of a third repository, keeps the label from
	// all of them, and each is listed under a label of its own.
	clone("a_b", "/elsewhere/three.git", "")
	oneRec.Repo = "a_b-caef38"
	list(againRec, oneRec, twoRec)
	if lines() != 5 || !strings.Contains(logged.String(), dot+" is labelled a_b-caef38") {
		t.Fatalf("logged %q", logged.String())
	}
	known("a_b", "a_b", "/elsewhere/three.git")
	// A clone of the third repository under another name shares it.
	_, three := clone("a+b", "/elsewhere/three.git", "three")
	list(againRec, oneRec, Record{Repo: "a_b", Source: "/elsewhere/three.git", Branch: "three", Root: three}, twoRec)
	if lines() != 5 {
		t.Fatalf("logged %q", logged.String())
	}

	// A made label that is the config's name for another repository,
	// which has no checkout here, gets the hash too, and Known by that
	// name is the config's.
	f.store.SetRepos(append(f.store.Repos(), Repo{Source: "/elsewhere/four.git", Name: "c_d"}))
	_, cd := clone("c.d", "/elsewhere/hand.git", "cd")
	list(againRec, Record{Repo: "c_d-06a376", Source: "/elsewhere/hand.git", Branch: "cd", Root: cd}, oneRec, Record{Repo: "a_b", Source: "/elsewhere/three.git", Branch: "three", Root: three}, twoRec)
	if lines() != 6 || !strings.Contains(logged.String(), " is labelled c_d-06a376: the label its name makes is this host's config's name for /elsewhere/four.git") {
		t.Fatalf("logged %q", logged.String())
	}
	known("c_d", "c_d", "/elsewhere/four.git")
	// A directory named c_d, of a fifth repository, keeps that name but
	// does not take the label from the config: c.d is still hashed
	// against the config's name, and nothing new is logged.
	clone("c_d", "/elsewhere/five.git", "")
	list(againRec, Record{Repo: "c_d-06a376", Source: "/elsewhere/hand.git", Branch: "cd", Root: cd}, oneRec, Record{Repo: "a_b", Source: "/elsewhere/three.git", Branch: "three", Root: three}, twoRec)
	if lines() != 6 {
		t.Fatalf("logged %q", logged.String())
	}
}

// The ssh and https forms of one hosted repository are one repository:
// a checkout cloned over one is found for an add over the other, which
// fetches with the checkout's own origin, and the store's lookups take
// either form. git's insteadOf points both at the fixture's remote.
func TestSourceFormsShareCheckout(t *testing.T) {
	f := newFixture(t)
	ssh, https := "git@example.com:laat/proj.git", "https://example.com/laat/proj"
	global := filepath.Join(t.TempDir(), "gitconfig")
	write(t, global, fmt.Sprintf("[url %q]\n\tinsteadOf = %s\n\tinsteadOf = %s\n", f.remote, ssh, https))
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	store := New(f.store.Dirs, nil)
	first, err := store.Add(f.ctx, Repo{Source: ssh, Name: "proj"}, "one", nil)
	if err != nil {
		t.Fatal(err)
	}
	var steps []step
	second, err := store.Add(f.ctx, Repo{Source: https, Name: "renamed"}, "two", func(stage, state, detail string) {
		steps = append(steps, step{stage, state, detail})
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Checkout != first.Checkout || !hasStep(steps, protocol.StageClone, protocol.StateSkip, "checkout exists") {
		t.Fatalf("second add: %+v steps %+v", second, steps)
	}
	if got := strings.TrimSpace(run(t, first.Checkout, "git", "config", "--get", "remote.origin.url")); got != ssh {
		t.Fatalf("origin changed to %s", got)
	}
	if co, ok, err := store.Checkout(f.ctx, Repo{Source: "ssh://git@EXAMPLE.com/laat/proj.git"}); err != nil || !ok || co != first.Checkout {
		t.Fatalf("checkout by a third form: %s %v %v", co, ok, err)
	}
	listed := New(f.store.Dirs, []config.Repo{{Source: https, Name: "mine"}})
	recs, err := listed.List(f.ctx)
	if err != nil || len(recs) != 2 || recs[0].Repo != "mine" || recs[0].Source != https {
		t.Fatalf("list with the other form listed: %+v %v", recs, err)
	}
	if r, ok := listed.Repo(ssh); !ok || r.Name != "mine" {
		t.Fatalf("repo by the other form: %+v %v", r, ok)
	}
}

// A checkout is asked only when one of its worktrees can be under the
// worktrees directory: git is not run for one with none, or with all of
// them elsewhere, which a repos directory of many checkouts relies on.
func TestCheckoutWithoutWorktreesNotAsked(t *testing.T) {
	f := newFixture(t)
	base := filepath.Dir(f.store.Dirs.Repos[0])
	c := filepath.Join(f.store.Dirs.Repos[0], "plain")
	run(t, base, "git", "clone", "-q", f.remote, c)
	if f.store.linked(c) {
		t.Fatal("a fresh clone is asked")
	}
	run(t, c, "git", "worktree", "add", "-q", "--detach", filepath.Join(base, "elsewhere"))
	if f.store.linked(c) {
		t.Fatal("a clone whose only worktree is elsewhere is asked")
	}
	// A relative gitdir is asked about: a symlink on the way to the
	// checkout can make it point elsewhere than it looks.
	admin := filepath.Join(c, ".git", "worktrees", "rel")
	write(t, filepath.Join(admin, "gitdir"), "../../../../somewhere/.git\n")
	if !f.store.linked(c) {
		t.Fatal("a relative gitdir is not asked about")
	}
	if err := os.RemoveAll(admin); err != nil {
		t.Fatal(err)
	}
	inside := f.store.Dirs.Worktree("plain", "x")
	run(t, c, "git", "worktree", "add", "-q", "--detach", inside)
	if !f.store.linked(c) {
		t.Fatal("a clone with a worktree under the directory is not asked")
	}
	// Deleted by hand, the worktree is still registered, and asked
	// about: rm prunes it.
	if err := os.RemoveAll(inside); err != nil {
		t.Fatal(err)
	}
	if !f.store.linked(c) {
		t.Fatal("a clone with a prunable worktree under the directory is not asked")
	}
}

// Two clones of one repository each list their worktrees, and rm's
// lookup by branch asks both; a root two checkouts register, one after
// the other's directory was deleted by hand, is listed once.
func TestDuplicateClones(t *testing.T) {
	f := newFixture(t)
	first, _, err := f.add("one")
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(f.store.Dirs.Repos[0], "proj2")
	run(t, filepath.Dir(f.store.Dirs.Repos[0]), "git", "clone", "-q", f.remote, second)
	topic := f.store.Dirs.Worktree("proj2", "topic")
	run(t, second, "git", "worktree", "add", "-q", "-b", "topic", topic)
	rec, co, ok, err := f.store.ByBranch(f.ctx, f.repo, "topic")
	if err != nil || !ok || co != second || rec.Root != topic {
		t.Fatalf("by branch in the second clone: %+v %s %v %v", rec, co, ok, err)
	}
	if _, co, ok, err := f.store.ByBranch(f.ctx, f.repo, "none"); err != nil || ok || co != first.Checkout {
		t.Fatalf("by a branch nowhere: %s %v %v", co, ok, err)
	}
	// The branch in both clones: which is meant is not known.
	both := f.store.Dirs.Worktree("proj2", "one")
	run(t, second, "git", "worktree", "add", "-q", "-b", "one", both)
	if _, _, ok, err := f.store.ByBranch(f.ctx, f.repo, "one"); ok || err == nil || !strings.Contains(err.Error(), "two clones") {
		t.Fatalf("a branch in both clones: %v %v", ok, err)
	}
	// A registration in the first clone whose directory was deleted by
	// hand does not compete with the live one in the second.
	stale := f.store.Dirs.Worktree("proj", "stale")
	run(t, first.Checkout, "git", "worktree", "add", "-q", "-b", "stale", stale)
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}
	live := f.store.Dirs.Worktree("proj2", "stale")
	run(t, second, "git", "worktree", "add", "-q", "-b", "stale", live)
	if rec, co, ok, err := f.store.ByBranch(f.ctx, f.repo, "stale"); err != nil || !ok || co != second || rec.Root != live {
		t.Fatalf("a stale registration and a live worktree: %+v %s %v %v", rec, co, ok, err)
	}
	run(t, second, "git", "worktree", "remove", live)
	run(t, first.Checkout, "git", "worktree", "prune")
	run(t, second, "git", "worktree", "remove", both)
	run(t, second, "git", "branch", "-D", "one")
	// The first clone's worktree deleted by hand, and its root taken by
	// the second clone for the same branch: both register it, and it is
	// the second's, listed once and found by branch without ambiguity.
	if err := os.RemoveAll(first.Root); err != nil {
		t.Fatal(err)
	}
	run(t, second, "git", "worktree", "add", "-q", "-b", "one", first.Root)
	if rec, co, ok, err := f.store.ByBranch(f.ctx, f.repo, "one"); err != nil || !ok || co != second || rec.Root != first.Root {
		t.Fatalf("by branch at a root two checkouts register: %+v %s %v %v", rec, co, ok, err)
	}
	recs, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]Record{}
	for _, r := range recs {
		roots[r.Root] = r
	}
	if r := roots[first.Root]; len(recs) != 2 || r.Repo != "proj" || r.Branch != "one" || roots[topic].Branch != "topic" {
		t.Fatalf("list %+v", recs)
	}
	// It is the second clone's, which the worktree points back to: Find
	// names that checkout.
	rec, co, ok, err = f.store.Find(f.ctx, first.Root)
	if err != nil || !ok || co != second || rec.Branch != "one" {
		t.Fatalf("find a root two checkouts register: %+v %s %v %v", rec, co, ok, err)
	}
	// That directory deleted by hand too: both registrations are stale,
	// and it is still one worktree for the branch, removed through the
	// first, the other left prunable.
	if err := os.RemoveAll(first.Root); err != nil {
		t.Fatal(err)
	}
	rec, co, ok, err = f.store.ByBranch(f.ctx, f.repo, "one")
	if err != nil || !ok || rec.Root != first.Root {
		t.Fatalf("by branch with both registrations stale: %+v %s %v %v", rec, co, ok, err)
	}
	if removed, err := Remove(f.ctx, co, first.Root, true); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if recs, err := f.store.List(f.ctx); err != nil || len(recs) != 1 || recs[0].Root != topic {
		t.Fatalf("list after: %+v %v", recs, err)
	}
}

// A label two repositories answer to, the config's name and a checkout
// the config does not list, is refused rather than guessed; the source
// still finds each.
func TestKnownAmbiguousLabel(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.add("one"); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.store.Dirs.Repos[0], "renamed")
	run(t, filepath.Dir(f.store.Dirs.Repos[0]), "git", "clone", "-q", f.remote, other)
	run(t, other, "git", "remote", "set-url", "origin", "/elsewhere/other.git")
	// The listed repository's checkout is proj; the unlisted one's
	// directory is renamed to take the listed name.
	if err := os.Rename(f.checkout(), filepath.Join(f.store.Dirs.Repos[0], "listed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, filepath.Join(f.store.Dirs.Repos[0], "proj")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.Known(f.ctx, "proj"); err == nil || !strings.Contains(err.Error(), "names both") {
		t.Fatalf("ambiguous label: %v", err)
	}
	for src, name := range map[string]string{f.remote: "proj", "/elsewhere/other.git": "proj"} {
		if r, ok, err := f.store.Known(f.ctx, src); err != nil || !ok || r.Source != src || r.Name != name {
			t.Fatalf("known %s: %+v %v %v", src, r, ok, err)
		}
	}
}

// A registered root whose .git cannot be told apart as a worktree's is
// an error in the lookups rm removes through, not a guess at its clone;
// the listing, which fails as a whole on an error, still lists it for
// its checkout. A root that is gone is still found for rm to prune.
func TestUnreadableDotGitIsAnError(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.add("one")
	if err != nil {
		t.Fatal(err)
	}
	dotgit := filepath.Join(a.Root, ".git")
	good, err := os.ReadFile(dotgit)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dotgit, "not a gitdir line\n")
	if recs, err := f.store.List(f.ctx); err != nil || len(recs) != 1 || recs[0].Root != a.Root {
		t.Fatalf("list with a malformed .git: %+v %v", recs, err)
	}
	if _, _, ok, err := f.store.Find(f.ctx, a.Root); err == nil || ok {
		t.Fatalf("find with a malformed .git: %v %v", ok, err)
	}
	if _, _, ok, err := f.store.ByBranch(f.ctx, f.repo, "one"); err == nil || ok {
		t.Fatalf("by branch with a malformed .git: %v %v", ok, err)
	}
	write(t, dotgit, string(good))
	if err := os.RemoveAll(a.Root); err != nil {
		t.Fatal(err)
	}
	if _, co, ok, err := f.store.Find(f.ctx, a.Root); err != nil || !ok || co != a.Checkout {
		t.Fatalf("find a root that is gone: %s %v %v", co, ok, err)
	}
}

// GitEnv drops every variable this machine's git rev-parse
// --local-env-vars prints, and GIT_INTERNAL_SUPER_PREFIX, which gits
// before 2.40 print, except GIT_CONFIG_PARAMETERS and GIT_CONFIG_COUNT,
// and keeps every other variable of the process. A variable a later git
// adds fails it until gitenv.Local has it.
func TestGitEnv(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cmd := exec.Command("git", "rev-parse", "--local-env-vars")
	cmd.Dir = t.TempDir()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	local := append(strings.Fields(string(out)), "GIT_INTERNAL_SUPER_PREFIX")
	if !slices.Contains(local, "GIT_DIR") {
		t.Fatalf("git rev-parse --local-env-vars: %q", out)
	}
	for _, k := range local {
		t.Setenv(k, "x")
	}
	t.Setenv("GIT_CONFIG_KEY_0", "a.b")
	t.Setenv("GIT_CONFIG_VALUE_0", "c")
	t.Setenv("GIT_CEILING_DIRECTORIES", "/x")
	got := map[string]string{}
	for _, kv := range GitEnv() {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for _, k := range local {
		_, ok := got[k]
		if kept := k == "GIT_CONFIG_PARAMETERS" || k == "GIT_CONFIG_COUNT"; ok != kept {
			t.Errorf("%s in the environment: %v", k, ok)
		}
	}
	for k, v := range map[string]string{"GIT_CONFIG_KEY_0": "a.b", "GIT_CONFIG_VALUE_0": "c", "GIT_CEILING_DIRECTORIES": "/x", "GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C"} {
		if got[k] != v {
			t.Errorf("%s=%q in the environment, want %q", k, got[k], v)
		}
	}
}

// An add run with another repository's GIT_DIR, work tree and index in
// its environment, as a hook's or a shell's can have them, clones,
// branches and adds the worktree in the store's checkout, and leaves the
// other repository as it was.
func TestAddRepoEnv(t *testing.T) {
	f := newFixture(t)
	// A clone of the same remote, so each step would succeed there too.
	other := filepath.Join(t.TempDir(), "other")
	run(t, "", "git", "clone", "-q", f.remote, other)
	state := func() string {
		return run(t, other, "git", "for-each-ref") + run(t, other, "git", "worktree", "list", "--porcelain") + run(t, other, "git", "status", "--porcelain")
	}
	before := state()
	var a Added
	t.Run("env", func(t *testing.T) {
		t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
		t.Setenv("GIT_WORK_TREE", other)
		t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
		var err error
		if a, _, err = f.add("task"); err != nil {
			t.Fatal(err)
		}
	})
	if after := state(); after != before {
		t.Errorf("the other repository changed:\n%s\nnow:\n%s", before, after)
	}
	if a.Checkout != f.store.Dirs.Checkout("proj") || a.Root != f.store.Dirs.Worktree("proj", "task") {
		t.Fatalf("added %+v", a)
	}
	entries, err := ListWorktrees(f.ctx, a.Checkout)
	if err != nil || !slices.ContainsFunc(entries, func(e Entry) bool { return e.Root == a.Root && e.Branch == "task" }) {
		t.Fatalf("the checkout's worktrees: %+v %v", entries, err)
	}
	if st := run(t, a.Root, "git", "status", "--porcelain", "--untracked-files=no"); st != "" {
		t.Errorf("the worktree's status: %q", st)
	}
}

// A host's repos is a list of directories (#339), each scanned as the
// one was. A checkout in a group directory listed after the directory
// it is in is the repository's: add makes the worktree from it and
// clones no second copy, a worktree made from it by hand is listed, and
// the lookups find it. A checkout in the first directory is listed
// before it. A directory listed twice is scanned once, and one that is
// not there is passed over.
func TestSeveralReposDirs(t *testing.T) {
	f := newFixture(t)
	base := filepath.Dir(f.store.Dirs.Repos[0])
	code := filepath.Join(base, "code")
	group := filepath.Join(code, "group")
	f.store.Dirs.Repos = []string{code, group}
	nested := filepath.Join(group, "service")
	run(t, base, "git", "clone", "-q", f.remote, nested)
	a, steps, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	if a.Checkout != nested || a.Root != f.store.Dirs.Worktree("proj", "task") || !hasStep(steps, protocol.StageClone, protocol.StateSkip, "checkout exists") {
		t.Fatalf("added %+v, steps %+v", a, steps)
	}
	if _, err := os.Stat(f.store.Dirs.Checkout("proj")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a second copy at %s: %v", f.store.Dirs.Checkout("proj"), err)
	}
	hand := filepath.Join(f.store.Dirs.Worktrees, "by-hand")
	run(t, nested, "git", "worktree", "add", "-q", "-b", "hand", hand)
	other := filepath.Join(code, "other")
	run(t, base, "git", "clone", "-q", f.remote, other)
	run(t, other, "git", "remote", "set-url", "origin", "/elsewhere/other.git")
	recs := []Record{
		{Repo: "proj", Source: f.remote, Branch: "hand", Root: hand},
		{Repo: "proj", Source: f.remote, Branch: "task", Root: a.Root},
	}
	mains := []Record{
		{Repo: "other", Source: "/elsewhere/other.git", Branch: "main", Root: other, Main: true},
		{Repo: "proj", Source: f.remote, Branch: "main", Root: nested, Main: true, Configured: true, Linked: true},
	}
	for _, dirs := range [][]string{{code, group}, {filepath.Join(base, "absent"), code, group, code + "/"}} {
		f.store.Dirs.Repos = dirs
		gotRecs, gotMains, err := f.store.ListAll(f.ctx)
		if err != nil || !slices.Equal(gotRecs, recs) || !slices.Equal(gotMains, mains) {
			t.Fatalf("%q: worktrees %+v\nmains %+v\n%v", dirs, gotRecs, gotMains, err)
		}
	}
	if rec, co, ok, err := f.store.Find(f.ctx, hand); err != nil || !ok || co != nested || rec != recs[0] {
		t.Fatalf("find: %+v %s %v %v", rec, co, ok, err)
	}
	if rec, co, ok, err := f.store.ByBranch(f.ctx, f.repo, "task"); err != nil || !ok || co != nested || rec != recs[1] {
		t.Fatalf("by branch: %+v %s %v %v", rec, co, ok, err)
	}
	if r, ok, err := f.store.Known(f.ctx, "other"); err != nil || !ok || r.Source != "/elsewhere/other.git" {
		t.Fatalf("known other: %+v %v %v", r, ok, err)
	}
	if ok, err := f.store.IsCheckout(f.ctx, nested); err != nil || !ok {
		t.Fatalf("is checkout: %v %v", ok, err)
	}
}

// A repository found in none of the repos directories is cloned into
// the first, made when it is not there, whatever the others hold, and
// the next add finds it there.
func TestCloneIntoFirstReposDir(t *testing.T) {
	f := newFixture(t)
	base := filepath.Dir(f.store.Dirs.Repos[0])
	first, second := filepath.Join(base, "first"), filepath.Join(base, "second")
	other := filepath.Join(second, "other")
	run(t, base, "git", "clone", "-q", f.remote, other)
	run(t, other, "git", "remote", "set-url", "origin", "/elsewhere/other.git")
	f.store.Dirs.Repos = []string{first, second}
	a, steps, err := f.add("task")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(first, "proj")
	if a.Checkout != want || f.checkout() != want || !hasStep(steps, protocol.StageClone, protocol.StateStart, "git clone "+f.remote+" "+want) {
		t.Fatalf("added %+v, steps %+v", a, steps)
	}
	if _, err := os.Stat(filepath.Join(second, "proj")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a clone in the second directory: %v", err)
	}
	if _, steps, err := f.add("next"); err != nil || !hasStep(steps, protocol.StageClone, protocol.StateSkip, "checkout exists") {
		t.Fatalf("the next add: %v %+v", err, steps)
	}
}

// Checkouts of two repositories named alike in two repos directories:
// the one in the earlier directory keeps the plain label, the later one
// gets the hash of its origin after it, logged once, as a made label
// does, and a clone of the first repository in a third directory shares
// the plain label. The directories' order decides. A made label in an
// earlier directory still gives way to a directory named as the label
// in a later one.
func TestReposDirsLabelCollision(t *testing.T) {
	f := newFixture(t)
	var logged bytes.Buffer
	f.store.Log = log.New(&logged, "", 0)
	base := filepath.Dir(f.store.Dirs.Repos[0])
	a, b, c := filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "c")
	f.store.Dirs.Repos = []string{a, b, c}
	clone := func(in, name, origin, branch string) (dir, root string) {
		t.Helper()
		dir = filepath.Join(in, name)
		run(t, base, "git", "clone", "-q", f.remote, dir)
		run(t, dir, "git", "remote", "set-url", "origin", origin)
		root = f.store.Dirs.Worktree(branch, "w")
		run(t, dir, "git", "worktree", "add", "-q", "-b", branch, root)
		return dir, root
	}
	list := func(want ...Record) {
		t.Helper()
		recs, err := f.store.List(f.ctx)
		if err != nil || !slices.Equal(recs, want) {
			t.Fatalf("list %+v, want %+v: %v", recs, want, err)
		}
	}
	known := func(by, name, src string) {
		t.Helper()
		if r, ok, err := f.store.Known(f.ctx, by); err != nil || !ok || r.Name != name || r.Source != src {
			t.Fatalf("known %s: %+v %v %v, want %s of %s", by, r, ok, err, name, src)
		}
	}
	apiA, one := clone(a, "api", "/elsewhere/one.git", "one")
	apiB, two := clone(b, "api", "/elsewhere/two.git", "two")
	_, again := clone(c, "api", "/elsewhere/one.git", "again")
	oneRec := Record{Repo: "api", Source: "/elsewhere/one.git", Branch: "one", Root: one}
	twoRec := Record{Repo: "api-f16526", Source: "/elsewhere/two.git", Branch: "two", Root: two}
	againRec := Record{Repo: "api", Source: "/elsewhere/one.git", Branch: "again", Root: again}
	for range 2 {
		list(againRec, oneRec, twoRec)
	}
	line := apiB + " is labelled api-f16526: the label its name makes is " + apiA + "'s"
	if strings.Count(logged.String(), "\n") != 1 || !strings.Contains(logged.String(), line) {
		t.Fatalf("logged %q, want once %q", logged.String(), line)
	}
	known("api", "api", "/elsewhere/one.git")
	known("api-f16526", "api-f16526", "/elsewhere/two.git")
	if rec, co, ok, err := f.store.Find(f.ctx, two); err != nil || !ok || co != apiB || rec != twoRec {
		t.Fatalf("find the later checkout's worktree: %+v %q %v %v", rec, co, ok, err)
	}

	f.store.Dirs.Repos = []string{b, a, c}
	oneRec.Repo, againRec.Repo, twoRec.Repo = "api-caef38", "api-caef38", "api"
	list(againRec, oneRec, twoRec)
	known("api", "api", "/elsewhere/two.git")

	_, dot := clone(a, "next.js", "/elsewhere/hand.git", "dot")
	_, plain := clone(c, "next_js", "/elsewhere/five.git", "plain")
	list(againRec, Record{Repo: "next_js-06a376", Source: "/elsewhere/hand.git", Branch: "dot", Root: dot}, oneRec,
		Record{Repo: "next_js", Source: "/elsewhere/five.git", Branch: "plain", Root: plain}, twoRec)
}
