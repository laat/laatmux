package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/term"
	"github.com/laat/laatmux/internal/view"
	"github.com/laat/laatmux/internal/workspace"
)

const pauseConfig = `hosts:
  - name: mac
    repos: /r
    worktrees: /w
  - name: vm
    ssh: vm.invalid   # the coder box
    repos: /r
    worktrees: /w
  - name: box
    ssh: box.invalid
agents:
  claude: {cmd: [claude]}
`

// pauseFixture is a config file with pauseConfig in it, which
// LAATMUX_CONFIG names, and a state directory of the test's own for the
// writer's lock.
func pauseFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(pauseConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", p)
	t.Setenv("LAATMUX_HOME", filepath.Join(dir, "home"))
	return p
}

// hosts lists a paused host without dialling it; the others are dialled
// as before.
func TestProbeHostsSkipsPaused(t *testing.T) {
	cfg, err := config.Parse([]byte(strings.Replace(pauseConfig, "    ssh: vm.invalid   # the coder box\n", "    ssh: vm.invalid\n    paused: true\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var dialled []string
	dial := func(ctx context.Context, h peer.Host) (*client.Conn, error) {
		mu.Lock()
		dialled = append(dialled, h.Name)
		mu.Unlock()
		return nil, errors.New("refused")
	}
	rows := probeHosts(context.Background(), cfg.Hosts, dial)
	slices.Sort(dialled)
	if strings.Join(dialled, " ") != "box mac" {
		t.Fatalf("dialled %v", dialled)
	}
	if rows[1].name != "vm" || rows[1].status != "paused; laatmux hosts resume vm connects it" || rows[2].status != "unreachable: refused" {
		t.Fatalf("rows %+v", rows)
	}
}

// hosts pause and resume set paused on the host's entry and take it off,
// the rest of the file kept; a host that is so already is said to be,
// and this machine, a host not listed and other words are refused.
func TestHostsPauseResume(t *testing.T) {
	p := pauseFixture(t)
	ctx := context.Background()
	read := func() string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	paused := strings.Replace(pauseConfig, "    worktrees: /w\n  - name: box", "    worktrees: /w\n    paused: true\n  - name: box", 1)
	if err := cmdHosts(ctx, []string{"pause", "vm"}); err != nil || read() != paused {
		t.Fatalf("pause: %v, file:\n%s", err, read())
	}
	if err := cmdHosts(ctx, []string{"pause", "vm"}); err != nil || read() != paused {
		t.Fatalf("pause again: %v, file:\n%s", err, read())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if vm, _ := cfg.Find("vm"); !vm.Paused {
		t.Fatalf("vm %+v", vm)
	}
	if err := cmdHosts(ctx, []string{"resume", "vm"}); err != nil || read() != pauseConfig {
		t.Fatalf("resume: %v, file:\n%s", err, read())
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"pause", "mac"}, "mac is this machine"},
		{[]string{"resume", "nope"}, `unknown host "nope"`},
		{[]string{"pause"}, "usage: laatmux hosts [pause|resume <host>]"},
		{[]string{"stop", "vm"}, "usage: laatmux hosts [pause|resume <host>]"},
	} {
		if err := cmdHosts(ctx, c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v", c.args, err)
		}
	}
	if read() != pauseConfig {
		t.Fatalf("a refusal changed the file:\n%s", read())
	}
	if l := pausedLine("vm", true, true); l != "vm paused; this machine dials it for upgrade alone until it is resumed" {
		t.Errorf("paused line %q", l)
	}
	if l := pausedLine("vm", false, false); l != "vm is not paused" {
		t.Errorf("not paused line %q", l)
	}
}

// hosts pause notes a local daemon that is older than pause, which
// dials the host all the same; one with the capability, and none
// running, get no note.
func TestPauseNoteForOlderDaemon(t *testing.T) {
	if n := pauseUnknown(protocol.Message{}); n != "" {
		t.Errorf("no daemon: %q", n)
	}
	pauseFixture(t)
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, nil)
	const note = "the local daemon fake is older than pause and dials a paused host all the same; laatmux stop ends it, and the next command starts this build"
	if n := pauseUnknown(localHello(context.Background())); n != note {
		t.Errorf("older daemon: %q", n)
	}
	// The CLI and H say it on a pause, not on a resume.
	out := func(args ...string) string {
		t.Helper()
		f, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		stdout := os.Stdout
		os.Stdout = f
		err = cmdHosts(context.Background(), args)
		os.Stdout = stdout
		f.Close()
		b, _ := os.ReadFile(f.Name())
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return string(b)
	}
	if got := out("pause", "vm"); !strings.HasSuffix(got, "\n"+note+"\n") {
		t.Errorf("hosts pause: %q", got)
	}
	if got := out("resume", "vm"); strings.Contains(got, "older than pause") {
		t.Errorf("hosts resume: %q", got)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	m := &view.Model{}
	if d.setPaused(m, "vm", true); !strings.HasSuffix(m.Message, "; "+note) {
		t.Errorf("H pausing: %q", m.Message)
	}
	if d.setPaused(m, "vm", false); strings.Contains(m.Message, "older than pause") {
		t.Errorf("H resuming: %q", m.Message)
	}
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged, protocol.CapPause}, nil)
	if n := pauseUnknown(localHello(context.Background())); n != "" {
		t.Errorf("daemon with pause: %q", n)
	}
}

