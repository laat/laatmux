// Package gittest keeps the git a test run starts to the test's own
// repositories, their config and the run's, and checks that a package's
// TestMain does.
package gittest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/gitenv"
)

// Isolate makes every git this process starts, and every git those
// start, read no config but its repository's own and runConfig: no
// global or system config, whatever file the environment names for
// either, no config the environment carries itself, and not the global
// ignore and attributes files git reads with no config naming them. A
// developer's commit signing, hooks, ignores and the like stay out of a
// test's repositories, and a commit there takes its identity from its
// repository's config, on every machine. Nor does that git take a
// repository, work tree, index or objects from the environment: none of
// the variables of repoEnv, which a hook's environment can carry, is
// left, so a test run from a hook leaves the hook's repository and index
// alone. Nor is any of hookEnv: a commit takes no author, committer or
// date from a hook's environment or the user's shell, git init copies no
// template from there, a ref update is neither put under a namespace nor
// refused in a quarantine, and a clone, fetch or push by a repository's
// path is not refused for its transport. A package whose tests run git
// calls it from TestMain.
func Isolate() {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// After the two above, which keep a global or system config git
	// cannot parse from failing the git that lists the variables, and
	// before the run config below: the list has GIT_CONFIG and
	// GIT_CONFIG_PARAMETERS, and GIT_CONFIG_COUNT too.
	for _, k := range slices.Concat(repoEnv(), hookEnv) {
		os.Unsetenv(k)
	}
	for i, kv := range runConfig {
		os.Setenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", i), kv[0])
		os.Setenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i), kv[1])
	}
	os.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(len(runConfig)))
}

// repoEnv returns the variables git clears when it starts a git for
// another repository, the ones that name a repository, its work tree,
// index and objects and config that goes with them: gitenv.Local, which
// Isolate unsets whatever the git on the machine prints, and what git
// rev-parse --local-env-vars prints on the machine, which has any
// variable a later git adds. Every git the tests run with prints that
// outside a repository: git 2.8.2 made it work there, and Isolate's
// GIT_CONFIG_GLOBAL takes 2.32.
func repoEnv() []string {
	// Isolate runs before m.Run starts the test timeout, so the deadline
	// is the only bound on a git that hangs; WaitDelay bounds the wait
	// for a process it started that holds the output pipe after git is
	// killed.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--local-env-vars")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return gitenv.Local
	}
	return slices.Concat(gitenv.Local, strings.Fields(string(out)))
}

// hookEnv is what else a hook's environment, git submodule's or the
// user's shell can carry that changes what a test's git does. git
// exports the author variables to the hooks git commit runs, and they
// and the committer's win over a repository's user.name and user.email,
// user.useConfigOnly or not. GIT_TEMPLATE_DIR has git init copy a
// template, its hooks too, into a test's repository. GIT_NAMESPACE puts
// the refs a push writes, and those a fetch reads, under a namespace.
// git exports GIT_QUARANTINE_PATH to the pre-receive hook, and it
// refuses every ref update. GIT_ALLOW_PROTOCOL, which a user can keep in
// the shell, refuses every transport it does not list, a path's too.
// GIT_PROTOCOL_FROM_USER=0, which git submodule gives every git and hook
// it runs and the commands of git submodule foreach, refuses a path's
// where protocol.file.allow defaults to user: from git 2.38.1, and from
// the release of each older line with the same security fix, 2.32.4
// among them. Isolate unsets these, and CheckIsolated's child run starts
// with each of them set.
var hookEnv = []string{
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
	"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE",
	"GIT_TEMPLATE_DIR", "GIT_NAMESPACE", "GIT_QUARANTINE_PATH",
	"GIT_ALLOW_PROTOCOL", "GIT_PROTOCOL_FROM_USER",
}

// hookIdentity is the author and committer CheckIsolated's child run
// starts with in the environment, as a hook's environment or the user's
// shell carries them, in the order of git log's %an, %ae, %ad, %cn, %ce
// and %cd, the dates as --date=raw prints them: a commit that has one of
// them took it from there.
var hookIdentity = [][2]string{
	{"GIT_AUTHOR_NAME", "hook"},
	{"GIT_AUTHOR_EMAIL", "hook@example.com"},
	{"GIT_AUTHOR_DATE", "1234567890 +0000"},
	{"GIT_COMMITTER_NAME", "hook"},
	{"GIT_COMMITTER_EMAIL", "hook@example.com"},
	{"GIT_COMMITTER_DATE", "1234567890 +0000"},
}

