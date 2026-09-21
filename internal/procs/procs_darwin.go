package procs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ListTTY returns the processes whose controlling terminal is tty, via
// sysctl kern.proc.tty. No ps, no /proc. Works inside sandbox-exec.
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

// procArgs reads argv and environment via kern.procargs2. Best effort: the
// call is denied inside some sandboxes, in which case both are empty.
func procArgs(pid int) (argv, env []string) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 4 {
		return nil, nil
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	// exec path, then NUL padding, then argv strings, then env strings.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, nil
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	parts := bytes.Split(rest, []byte{0})
	var strs []string
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		strs = append(strs, string(p))
	}
	if argc > len(strs) {
		argc = len(strs)
	}
	return strs[:argc], strs[argc:]
}