// H opens a picker of the hosts reached over ssh with their state, in
// the dashboard and the sidebar alike; Enter on one pauses it in the
// config, and on a paused one resumes it.
func TestHostPicker(t *testing.T) {
	p := pauseFixture(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	st := merged.New()
	st.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{
		{Name: "mac", Connected: true, Listed: true},
		{Name: "vm", SSH: "vm", Connected: true, Listed: true},
		{Name: "box", SSH: "box", Error: "ssh: no route"},
	}})
	d := &dash{ctx: context.Background(), cfg: cfg, st: st, reload: config.LoadSettled}
	m := dashModel(cfg)
	key := view.Action{Kind: view.ActionOther, Key: term.Key{Kind: term.KeyRune, Rune: 'H'}}
	if !sidebarAction(m, key) {
		t.Fatal("the sidebar does not take H")
	}
	d.act(m, key)
	hp, ok := m.Overlay.(*hostPicker)
	if !ok {
		t.Fatalf("overlay %T, message %q", m.Overlay, m.Message)
	}
	if got := []view.Choice{{Label: "vm", Detail: "connected"}, {Label: "box", Detail: "down: ssh: no route"}}; !slices.Equal(hp.Choices, got) {
		t.Fatalf("choices %+v", hp.Choices)
	}
	hp.Handle(term.Key{Kind: term.KeyEnter})
	a := m.Poll()
	if !sidebarAction(m, a) {
		t.Fatal("the sidebar does not take the picker's end")
	}
	d.act(m, a)
	if m.Overlay != nil || m.Message != "vm paused; this machine dials it for upgrade alone until it is resumed" {
		t.Fatalf("overlay %v message %q", m.Overlay, m.Message)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "    worktrees: /w\n    paused: true\n  - name: box") {
		t.Fatalf("file:\n%s", b)
	}
	// The view's config has it at once, for a form opened next.
	if h, _ := d.cfg.Find("vm"); !h.Paused {
		t.Fatalf("the view's config after H: %+v", h)
	}
	// The stream has not caught up: the config says paused.
	d.act(m, key)
	hp = m.Overlay.(*hostPicker)
	if hp.Choices[0].Detail != "paused" {
		t.Fatalf("choices after the pause %+v", hp.Choices)
	}
	hp.Handle(term.Key{Kind: term.KeyEnter})
	d.act(m, m.Poll())
	if b, _ := os.ReadFile(p); string(b) != pauseConfig || m.Message != "vm resumed; it is dialled again" {
		t.Fatalf("message %q, file:\n%s", m.Message, b)
	}
	// The stream still has vm paused, and box no record: the daemon has
	// yet to read the file.
	st.Apply(protocol.Message{Type: protocol.TypeUpsert, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", Paused: true}})
	st.Apply(protocol.Message{Type: protocol.TypeRemove, HostName: "box"})
	d.act(m, key)
	if got := []view.Choice{{Label: "vm", Detail: "resuming"}, {Label: "box", Detail: "connecting"}}; !slices.Equal(m.Overlay.(*hostPicker).Choices, got) {
		t.Fatalf("choices while the daemon catches up %+v", m.Overlay.(*hostPicker).Choices)
	}
	m.Overlay.Handle(term.Key{Kind: term.KeyEsc})
	d.act(m, m.Poll())
	// vm connected again, its snapshot not yet in.
	st.Apply(protocol.Message{Type: protocol.TypeUpsert, HostStatus: &protocol.HostStatus{Name: "vm", SSH: "vm", Connected: true}})
	d.act(m, key)
	if got := m.Overlay.(*hostPicker).Choices[0]; got != (view.Choice{Label: "vm", Detail: "connected, snapshot pending"}) {
		t.Fatalf("a snapshot pending: %+v", got)
	}
	m.Overlay.Handle(term.Key{Kind: term.KeyEsc})
	d.act(m, m.Poll())
	// Esc leaves the file alone.
	d.act(m, key)
	m.Overlay.Handle(term.Key{Kind: term.KeyEsc})
	d.act(m, m.Poll())
	if b, _ := os.ReadFile(p); string(b) != pauseConfig || m.Overlay != nil {
		t.Fatalf("after esc: overlay %v, file:\n%s", m.Overlay, b)
	}
}

// The hosts line has an entry per host the config reaches over ssh, in
// its order, this machine's left out: paused as the config says,
// whatever the stream has; else connected and listed, down, or
// connecting, which a host with no record yet, one the daemon still has
// paused and one with its snapshot pending are.
func TestHostEntries(t *testing.T) {
	cfg, err := config.Parse([]byte(`hosts:
  - name: mac
  - {name: vm, ssh: vm}
  - {name: box, ssh: box}
  - {name: coder, ssh: coder, paused: true}
  - {name: new, ssh: new}
  - {name: late, ssh: late}
  - {name: pend, ssh: pend}
`))
	if err != nil {
		t.Fatal(err)
	}
	st := merged.New()
	st.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{
		{Name: "mac", Connected: true, Listed: true},
		{Name: "vm", SSH: "vm", Connected: true, Listed: true},
		{Name: "box", SSH: "box", Error: "ssh: no route"},
		{Name: "coder", SSH: "coder", Connected: true, Listed: true},
		{Name: "late", SSH: "late", Paused: true},
		{Name: "pend", SSH: "pend", Connected: true},
	}})
	want := []view.HostEntry{{Name: "vm"}, {Name: "box", Down: true}, {Name: "coder", Paused: true},
		{Name: "new", Connecting: true}, {Name: "late", Connecting: true}, {Name: "pend", Connecting: true}}
	if got := hostEntries(cfg, st.Status("")); !slices.Equal(got, want) {
		t.Fatalf("entries %+v", got)
	}
}

