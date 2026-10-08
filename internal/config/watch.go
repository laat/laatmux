package config

import (
	"errors"
	"os"
	"time"

	"github.com/laat/laatmux/internal/tmux"
)

// Watch reads the config file again when it has changed, for a daemon
// or a view that follows the file without a restart: the repositories
// an add or the task form appended, or a hand edit. A change is a file
// that is no longer the one last read, as a rename over it makes, or
// whose modification time or size differs; a missing file is a state
// too. The first call reads the file. The zero value watches Path.
type Watch struct {
	read   bool
	exists bool
	file   os.FileInfo
	mtime  time.Time
	size   int64
}

// Changed is the config when the file changed since the last call,
// with changed true; else changed is false and the config zero. A file
// that changed and does not load answers its error once, and again
// only after it changes again. A file read empty after the first call
// while it is being written (beingWritten) is no change: the default
// config it would parse as is not what the user means, and the next
// call reads the file again. One left empty is the default config.
func (w *Watch) Changed() (Config, bool, error) {
	fi, err := os.Stat(Path())
	exists := err == nil
	if w.read && exists == w.exists && (!exists || os.SameFile(fi, w.file) && fi.ModTime().Equal(w.mtime) && fi.Size() == w.size) {
		return Config{}, false, nil
	}
	b, rerr := os.ReadFile(Path())
	if rerr == nil && len(b) == 0 && w.read && beingWritten() {
		return Config{}, false, nil
	}
	w.read, w.exists, w.file = true, exists, fi
	if exists {
		w.mtime, w.size = fi.ModTime(), fi.Size()
	}
	if rerr != nil && !os.IsNotExist(rerr) {
		return Config{}, false, tmux.PrintablePath(rerr)
	}
	// As Load reads it: a file gone since the look is the missing
	// file's config.
	cfg, err := Parse(b)
	if err != nil {
		return Config{}, false, err
	}
	return cfg, true, nil
}

// ErrWriting is a config file read empty while it is being written,
// for a reader that has a config to keep (LoadSettled).
var ErrWriting = errors.New("the config file is empty while it is being written")

// LoadSettled is Load for a reader that has a config to keep, a daemon
// re-reading the hosts say: a file read empty while it is being
// written is ErrWriting, not the default config.
func LoadSettled() (Config, error) {
	b, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return Parse(nil)
		}
		return Config{}, tmux.PrintablePath(err)
	}
	if len(b) == 0 && beingWritten() {
		return Config{}, ErrWriting
	}
	return Parse(b)
}

// settling is how long after its last change a config file read empty
// is taken for one a writer has truncated and not yet filled, an editor
// saving it in place say, rather than for one emptied. A follower looks
// again a poll later, after the time has passed.
const settling = 2 * time.Second

// beingWritten reports whether the config file, read empty, is being
// written: it has content again, or it changed within settling.
func beingWritten() bool {
	fi, err := os.Stat(Path())
	return err == nil && (fi.Size() > 0 || time.Since(fi.ModTime()) < settling)
}
