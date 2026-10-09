package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// testPNG is a PNG as the clipboard gives it: the signature, then
// anything.
var testPNG = []byte(protocol.PNGSignature + "image data")

// The clipboard is read with the system's tools, here a fake that
// answers by command line and records each: on macOS osascript's
// clipboard info decides. PNG data with no file reference is read by
// pngpaste, or by osascript as hex without it; TIFF alone by pngpaste,
// which converts it, and by nothing without it. Text, a Finder copy
// with its icon, and nothing are no image, and nothing more is run; a
// failed info is an error. Under Wayland wl-paste, else xclip, read
// image/png when the listing has it; a listing that fails for an empty
// clipboard, in the tool's words, is no image, and one that fails
// otherwise is the error; neither tool is an error. What a tool
// printed must be a PNG.
func TestClipboardRead(t *testing.T) {
	hexPNG := fmt.Sprintf("«data PNGf%X»\n", testPNG)
	info := map[string]string{
		"image":  "«class PNGf», 1146181, «class 8BPS», 3734670, TIFF picture, 13600626\n",
		"tiff":   "TIFF picture, 13600626, «class utf8», 5\n",
		"text":   "«class utf8», 5, «class ut16», 12, string, 5, Unicode text, 10\n",
		"finder": "«class furl», 63, «class icns», 84, «class PNGf», 2000, «class ut16», 22\n",
		"empty":  "\n",
	}
	for _, c := range []struct {
		name    string
		goos    string
		env     string
		tools   []string
		answers map[string]string // command line -> stdout; "!" first fails with the rest
		want    []byte
		wantErr string
		ran     []string
	}{
		{"pngpaste", "darwin", "", []string{"pngpaste"}, map[string]string{"osascript -e clipboard info": info["image"], "pngpaste -": string(testPNG)}, testPNG, "", []string{"osascript -e clipboard info", "pngpaste -"}},
		{"osascript", "darwin", "", nil, map[string]string{"osascript -e clipboard info": info["image"], "osascript -e the clipboard as «class PNGf»": hexPNG}, testPNG, "", []string{"osascript -e clipboard info", "osascript -e the clipboard as «class PNGf»"}},
		{"tiff", "darwin", "", []string{"pngpaste"}, map[string]string{"osascript -e clipboard info": info["tiff"], "pngpaste -": string(testPNG)}, testPNG, "", []string{"osascript -e clipboard info", "pngpaste -"}},
		{"tiff without pngpaste", "darwin", "", nil, map[string]string{"osascript -e clipboard info": info["tiff"]}, nil, "", []string{"osascript -e clipboard info"}},
		{"jpeg", "darwin", "", []string{"pngpaste"}, map[string]string{"osascript -e clipboard info": "JPEG picture, 299024\n", "pngpaste -": string(testPNG)}, testPNG, "", []string{"osascript -e clipboard info", "pngpaste -"}},
		{"gif", "darwin", "", []string{"pngpaste"}, map[string]string{"osascript -e clipboard info": "GIF picture, 230329\n", "pngpaste -": string(testPNG)}, testPNG, "", []string{"osascript -e clipboard info", "pngpaste -"}},
		{"text", "darwin", "", []string{"pngpaste"}, map[string]string{"osascript -e clipboard info": info["text"]}, nil, "", []string{"osascript -e clipboard info"}},
		{"finder", "darwin", "", []string{"pngpaste"}, map[string]string{"osascript -e clipboard info": info["finder"]}, nil, "", []string{"osascript -e clipboard info"}},
		{"empty", "darwin", "", nil, map[string]string{"osascript -e clipboard info": info["empty"]}, nil, "", []string{"osascript -e clipboard info"}},
		{"info fails", "darwin", "", nil, map[string]string{"osascript -e clipboard info": "!osascript: no"}, nil, "osascript: no", []string{"osascript -e clipboard info"}},
		{"not a PNG", "darwin", "", []string{"pngpaste"}, map[string]string{"osascript -e clipboard info": info["image"], "pngpaste -": "GIF89a"}, nil, "the image is not a PNG", []string{"osascript -e clipboard info", "pngpaste -"}},
		{"wayland", "linux", ":0", []string{"wl-paste", "xclip"}, map[string]string{"wl-paste --list-types": "text/plain\nimage/png\n", "wl-paste --no-newline --type image/png": string(testPNG)}, testPNG, "", []string{"wl-paste --list-types", "wl-paste --no-newline --type image/png"}},
		{"wayland text", "linux", ":0", []string{"wl-paste"}, map[string]string{"wl-paste --list-types": "text/plain\n"}, nil, "", []string{"wl-paste --list-types"}},
		{"wayland nothing copied", "linux", ":0", []string{"wl-paste"}, map[string]string{"wl-paste --list-types": "!wl-paste: Nothing is copied"}, nil, "", []string{"wl-paste --list-types"}},
		{"wayland no selection", "linux", ":0", []string{"wl-paste"}, map[string]string{"wl-paste --list-types": "!wl-paste: No selection"}, nil, "", []string{"wl-paste --list-types"}},
		{"wayland unreachable", "linux", ":0", []string{"wl-paste"}, map[string]string{"wl-paste --list-types": "!wl-paste: Failed to connect to a Wayland server"}, nil, "wl-paste: Failed to connect to a Wayland server", []string{"wl-paste --list-types"}},
		{"x11", "linux", "", []string{"wl-paste", "xclip"}, map[string]string{"xclip -selection clipboard -t TARGETS -o": "TARGETS\nimage/png\n", "xclip -selection clipboard -t image/png -o": string(testPNG)}, testPNG, "", []string{"xclip -selection clipboard -t TARGETS -o", "xclip -selection clipboard -t image/png -o"}},
		{"x11 text", "linux", "", []string{"xclip"}, map[string]string{"xclip -selection clipboard -t TARGETS -o": "TARGETS\nUTF8_STRING\n"}, nil, "", []string{"xclip -selection clipboard -t TARGETS -o"}},
		{"x11 nothing copied", "linux", "", []string{"xclip"}, map[string]string{"xclip -selection clipboard -t TARGETS -o": "!xclip: Error: target TARGETS not available"}, nil, "", []string{"xclip -selection clipboard -t TARGETS -o"}},
		{"x11 unreachable", "linux", "", []string{"xclip"}, map[string]string{"xclip -selection clipboard -t TARGETS -o": "!xclip: Error: Can't open display: (null)"}, nil, "xclip: Error: Can't open display: (null)", []string{"xclip -selection clipboard -t TARGETS -o"}},
		{"no tool", "linux", ":0", nil, nil, nil, "no clipboard tool: install wl-clipboard or xclip", nil},
	} {
		var ran []string
		cb := clipboard{
			goos:   c.goos,
			getenv: func(k string) string { return map[string]string{"WAYLAND_DISPLAY": c.env}[k] },
			has:    func(name string) bool { return slices.Contains(c.tools, name) },
			run: func(_ context.Context, name string, args ...string) ([]byte, error) {
				line := strings.Join(append([]string{name}, args...), " ")
				ran = append(ran, line)
				out, ok := c.answers[line]
				if !ok {
					return nil, errors.New("not expected: " + line)
				}
				if msg, failed := strings.CutPrefix(out, "!"); failed {
					return nil, errors.New(msg)
				}
				return []byte(out), nil
			},
		}
		got, err := cb.png(context.Background())
		if !bytes.Equal(got, c.want) || (err == nil) != (c.wantErr == "") || err != nil && err.Error() != c.wantErr || !slices.Equal(ran, c.ran) {
			t.Errorf("%s: %q %v, ran %q; want %q, error %q, ran %q", c.name, got, err, ran, c.want, c.wantErr, c.ran)
		}
	}
}

