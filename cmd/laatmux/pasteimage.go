package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// The image paste. Claude Code takes an image on Ctrl+V from the
// clipboard of the machine it runs on, which on a host reached over
// ssh has none, and the terminal carries only text; the image is on
// this machine's clipboard, and the agent takes a file's path as the
// image. `on` binds C-v in the default server's root table: in a
// workspace session's attach pane it runs `paste-image run` with the
// pane and the client, in the background, and any other pane gets the
// key as it would without the binding. run reads a PNG from the
// clipboard and sends it to the host's daemon, which writes it to a
// file and types the file's path into the agent's pane with no Enter
// (internal/daemon/paste.go). With no image on the clipboard, and in a
// workspace on this machine, where the agent reads this clipboard
// itself, run presses C-v in the pane instead, so a text paste and
// every other use of the key are as they were. A failure is shown
// with display-message, and nothing is typed: run has no terminal.

// pasteKey is the key the binding takes.
const pasteKey = "C-v"

// pasteTimeout bounds run's round trip to the host: a dial over ssh
// and an image of a few megabytes, a third more as base64, over a slow
// uplink. A variable so a test can shorten it.
var pasteTimeout = 60 * time.Second

func cmdPasteImage(ctx context.Context, args []string) error {
	usage := errors.New("usage: laatmux paste-image [toggle|on|off]\n       laatmux paste-image run <pane> [<client>]")
	sub := "toggle"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "toggle", "on", "off":
		if len(args) > 0 {
			return usage
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return pasteSwitch(ctx, sub, exe)
	case "run":
		if len(args) < 1 || len(args) > 2 {
			return usage
		}
		client := ""
		if len(args) == 2 {
			client = args[1]
		}
		pasteRun(ctx, args[0], client)
		return nil
	}
	return usage
}

// pasteSwitch binds C-v or unbinds it; toggle reads the binding,
// laatmux's meaning on. on is idempotent, and a binding of the user's
// on C-v is left alone: on reports it, off leaves it. No server
// running is nothing to turn off, and nothing to bind on: a server
// started for the binding would end at once with no session.
func pasteSwitch(ctx context.Context, sub, exe string) error {
	line, ours, err := pasteBinding(ctx)
	if tmux.NoServer(err) {
		if sub == "off" {
			return nil
		}
		return errors.New("no tmux server is running to bind " + pasteKey + " on")
	}
	if err != nil {
		return err
	}
	if sub == "toggle" {
		sub = "on"
		if ours {
			sub = "off"
		}
	}
	switch {
	case sub == "off" && ours:
		_, err := workspace.Server.Run(ctx, "unbind-key", "-n", pasteKey)
		return err
	case sub == "off":
		return nil
	case line != "" && !ours:
		return fmt.Errorf("%s in tmux's root table is bound already, and left alone: %s", pasteKey, strings.Join(strings.Fields(line), " "))
	}
	_, err = workspace.Server.Run(ctx, pasteBindArgs(exe)...)
	return err
}

// pasteBinding is C-v's binding in the root table as list-keys prints
// it, "" when it has none, and whether it is laatmux's (pasteOurs). -N,
// so a server not running is not started.
func pasteBinding(ctx context.Context) (line string, ours bool, err error) {
	out, err := workspace.Server.Run(ctx, "-N", "list-keys", "-T", "root")
	var te *tmux.Error
	if errors.As(err, &te) && te.Msg == "table root doesn't exist" {
		// No root table, for a tmux that drops a table with no key
		// left in it, is no binding; 3.6 lists such a table empty.
		// Any other error, a user's after-list-keys hook that failed
		// say, may hide a binding, and is returned.
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, l := range strings.Split(string(out), "\n") {
		// bind-key [-r] -T root C-v if-shell -F "#{@laatmux_attach_pane}" "run-shell -b '... paste-image run ...'" "send-keys C-v"
		f := commandWords(l)
		i := slices.Index(f, "-T")
		if len(f) == 0 || f[0] != "bind-key" || i < 0 || i+2 >= len(f) || f[i+1] != "root" || f[i+2] != pasteKey {
			continue
		}
		return l, pasteOurs(f[i+3:]), nil
	}
	return "", false, nil
}

// pasteOurs reports whether a binding's command, its words, is
// laatmux's as pasteBindArgs binds it: an if-shell -F on the attach
// pane's tag whose command for an attach pane is a run-shell of a
// binary's paste-image run. The words elsewhere, in the other branch
// or in a string a command prints, make no binding laatmux's.
func pasteOurs(cmd []string) bool {
	if len(cmd) < 4 || cmd[0] != "if-shell" || cmd[1] != "-F" || cmd[2] != "#{@laatmux_attach_pane}" {
		return false
	}
	run := commandWords(cmd[3])
	if len(run) < 2 || run[0] != "run-shell" {
		return false
	}
	sh := commandWords(run[len(run)-1])
	return len(sh) >= 3 && sh[1] == "paste-image" && sh[2] == "run"
}

// commandWords splits s into words as tmux's command parser, and the
// shell for the words laatmux writes, read them: single quotes keep
// what they hold, a backslash outside them takes the next byte as it
// is, inside double quotes too, and blanks part the words. list-keys
// prints a binding's arguments quoted that way, a command inside a
// branch quoted again, and the shell command inside run-shell once
// more.
func commandWords(s string) []string {
	var words []string
	var w strings.Builder
	in := false // a word has begun
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			in = true
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				end = len(s) - i - 1
			}
			w.WriteString(s[i+1 : i+1+end])
			i += end + 1
		case c == '"':
			in = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				w.WriteByte(s[i])
			}
		case c == '\\' && i+1 < len(s):
			in = true
			i++
			w.WriteByte(s[i])
		case c == ' ' || c == '\t':
			if in {
				words = append(words, w.String())
				w.Reset()
				in = false
			}
		default:
			in = true
			w.WriteByte(c)
		}
	}
	if in {
		words = append(words, w.String())
	}
	return words
}

