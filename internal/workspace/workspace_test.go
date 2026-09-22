package workspace

import (
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/tmux"
)

func TestKeyRoundTrip(t *testing.T) {
	key := Key("3fa9c1d2e4b5a6f7", "/home/u/src/worktrees/proj/fix/v1.2")
	env, root := SplitKey(key)
	if env != "3fa9c1d2e4b5a6f7" || root != "/home/u/src/worktrees/proj/fix/v1.2" {
		t.Fatalf("SplitKey(%q) = %q, %q", key, env, root)
	}
}

func TestSessionName(t *testing.T) {
	if got := SessionName("vm", "proj", "fix/v1.2"); got != "vm/proj/fix/v1%2e2" {
		t.Fatalf("SessionName = %q", got)
	}
}

func TestParseSessions(t *testing.T) {
	sep := tmux.Sep
	out := strings.Join([]string{
		strings.Join([]string{"vm/proj/fix", "env1/root/a", "vm", "", "1"}, sep),
		strings.Join([]string{"mac/work", "", "mac", "mac/work", ""}, sep),
		strings.Join([]string{"notes", "", "", "", ""}, sep),
		"",
	}, "\n")
	locals := parseSessions(out)
	if len(locals) != 3 {
		t.Fatalf("got %d sessions: %+v", len(locals), locals)
	}
	ws := locals[0]
	if !ws.Workspace() || ws.Key != "env1/root/a" || ws.Host != "vm" || !ws.Settled {
		t.Errorf("workspace session parsed as %+v", ws)
	}
	if at := locals[1]; at.Workspace() || at.Attach != "mac/work" || at.Settled {
		t.Errorf("attach session parsed as %+v", at)
	}
	if plain := locals[2]; plain.Workspace() || plain.Attach != "" || plain.Host != "" {
		t.Errorf("plain session parsed as %+v", plain)
	}
	if l, ok := Find(locals, "env1/root/a", ""); !ok || l.Name != "vm/proj/fix" {
		t.Errorf("Find by key: %+v %v", l, ok)
	}
	if l, ok := Find(locals, "", "mac/work"); !ok || l.Name != "mac/work" {
		t.Errorf("Find by attach: %+v %v", l, ok)
	}
	if _, ok := Find(locals, "", ""); ok {
		t.Error("Find with nothing to match found something")
	}
	if _, ok := ByName(locals, "vm/proj"); ok {
		t.Error("ByName matched a prefix")
	}
}

// The remote shell command passes the root through as one argument
// whatever it contains, and $SHELL is left for the remote side to expand.
func TestShellCommand(t *testing.T) {
	h := client.Host{Name: "vm", SSH: "vm"}
	got := ShellCommand(h, "/home/u/src/worktrees/proj/it's here")
	want := `ssh -t vm 'cd '\''/home/u/src/worktrees/proj/it'\''\'\'''\''s here'\'' && exec "$SHELL" -l'`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if !strings.Contains(AttachCommand(h, "proj/x"), "ssh -t") || strings.Contains(AttachCommand(client.Host{Name: "mac"}, "proj/x"), "ssh") {
		t.Error("AttachCommand picked the wrong transport")
	}
}