// runConfig is the config Isolate gives every git of the run, in the
// environment, above a repository's own: an identity is never guessed
// from the user and host names, which works on one machine and not on
// another, and the global ignore and attributes files are none. Read
// last, it wins over a repository's or a test's own global config of
// the same keys: a test that needs one of its own sets GIT_CONFIG_COUNT
// and its keys and values itself, with t.Setenv.
var runConfig = [][2]string{
	{"user.useConfigOnly", "true"},
	{"core.excludesFile", os.DevNull},
	{"core.attributesFile", os.DevNull},
}

// child marks the run of the test binary CheckIsolated starts.
const child = "LAATMUX_TEST_GIT_ISOLATED"

// CheckIsolated runs commit in a run of this test binary with the
// top-level test t alone, and checks there that git reads runConfig and
// no other config outside a repository. commit makes a commit, with its
// pre-commit hook run, in a repository the test makes with an identity
// in its config, pushes it by path to the branch another repository's
// HEAD names, and returns that repository's path. That run's
// environment has none of this run's GIT_CONFIG variables or of repoEnv
// and hookEnv, and names config in each way git takes it from the
// environment: a global config that signs with a program that fails; a
// system config, config in GIT_CONFIG_PARAMETERS and in GIT_CONFIG_COUNT
// that each run a pre-commit hook that fails; a GIT_CONFIG file, which
// git config reads and writes in place of the repository's config, so
// the commit's identity would land there; and a global ignore file that
// ignores everything. It sets every other variable of gitenv.Local too,
// as a hook's environment can carry them, each to a path in a directory
// of their own where nothing is: none of them is left in the
// environment there, and the commit leaves that directory empty. And it
// sets those of hookEnv: hookIdentity, which the pushed commit must not
// have; a template directory with that failing pre-commit hook; a
// namespace, under which the push would write refs that the HEAD of the
// repository pushed to does not find; a quarantine; and both ways to
// refuse the push's transport. All of it passes when TestMain called
// Isolate. The global and the system config are first shown to fail a
// commit read alone, and a commit to succeed with neither.
func CheckIsolated(t *testing.T, commit func(t *testing.T) string) {
	t.Helper()
	if os.Getenv(child) != "" {
		// GIT_CONFIG_COUNT is set again, for runConfig.
		for _, k := range slices.Concat(gitenv.Local, hookEnv) {
			if v, ok := os.LookupEnv(k); ok && k != "GIT_CONFIG_COUNT" {
				t.Errorf("%s=%s is in the environment", k, v)
			}
		}
		var got []string
		for _, line := range strings.Split(strings.TrimSpace(output(t, t.TempDir(), "config", "--list", "--show-scope")), "\n") {
			if scope, _, _ := strings.Cut(line, "\t"); line != "" && scope != "local" && scope != "worktree" {
				got = append(got, line)
			}
		}
		// Spelled out, not made from runConfig: an entry gone from it
		// is gone from here only by hand.
		want := []string{"command\tuser.useconfigonly=true", "command\tcore.excludesfile=/dev/null", "command\tcore.attributesfile=/dev/null"}
		if !slices.Equal(got, want) {
			t.Errorf("git reads %q, not %q", got, want)
		}
		repo := commit(t)
		ident := strings.Split(strings.TrimSuffix(output(t, repo, "log", "-1", "--date=raw", "--format=%an%n%ae%n%ad%n%cn%n%ce%n%cd"), "\n"), "\n")
		if len(ident) != len(hookIdentity) {
			t.Fatalf("git log prints %q, not an author and a committer", ident)
		}
		for i, kv := range hookIdentity {
			if ident[i] == kv[1] {
				t.Errorf("the commit took %s=%s from the environment", kv[0], kv[1])
			}
		}
		return
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	global, system, template, xdg := filepath.Join(dir, "global"), filepath.Join(dir, "system"), filepath.Join(dir, "template"), filepath.Join(dir, "xdg")
	// The template's hooks, which git init copies into a repository.
	hooks := filepath.Join(template, "hooks")
	write(t, global, "[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = false\n", 0o644)
	// Quoted: a # or ; in the temporary directory's path starts a
	// comment in a value that is not.
	write(t, system, fmt.Sprintf("[core]\n\thooksPath = %q\n", hooks), 0o644)
	// Its path tells a failure by core.hooksPath, which runs it where it
	// is, from one by the template, which runs a copy in the repository.
	write(t, filepath.Join(hooks, "pre-commit"), "#!/bin/sh\necho \"$0 ran\" >&2\nexit 1\n", 0o755)
	write(t, filepath.Join(xdg, "git", "ignore"), "*\n", 0o644)
	// Without repoEnv's and hookEnv's variables for the git here as well:
	// in a package whose TestMain does not call Isolate, run from a hook,
	// the init and the commits below would be the hook's repository's, or
	// refused in its quarantine.
	var env []string
	local := slices.Concat(repoEnv(), hookEnv)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, "GIT_CONFIG") && k != "EMAIL" && !slices.Contains(local, k) {
			env = append(env, kv)
		}
	}
	neither := []string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"}
	repo := filepath.Join(dir, "repo")
	mk := exec.Command("git", "init", "-q", repo)
	mk.Env = slices.Concat(env, neither)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for _, c := range []struct {
		what string
		env  []string
		ok   bool
	}{
		{"the global config alone", []string{"GIT_CONFIG_GLOBAL=" + global, "GIT_CONFIG_NOSYSTEM=1"}, false},
		{"the system config alone", []string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + system}, false},
		{"neither config", neither, true},
	} {
		cmd := exec.Command("git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "c")
		cmd.Env = slices.Concat(env, c.env)
		if out, err := cmd.CombinedOutput(); (err == nil) != c.ok {
			t.Fatalf("a commit reading %s: %v\n%s", c.what, err, out)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// The repository variables a hook's environment can carry, each
	// naming a path in other that is not there. Left in place, GIT_DIR, GIT_COMMON_DIR,
	// GIT_INDEX_FILE and GIT_OBJECT_DIRECTORY have git write in other and
	// GIT_WORK_TREE fails git init; the run's look at its environment
	// finds any of them, those a commit in a new repository does not
	// notice as well.
	other := filepath.Join(dir, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	var hook []string
	for _, k := range gitenv.Local {
		if !strings.HasPrefix(k, "GIT_CONFIG") {
			hook = append(hook, k+"="+filepath.Join(other, k))
		}
	}
	// And hookEnv's. Left in place, the author and committer variables
	// are the commit's, the template's hook fails it, the push writes its
	// refs under the namespace, the quarantine refuses every ref update,
	// and either of the last two refuses the push to a path.
	for _, kv := range hookIdentity {
		hook = append(hook, kv[0]+"="+kv[1])
	}
	hook = append(hook, "GIT_TEMPLATE_DIR="+template, "GIT_NAMESPACE=hook", "GIT_QUARANTINE_PATH="+filepath.Join(other, "GIT_QUARANTINE_PATH"),
		"GIT_ALLOW_PROTOCOL=none", "GIT_PROTOCOL_FROM_USER=0")
	run := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	run.Env = slices.Concat(env, hook, []string{
		child + "=1",
		"GIT_CONFIG_GLOBAL=" + global,
		"GIT_CONFIG_SYSTEM=" + system,
		"GIT_CONFIG=" + system,
		"GIT_CONFIG_PARAMETERS=" + sq("core.hooksPath") + "=" + sq(hooks),
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=" + hooks,
		"XDG_CONFIG_HOME=" + xdg,
	})
	// Killed at the deadline, the child's output is not waited for past
	// a moment: something it left running may hold the pipe.
	run.WaitDelay = 5 * time.Second
	out, err := run.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: "+t.Name()+" ") {
		t.Errorf("the commit under config that fails every commit and a hook's environment: %v\n%s", err, out)
	}
	left, err := os.ReadDir(other)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		t.Errorf("the commit wrote %s", filepath.Join(other, e.Name()))
	}
}

// output runs git with args in dir and returns what it prints, and
// fails t with git's reason, which Output keeps only in the error, if
// git fails.
func output(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, exit.Stderr)
		}
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// sq quotes s for GIT_CONFIG_PARAMETERS, as a shell would in single
// quotes.
func sq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
