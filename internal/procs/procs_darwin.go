package procs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// sZomb is P_stat of a process that has exited and is not yet reaped,
// SZOMB in <sys/proc.h>.
const sZomb = 5

// ListTTY returns the processes whose controlling terminal is tty, via
// sysctl kern.proc.tty. No ps, no /proc. Works inside sandbox-exec. A
// zombie is not listed: it has exited, and only waits for its parent
// to reap it.
func ListTTY(tty string) ([]Proc, error) {
	st, err := os.Stat(tty)
	if err != nil {
		return nil, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("procs: unexpected stat type for %s", tty)
	}
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.tty", int(sys.Rdev))
	if err != nil {
		return nil, fmt.Errorf("procs: kern.proc.tty: %w", err)
	}
	out := make([]Proc, 0, len(kps))
	for _, kp := range kps {
		if kp.Proc.P_stat == sZomb {
			continue
		}
		p := Proc{
			PID:   int(kp.Proc.P_pid),
			PPID:  int(kp.Eproc.Ppid),
			PGID:  int(kp.Eproc.Pgid),
			TPGID: int(kp.Eproc.Tpgid),
			Comm:  unix.ByteSliceToString(kp.Proc.P_comm[:]),
			Start: time.Unix(kp.Proc.P_starttime.Sec, int64(kp.Proc.P_starttime.Usec)*1000),
		}
		p.Argv, p.Env = procArgs(p.PID)
		out = append(out, p)
	}
	return out, nil
}

// Lookup is the process with the pid: its comm, start time and start
// identity, the last two with the pid identifying it as Identity does
// an agent; not ok when there is no such process. Unlike ListTTY it
// finds a zombie: the pid names it, and no other, until the reap.
func Lookup(pid int) (Proc, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return Proc{}, false
	}
	return Proc{
		PID:     pid,
		PPID:    int(kp.Eproc.Ppid),
		PGID:    int(kp.Eproc.Pgid),
		TPGID:   int(kp.Eproc.Tpgid),
		Comm:    unix.ByteSliceToString(kp.Proc.P_comm[:]),
		Start:   time.Unix(kp.Proc.P_starttime.Sec, int64(kp.Proc.P_starttime.Usec)*1000),
		StartID: strconv.FormatInt(kp.Proc.P_starttime.Sec*1_000_000+int64(kp.Proc.P_starttime.Usec), 10),
	}, true
}

// procArgs reads argv and environment via kern.procargs2. Best effort: the
// call is denied inside some sandboxes, in which case both are empty.
func procArgs(pid int) (argv, env []string) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, nil
	}
	return parseProcArgs(raw)
}

// parseProcArgs splits kern.procargs2's buffer: argc, the exec path,
// NUL padding, then argv and env strings, each ended by a NUL. An empty
// argument is a lone NUL and keeps its place in argv; an empty env
// string carries nothing and is dropped, as the NULs that pad the env
// from the strings the kernel adds after it are. An empty argv[0] is
// taken for padding: nothing tells the two apart.
func parseProcArgs(raw []byte) (argv, env []string) {
	if len(raw) < 4 {
		return nil, nil
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, nil
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	parts := bytes.Split(rest, []byte{0})
	// The last part is what follows the last NUL: no whole string.
	parts = parts[:len(parts)-1]
	if argc > len(parts) {
		argc = len(parts)
	}
	argv = make([]string, argc)
	for i, p := range parts[:argc] {
		argv[i] = string(p)
	}
	for _, p := range parts[argc:] {
		if len(p) > 0 {
			env = append(env, string(p))
		}
	}
	return argv, env
}
