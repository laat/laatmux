package home

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Last is the last-used host and agent per repository: state, not config,
// kept on the laptop in $LAATMUX_HOME/last.json. Keyed by the repository's
// source, since that is its identity; a renamed label keeps its defaults.
type Last struct {
	Repos map[string]LastRepo `json:"repos"`
}

// LastRepo is what add used for one repository last time.
type LastRepo struct {
	Host  string `json:"host,omitempty"`
	Agent string `json:"agent,omitempty"`
}

// Get returns the defaults for a source, zero when unknown.
func (l Last) Get(source string) LastRepo {
	return l.Repos[source]
}

// Set records the defaults for a source.
func (l *Last) Set(source string, r LastRepo) {
	if l.Repos == nil {
		l.Repos = map[string]LastRepo{}
	}
	l.Repos[source] = r
}

func lastPath() string { return filepath.Join(Dir(), "last.json") }

// ReadLast reads last.json. A missing file is empty state.
func ReadLast() (Last, error) {
	var l Last
	b, err := os.ReadFile(lastPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return l, nil
		}
		return l, err
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return l, err
	}
	return l, nil
}

// UpdateLast applies fn to the current state and writes it back. The read,
// modify and write run under an exclusive flock on a lock file beside
// last.json, so two concurrent adds keep each other's defaults. The write
// goes to a temporary file renamed into place, so a crash mid-write leaves
// the old file. The lock is a separate file because the rename replaces
// last.json's inode.
func UpdateLast(fn func(*Last)) error {
	if err := ensure(); err != nil {
		return err
	}
	lock, err := os.OpenFile(lastPath()+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	l, err := ReadLast()
	if err != nil {
		return err
	}
	fn(&l)
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := lastPath() + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, lastPath()); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
