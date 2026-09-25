package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

// trustScreen is Claude Code's folder trust question about root, as a
// pane on the VM showed it, the cursor on no or on yes.
func trustScreen(root string, onYes bool) []string {
	no, yes := "   No, exit", "   Yes, I trust this folder"
	if onYes {
		yes = " ❯ Yes, I trust this folder"
	} else {
		no = " ❯ No, exit"
	}
	return []string{
		"────────────────────────────────────────────────────────────────────────────────",
		" Accessing workspace:",
		" " + root,
		" Quick safety check: Is this a project you created or one you trust? (Like your",
		" own code, a well-known open source project, or work from your team). If not,",
		" take a moment to review what's in this folder first.",
		" Claude Code'll be able to read, edit, and execute files here.",
		" Security guide",
		no,
		yes,
		" Enter to confirm · Esc to cancel",
	}
}

func TestTrustChoice(t *testing.T) {
	root := "/home/me/src/worktrees/proj/yo"
	if m, ok := trustChoice(trustScreen(root, false), root); !ok || m != 1 {
		t.Fatalf("cursor on no: %d %v", m, ok)
	}
	if m, ok := trustChoice(trustScreen(root, true), root); !ok || m != 0 {
		t.Fatalf("cursor on yes: %d %v", m, ok)
	}
	// Yes above the cursor is Up.
	s := trustScreen(root, true)
	s[8], s[9] = "   Yes, I trust this folder", " ❯ No, exit"
	if m, ok := trustChoice(s, root); !ok || m != -1 {
		t.Fatalf("yes above: %d %v", m, ok)
	}
	// A path the pane wrapped is joined back.
	wrapped := trustScreen(root, false)
	wrapped = append(wrapped[:2], append([]string{" /home/me/src/worktree", "s/proj/yo"}, wrapped[3:]...)...)
	if m, ok := trustChoice(wrapped, root); !ok || m != 1 {
		t.Fatalf("wrapped: %d %v", m, ok)
	}
	// Numbered options read the same.
	numbered := trustScreen(root, true)
	numbered[8], numbered[9] = "   2. No, exit", " ❯ 1. Yes, I trust this folder"
	if m, ok := trustChoice(numbered, root); !ok || m != 0 {
		t.Fatalf("numbered: %d %v", m, ok)
	}
	// Blank lines after the footer are the pane's bottom, not more text.
	padded := append(trustScreen(root, false), "", "  ")
	if m, ok := trustChoice(padded, root); !ok || m != 1 {
		t.Fatalf("padded: %d %v", m, ok)
	}
	// Nothing to answer: another folder, a folder under the root whole
	// or wrapped, a sibling whose name extends the root's, no question,
	// no cursor, two cursors, an option that only contains the label,
	// a third option, text after the footer, the prompt box below the
	// question, no footer, no question line under the path.
	cut := func(s []string, i int, with ...string) []string {
		return append(append(append([]string(nil), s[:i]...), with...), s[i+1:]...)
	}
	base := trustScreen(root, false)
	for name, screen := range map[string][]string{
		"another root":      trustScreen("/home/me/src/worktrees/proj/other", false),
		"a subfolder":       trustScreen(root+"/sub", false),
		"wrapped subfolder": cut(base, 2, " "+root, "/sub"),
		"sibling suffix":    cut(base, 2, " "+root, "2"),
		"no question":       idleScreen,
		"no cursor":         cut(base, 8, "   No, exit"),
		"two cursors":       cut(base, 9, " ❯ Yes, I trust this folder"),
		"label inside":      cut(base, 9, "   Do not select Yes, I trust this folder"),
		"third option":      cut(base, 9, "   Yes, I trust this folder", "   Maybe"),
		"text after":        append(append([]string(nil), base...), " some output"),
		"prompt box after":  append(append([]string(nil), base...), idleScreen...),
		"no footer":         base[:len(base)-1],
		"no question line":  cut(base, 3, " (Like your"),
		"two yes":           cut(base, 8, "   Yes, I trust this folder"),
		"two yes numbered":  cut(cut(base, 8, "   1. Yes, I trust this folder"), 9, " ❯ 2. Yes, I trust this folder"),
		"two no":            cut(base, 9, "   No, exit"),
	} {
		if _, ok := trustChoice(screen, root); ok {
			t.Errorf("%s: read as the question", name)
		}
	}
}

