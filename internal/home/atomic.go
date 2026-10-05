package home

import (
	"os"
	"strconv"
)

// WriteAtomic writes data to path whole: through a temporary beside it,
// named for this process so two writers never share one, renamed into
// place, so a reader sees the old contents or the new and never a
// partial file. A failed write or rename leaves no temporary behind;
// one left by a process that died mid-write has ".tmp." in its name.
func WriteAtomic(path string, data []byte) error {
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