// The hosts line is filled by the view's refresh, and a click on it in
// a sidebar pane, whose host takes it as the dashboard's does, pauses
// the host clicked through the write H makes, the line showing the flip
// on the next draw, with a local daemon older than pause noted in the
// footer; a click on the paused one resumes it. A write refused, a
// config file that does not parse, is in the footer, the line and the
// file as they were.
func TestHostsLineClick(t *testing.T) {
	p := pauseFixture(t)
	startFakeDaemon(t, []string{protocol.CapStatus, protocol.CapMerged}, nil)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	st := merged.New()
	st.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{
		{Name: "mac", Connected: true, Listed: true},
		{Name: "vm", SSH: "vm", Connected: true, Listed: true},
		{Name: "box", SSH: "box", Error: "ssh: no route"},
	}})
	// A sidebar pane's: the dashboard's keys are off.
	d := &dash{ctx: context.Background(), st: st, reload: config.LoadSettled}
	taker := &configTaker{d: d, st: st, o: viewOptions{listen: true}, bg: &background{}, theme: func(palette.Theme) {}}
	vh := viewHost(taker, nil, settingsHost{})
	m := dashModel(cfg)
	m.Hint = "q quit"
	taker.take(m, cfg, false)
	vh.Refresh(m)
	if want := []view.HostEntry{{Name: "vm"}, {Name: "box", Down: true}}; !slices.Equal(m.Hosts, want) {
		t.Fatalf("the hosts line %+v", m.Hosts)
	}
	// The dashboard's a is not the pane's.
	if vh.Act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Kind: term.KeyRune, Rune: 'a'}}); m.Overlay != nil || m.Message != "" {
		t.Fatalf("the pane took a: overlay %T, message %q", m.Overlay, m.Message)
	}
	// click clicks the first column of the hosts line, the line above
	// the footer, and hands the action to the pane's host.
	click := func() view.Action {
		t.Helper()
		out := m.Render()
		y := len(out) - 1
		for y > 0 && !strings.HasPrefix(view.Text(out[y-1:y]), "[") {
			y--
		}
		a := m.Handle(term.Key{Kind: term.KeyMouse, X: 1, Y: y})
		if vh.Act(m, a) {
			t.Fatalf("%+v ended the view", a)
		}
		return a
	}
	const note = "the local daemon fake is older than pause and dials a paused host all the same; laatmux stop ends it, and the next command starts this build"
	if a := click(); a.Kind != view.ActionPause || a.HostName != "vm" || !a.Pause {
		t.Fatalf("the click: %+v", a)
	}
	if m.Message != "vm paused; this machine dials it for upgrade alone until it is resumed; "+note {
		t.Fatalf("message %q", m.Message)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "    worktrees: /w\n    paused: true\n  - name: box") {
		t.Fatalf("file:\n%s", b)
	}
	if !m.Hosts[0].Paused || !strings.Contains(view.Text(m.Render()), "[ ] vm  [x] box") {
		t.Fatalf("the line after the click %+v:\n%s", m.Hosts, view.Text(m.Render()))
	}
	if a := click(); a.Pause || m.Message != "vm resumed; it is dialled again" || m.Hosts[0].Paused {
		t.Fatalf("the second click: %+v, message %q, line %+v", a, m.Message, m.Hosts)
	}
	if b, _ := os.ReadFile(p); string(b) != pauseConfig {
		t.Fatalf("file after the resume:\n%s", b)
	}
	if err := os.WriteFile(p, []byte("hosts: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	click()
	if !strings.Contains(m.Message, "config.yaml: ") || m.Hosts[0].Paused {
		t.Fatalf("a bad file: message %q, line %+v", m.Message, m.Hosts)
	}
	if out := view.Text(m.Render()); !strings.Contains(out, "config.yaml: ") || !strings.Contains(out, "[x] vm  [x] box") {
		t.Fatalf("the footer of a bad file:\n%s", out)
	}
	if b, _ := os.ReadFile(p); string(b) != "hosts: [\n" {
		t.Fatalf("the bad file written over:\n%s", b)
	}
}

// A command aimed at a paused host is refused before anything dials
// it: jump, an add run or submitted, a shell, a workspace session's
// attach, and the dial itself, whoever makes it.
func TestPausedHostRefused(t *testing.T) {
	p := pauseFixture(t)
	if err := os.WriteFile(p, []byte(strings.Replace(pauseConfig, "    ssh: vm.invalid   # the coder box\n", "    ssh: vm.invalid\n    paused: true\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	vm, _ := cfg.Find("vm")
	ctx := context.Background()
	add := command.Add{Host: vm, Repo: config.Repo{Source: "git@x:o/proj.git", Name: "proj"}, Branch: "fix", Agent: "claude"}
	_, runErr := add.Run(ctx, nil)
	_, submitErr := add.Submit(ctx)
	_, _, ensureErr := workspace.Ensure(ctx, workspace.Spec{Host: vm.Host, Managed: "proj/fix", Name: "vm/proj/fix", Key: "venv//w/proj/fix"})
	_, dialErr := client.Dial(ctx, vm.Host)
	for name, err := range map[string]error{
		"jump":   cmdJump(ctx, []string{"vm/proj/fix"}),
		"run":    runErr,
		"submit": submitErr,
		"shell":  command.Shell(ctx, vm, protocol.Session{Name: "vm/proj/fix", Key: "venv//w/proj/fix", Host: "vm"}),
		"ensure": ensureErr,
		"dial":   dialErr,
	} {
		var paused *peer.PausedError
		if !errors.As(err, &paused) || err.Error() != "host vm is paused; laatmux hosts resume vm connects it" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// prune passes over a paused host, saying so, and refuses --host
// naming it before anything is asked.
func TestPrunePausedHost(t *testing.T) {
	pauseFixture(t)
	cfg, err := config.Parse([]byte(strings.Replace(pauseConfig, "    ssh: vm.invalid   # the coder box\n", "    ssh: vm.invalid\n    paused: true\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	st := merged.New()
	st.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{
		{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Capabilities: []string{protocol.CapStatus, protocol.CapWorktrees}},
		{Name: "vm", SSH: "vm.invalid", Paused: true},
	}})
	var out strings.Builder
	if err := prune(context.Background(), cfg, pruneArgs{}, st.Status(""), &out); err != nil || !strings.HasPrefix(out.String(), "vm  paused; its worktrees are not looked at\n") {
		t.Errorf("prune: %v\n%s", err, out.String())
	}
	if err := os.WriteFile(os.Getenv("LAATMUX_CONFIG"), []byte(strings.Replace(pauseConfig, "    ssh: vm.invalid   # the coder box\n", "    ssh: vm.invalid\n    paused: true\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdPrune(context.Background(), []string{"--host", "vm", "-n"}); err == nil || err.Error() != "host vm is paused; laatmux hosts resume vm connects it" {
		t.Errorf("prune --host vm: %v", err)
	}
}

// The task form lists a paused host as paused, says so when it is the
// one chosen, and refuses the submit with the message, the form kept
// up, so no task is queued for a host nothing dials.
func TestFormRefusesPausedHost(t *testing.T) {
	t.Setenv("LAATMUX_HOME", t.TempDir())
	cfg, err := config.Parse([]byte(strings.Replace(pauseConfig, "    ssh: vm.invalid   # the coder box\n", "    ssh: vm.invalid\n    paused: true\n", 1) + "repos:\n  - git@github.com:laat/proj.git\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := &dash{ctx: context.Background(), cfg: cfg, st: merged.New()}
	submitted := false
	d.submit = func(command.Add) (string, error) {
		submitted = true
		return "add-1", nil
	}
	m := dashModel(cfg)
	d.act(m, view.Action{Kind: view.ActionOther, Key: term.Key{Kind: term.KeyRune, Rune: 'a'}})
	f, ok := m.Overlay.(*view.Form)
	if !ok {
		t.Fatalf("overlay %T, message %q", m.Overlay, m.Message)
	}
	if c := f.Chips[1].Choices; len(c) != 2 || c[1].Label != "vm" || !strings.HasPrefix(c[1].Detail, "(paused)") || strings.Contains(c[0].Detail, "paused") {
		t.Fatalf("host choices %+v", c)
	}
	f.Chips[1].Selected = 1
	const refusal = "host vm is paused; laatmux hosts resume vm connects it"
	if n := f.Note(f); n != refusal {
		t.Fatalf("note %q", n)
	}
	f.SetPrompt("Fix it")
	f.Handle(term.Key{Kind: term.KeyEnter})
	if d.act(m, m.Poll()) || submitted {
		t.Fatalf("submitted for a paused host")
	}
	if back, ok := m.Overlay.(*view.Form); !ok || back != f || back.Error != refusal || back.Prompt() != "Fix it" {
		t.Fatalf("form after the refusal: %+v", m.Overlay)
	}
	f.Chips[1].Selected = 0
	if n := f.Note(f); n == refusal {
		t.Fatalf("note for mac %q", n)
	}
	// Resumed while the form is up: the view's next look at the file
	// lets the submit through, the choice and the prompt as they were.
	f.Chips[1].Selected = 1
	resumed, err := config.Parse([]byte(pauseConfig + "repos:\n  - git@github.com:laat/proj.git\n"))
	if err != nil {
		t.Fatal(err)
	}
	taker := &configTaker{d: d, st: d.st, bg: &background{}, theme: func(palette.Theme) {}}
	taker.take(m, resumed, false)
	if c := f.Chips[1].Choices; strings.Contains(c[1].Detail, "paused") || f.Chips[1].Label() != "vm" || f.Note(f) == refusal {
		t.Fatalf("after the resume: choices %+v note %q", c, f.Note(f))
	}
	f.Handle(term.Key{Kind: term.KeyEnter})
	if !d.act(m, m.Poll()) || !submitted {
		t.Fatalf("not submitted after the resume: overlay %+v message %q", m.Overlay, m.Message)
	}
}
