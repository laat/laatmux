package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetupFile is the shared, committed setup at a repository root.
const SetupFile = ".laatmux.yaml"

// Setup is what a new worktree needs before an agent starts. It is read
// from the worktree after checkout, so a branch carries its own setup.
//
//	copy: [.envrc, .env.local]   # from the main checkout, skipped when present
//	setup: ["pnpm install"]      # each runs at least once; must tolerate a rerun
//
// Each Setup entry is one string run as `sh -c <string>` in the worktree
// root, with the author responsible for its quoting. Setup is at-least-once:
// a crash after a command's effects but before its completion marker reruns
// it on retry, so commands must tolerate a rerun and a partial earlier run.
type Setup struct {
	Copy  []string `yaml:"copy"`
	Setup []string `yaml:"setup"`
}

// SetupFieldError is a .laatmux.yaml entry that failed validation, with
// the field it belongs to, "copy" or "setup", so add can report the
// stage that failed rather than the one that read the file.
type SetupFieldError struct {
	Field string
	Err   error
}

func (e *SetupFieldError) Error() string { return SetupFile + ": " + e.Field + ": " + e.Err.Error() }
func (e *SetupFieldError) Unwrap() error { return e.Err }

// LoadSetup reads <root>/.laatmux.yaml. A missing file means nothing to
// copy and nothing to run. An entry that fails validation is returned as
// a *SetupFieldError naming its field.
func LoadSetup(root string) (Setup, error) {
	var s Setup
	b, err := os.ReadFile(filepath.Join(root, SetupFile))
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	if err := yaml.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", SetupFile, err)
	}
	for _, p := range s.Copy {
		if err := checkCopyPath(p); err != nil {
			return s, &SetupFieldError{Field: "copy", Err: err}
		}
	}
	for i, cmd := range s.Setup {
		if strings.TrimSpace(cmd) == "" {
			return s, &SetupFieldError{Field: "setup", Err: fmt.Errorf("entry %d is empty", i+1)}
		}
	}
	return s, nil
}

// checkCopyPath keeps copy entries inside the checkout: relative, and no
// .. component, since the same relative path names the file in the main
// checkout and in the worktree.
func checkCopyPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("%s is absolute; entries are relative to the repository root", p)
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".." {
			return fmt.Errorf("%s leaves the repository root", p)
		}
	}
	return nil
}