// An add whose agent asks the trust question: the cursor is moved onto
// yes, Enter pressed once it is there, and the typed prompt delivered
// once the prompt box is up.
func TestAddAnswersTrust(t *testing.T) {
	was := trustPoll
	trustPoll = 20 * time.Millisecond
	t.Cleanup(func() { trustPoll = was })
	shortWait(t, 10*time.Second)
	d, ft, store, remote := taskDaemon(t, nil, nil)
	root := store.Dirs.Worktree("proj", "task")
	ft.set(func() {
		ft.screen = trustScreen(root, false)
		ft.onKeys = func(f *fakeServer, keys []string) {
			switch keys[len(keys)-1] {
			case "Down":
				f.screen = trustScreen(root, true)
			case "Enter":
				f.screen = idleScreen
			}
		}
	})
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "t1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "do the thing"})
	res, _ := result(t, pc, "t1")
	if !res.OK || res.Prompt != protocol.DeliveryDelivered {
		t.Fatalf("result %+v", res)
	}
	ft.mu.Lock()
	keys := append([][]string(nil), ft.keys...)
	ft.mu.Unlock()
	if len(keys) != 2 || strings.Join(keys[0], " ") != "%1 Down" || strings.Join(keys[1], " ") != "%1 Enter" {
		t.Fatalf("keys %v", keys)
	}
}

// The question about another folder is not answered: the wait times out
// as it did, and no key is pressed.
func TestAddLeavesOtherTrust(t *testing.T) {
	was := trustPoll
	trustPoll = 20 * time.Millisecond
	t.Cleanup(func() { trustPoll = was })
	shortWait(t, 500*time.Millisecond)
	d, ft, _, remote := taskDaemon(t, trustScreen("/somewhere/else", false), nil)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "t2", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "do the thing"})
	res, _ := result(t, pc, "t2")
	if !res.OK || res.Prompt != protocol.DeliveryNotDelivered {
		t.Fatalf("result %+v", res)
	}
	time.Sleep(100 * time.Millisecond)
	ft.mu.Lock()
	n := len(ft.keys)
	ft.mu.Unlock()
	if n != 0 {
		t.Fatalf("pressed keys for another folder: %v", ft.keys)
	}
}

