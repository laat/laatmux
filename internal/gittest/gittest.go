// Package gittest keeps the git a test run starts to the config of the
// test's own repositories and the run's, and checks that a package's
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
)

// Isolate makes every git this process starts, and every git those
// start, read no config but its repository's own and runConfig: no
// global or system config, whatever file the environment names for
// either, no config the environment carries itself, and not the global
// ignore and attributes files git reads with no config naming them. A
// developer's commit signing, hooks, ignores and the like stay out of a
// test's repositories, and a commit there takes its identity from its
// repository's config, on every machine. A package whose tests run git
// calls it from TestMain.
func Isolate() {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Unsetenv("GIT_CONFIG")
	os.Unsetenv("GIT_CONFIG_PARAMETERS")
	for i, kv := range runConfig {
		os.Setenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", i), kv[0])
		os.Setenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i), kv[1])
	}
	os.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(len(runConfig)))
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

// CheckIsolated runs commit, a commit in a repository the test makes
// with an identity in its config, in a run of this test binary with the
// top-level test t alone, and checks there that git reads runConfig and
// no other config outside a repository. That run's environment has
// none of this run's GIT_CONFIG, author and committer variables, and
// names config in each way git takes it from the environment: a global
// config that signs with a program that fails; a system config, config
// in GIT_CONFIG_PARAMETERS and in GIT_CONFIG_COUNT that each run a
// pre-commit hook that fails; a GIT_CONFIG file, which git config reads
// and writes in place of the repository's config, so the commit's
// identity would land there; and a global ignore file that ignores
// everything. Both pass when TestMain called Isolate. The global and the
// system config are first shown to fail a commit read alone, and a
// commit to succeed with neither.
func CheckIsolated(t *testing.T, commit func(t *testing.T)) {
	t.Helper()
	if os.Getenv(child) != "" {
		list := exec.Command("git", "config", "--list", "--show-scope")
		list.Dir = t.TempDir()
		out, err := list.Output()
		if err != nil {
			// git's reason, which Output keeps only in the error.
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				t.Fatalf("git config --list: %v\n%s", err, exit.Stderr)
			}
			t.Fatalf("git config --list: %v", err)
		}
		var got []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
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
		commit(t)
		return
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	global, system, hooks, xdg := filepath.Join(dir, "global"), filepath.Join(dir, "system"), filepath.Join(dir, "hooks"), filepath.Join(dir, "xdg")
	write(t, global, "[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = false\n", 0o644)
	// Quoted: a # or ; in the temporary directory's path starts a
	// comment in a value that is not.
	write(t, system, fmt.Sprintf("[core]\n\thooksPath = %q\n", hooks), 0o644)
	write(t, filepath.Join(hooks, "pre-commit"), "#!/bin/sh\nexit 1\n", 0o755)
	write(t, filepath.Join(xdg, "git", "ignore"), "*\n", 0o644)
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, "GIT_CONFIG") && !strings.HasPrefix(k, "GIT_AUTHOR_") && !strings.HasPrefix(k, "GIT_COMMITTER_") && k != "EMAIL" {
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
	run := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	run.Env = slices.Concat(env, []string{
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
		t.Errorf("the commit under config that fails every commit: %v\n%s", err, out)
	}
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
