package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

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
	if _, _, ok, _ := f.store.Find(f.ctx, filepath.Join(filepath.Dir(f.store.Dirs.Repos), "outside")); ok {
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
	s := New(config.Dirs{Repos: "/r", Worktrees: "/w/trees/"}, nil)
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
	s := New(config.Dirs{Repos: base, Worktrees: filepath.Join(base, "alias")}, nil)
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
	outside := filepath.Join(filepath.Dir(f.store.Dirs.Repos), "outside")
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

// With the repos directory under the worktrees one, the main checkout
// satisfies Owns but is not a worktree: it is not listed.
func TestMainCheckoutNotListed(t *testing.T) {
	f := newFixture(t)
	f.store.Dirs.Repos = filepath.Join(f.store.Dirs.Worktrees, "checkouts")
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
		f.store.Repos = append(f.store.Repos, Repo{Source: fmt.Sprintf("/nowhere/%d.git", i), Name: fmt.Sprintf("r%d", i)})
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
	f.store.Repos[0].Copy = []string{"notes.txt", "config/*.local"}
	f.store.Repos[0].Setup = []string{"echo personal >> log"}
	repo := f.store.Repos[0]
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
	f.store.Repos[0].Setup = []string{"echo first >> log", "echo personal >> log"}
	repo = f.store.Repos[0]
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
	other := filepath.Join(f.store.Dirs.Repos, "other")
	run(t, f.store.Dirs.Repos, "git", "clone", "-q", f.remote, other)
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
	base := filepath.Dir(f.store.Dirs.Repos)
	c := filepath.Join(f.store.Dirs.Repos, "plain")
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
	second := filepath.Join(f.store.Dirs.Repos, "proj2")
	run(t, filepath.Dir(f.store.Dirs.Repos), "git", "clone", "-q", f.remote, second)
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
	run(t, second, "git", "worktree", "remove", both)
	run(t, second, "git", "branch", "-D", "one")
	// The first clone's worktree deleted by hand, and its root taken by
	// the second clone: both register it, and it is listed once.
	if err := os.RemoveAll(first.Root); err != nil {
		t.Fatal(err)
	}
	run(t, second, "git", "worktree", "add", "-q", "-b", "again", first.Root)
	recs, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]Record{}
	for _, r := range recs {
		roots[r.Root] = r
	}
	if r := roots[first.Root]; len(recs) != 2 || r.Repo != "proj" || r.Branch != "again" || roots[topic].Branch != "topic" {
		t.Fatalf("list %+v", recs)
	}
	// It is the second clone's, which the worktree points back to: Find
	// names that checkout, and git removes it from there.
	rec, co, ok, err = f.store.Find(f.ctx, first.Root)
	if err != nil || !ok || co != second || rec.Branch != "again" {
		t.Fatalf("find a root two checkouts register: %+v %s %v %v", rec, co, ok, err)
	}
	if removed, err := Remove(f.ctx, co, first.Root, true); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
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
	other := filepath.Join(f.store.Dirs.Repos, "renamed")
	run(t, filepath.Dir(f.store.Dirs.Repos), "git", "clone", "-q", f.remote, other)
	run(t, other, "git", "remote", "set-url", "origin", "/elsewhere/other.git")
	// The listed repository's checkout is proj; the unlisted one's
	// directory is renamed to take the listed name.
	if err := os.Rename(f.checkout(), filepath.Join(f.store.Dirs.Repos, "listed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, filepath.Join(f.store.Dirs.Repos, "proj")); err != nil {
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
