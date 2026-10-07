// Package gittest keeps the git a test run starts to the config of the
// test's own repositories, and checks that a package's TestMain does.
package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Isolate makes every git this process starts, and every git those
// start, read neither a global nor a system config, whatever the
// environment names: a developer's commit signing, hooks and the like
// stay out of a test's repositories, and a commit there takes its
// identity from its repository's config. A package whose tests run git
// calls it from TestMain.
func Isolate() {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// child marks the run of the test binary CheckIsolated starts.
const child = "LAATMUX_TEST_GIT_ISOLATED"

// CheckIsolated runs commit, a commit in a repository the test makes
// with an identity in its config, in a run of this test binary with the
// top-level test t alone. That run's environment names a global config
// that signs every commit with a program that fails and a system config
// that runs a pre-commit hook that fails, and has git take a commit's
// identity from config only; the GIT_CONFIG, author and committer
// variables of this run's environment are gone from it. The commit
// succeeds there when TestMain called Isolate. Each config is first
// shown to fail a commit read alone, and a commit to succeed with
// neither.
func CheckIsolated(t *testing.T, commit func(t *testing.T)) {
	if os.Getenv(child) != "" {
		commit(t)
		return
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	global, system, hooks := filepath.Join(dir, "global"), filepath.Join(dir, "system"), filepath.Join(dir, "hooks")
	write(t, global, "[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = false\n", 0o644)
	write(t, system, "[core]\n\thooksPath = "+hooks+"\n", 0o644)
	write(t, filepath.Join(hooks, "pre-commit"), "#!/bin/sh\nexit 1\n", 0o755)
	env := []string{"GIT_CONFIG_GLOBAL=" + global, "GIT_CONFIG_SYSTEM=" + system, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.useConfigOnly", "GIT_CONFIG_VALUE_0=true"}
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
		{"the global config alone", []string{"GIT_CONFIG_NOSYSTEM=1"}, false},
		{"the system config alone", []string{"GIT_CONFIG_GLOBAL=" + os.DevNull}, false},
		{"neither config", neither, true},
	} {
		cmd := exec.Command("git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "c")
		cmd.Env = slices.Concat(env, c.env)
		if out, err := cmd.CombinedOutput(); (err == nil) != c.ok {
			t.Fatalf("a commit reading %s: %v\n%s", c.what, err, out)
		}
	}
	run := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	run.Env = slices.Concat(env, []string{child + "=1"})
	out, err := run.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: "+t.Name()+" ") {
		t.Errorf("the commit under configs that fail every commit: %v\n%s", err, out)
	}
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