// pasteBindArgs is the bind-key command for C-v: in an attach pane,
// paste-image run with the pane and the client, in the background so
// the key returns at once; in any other pane the key itself. run-shell
// expands its command as a format, which keeps the binary's path as
// the sidebar's keys keep it and gives the pane and the client.
func pasteBindArgs(exe string) []string {
	run := fmt.Sprintf("run-shell -b %s", tmux.ShellJoin([]string{runShellExe(exe) + " paste-image run '#{pane_id}' '#{client_name}'"}))
	return []string{"bind-key", "-n", pasteKey, "if-shell", "-F", "#{@laatmux_attach_pane}", run, "send-keys " + pasteKey}
}

// pasteRun is the binding's work for the pane, a failure shown to the
// client. It never fails itself: a background run-shell that exits
// with an error, or prints, puts that over the pane.
func pasteRun(ctx context.Context, pane, client string) {
	if err := pasteImage(ctx, pane, client); err != nil {
		pasteNotice(ctx, pane, client, 5*time.Second, err.Error())
	}
}

// pasteImage sends the clipboard's image to the agent of the workspace
// the pane is in, or presses C-v in the pane when there is no image to
// send. The pane is looked up, and the key pressed, on the server TMUX
// names, where the binding ran, as split does: a pane id is per server.
// A config that does not load, or that has not the session's host,
// matters to an image alone: with none the key goes to the pane as it
// does unbound. The client is told while the image is on its way,
// which takes a dial and an upload, keys typed meanwhile reaching the
// pane before the path, and told when it has arrived.
func pasteImage(ctx context.Context, pane, client string) error {
	l, _, err := workspace.PaneSession(ctx, pane)
	if err != nil && !tmux.HookOnly(err) {
		return err
	}
	if l.Host == "" {
		// Not a session of laatmux's: the key is the pane's.
		return pressPasteKey(ctx, pane)
	}
	h, hostErr := pasteHost(l.Host)
	if hostErr == nil && h.Local() {
		// The agent reads this clipboard itself.
		return pressPasteKey(ctx, pane)
	}
	png, err := readClipboard(ctx)
	if err != nil {
		return err
	}
	if png == nil {
		return pressPasteKey(ctx, pane)
	}
	if hostErr != nil {
		return hostErr
	}
	if !l.Workspace() {
		return fmt.Errorf("%s is not a workspace session: the image has no agent to go to", l.Name)
	}
	if h.Paused {
		// Refused before the client is told anything is sent.
		return &peer.PausedError{Name: h.Name}
	}
	size := sizeText(len(png))
	// The notice stays until the paste ends, which replaces it, or a
	// key: a C-v pressed again meanwhile would send a second image.
	pasteNotice(ctx, pane, client, pasteTimeout, fmt.Sprintf("sending a %s image to %s", size, h.Name))
	env, root := protocol.SplitSessionKey(l.Key)
	sctx, cancel := context.WithTimeout(ctx, pasteTimeout)
	defer cancel()
	err = sendPaste(sctx, h.Host, env, root, png)
	switch {
	case err != nil && errors.Is(sctx.Err(), context.DeadlineExceeded):
		// The connection closed under a write in flight fails it with
		// its own error, not the context's.
		return fmt.Errorf("%s: no answer within %s sending a %s image", h.Name, durationText(pasteTimeout), size)
	case err != nil:
		return err
	}
	pasteNotice(ctx, pane, client, 0, fmt.Sprintf("sent a %s image to %s", size, h.Name))
	return nil
}

