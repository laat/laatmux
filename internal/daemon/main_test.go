package daemon

import (
	"os"
	"os/exec"
	"testing"

	"github.com/laat/laatmux/internal/gittest"
)

// No test reaches the user's tmux: every server's socket, the default
// and the managed one included, is under a directory of the run's own,
// and the variables that name the user's session are cleared. A test
// that wants a server of its own sets TMUX_TMPDIR itself.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "lmxt")
	if err != nil {
		panic(err)
	}
	os.Setenv("TMUX_TMPDIR", dir)
	os.Unsetenv("TMUX")
	os.Unsetenv("TMUX_PANE")
	// Nor the user's git config: a commit in a test's repository is not
	// signed, runs no hook of theirs and takes the repository's identity.
	// Run from a hook, it lands in that repository as well, not in the
	// one the hook's environment names.
	gittest.Isolate()
	code := m.Run()
	for _, name := range []string{"default", "laatmux"} {
		exec.Command("tmux", "-L", name, "kill-server").Run()
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// The store's seed commit succeeds in an environment that names config
// that fails every commit, with an identity from its repository's
// config alone, and leaves alone the repository and index the
// environment names as well.
func TestGitIsolated(t *testing.T) {
	gittest.CheckIsolated(t, func(t *testing.T) { newStore(t) })
}
