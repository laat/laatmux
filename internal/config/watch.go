package config

import (
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
// is one a writer has truncated and not yet filled, an editor saving in
// place say, and is no change: the default config it would parse as is
// not what the user means, and the next call reads the file again.
func (w *Watch) Changed() (Config, bool, error) {
	fi, err := os.Stat(Path())
	exists := err == nil
	if w.read && exists == w.exists && (!exists || os.SameFile(fi, w.file) && fi.ModTime().Equal(w.mtime) && fi.Size() == w.size) {
		return Config{}, false, nil
	}
	b, rerr := os.ReadFile(Path())
	if rerr == nil && len(b) == 0 && w.read {
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
