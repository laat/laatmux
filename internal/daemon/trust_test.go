package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/protocol"
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
	// Another folder, a folder under the root, no question, no cursor:
	// nothing to answer.
	for name, screen := range map[string][]string{
		"another root": trustScreen("/home/me/src/worktrees/proj/other", false),
		"a subfolder":  trustScreen(root+"/sub", false),
		"no question":  idleScreen,
		"no cursor":    append(trustScreen(root, false)[:8], "   No, exit", "   Yes, I trust this folder"),
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
