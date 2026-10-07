package worktree

import (
	"os"
	"testing"

	"github.com/laat/laatmux/internal/gittest"
)

// No test's git reads the user's git config or the repository a hook's
// environment names: a commit in a test's repository is not signed,
// runs no hook of theirs, takes the repository's identity and is that
// repository's, in a run a hook starts as well.
func TestMain(m *testing.M) {
	gittest.Isolate()
	os.Exit(m.Run())
}

// The fixture's commit succeeds in an environment that names config
// that fails every commit, with an identity from its repository's
// config alone, and leaves alone the repository and index the
// environment names as well.
func TestGitIsolated(t *testing.T) {
	gittest.CheckIsolated(t, func(t *testing.T) { newFixture(t) })
}
