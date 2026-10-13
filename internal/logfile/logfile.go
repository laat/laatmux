// Package logfile is the daemon's log file, rotated by age and size: the
// file is daemon.log while it is written, then daemon.log.1, .2 and .3
// as newer ones take its place, so three days of a daemon's log are
// kept and no more, and a flood, one message every poll say, costs the
// cap in size rather than the disk.
package logfile

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Options bound a file. Zero values take the defaults: a day, 20 MB,
// three files kept beside the live one.
type Options struct {
	MaxAge  time.Duration // a file older than this is rotated at the next write
	MaxSize int64         // a write that would take the file past this rotates first
	Keep    int           // rotated files kept: .1 is the newest, .Keep the oldest
	Now     func() time.Time
}

const (
	DefaultMaxAge  = 24 * time.Hour
	DefaultMaxSize = 20 << 20
	DefaultKeep    = 3
)

// Writer appends to the live file and rotates it. It is safe for
// concurrent use, as a log.Logger's output must be.
type Writer struct {
	path string
	opts Options
	mu   sync.Mutex
	f    *os.File
	size int64
	from time.Time // when the live file's first line was written
}

// stamp is the date log.LstdFlags writes, after any prefix.
var stamp = regexp.MustCompile(`\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}`)

// Open opens the live file for appending, or creates it. The age of an
// existing file is read from the date on its first line, which the
// standard logger's flags put there; a file without one is as old as
// its last change.
func Open(path string, opts Options) (*Writer, error) {
	if opts.MaxAge <= 0 {
		opts.MaxAge = DefaultMaxAge
	}
	if opts.MaxSize <= 0 {
		opts.MaxSize = DefaultMaxSize
	}
	if opts.Keep <= 0 {
		opts.Keep = DefaultKeep
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	w := &Writer{path: path, opts: opts}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, st.Size()
	w.from = w.opts.Now()
	if st.Size() > 0 {
		w.from = st.ModTime()
		if t, ok := firstStamp(w.path); ok {
			w.from = t
		}
	}
	return nil
}

// firstStamp reads the date on the file's first line.
func firstStamp(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	r := bufio.NewReader(f)
	line, _ := r.ReadString('\n')
	m := stamp.FindString(line)
	if m == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006/01/02 15:04:05", m, time.Local)
	return t, err == nil
}

// Write appends p, after rotating when the live file is older than
// MaxAge or p would take it past MaxSize. A rotation that fails is
// reported in the file itself and the live file kept, so the log goes
// on either way.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.opts.Now()
	if w.size > 0 && (now.Sub(w.from) >= w.opts.MaxAge || w.size+int64(len(p)) > w.opts.MaxSize) {
		if err := w.rotate(); err != nil {
			fmt.Fprintf(w.f, "laatmux %s log: rotate: %v\n", now.Format("2006/01/02 15:04:05"), err)
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate shifts the kept files up one, the oldest going, renames the
// live file to .1 and opens a new live file. A kept file missing is no
// error. Standard error is pointed at the new file (Stderr), so what
// the runtime writes there, a panic's trace say, lands in the live file
// and not in the one just rotated.
func (w *Writer) rotate() error {
	for i := w.opts.Keep; i > 1; i-- {
		if err := os.Rename(w.rotated(i-1), w.rotated(i)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(w.path, w.rotated(1)); err != nil {
		return err
	}
	old := w.f
	if err := w.open(); err != nil {
		// The live file is .1 now; keep writing to it.
		return err
	}
	old.Close()
	return w.Stderr()
}

func (w *Writer) rotated(i int) string { return fmt.Sprintf("%s.%d", w.path, i) }

// Stderr points the process's standard error at the live file, so a
// panic's trace is in it.
func (w *Writer) Stderr() error {
	return unix.Dup2(int(w.f.Fd()), 2)
}

// Close closes the live file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
