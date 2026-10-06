package procs

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	bootTime time.Time
	bootID   string // the kernel's boot id, in StartID: starts count from the boot
	clkTck   = int64(100)
)

func init() {
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		bootID = strings.TrimSpace(string(b))
	}
	f, err := os.Open("/proc/stat")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "btime ") {
			if v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(sc.Text(), "btime ")), 10, 64); err == nil {
				bootTime = time.Unix(v, 0)
			}
		}
	}
}

// ListTTY returns the processes whose controlling terminal is tty, by
// scanning /proc/*/stat for a matching tty_nr. A zombie is not listed:
// it has exited, and only waits for its parent to reap it.
func ListTTY(tty string) ([]Proc, error) {
	st, err := os.Stat(tty)
	if err != nil {
		return nil, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("procs: unexpected stat type for %s", tty)
	}
	want := uint64(sys.Rdev)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []Proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, tty, state, ok := readStat(pid)
		if !ok || tty != want || state == "Z" {
			continue
		}
		p.Argv = readNulFile(filepath.Join("/proc", e.Name(), "cmdline"))
		p.Env = readNulFile(filepath.Join("/proc", e.Name(), "environ"))
		out = append(out, p)
	}
	return out, nil
}

// Lookup is the process with the pid: its comm, start time and start
// identity, the last two with the pid identifying it as Identity does
// an agent; not ok when there is no such process. Unlike ListTTY it
// finds a zombie: the pid names it, and no other, until the reap.
func Lookup(pid int) (Proc, bool) {
	p, _, _, ok := readStat(pid)
	return p, ok
}

// readStat reads /proc/<pid>/stat: the process, its tty_nr and its
// state ("Z" for a zombie).
func readStat(pid int) (Proc, uint64, string, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Proc{}, 0, "", false
	}
	// pid (comm) state ppid pgrp session tty_nr tpgid ... starttime(22)
	open := bytes.IndexByte(b, '(')
	close := bytes.LastIndexByte(b, ')')
	if open < 0 || close < 0 || close < open {
		return Proc{}, 0, "", false
	}
	comm := string(b[open+1 : close])
	fields := strings.Fields(string(b[close+2:]))
	// fields[0]=state fields[1]=ppid fields[2]=pgrp fields[3]=session fields[4]=tty_nr fields[5]=tpgid ... fields[19]=starttime
	if len(fields) < 20 {
		return Proc{}, 0, "", false
	}
	ttyNr, _ := strconv.ParseUint(fields[4], 10, 64)
	ppid, _ := strconv.Atoi(fields[1])
	pgid, _ := strconv.Atoi(fields[2])
	tpgid, _ := strconv.Atoi(fields[5])
	startTicks, _ := strconv.ParseInt(fields[19], 10, 64)
	start := bootTime.Add(time.Duration(startTicks*1e9/clkTck) * time.Nanosecond)
	startID := ""
	if bootID != "" {
		startID = bootID + "/" + fields[19]
	}
	return Proc{PID: pid, PPID: ppid, PGID: pgid, TPGID: tpgid, Comm: comm, Start: start, StartID: startID}, ttyNr, fields[0], true
}

func readNulFile(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return nil
	}
	parts := bytes.Split(bytes.TrimRight(b, "\x00"), []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, string(p))
	}
	return out
}
