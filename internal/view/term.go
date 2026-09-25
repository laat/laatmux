package view

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Term is the terminal the view draws on: raw mode through termios,
// the alternate screen, cursor hidden, SGR mouse reporting on. Close
// restores everything.
type Term struct {
	in, out *os.File
	saved   *unix.Termios
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
	// coordinates so columns past 223 report correctly, and bracketed
	// paste, so pasted text arrives marked and is inserted rather than
	// read as keys. A tmux popup passes the markers through once the
	// application has asked for them.
	// The alternate screen, no cursor, mouse presses and wheel as SGR
	// reports, bracketed paste, and extended keys at xterm's first
	// level (modifyOtherKeys 1): a modified Enter comes as a sequence
	// rather than as Enter, so Shift-Enter can break a line, while
	// Esc, Enter, Tab and Ctrl-C stay the bytes they are. tmux with
	// extended-keys on forwards them to a pane that asked.
	t.write("\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h\x1b[?2004h\x1b[>4;1m")
	return t, nil
}

// Close restores the terminal.
func (t *Term) Close() {
	if t.saved == nil {
		return
	}
	t.write("\x1b[>4;0m\x1b[?2004l\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l")
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
		b.WriteString(ANSI(l))
		b.WriteString("\x1b[K")
	}
	t.write(b.String())
}

func (t *Term) write(s string) { _, _ = t.out.WriteString(s) }