// pasteHost is the configured host of the name.
func pasteHost(name string) (config.Host, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Host{}, err
	}
	h, ok := cfg.Find(name)
	if !ok {
		return config.Host{}, fmt.Errorf("unknown host %q", name)
	}
	return h, nil
}

// clipboardTimeout bounds the clipboard's read: osascript without
// pngpaste prints a large screenshot as hex, twice its size. A variable
// so a test can shorten it.
var clipboardTimeout = 30 * time.Second

// readClipboard is clipboardPNG within clipboardTimeout: a clipboard
// whose owner never answers, a hung application or X selection owner
// say, would hold the key's job, and each one after it, for ever.
func readClipboard(ctx context.Context) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, clipboardTimeout)
	defer cancel()
	png, err := clipboardPNG(cctx)
	switch {
	case err != nil && errors.Is(cctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("reading the clipboard: no answer within %s", durationText(clipboardTimeout))
	case err != nil:
		return nil, fmt.Errorf("reading the clipboard: %w", err)
	}
	return png, nil
}

// durationText is d as the notices say it: whole seconds as 60s, where
// Duration's own form is 1m0s.
func durationText(d time.Duration) string {
	if d >= time.Second && d%time.Second == 0 {
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}

// sizeText is n bytes as the notices say it.
func sizeText(n int) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// pressPasteKey presses C-v in the pane, as the key does unbound.
func pressPasteKey(ctx context.Context, pane string) error {
	_, err := (tmux.Server{}).Run(ctx, "send-keys", "-t", pane, pasteKey)
	return err
}

// sendPaste sends the image to the host's daemon for the agent of the
// workspace at root, over a connection of its own as every command
// takes; a paused host is refused as it is for any command. The daemon
// must have paste and answer as the workspace's environment. A
// variable so a test sends it nowhere.
var sendPaste = func(ctx context.Context, h peer.Host, env, root string, png []byte) error {
	c, err := client.Dial(ctx, h)
	if err != nil {
		return err
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapPaste) {
		return fmt.Errorf("%s: daemon %s has no paste; laatmux upgrade %s installs one that has", h.Name, c.Hello.Version, h.Name)
	}
	if c.Hello.EnvironmentID != env {
		return fmt.Errorf("%s: answers as environment %s, not %s the workspace is of", h.Name, c.Hello.EnvironmentID, env)
	}
	if _, err := c.Request(ctx, protocol.Message{Type: protocol.TypePaste, ID: command.ID("paste"), EnvironmentID: env, Root: root, Image: png}); err != nil {
		return fmt.Errorf("%s: %w", h.Name, err)
	}
	return nil
}

// pasteNotice shows msg to the client with display-message, for delay
// or until a key, or for the display-time with no delay: from a key's
// background job it is the only way the user sees it. A client gone
// since the key is passed over, display-message then picking one, and
// a tmux that refuses -d shows it for its display-time. Nothing is
// returned: there is nowhere else to say it.
func pasteNotice(ctx context.Context, pane, client string, delay time.Duration, msg string) {
	msg = displayLiteral("laatmux paste-image: " + msg)
	var d []string
	if delay > 0 {
		d = []string{"-d", strconv.FormatInt(delay.Milliseconds(), 10)}
	}
	var tries [][]string
	if client != "" {
		tries = append(tries, append([]string{"-c", client}, d...))
	}
	tries = append(tries, d)
	if d != nil {
		tries = append(tries, nil)
	}
	for _, flags := range tries {
		args := append(append([]string{"display-message"}, flags...), "-t", pane, msg)
		if _, err := (tmux.Server{}).Run(ctx, args...); err == nil {
			return
		}
	}
}

// displayLiteral is a message display-message shows as s: the message
// is a format, where # starts one, and goes through strftime, where %
// does; a line break, which the status line cannot show, is a space.
func displayLiteral(s string) string {
	return strings.ReplaceAll(tmux.FormatLiteral(strings.Join(strings.Fields(s), " ")), "%", "%%")
}

// clipboardPNG is the PNG image on this machine's clipboard, nil when
// it holds none; a variable so a test reads none of the user's.
var clipboardPNG = func(ctx context.Context) ([]byte, error) { return systemClipboard().png(ctx) }

// clipboard reads a clipboard with its system's tools: run runs one
// and returns what it printed, has says whether one is on PATH.
type clipboard struct {
	goos   string
	getenv func(string) string
	has    func(name string) bool
	run    func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// systemClipboard is this machine's clipboard. A tool's error is what
// it said on stderr.
func systemClipboard() clipboard {
	return clipboard{
		goos:   runtime.GOOS,
		getenv: os.Getenv,
		has:    func(name string) bool { _, err := exec.LookPath(name); return err == nil },
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			if err := cmd.Run(); err != nil {
				if msg := strings.TrimSpace(errb.String()); msg != "" {
					return nil, fmt.Errorf("%s: %s", name, msg)
				}
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			return out.Bytes(), nil
		},
	}
}

// png is the clipboard's image, nil for a clipboard without one; what
// a tool printed for it must be a PNG.
func (c clipboard) png(ctx context.Context) ([]byte, error) {
	out, err := c.read(ctx)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	if !bytes.HasPrefix(out, []byte(protocol.PNGSignature)) {
		return nil, errors.New("the image is not a PNG")
	}
	return out, nil
}

// read reads the image. On macOS osascript says what the clipboard
// holds. A file reference is no image, since a file copied in Finder
// is one, with its icon as image data. pngpaste, when it is on PATH,
// reads PNG data and converts TIFF, JPEG and GIF; osascript otherwise
// reads PNG data alone. Elsewhere, under Wayland with wl-paste, else
// with xclip, when the clipboard offers image/png; the listing fails
// with its own words for a clipboard with nothing in it, which is no
// image, and any other failure, a display it cannot reach say, is the
// error.
func (c clipboard) read(ctx context.Context) ([]byte, error) {
	switch {
	case c.goos == "darwin":
		info, err := c.run(ctx, "osascript", "-e", "clipboard info")
		if err != nil {
			return nil, err
		}
		// The class names are in guillemets, printed in the locale's
		// encoding, so they are matched without them.
		has := func(class string) bool { return bytes.Contains(info, []byte(class)) }
		if has("class furl") {
			return nil, nil
		}
		if c.has("pngpaste") {
			if !has("class PNGf") && !has("TIFF picture") && !has("JPEG picture") && !has("GIF picture") {
				return nil, nil
			}
			return c.run(ctx, "pngpaste", "-")
		}
		if !has("class PNGf") {
			return nil, nil
		}
		out, err := c.run(ctx, "osascript", "-e", "the clipboard as «class PNGf»")
		if err != nil {
			return nil, err
		}
		return osaData(out)
	case c.getenv("WAYLAND_DISPLAY") != "" && c.has("wl-paste"):
		types, err := c.run(ctx, "wl-paste", "--list-types")
		if err != nil {
			return nil, emptyClipboard(err, "Nothing is copied", "No selection")
		}
		if !hasLine(types, "image/png") {
			return nil, nil
		}
		return c.run(ctx, "wl-paste", "--no-newline", "--type", "image/png")
	case c.has("xclip"):
		targets, err := c.run(ctx, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
		if err != nil {
			return nil, emptyClipboard(err, "target TARGETS not available")
		}
		if !hasLine(targets, "image/png") {
			return nil, nil
		}
		return c.run(ctx, "xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	}
	return nil, errors.New("no clipboard tool: install wl-clipboard or xclip")
}

// emptyClipboard is nil for a tool's error that says the clipboard has
// nothing in it, in one of the tool's phrasings, and err otherwise.
func emptyClipboard(err error, phrasings ...string) error {
	for _, p := range phrasings {
		if strings.Contains(err.Error(), p) {
			return nil
		}
	}
	return err
}

// osaData is the bytes osascript prints a clipboard read as PNG as:
// «data PNGf…» with the bytes in hex.
func osaData(out []byte) ([]byte, error) {
	s := string(out)
	i := strings.Index(s, "data PNGf")
	if i < 0 {
		return nil, errors.New("osascript printed no PNG data")
	}
	h := s[i+len("data PNGf"):]
	if end := strings.IndexFunc(h, func(r rune) bool { return !strings.ContainsRune("0123456789abcdefABCDEF", r) }); end >= 0 {
		h = h[:end]
	}
	return hex.DecodeString(h)
}

// hasLine reports whether a line of out, trimmed, is want.
func hasLine(out []byte, want string) bool {
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}