// The watcher answers only for its launch: a pane taken by another
// session or server instance ends it, an agent that is not a verified
// Claude gets no key, and StopRuns cancels it and waits for it.
func TestTrustWatcherBounds(t *testing.T) {
	was := trustPoll
	trustPoll = 10 * time.Millisecond
	t.Cleanup(func() { trustPoll = was })
	d, ft, store, _ := taskDaemon(t, nil, nil)
	root := store.Dirs.Worktree("proj", "w")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ft.set(func() {
		ft.screen = trustScreen(root, true)
		ft.panes = append(ft.panes, tmux.Pane{ID: "%9", Session: "proj/w", Cwd: root, CurrentPath: root, Managed: true, ServerPID: 5})
	})
	// Wait for the poll to identify the pane.
	for i := 0; ; i++ {
		if _, claude, _, _ := d.trustState(trustTarget{pane: "%9", session: "proj/w", root: root, serverPID: 5}); claude {
			break
		}
		if i > 200 {
			t.Fatal("the pane was never seen with a verified Claude")
		}
		time.Sleep(10 * time.Millisecond)
	}
	keys := func() int {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		return len(ft.keys)
	}
	// Another session name, or another server instance: gone, no key.
	for _, target := range []trustTarget{
		{pane: "%9", session: "proj/other", root: root, serverPID: 5},
		{pane: "%9", session: "proj/w", root: root, serverPID: 6},
	} {
		if gone, _, _, _ := d.trustState(target); !gone {
			t.Fatalf("%+v: not gone", target)
		}
		d.answerTrust(context.Background(), target, 10*time.Millisecond)
		if keys() != 0 {
			t.Fatalf("%+v: pressed keys", target)
		}
	}
	// Before a key the pane itself is asked: its working directory
	// elsewhere is no key yet; tmux showing it on another server
	// instance, or a Claude other than the one bound, ends the watcher.
	target := trustTarget{pane: "%9", session: "proj/w", root: root, serverPID: 5}
	_, _, _, id := d.trustState(target)
	moved := false
	ft.set(func() { ft.panes[len(ft.panes)-1].CurrentPath = "/elsewhere" })
	if done, stop := d.trustStep(context.Background(), target, id, &moved); done || stop || keys() != 0 {
		t.Fatalf("elsewhere: done %v stop %v keys %d", done, stop, keys())
	}
	ft.set(func() {
		ft.panes[len(ft.panes)-1].CurrentPath = root
		ft.panes[len(ft.panes)-1].ServerPID = 6
	})
	if done, stop := d.trustStep(context.Background(), target, id, &moved); done || !stop || keys() != 0 {
		t.Fatalf("another server: done %v stop %v keys %d", done, stop, keys())
	}
	ft.set(func() { ft.panes[len(ft.panes)-1].ServerPID = 5 })
	other := id
	other.PID++
	if done, stop := d.trustStep(context.Background(), target, other, &moved); done || !stop || keys() != 0 {
		t.Fatalf("another Claude: done %v stop %v keys %d", done, stop, keys())
	}
	// With all of it as launched, the cursor already on yes: Enter.
	if done, stop := d.trustStep(context.Background(), target, id, &moved); !done || stop || keys() != 1 {
		t.Fatalf("as launched: done %v stop %v keys %d", done, stop, keys())
	}
	ft.set(func() { ft.keys = nil })
	// A root outside the worktrees directory starts nothing.
	d.startTrust(trustTarget{pane: "%9", session: "proj/w", root: t.TempDir(), serverPID: 5}, time.Second, 10*time.Millisecond)
	d.mu.Lock()
	n := d.trusting
	d.mu.Unlock()
	if n != 0 {
		t.Fatal("a watcher for a root outside the worktrees directory")
	}
	// StopRuns cancels a watcher still waiting for the question.
	ft.set(func() { ft.screen = []string{"loading"} })
	d.startTrust(trustTarget{pane: "%9", session: "proj/w", root: root, serverPID: 5}, time.Minute, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.StopRuns(ctx)
	d.mu.Lock()
	n = d.trusting
	d.mu.Unlock()
	if n != 0 || ctx.Err() != nil {
		t.Fatalf("StopRuns left %d watchers", n)
	}
}

// A screen that does not show the cursor on yes after the move, lagging
// or with the key gone astray, gets no second move and no Enter.
func TestTrustNoSecondMove(t *testing.T) {
	was := trustPoll
	trustPoll = 10 * time.Millisecond
	t.Cleanup(func() { trustPoll = was })
	shortWait(t, 500*time.Millisecond)
	d, ft, store, remote := taskDaemon(t, nil, nil)
	root := store.Dirs.Worktree("proj", "lag")
	ft.set(func() { ft.screen = trustScreen(root, false) }) // Down changes nothing
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "t3", Repo: remote, Branch: "lag", AgentName: "claude", Prompt: "do the thing"})
	if res, _ := result(t, pc, "t3"); !res.OK || res.Prompt != protocol.DeliveryNotDelivered {
		t.Fatalf("result %+v", res)
	}
	time.Sleep(100 * time.Millisecond)
	ft.mu.Lock()
	keys := append([][]string(nil), ft.keys...)
	ft.mu.Unlock()
	if len(keys) != 1 || strings.Join(keys[0], " ") != "%1 Down" {
		t.Fatalf("keys %v, want one Down", keys)
	}
}

