package config

import (
	"os"
	"time"
)

// Watch reads the config file again when it has changed, for a daemon
// that follows the file without a restart: the repositories an add or
// the task form appended, or a hand edit. A change is a file that is no
// longer the one last read, as a rename over it makes, or whose
// modification time or size differs; a missing file is a state too. The
// first call reads the file. The zero value watches Path.
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
// only after it changes again.
func (w *Watch) Changed() (Config, bool, error) {
	fi, err := os.Stat(Path())
	exists := err == nil
	if w.read && exists == w.exists && (!exists || os.SameFile(fi, w.file) && fi.ModTime().Equal(w.mtime) && fi.Size() == w.size) {
		return Config{}, false, nil
	}
	w.read, w.exists, w.file = true, exists, fi
	if exists {
		w.mtime, w.size = fi.ModTime(), fi.Size()
	}
	cfg, err := Load()
	if err != nil {
		return Config{}, false, err
	}
	return cfg, true, nil
}