// A message display-message shows is the text as it was, whatever # and
// % it has; a line break becomes a space. Read back with -p, which
// expands the message the same way.
func TestDisplayLiteral(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	for _, s := range []string{"plain", "a #{pane_id} b ## c #", "100% %H %%", "x #[fg=red] y", "two\nlines"} {
		out, err := workspace.Server.Run(ctx, "display-message", "-p", "-t", "boot", displayLiteral(s))
		want := strings.Join(strings.Fields(s), " ")
		if got := strings.TrimSuffix(string(out), "\n"); err != nil || got != want {
			t.Errorf("%q shown as %q %v, want %q", s, got, err, want)
		}
	}
}

// on binds C-v in the root table to the attach pane's paste-image run,
// in the background, with the pane and the client, and the key itself
// elsewhere, once however often it runs; off unbinds it; toggle goes
// by the binding. A user's binding of C-v is left alone and reported
// by on, and left by off, one that has the words paste-image run in it
// too. A failure of list-keys other than no root table, a user's
// after-list-keys hook, is an error, which binds nothing. With no
// server running, off has nothing to do and on says so.
func TestPasteBinding(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	// A path with a blank and a quote, which the run-shell command
	// quotes, and list-keys quotes again.
	const exe = "/usr/local/my bin/it's/laatmux"
	bound := func() string {
		t.Helper()
		line, _, err := pasteBinding(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	for _, sub := range []string{"on", "on"} {
		if err := pasteSwitch(ctx, sub, exe); err != nil {
			t.Fatalf("%s: %v", sub, err)
		}
	}
	if err := pasteSwitch(ctx, "toggle", exe); err != nil || bound() != "" {
		t.Fatalf("toggle from on: %v, bound %q", err, bound())
	}
	if err := pasteSwitch(ctx, "on", exe); err != nil {
		t.Fatal(err)
	}
	// As list-keys prints it, read back into the words bound, with
	// run-shell in the background.
	if line, ours, err := pasteBinding(ctx); err != nil || !ours || !slices.Equal(commandWords(line)[4:], pasteBindArgs(exe)[3:]) {
		t.Errorf("bound %q, ours %v, %v; want the words %q", line, ours, err, pasteBindArgs(exe)[3:])
	}
	if run := commandWords(pasteBindArgs(exe)[6]); len(run) < 3 || run[0] != "run-shell" || run[1] != "-b" {
		t.Errorf("the attach pane's command %q, want run-shell -b", run)
	}
	if keys := string(must(workspace.Server.Run(ctx, "list-keys", "-T", "root"))); strings.Count(keys, "paste-image run") != 1 {
		t.Errorf("bound more than once:\n%s", keys)
	}
	if err := pasteSwitch(ctx, "off", exe); err != nil || bound() != "" {
		t.Fatalf("off: %v, bound %q", err, bound())
	}
	if err := pasteSwitch(ctx, "toggle", exe); err != nil || !strings.Contains(bound(), "paste-image run") {
		t.Fatalf("toggle from off: %v, bound %q", err, bound())
	}
	must(workspace.Server.Run(ctx, "bind-key", "-n", "C-v", "display-message", "mine"))
	for _, sub := range []string{"on", "toggle"} {
		if err := pasteSwitch(ctx, sub, exe); err == nil || !strings.Contains(err.Error(), "C-v in tmux's root table is bound already, and left alone: bind-key -T root C-v display-message mine") {
			t.Errorf("%s over the user's binding: %v", sub, err)
		}
	}
	if err := pasteSwitch(ctx, "off", exe); err != nil || !strings.Contains(bound(), "display-message mine") {
		t.Errorf("off over the user's binding: %v, bound %q", err, bound())
	}
	for _, user := range [][]string{
		{"display-message", "use laatmux paste-image run for images"},
		{"if-shell", "-F", "#{@laatmux_attach_pane}", "display-message 'use paste-image run now'", "send-keys C-v"},
		{"if-shell", "-F", "#{@other}", "run-shell -b 'laatmux paste-image run x'", "send-keys C-v"},
		{"if-shell", "-F", "#{@laatmux_attach_pane}", "run-shell -b custom-paste", `display-message "use laatmux paste-image run for images"`},
		{"if-shell", "-F", "#{@laatmux_attach_pane}", `run-shell -b 'printf "use paste-image run now"'`, "send-keys C-v"},
	} {
		must(workspace.Server.Run(ctx, append([]string{"bind-key", "-n", "C-v"}, user...)...))
		was := bound()
		for _, sub := range []string{"on", "toggle", "off"} {
			err := pasteSwitch(ctx, sub, exe)
			if bound() != was || sub != "off" && err == nil {
				t.Errorf("%s over the user's %q: %v, bound %q", sub, user, err, bound())
			}
		}
	}
	// A root table with no key left in it: tmux 3.6 lists it empty.
	must(workspace.Server.Run(ctx, "unbind-key", "-a", "-T", "root"))
	if err := pasteSwitch(ctx, "on", exe); err != nil || !strings.Contains(bound(), "paste-image run") {
		t.Errorf("on with an empty root table: %v, bound %q", err, bound())
	}
	must(workspace.Server.Run(ctx, "bind-key", "-n", "C-v", "display-message", "use laatmux paste-image run for images"))
	must(workspace.Server.Run(ctx, "set-hook", "-g", "after-list-keys", "list-keys -T nosuch"))
	if err := pasteSwitch(ctx, "on", exe); err == nil || !strings.Contains(err.Error(), "table nosuch doesn't exist") {
		t.Errorf("on with a failing after-list-keys hook: %v", err)
	}
	must(workspace.Server.Run(ctx, "set-hook", "-gu", "after-list-keys"))
	if !strings.Contains(bound(), "display-message \"use laatmux") {
		t.Errorf("a failing after-list-keys hook let on bind: %q", bound())
	}
	must(workspace.Server.Run(ctx, "kill-server"))
	// kill-server returns before the server has gone, and a client
	// that connects meanwhile is told the server exited unexpectedly.
	for i := 0; ; i++ {
		if _, err := workspace.Server.Run(ctx, "-N", "list-sessions"); tmux.NoServer(err) {
			break
		}
		if i == 250 {
			t.Fatal("the server is still there after kill-server")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := pasteSwitch(ctx, "off", exe); err != nil {
		t.Errorf("off with no server: %v", err)
	}
	if err := pasteSwitch(ctx, "on", exe); err == nil || !strings.Contains(err.Error(), "no tmux server is running") {
		t.Errorf("on with no server: %v", err)
	}
	if _, err := workspace.Server.Run(ctx, "-N", "list-sessions"); !tmux.NoServer(err) {
		t.Errorf("a server after on with none: %v", err)
	}
}

// The words of laatmux's binding as list-keys prints it, its arguments
// in double quotes with a backslash before a " or a \ as tmux 3.6 does,
// for a binary path with a blank, a quote and a # in it, and with -r:
// each level down to the shell command's words, which pasteOurs reads,
// is the command pasteBindArgs bound.
func TestCommandWords(t *testing.T) {
	for _, exe := range []string{"/usr/local/bin/laatmux", "/a b/it's #1/laatmux"} {
		args := pasteBindArgs(exe)
		quoted := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
		line := "bind-key -r -T root C-v " + strings.Join(args[3:5], " ") + " " + quoted(args[5]) + " " + quoted(args[6]) + " " + quoted(args[7])
		f := commandWords(line)
		if !slices.Equal(f[:5], []string{"bind-key", "-r", "-T", "root", "C-v"}) || !slices.Equal(f[5:], args[3:]) {
			t.Fatalf("%s: words %q, want the bound command %q", line, f, args[3:])
		}
		if !pasteOurs(f[5:]) {
			t.Errorf("%s: not ours", line)
		}
		run := commandWords(f[8])
		if sh := commandWords(run[len(run)-1]); !slices.Equal(sh, []string{tmux.FormatLiteral(exe), "paste-image", "run", "#{pane_id}", "#{client_name}"}) {
			t.Errorf("%s: the shell command's words %q", line, sh)
		}
	}
}

// rawPane opens a window in the session whose program puts the
// terminal in raw mode and writes the first byte it reads to a file,
// and returns the pane and the file once it reads.
func rawPane(t *testing.T, session string) (pane, out string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	out, ready := filepath.Join(dir, "out"), filepath.Join(dir, "ready")
	script := "stty raw -echo; : > " + tmux.ShellJoin([]string{ready}) + "; dd bs=1 count=1 of=" + tmux.ShellJoin([]string{out}) + " 2>/dev/null; sleep 1000"
	pane = strings.TrimSpace(string(must(workspace.Server.Run(ctx, "new-window", "-d", "-t", tmux.SessionTarget(session), "-P", "-F", "#{pane_id}", script))))
	for i := 0; ; i++ {
		if _, err := os.Stat(ready); err == nil {
			return pane, out
		}
		if i == 250 {
			t.Fatalf("pane %s never read", pane)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// firstByte is the byte the raw pane read, after an x typed into it:
// the x when nothing was typed before it.
func firstByte(t *testing.T, pane, out string) string {
	t.Helper()
	must(workspace.Server.Run(context.Background(), "send-keys", "-t", pane, "-l", "x"))
	for i := 0; i < 250; i++ {
		if b, _ := os.ReadFile(out); len(b) > 0 {
			return string(b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pane %s read nothing", pane)
	return ""
}

// controlClient attaches a control-mode client to the session and
// returns its name and what it has printed so far: a display-message
// to it is a line of its output.
func controlClient(t *testing.T, session string) (name string, output func() string) {
	t.Helper()
	ctx := context.Background()
	c := exec.Command("tmux", "-L", "default", "-C", "attach", "-t", tmux.SessionTarget(session))
	in, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var buf bytes.Buffer
	c.Stdout = writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	})
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); _ = c.Process.Kill(); _ = c.Wait() })
	for i := 0; i < 50 && name == ""; i++ {
		time.Sleep(100 * time.Millisecond)
		name = strings.TrimSpace(string(must(workspace.Server.Run(ctx, "list-clients", "-F", "#{client_name}"))))
	}
	if name == "" {
		t.Fatal("no client attached")
	}
	return name, func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// writerFunc is a function as an io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// run, by the pane's session and the clipboard: in a workspace of a
// remote host, with an image, the client is told while the image is on
// its way and once it has arrived, the image goes to the host for the
// workspace's root, and nothing is typed; with no image the pane gets
// C-v. A workspace on this machine and a session not laatmux's get C-v
// without the clipboard being read; so does a session whose host the
// config has not, or a config that does not load, when the clipboard
// has no image. A failure is a message to the client, and then nothing
// is typed: the host not configured or the config broken, with an
// image; the clipboard failing or not answering in time; a plain
// attachment with an image, which has no agent; a paused host, before
// anything is said to be sent; the host's refusal; no answer from the
// host in time, said with the image's size, though the send fails
// with the transport's error. run itself never fails.
func TestPasteRun(t *testing.T) {
	isolatedDefault(t)
	ctx := context.Background()
	dir := t.TempDir()
	good, broken := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(good, []byte("hosts:\n  - name: mac\n  - name: vm\n    ssh: vm\n  - name: pz\n    ssh: pz\n    paused: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("hosts: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", broken)
	_, brokenErr := config.Load()
	if brokenErr == nil {
		t.Fatal("the broken config loads")
	}
	// A clipboard that never answers, and a host that does not, wait
	// for the bound; a bound gone fails the case in five seconds rather
	// than hanging the test.
	unbounded := func(ctx context.Context, err error) error {
		select {
		case <-ctx.Done():
			return err
		case <-time.After(5 * time.Second):
			return errors.New("not bounded")
		}
	}
	errHang := errors.New("hang")
	var clipPNG []byte
	var clipErr, sendErr error
	reads := 0
	var sent []string
	wasClip, wasSend, wasClipTimeout, wasPasteTimeout := clipboardPNG, sendPaste, clipboardTimeout, pasteTimeout
	t.Cleanup(func() {
		clipboardPNG, sendPaste, clipboardTimeout, pasteTimeout = wasClip, wasSend, wasClipTimeout, wasPasteTimeout
	})
	clipboardTimeout, pasteTimeout = 200*time.Millisecond, 300*time.Millisecond
	clipboardPNG = func(ctx context.Context) ([]byte, error) {
		reads++
		if clipErr == errHang {
			// The tool the context kills fails as killed.
			return nil, unbounded(ctx, errors.New("osascript: signal: killed"))
		}
		return clipPNG, clipErr
	}
	sendPaste = func(ctx context.Context, h peer.Host, env, root string, png []byte) error {
		sent = append(sent, fmt.Sprintf("%s %s %s %s", h.Name, env, root, png))
		if sendErr == errHang {
			// A write the closed connection cuts short fails with
			// the transport's error, not the context's.
			return unbounded(ctx, errors.New("write |1: file already closed"))
		}
		return sendErr
	}
	client, output := controlClient(t, "boot")
	tag := func(host, key, attach string) {
		t.Helper()
		for opt, v := range map[string]string{"@laatmux_host": host, "@laatmux_workspace": key, "@laatmux_attach": attach} {
			if v == "" {
				must(workspace.Server.Run(ctx, "set-option", "-u", "-t", "boot", opt))
			} else {
				must(workspace.Server.Run(ctx, "set-option", "-t", "boot", opt, v))
			}
		}
	}
	const sending, arrived = "sending a 1 KB image to vm", "sent a 1 KB image to vm"
	toVM := "vm venv /w/a " + string(testPNG)
	for _, c := range []struct {
		name              string
		cfg               string
		host, key, attach string
		png               []byte
		clipErr, sendErr  error
		typed             string // the first byte the pane read
		reads             int
		sent              string
		notices, hidden   []string
	}{
		{name: "remote image", host: "vm", key: "venv//w/a", png: testPNG, typed: "x", reads: 1, sent: toVM, notices: []string{sending, arrived}},
		{name: "remote no image", host: "vm", key: "venv//w/a", typed: "\x16", reads: 1},
		{name: "local", host: "mac", key: "menv//w/a", png: testPNG, typed: "\x16"},
		{name: "not laatmux's", png: testPNG, typed: "\x16"},
		{name: "unknown host", host: "gone", key: "genv//w/a", png: testPNG, typed: "x", reads: 1, notices: []string{`unknown host "gone"`}},
		{name: "unknown host no image", host: "gone", key: "genv//w/a", typed: "\x16", reads: 1},
		{name: "broken config", cfg: broken, host: "vm", key: "venv//w/a", png: testPNG, typed: "x", reads: 1, notices: []string{brokenErr.Error()}},
		{name: "broken config no image", cfg: broken, host: "vm", key: "venv//w/a", typed: "\x16", reads: 1},
		{name: "clipboard fails", host: "vm", key: "venv//w/a", clipErr: errors.New("osascript: 100% #broken"), typed: "x", reads: 1, notices: []string{"reading the clipboard: osascript: 100% #broken"}},
		{name: "clipboard hangs", host: "vm", key: "venv//w/a", clipErr: errHang, typed: "x", reads: 1, notices: []string{"reading the clipboard: no answer within 200ms"}},
		{name: "plain attachment", host: "vm", attach: "vm/s", png: testPNG, typed: "x", reads: 1, notices: []string{"boot is not a workspace session: the image has no agent to go to"}},
		{name: "paused host", host: "pz", key: "penv//w/a", png: testPNG, typed: "x", reads: 1, notices: []string{"host pz is paused; laatmux hosts resume pz connects it"}, hidden: []string{"sending"}},
		{name: "host refuses", host: "vm", key: "venv//w/a", png: testPNG, sendErr: errors.New("vm: no agent to deliver to"), typed: "x", reads: 1, sent: toVM, notices: []string{sending, "vm: no agent to deliver to"}, hidden: []string{arrived}},
		{name: "host does not answer", host: "vm", key: "venv//w/a", png: testPNG, sendErr: errHang, typed: "x", reads: 1, sent: toVM, notices: []string{sending, "vm: no answer within 300ms sending a 1 KB image"}, hidden: []string{arrived}},
	} {
		if c.cfg == "" {
			c.cfg = good
		}
		t.Setenv("LAATMUX_CONFIG", c.cfg)
		tag(c.host, c.key, c.attach)
		clipPNG, clipErr, sendErr, reads, sent = c.png, c.clipErr, c.sendErr, 0, nil
		pane, out := rawPane(t, "boot")
		before := output()
		if err := cmdPasteImage(ctx, []string{"run", pane, client}); err != nil {
			t.Errorf("%s: run failed: %v", c.name, err)
		}
		if got := firstByte(t, pane, out); got != c.typed {
			t.Errorf("%s: the pane read %q first, want %q", c.name, got, c.typed)
		}
		if reads != c.reads || strings.Join(sent, "\n") != c.sent {
			t.Errorf("%s: the clipboard read %d times, sent %q; want %d, %q", c.name, reads, sent, c.reads, c.sent)
		}
		shown := func() string { return strings.TrimPrefix(output(), before) }
		for _, n := range c.notices {
			n = "laatmux paste-image: " + n
			for i := 0; i < 100 && !strings.Contains(shown(), n); i++ {
				time.Sleep(20 * time.Millisecond)
			}
			if !strings.Contains(shown(), n) {
				t.Errorf("%s: the client was shown %q, want %q in it", c.name, shown(), n)
			}
		}
		for _, n := range c.hidden {
			if strings.Contains(shown(), "laatmux paste-image: "+n) {
				t.Errorf("%s: the client was shown %q, with %q", c.name, shown(), n)
			}
		}
		if len(c.notices) == 0 && strings.Contains(shown(), "laatmux paste-image") {
			t.Errorf("%s: the client was shown %q", c.name, shown())
		}
	}
}

// The key itself, pressed through a client: in an attach pane C-v runs
// paste-image run with the pane and the client, here a script that
// records its arguments, under a path with a # in it; in any other pane
// the program gets C-v. send-keys -K, which looks the key up as if the
// client typed it, needs tmux 3.4.
func TestPasteKey(t *testing.T) {
	isolatedDefault(t)
	if out, err := exec.Command("tmux", "-V").Output(); err == nil {
		if v := strings.TrimPrefix(strings.TrimSpace(string(out)), "tmux "); v < "3.4" {
			t.Skipf("tmux %s has no send-keys -K", v)
		}
	}
	ctx := context.Background()
	logf := filepath.Join(t.TempDir(), "args")
	dir := filepath.Join(t.TempDir(), "x#{pane_id}y")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "laatmux")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho \"$@\" >> "+tmux.ShellJoin([]string{logf})+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pasteSwitch(ctx, "on", exe); err != nil {
		t.Fatal(err)
	}
	client, _ := controlClient(t, "boot")
	attach, _ := rawPane(t, "boot")
	must(workspace.Server.Run(ctx, "set-option", "-p", "-t", attach, "@laatmux_attach_pane", "1"))
	must(workspace.Server.Run(ctx, "select-window", "-t", attach))
	must(workspace.Server.Run(ctx, "send-keys", "-K", "-c", client, "C-v"))
	want := "paste-image run " + attach + " " + client + "\n"
	got := ""
	for i := 0; i < 250 && got != want; i++ {
		time.Sleep(20 * time.Millisecond)
		b, _ := os.ReadFile(logf)
		got = string(b)
	}
	if got != want {
		t.Errorf("C-v in the attach pane ran %q, want %q", got, want)
	}
	other, out := rawPane(t, "boot")
	must(workspace.Server.Run(ctx, "select-window", "-t", other))
	must(workspace.Server.Run(ctx, "send-keys", "-K", "-c", client, "C-v"))
	if b := firstByte(t, other, out); b != "\x16" {
		t.Errorf("C-v in another pane: it read %q first", b)
	}
	if b, _ := os.ReadFile(logf); string(b) != want {
		t.Errorf("C-v in another pane ran %q", b)
	}
	if err := pasteSwitch(ctx, "off", exe); err != nil {
		t.Fatal(err)
	}
}

// sendPaste dials the host's daemon, here a fake reached as this
// machine's, and sends the image for the root under the workspace's
// environment, the bytes whole through the JSON line; the daemon's
// refusal is the error, named by the host. A daemon without paste, or
// answering as another environment, is refused before anything is
// sent.
func TestSendPaste(t *testing.T) {
	big := append(bytes.Clone(testPNG), bytes.Repeat([]byte{0, 0xff, '\n', '"'}, 1<<20)...)
	for _, c := range []struct {
		name, env string
		caps      []string
		reply     string
		want      string
	}{
		{"pasted", "venv", []string{protocol.CapPaste}, "", ""},
		{"refused", "venv", []string{protocol.CapPaste}, "no agent to deliver to: no managed session in /w/a", "vm: no agent to deliver to: no managed session in /w/a"},
		{"no paste", "venv", []string{protocol.CapStatus}, "", "vm: daemon fake has no paste; laatmux upgrade vm installs one that has"},
		{"another environment", "other", []string{protocol.CapPaste}, "", "vm: answers as environment other, not venv the workspace is of"},
	} {
		got := make(chan protocol.Message, 1)
		startFakeDaemonAs(t, c.env, c.caps, func(pc *protocol.Conn, m protocol.Message) bool {
			if m.Type == protocol.TypeHello {
				return true
			}
			got <- m
			pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: c.reply == "", Error: c.reply})
			return true
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := sendPaste(ctx, peer.Host{Name: "vm"}, "venv", "/w/a", big)
		cancel()
		if (err == nil) != (c.want == "") || err != nil && err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
		select {
		case m := <-got:
			if c.name == "no paste" || c.name == "another environment" {
				t.Errorf("%s: sent %s", c.name, m.Type)
			} else if m.Type != protocol.TypePaste || m.EnvironmentID != "venv" || m.Root != "/w/a" || !bytes.Equal(m.Image, big) || m.ID == "" {
				t.Errorf("%s: sent %s %s %s, %d bytes", c.name, m.Type, m.EnvironmentID, m.Root, len(m.Image))
			}
		default:
			if c.name == "pasted" || c.name == "refused" {
				t.Errorf("%s: nothing sent", c.name)
			}
		}
	}
}