// A prompt on the command line waits behind the question too: the
// watcher answers it after the add has returned delivered.
func TestAddArgvAnswersTrust(t *testing.T) {
	was := trustPoll
	trustPoll = 10 * time.Millisecond
	t.Cleanup(func() { trustPoll = was })
	shortWait(t, 10*time.Second)
	d, ft, store, remote := taskDaemon(t, nil, map[string][]string{"claude": {"claude", PromptPlaceholder}})
	root := store.Dirs.Worktree("proj", "argv")
	ft.set(func() {
		ft.screen = trustScreen(root, false)
		ft.onKeys = func(f *fakeServer, keys []string) {
			switch keys[len(keys)-1] {
			case "Down":
				f.screen = trustScreen(root, true)
			case "Enter":
				f.screen = idleScreen
			}
		}
	})
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "t4", Repo: remote, Branch: "argv", AgentName: "claude", Prompt: "do the thing"})
	if res, _ := result(t, pc, "t4"); !res.OK || res.Prompt != protocol.DeliveryDelivered {
		t.Fatalf("result %+v", res)
	}
	for i := 0; ; i++ {
		ft.mu.Lock()
		n := len(ft.keys)
		last := ""
		if n > 0 {
			last = ft.keys[n-1][1]
		}
		ft.mu.Unlock()
		if n == 2 && last == "Enter" {
			break
		}
		if i > 300 {
			t.Fatalf("the question was not answered: %d keys", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Only a verified Claude is answered for: an agent identified as
// something else, or a Claude not verified by the process check, is not.
func TestTrustNeedsVerifiedClaude(t *testing.T) {
	d, _, _, _ := newAddDaemon(t)
	key := paneKey(d.managed.Label, "%1")
	target := trustTarget{pane: "%1", session: "s", serverPID: 5}
	for name, obs := range map[string]observation{
		"codex":      {session: "s", serverPID: 5, verified: true, identity: procs.Identity{Agent: "codex"}},
		"unverified": {session: "s", serverPID: 5, verified: false, identity: procs.Identity{Agent: "claude"}},
	} {
		d.mu.Lock()
		d.panes[key] = &paneState{obs: obs}
		d.mu.Unlock()
		if _, claude, _, _ := d.trustState(target); claude {
			t.Errorf("%s: read as a verified Claude", name)
		}
	}
	d.mu.Lock()
	d.panes[key] = &paneState{obs: observation{session: "s", serverPID: 5, verified: true, identity: procs.Identity{Agent: "claude"}}}
	d.mu.Unlock()
	if _, claude, _, _ := d.trustState(target); !claude {
		t.Error("a verified Claude not read as one")
	}
}

// A press waits for the root's delivery lock, which a paste into the
// pane holds; and a root reached through a symlink is answered when
// tmux and Claude name its resolved directory.
func TestTrustStepLockAndSymlink(t *testing.T) {
	d, ft, _, _ := newAddDaemon(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	key := paneKey(d.managed.Label, "%1")
	id := procs.Identity{Agent: "claude", PID: 42, Start: time.Unix(1, 0)}
	d.mu.Lock()
	d.panes[key] = &paneState{obs: observation{session: "s", serverPID: 5, verified: true, identity: id}}
	d.mu.Unlock()
	ft.set(func() {
		ft.panes = []tmux.Pane{{ID: "%1", Session: "s", ServerPID: 5, CurrentPath: real, Managed: true}}
		ft.screen = trustScreen(real, true)
	})
	target := trustTarget{pane: "%1", session: "s", root: link, real: real, serverPID: 5}
	unlock := d.lockDeliveries(link)
	type step struct{ done, stop bool }
	got := make(chan step, 1)
	go func() {
		moved := false
		done, stop := d.trustStep(context.Background(), target, id, &moved)
		got <- step{done, stop}
	}()
	time.Sleep(100 * time.Millisecond)
	ft.mu.Lock()
	early := len(ft.keys)
	ft.mu.Unlock()
	if early != 0 {
		t.Fatal("pressed while the delivery lock was held")
	}
	unlock()
	if s := <-got; !s.done || s.stop {
		t.Fatalf("through the symlink: %+v", s)
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.keys) != 1 || ft.keys[0][1] != "Enter" {
		t.Fatalf("keys %v", ft.keys)
	}
}
