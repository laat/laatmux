// Package gitenv names the variables of git's environment that tie a
// git to one repository.
package gitenv

// Local is what git 2.54 prints for git rev-parse --local-env-vars:
// the variables that name a repository, its work tree, index and
// objects, and config that goes with them. git clears them, but for
// GIT_CONFIG_PARAMETERS and GIT_CONFIG_COUNT, when it starts a git for
// another repository. A hook's environment can carry them, and so can a
// shell that exports them; left in place, they have git act on the
// repository they name, not on the one of the directory it runs in.
var Local = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_COUNT", "GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE",
	"GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE", "GIT_INDEX_FILE",
	"GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX",
	"GIT_SHALLOW_FILE", "GIT_COMMON_DIR",
}
