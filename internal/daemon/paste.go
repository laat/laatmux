package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

// The paste message: an image from the clipboard of the machine the
// user sits at, for the agent of a workspace on this host. Claude Code
// reads the clipboard of the machine it runs on, which on a host
// reached over ssh has no image, and takes a path to an image file as
// the image; so the daemon writes the PNG to a file and types the
// file's path into the agent's pane, as one bracketed paste with no
// Enter, for the user to go on typing after it. The pane is found as a
// prompt delivery without a target finds it (adopt): the managed
// session made at the root, alone there, with a verified agent in its
// single pane.

// pasteKeep is how long a pasted image is kept: each paste first
// removes the images older than it, by their modification time. The
// agent reads the file when the message with the path is sent.
const pasteKeep = time.Hour

// pasteTmp is where the images go on a daemon with no paste directory,
// each named with pastePrefix, so the pruning there takes laatmux's
// alone; a variable so a test can move it.
var pasteTmp = "/tmp"

const pastePrefix = "laatmux-paste-"

// paste writes the image and types its path into the agent's pane at
// the root, answering whether the path reached the pane. A refusal
// writes nothing.
func (c *clientConn) paste(m protocol.Message) error {
	d := c.d
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	switch {
	case d.managed == nil:
		res.Error = errNoManaged
	case m.EnvironmentID != d.cfg.EnvironmentID:
		res.Error = fmt.Sprintf("this host is environment %s, not %s", d.cfg.EnvironmentID, m.EnvironmentID)
	case m.Root == "":
		res.Error = "root required"
	case !bytes.HasPrefix(m.Image, []byte(protocol.PNGSignature)):
		res.Error = "not a PNG image"
	}
	if res.Error != "" {
		return c.pc.Write(res)
	}
	ctx, cancel := c.context()
	go func() {
		defer cancel()
		if err := d.tasks.pasteImage(ctx, m.Root, m.Image); err != nil {
			res.Error = err.Error()
		} else {
			res.OK = true
		}
		if err := c.pc.Write(res); err != nil {
			c.drop()
		}
	}()
	return nil
}

// pasteImage writes png to a file and types the file's path into the
// pane of the agent at root, with no Enter. It holds the root's
// delivery lock throughout, as a prompt's paste does, so the path
// never lands between a prompt and its Enter, nor in a root rm has
// taken; and it counts as a paste in flight, so a daemon shutting down
// starts none and waits for one started, whose buffer is then deleted.
// No agent found, or none observed in the pane as it is listed now,
// writes nothing. Only a failure to load the buffer
// proves the path did not reach the pane, as for a prompt. The file
// stays until a later paste prunes it, whatever became of the paste.
func (rn *taskRunner) pasteImage(ctx context.Context, root string, png []byte) error {
	unlock := rn.lockDeliveries(root)
	defer unlock()
	target, why := rn.adopt(ctx, root)
	if why == "" {
		why = rn.observedAs(target)
	}
	if why != "" {
		return errors.New(why)
	}
	if !rn.beginDelivery() {
		return errors.New("daemon shutting down")
	}
	defer rn.endDelivery()
	path, err := writePaste(rn.cfg.Paste, png, time.Now())
	if err != nil {
		return err
	}
	// The buffer is named as the prompts' are, which a daemon that
	// starts sweeps, after the file, whose name no other paste has.
	buffer := attemptBufferPrefix + "paste-" + filepath.Base(path)
	err = rn.managed.Tmux.PasteNoEnter(ctx, buffer, target.ID, path)
	// The pane's observation is spent, as after a prompt's paste: a
	// delivery to it needs one made after this.
	rn.mu.Lock()
	rn.pasted[paneKey(rn.managed.Label, target.ID)] = time.Now()
	rn.mu.Unlock()
	var pe *tmux.PasteError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &pe) && pe.Step == "load":
		return fmt.Errorf("paste refused: %w", err)
	default:
		return fmt.Errorf("the path %s may have reached pane %s: %w", tmux.Printable(path), target.ID, err)
	}
}

// observedAs says why the last observation of the pane does not vouch
// for the agent in it as the pane is listed now, "" when it does.
// adopt takes the verified observation whatever server instance it was
// made on, and a delivery then waits for one of the pane as recorded; a
// paste does not wait, so the observation must be of the server
// instance and the session the pane is listed in: a server restarted
// since can have given the pane's id to a shell at the same root.
func (rn *taskRunner) observedAs(p tmux.Pane) string {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	st, ok := rn.panes[paneKey(rn.managed.Label, p.ID)]
	if !ok || st.obs.serverPID != p.ServerPID || st.obs.session != p.Session {
		return fmt.Sprintf("no agent to deliver to: pane %s has no observation yet as it is listed, in session %s on server %d", p.ID, tmux.Printable(p.Session), p.ServerPID)
	}
	return ""
}

// writePaste writes png to a new file in dir named <timestamp>.png, or
// without a dir in pasteTmp as laatmux-paste-<timestamp>.png, after
// removing the images there older than pasteKeep, and returns its
// absolute path: the agent resolves a relative one, from a relative
// LAATMUX_HOME say, against its own directory. A file there is never
// overwritten: a second paste in the same millisecond gets -2.
func writePaste(dir string, png []byte, now time.Time) (string, error) {
	prefix := ""
	if dir == "" {
		dir, prefix = pasteTmp, pastePrefix
	}
	// os's errors name the state directory as it is; the result
	// carries them to a client, which shows them.
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", tmux.PrintablePath(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", tmux.PrintablePath(err)
	}
	prunePastes(dir, prefix, now)
	stamp := prefix + now.Format("20060102-150405.000")
	for n := 1; ; n++ {
		name := stamp
		if n > 1 {
			name += "-" + strconv.Itoa(n)
		}
		path := filepath.Join(dir, name+".png")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) && n < 100 {
			continue
		}
		if err != nil {
			return "", tmux.PrintablePath(err)
		}
		_, err = f.Write(png)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(path)
			return "", tmux.PrintablePath(err)
		}
		return path, nil
	}
}

// prunePastes removes the images in dir whose names start with prefix
// and that were written more than pasteKeep before now. Best effort:
// one that cannot be removed, another user's in /tmp say, stays.
func prunePastes(dir, prefix string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".png") {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > pasteKeep {
			os.Remove(filepath.Join(dir, name))
		}
	}
}
