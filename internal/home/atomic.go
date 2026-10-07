package home

import (
	"os"
	"strconv"
	"strings"

	"github.com/laat/laatmux/internal/tmux"
)

// WriteAtomic writes data to path whole: through a temporary beside it,
// named for this process so two processes never share one, renamed into
// place, so a reader sees the old contents or the new and never a
// partial file. A failed write or rename leaves no temporary behind;
// one left by a process that died mid-write is named as Temporary
// says. Its error names the temporary, or both files, as
// tmux.Printable shows them: the daemon passes it on to a client, and
// the state directory's name can have any byte in it.
func WriteAtomic(path string, data []byte) error {
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		os.Remove(tmp)
		return tmux.PrintablePath(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return tmux.PrintablePath(err)
	}
	return nil
}

// Temporary reports whether a file name is a temporary of a write that
// died before its rename: "<file>.tmp.<pid>" as WriteAtomic names it,
// or "<file>.tmp" as builds before it did. A file whose own name ends
// in ".tmp" is not one; the sweeps ask about record files, which end
// in ".json".
func Temporary(name string) bool {
	if strings.HasSuffix(name, ".json.tmp") {
		return true
	}
	i := strings.LastIndex(name, ".json.tmp.")
	if i < 0 {
		return false
	}
	pid := name[i+len(".json.tmp."):]
	return pid != "" && strings.Trim(pid, "0123456789") == ""
}
