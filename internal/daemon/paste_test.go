package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

// pasteDaemon is a daemon whose managed server has one pane, made by
// laatmux at root, with a verified claude in it, observed once; its
// images go to dir.
func pasteDaemon(t *testing.T, root, dir string) (*Daemon, *fakeServer) {
	t.Helper()
	ft := &fakeServer{panes: []tmux.Pane{{ID: "%1", Session: "proj/a", SessionID: "$1", TTY: "/dev/pts/9", Managed: true, Cwd: root, ServerPID: 5}}, screen: idleScreen}
	d := New(Config{EnvironmentID: "env", Targets: managed(ft), Procs: &fakeProcs{tables: []procTable{{procs: []procs.Proc{shell, claude}}}}, Paste: dir})
	if err := d.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d, ft
}

// pngData is a PNG as the paste message takes it: the signature, then
// anything.
var pngData = []byte(protocol.PNGSignature + "image data")

// pasted is every paste the fake took, read under its lock.
func (f *fakeServer) pasted() []fakePaste {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pastes)
}

// files is the names in dir.
func files(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// paste writes the image to a new file in the paste directory, mode
// 0600, after removing the images there older than an hour, and types
// the file's path into the agent's pane at the root with no Enter,
// through a buffer the start's sweep takes; a newer image stays, and
// so does a file that is no image. A second paste gets a file of its
// own. The pane's observation is spent, as after a prompt, and no
// paste is left counted in flight. A daemon without the managed server
// has no paste.
func TestPaste(t *testing.T) {
	root := "/w/proj/a"
	dir := filepath.Join(t.TempDir(), "paste")
	d, ft := pasteDaemon(t, root, dir)
	if !protocol.Has(d.capabilities(), protocol.CapPaste) {
		t.Fatalf("no paste: %v", d.capabilities())
	}
	if caps := New(Config{Targets: unmanaged(onePane(pane, nil))}).capabilities(); protocol.Has(caps, protocol.CapPaste) {
		t.Fatalf("paste without the managed server: %v", caps)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for name, age := range map[string]time.Duration{"old.png": 2 * time.Hour, "recent.png": 10 * time.Minute, "notes.txt": 2 * time.Hour} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	pc := conn(t, d)
	var paths []string
	sent := time.Now()
	for _, id := range []string{"p1", "p2"} {
		pc.Write(protocol.Message{Type: protocol.TypePaste, ID: id, EnvironmentID: "env", Root: root, Image: pngData})
		if res, _ := result(t, pc, id); !res.OK || res.Error != "" {
			t.Fatalf("%s: %+v", id, res)
		}
		ps := ft.pasted()
		if len(ps) != len(paths)+1 {
			t.Fatalf("%s: pasted %+v", id, ps)
		}
		p := ps[len(ps)-1]
		if p.pane != "%1" || p.enter || filepath.Dir(p.text) != dir || !strings.HasSuffix(p.text, ".png") || !strings.HasPrefix(p.buffer, attemptBufferPrefix) {
			t.Errorf("%s: pasted %+v, want the path of a .png in %s into %%1 with no Enter, through a buffer named %s...", id, p, dir, attemptBufferPrefix)
		}
		b, err := os.ReadFile(p.text)
		if err != nil || string(b) != string(pngData) {
			t.Errorf("%s: the file has %q %v", id, b, err)
		}
		if fi, err := os.Stat(p.text); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: the file's mode %v %v", id, fi.Mode(), err)
		}
		paths = append(paths, p.text)
	}
	if paths[0] == paths[1] {
		t.Errorf("two pastes wrote one file %s", paths[0])
	}
	if got := files(t, dir); slices.Contains(got, "old.png") || !slices.Contains(got, "recent.png") || !slices.Contains(got, "notes.txt") || len(got) != 4 {
		t.Errorf("the directory after two pastes: %q, want old.png gone, recent.png and notes.txt kept and the two images", got)
	}
	d.mu.Lock()
	spent, pasting := d.tasks.pasted[paneKey("laatmux", "%1")], d.tasks.pasting
	d.mu.Unlock()
	if spent.Before(sent) || pasting != 0 {
		t.Errorf("after the pastes: the pane's observation spent at %v, before %v; %d pastes in flight", spent, sent, pasting)
	}
}

// A refusal writes and pastes nothing: another environment, no root,
// data that is not a PNG, a root with no agent of laatmux's in it, a
// pane whose verified observation is from the server instance before
// a restart or from another session, a daemon shutting down. A paste
// that fails says how far
// it got: a buffer that would not load reached nothing, a paste-buffer
// that failed may have reached the pane.
func TestPasteRefused(t *testing.T) {
	root := "/w/proj/a"
	dir := filepath.Join(t.TempDir(), "paste")
	d, ft := pasteDaemon(t, root, dir)
	pc := conn(t, d)
	restarted := func(pid int) func() { return func() { ft.panes[0].ServerPID = pid } }
	renamed := func(session string) func() { return func() { ft.panes[0].ServerPID, ft.panes[0].Session = 5, session } }
	for _, c := range []struct {
		name   string
		m      protocol.Message
		before func()
		want   string
	}{
		{"another environment", protocol.Message{EnvironmentID: "other", Root: root, Image: pngData}, nil, "this host is environment env, not other"},
		{"no root", protocol.Message{EnvironmentID: "env", Image: pngData}, nil, "root required"},
		{"not a PNG", protocol.Message{EnvironmentID: "env", Root: root, Image: []byte("GIF89a")}, nil, "not a PNG image"},
		{"no image", protocol.Message{EnvironmentID: "env", Root: root}, nil, "not a PNG image"},
		{"no agent at the root", protocol.Message{EnvironmentID: "env", Root: "/w/proj/b", Image: pngData}, nil, "no agent to deliver to: no managed session in /w/proj/b"},
		{"server restarted", protocol.Message{EnvironmentID: "env", Root: root, Image: pngData}, restarted(6), "no agent to deliver to: pane %1 has no observation yet as it is listed, in session proj/a on server 6"},
		{"another session", protocol.Message{EnvironmentID: "env", Root: root, Image: pngData}, renamed("proj/b"), "no agent to deliver to: pane %1 has no observation yet as it is listed, in session proj/b on server 5"},
	} {
		if c.before != nil {
			ft.set(c.before)
		}
		c.m.Type, c.m.ID = protocol.TypePaste, c.name
		pc.Write(c.m)
		if res, _ := result(t, pc, c.name); res.OK || res.Error != c.want {
			t.Errorf("%s: %+v, want the error %q", c.name, res, c.want)
		}
	}
	ft.set(renamed("proj/a"))
	if ps := ft.pasted(); len(ps) != 0 {
		t.Errorf("pasted after refusals: %+v", ps)
	}
	if got := files(t, dir); len(got) != 0 {
		t.Errorf("files after refusals: %q", got)
	}
	for _, c := range []struct {
		step, want string
	}{
		{"load", "paste refused: paste (load): boom"},
		{"paste", "may have reached pane %1: paste (paste): boom"},
	} {
		ft.set(func() { ft.pasteErr = &tmux.PasteError{Step: c.step, Err: errors.New("boom")} })
		pc.Write(protocol.Message{Type: protocol.TypePaste, ID: c.step, EnvironmentID: "env", Root: root, Image: pngData})
		if res, _ := result(t, pc, c.step); res.OK || !strings.HasSuffix(res.Error, c.want) {
			t.Errorf("%s fails: %+v, want an error ending %q", c.step, res, c.want)
		}
	}
	ft.set(func() { ft.pasteErr = nil })
	before := files(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.StopRuns(ctx)
	pc.Write(protocol.Message{Type: protocol.TypePaste, ID: "stopping", EnvironmentID: "env", Root: root, Image: pngData})
	if res, _ := result(t, pc, "stopping"); res.OK || res.Error != "daemon shutting down" {
		t.Errorf("a daemon shutting down: %+v", res)
	}
	if got := files(t, dir); len(got) != len(before) || len(ft.pasted()) != 0 {
		t.Errorf("a daemon shutting down wrote %q, pasted %+v", got, ft.pasted())
	}
}

// A paste waits for the root's delivery lock, which a prompt's paste
// and rm hold, before it looks for the agent or writes; and a daemon
// that stops waits for a paste in flight, and no longer once it is
// done. The waits are seen not to end within a fifth of a second.
func TestPasteWaits(t *testing.T) {
	root := "/w/proj/a"
	dir := filepath.Join(t.TempDir(), "paste")
	d, ft := pasteDaemon(t, root, dir)
	pc := conn(t, d)
	unlock := d.tasks.lockDeliveries(root)
	pc.Write(protocol.Message{Type: protocol.TypePaste, ID: "locked", EnvironmentID: "env", Root: root, Image: pngData})
	time.Sleep(200 * time.Millisecond)
	if got := files(t, dir); len(got) != 0 || len(ft.pasted()) != 0 {
		t.Errorf("under the root's delivery lock: wrote %q, pasted %+v", got, ft.pasted())
	}
	unlock()
	if res, _ := result(t, pc, "locked"); !res.OK {
		t.Fatalf("after the lock: %+v", res)
	}
	hold := make(chan struct{})
	ft.set(func() { ft.pasteHold = hold })
	pc.Write(protocol.Message{Type: protocol.TypePaste, ID: "held", EnvironmentID: "env", Root: root, Image: pngData})
	// The file is written before the paste starts.
	for i := 0; len(files(t, dir)) < 2; i++ {
		if i == 250 {
			t.Fatal("the second image was never written")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopped := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		d.StopRuns(ctx)
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("the daemon stopped with a paste in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(hold)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon still waits once the paste is done")
	}
	if res, _ := result(t, pc, "held"); !res.OK {
		t.Fatalf("the paste in flight: %+v", res)
	}
}

// Without a paste directory the images go to /tmp as
// laatmux-paste-<timestamp>.png, and the pruning there takes only
// laatmux's: another program's old image stays. Two images written in
// the same millisecond get two files. A paste directory not there is
// made, mode 0700, and a relative one is written as an absolute path.
func TestPasteTmp(t *testing.T) {
	was := pasteTmp
	pasteTmp = t.TempDir()
	t.Cleanup(func() { pasteTmp = was })
	now := time.Now()
	for _, name := range []string{"laatmux-paste-old.png", "other.png"} {
		p := filepath.Join(pasteTmp, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	a, err := writePaste("", pngData, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := writePaste("", pngData, now)
	if err != nil {
		t.Fatal(err)
	}
	stamp := "laatmux-paste-" + now.Format("20060102-150405.000")
	if a != filepath.Join(pasteTmp, stamp+".png") || b != filepath.Join(pasteTmp, stamp+"-2.png") {
		t.Errorf("wrote %s and %s, want %s.png and %s-2.png in %s", a, b, stamp, stamp, pasteTmp)
	}
	if got := files(t, pasteTmp); slices.Contains(got, "laatmux-paste-old.png") || !slices.Contains(got, "other.png") {
		t.Errorf("/tmp after the pastes: %q", got)
	}
	t.Chdir(t.TempDir())
	p, err := writePaste(filepath.Join("state", "paste"), pngData, now)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Dir(p))
	if !filepath.IsAbs(p) || err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("a relative paste directory: wrote %s, the directory %v %v", p, fi.Mode(), err)
	}
}
