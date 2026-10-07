package worktree

import (
	"os"
	"testing"

	"github.com/laat/laatmux/internal/gittest"
)

// No test's git reads the user's git config: a commit in a test's
// repository is not signed, runs no hook of theirs and takes the
// repository's identity.
func TestMain(m *testing.M) {
	gittest.Isolate()
	os.Exit(m.Run())
}

// The fixture's commit succeeds in an environment that names config
// that fails every commit, with an identity from its repository's
// config alone.
func TestGitIsolated(t *testing.T) {
	gittest.CheckIsolated(t, func(t *testing.T) { newFixture(t) })
}
