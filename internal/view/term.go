package view

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/palette"
	"golang.org/x/sys/unix"
)

// Term is the terminal the view draws on: raw mode through termios,
// the alternate screen, cursor hidden, SGR mouse reporting on. Close
// restores everything.
type Term struct {
	in, out *os.File
	saved   *unix.Termios
	// Theme is what the lines are drawn in; the zero Theme has no
	// colours and draws with the attributes alone.
	Theme palette.Theme
	// pending is input Background read that was not its answer, for
	// Run to decode first.
	pending []byte
}

// Open puts the terminal into raw mode. Not a terminal is an error.
func Open(in, out *os.File) (*Term, error) {
	t := &Term{in: in, out: out}
	saved, err := unix.IoctlGetTermios(int(in.Fd()), ioctlGetTermios)
	if err != nil {
		return nil, errors.New("stdin is not a terminal")
	}
	raw := *saved
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(int(in.Fd()), ioctlSetTermios, &raw); err != nil {
		return nil, err
	}
	t.saved = saved
	// Alternate screen, cursor hidden, mouse buttons and wheel with SGR
	// coordinates so columns past 223 report correctly, bracketed
	// paste, so pasted text arrives marked and is inserted rather than
	// read as keys, and extended keys at xterm's first level
	// (modifyOtherKeys 1), so a modified Enter comes as a sequence
	// rather than as Enter and Shift-Enter can break a line, while Esc,
	// Enter, Tab and Ctrl-C stay the bytes they are. A tmux popup
	// passes the markers through once the application has asked for
	// them, and with extended-keys on forwards the sequences to a pane
	// that asked; a terminal that knows neither ignores both.
	t.write("\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h\x1b[?2004h\x1b[>4;1m")
	return t, nil
}

// Background asks the terminal for its background colour with OSC 11 and
// reports whether it is dark, waiting at most wait for the answer; ok is
// false when none came or it could not be read. It is called before Run
// reads the keys, so it reads the terminal itself: whatever else comes
// meanwhile, keys typed or the start of a paste, is kept for Run, and
// an answer begun by the deadline is waited on a while longer for its
// end. One cut short is left for Run's decoder, which swallows it; so
// is one that comes later. tmux answers for its pane.
func (t *Term) Background(wait time.Duration) (dark, ok bool) {
	t.write("\x1b]11;?\x1b\\")
	deadline := time.Now().Add(wait)
	extended := false
	var got []byte
	buf := make([]byte, 64)
	for len(got) < 1024 {
		left := time.Until(deadline)
		if left <= 0 {
			// An answer under way gets its end once.
			if start := strings.Index(string(got), oscAnswer); start >= 0 && oscEnd(got[start:]) < 0 && !extended {
				extended, deadline = true, time.Now().Add(oscWait)
				continue
			}
			break
		}
		fds := []unix.PollFd{{Fd: int32(t.in.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(left.Milliseconds())+1)
		if err != nil && err != unix.EINTR {
			break
		}
		if n <= 0 {
			continue
		}
		k, err := unix.Read(int(t.in.Fd()), buf)
		if err != nil || k <= 0 {
			break
		}
		got = append(got, buf[:k]...)
		if answer, rest, found := takeAnswer(got); found {
			t.pending = rest
			return palette.DarkBackground(answer)
		}
	}
	t.pending = got
	return false, false
}

// Close restores the terminal.
func (t *Term) Close() {
	if t.saved == nil {
		return
	}
	// The extended keys are reset without a value, which in xterm is
	// the terminal's own setting rather than off.
	t.write("\x1b[>4m\x1b[?2004l\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l")
	_ = unix.IoctlSetTermios(int(t.in.Fd()), ioctlSetTermios, t.saved)
	t.saved = nil
}

// Size is the terminal's columns and rows.
func (t *Term) Size() (w, h int) {
	ws, err := unix.IoctlGetWinsize(int(t.out.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 || ws.Row == 0 {
		return 80, 24
	}
	return int(ws.Col), int(ws.Row)
}

// Draw writes a whole frame: every line from the top, each cleared to
// the end, so nothing of the previous frame shows through.
func (t *Term) Draw(lines []Line) {
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(ANSI(l, t.Theme))
		b.WriteString("\x1b[K")
	}
	t.write(b.String())
}

func (t *Term) write(s string) { _, _ = t.out.WriteString(s) }

// takeAnswer finds the terminal's answer to the background query in b,
// an OSC 11 string with a colour, and returns it with the bytes around
// it. Other OSC strings, the query echoed back say, are dropped, and
// anything else, the user's keys, an Alt-], is kept.
func takeAnswer(b []byte) (answer string, rest []byte, found bool) {
	var kept []byte
	for i := 0; i < len(b); {
		if b[i] != 0x1b || i+1 == len(b) || b[i+1] != ']' {
			kept = append(kept, b[i])
			i++
			continue
		}
		kind, n, _ := oscScan(b[i:])
		switch kind {
		case oscDone:
			s := string(b[i : i+n])
			if strings.HasPrefix(s, oscAnswer) {
				if _, ok := palette.DarkBackground(s); ok {
					return s, append(kept, b[i+n:]...), true
				}
			}
			i += n
		case oscMore:
			return "", nil, false
		default:
			kept = append(kept, b[i:i+n]...)
			i += n
		}
	}
	return "", nil, false
}
