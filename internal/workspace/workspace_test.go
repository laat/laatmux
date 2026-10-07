package workspace

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/tmux"
)

func TestSessionName(t *testing.T) {
	if got := SessionName("vm", "proj", "fix/v1.2"); got != "vm/proj/fix/v1%2e2" {
		t.Fatalf("SessionName = %q", got)
	}
}

// A machine without tmux has no sessions: with no tmux on PATH and
// nothing at the default server's socket, List is an empty list, as with
// no server running. With the socket there, a server may be running that
// cannot be reached, and List says so. A tmux that is there and fails is
// still an error, socket or not.
func TestListWithoutTmux(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	tmpdir := t.TempDir()
	t.Setenv("TMUX_TMPDIR", tmpdir)
	if locals, err := List(ctx); err != nil || locals != nil {
		t.Errorf("no tmux: %+v %v, want no sessions", locals, err)
	}
	fake := filepath.Join(dir, "tmux")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho boom >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := List(ctx); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("failing tmux: %v, want its error", err)
	}
	if err := os.Remove(fake); err != nil {
		t.Fatal(err)
	}
	sockDir := filepath.Join(tmpdir, "tmux-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sockDir, "default"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := List(ctx); !tmux.NotInstalled(err) {
		t.Errorf("no tmux, socket there: %v, want the not-found error", err)
	}
}

func TestParseSessions(t *testing.T) {
	sep := tmux.Sep
	out := strings.Join([]string{
		strings.Join([]string{"vm/proj/fix", "env1/root/a", "vm", "", "1", "git@x:o/proj.git", "fix"}, sep),
		strings.Join([]string{"mac/work", "", "mac", "mac/work", "", "", ""}, sep),
		strings.Join([]string{"notes", "", "", "", "", "", ""}, sep),
		"",
	}, "\n")
	locals := parseSessions(out)
	if len(locals) != 3 {
		t.Fatalf("got %d sessions: %+v", len(locals), locals)
	}
	ws := locals[0]
	if !ws.Workspace() || ws.Key != "env1/root/a" || ws.Host != "vm" || !ws.Settled || ws.Source != "git@x:o/proj.git" || ws.Branch != "fix" {
		t.Errorf("workspace session parsed as %+v", ws)
	}
	// Published: the workspace and the plain attachment, not the
	// user's own session.
	if recs := Records(locals); len(recs) != 2 || recs[0].Name != "vm/proj/fix" || recs[1].Name != "mac/work" {
		t.Errorf("Records: %+v", recs)
	}
	// Found by identity tags whatever the name, and not across hosts.
	if l, ok := FindWorktree(locals, "env1", "git@x:o/proj.git", "fix"); !ok || l.Name != "vm/proj/fix" {
		t.Errorf("FindWorktree: %+v %v", l, ok)
	}
	if _, ok := FindWorktree(locals, "env2", "git@x:o/proj.git", "fix"); ok {
		t.Error("FindWorktree matched another environment")
	}
	if _, ok := FindWorktree(locals, "env1", "", ""); ok {
		t.Error("FindWorktree matched an untagged session")
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
	h := peer.Host{Name: "vm", SSH: "vm"}
	got := ShellCommand(h, "/home/u/src/worktrees/proj/it's here")
	want := `ssh -t vm 'cd '\''/home/u/src/worktrees/proj/it'\''\'\'''\''s here'\'' && exec "$SHELL" -l'`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if got, want := AttachCommand(h, "proj/x"), "ssh -t -o 'ServerAliveInterval=15' -o 'ServerAliveCountMax=3' vm 'tmux -u -L laatmux attach-session -t '\\''=proj/x'\\'''"; got != want {
		t.Fatalf("remote attach:\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(AttachCommand(h, "proj/x"), "ssh -t") || strings.Contains(AttachCommand(peer.Host{Name: "mac"}, "proj/x"), "ssh") {
		t.Error("AttachCommand picked the wrong transport")
	}
}

// A reuse that does not know the source or branch leaves the session's
// tags alone rather than clearing them. The tags are one sequence, its
// commands separated by tmux.Next, so a branch that is ; is a value.
func TestTagArgsPreserveUnknownIdentity(t *testing.T) {
	full := tagArgs("s", Spec{Host: peer.Host{Name: "vm"}, Key: "k", Source: "src", Branch: ";"})
	want := []string{"set-option", "-t", "=s:", "@laatmux_host", "vm",
		tmux.Next, "set-option", "-t", "=s:", "@laatmux_repo", "src",
		tmux.Next, "set-option", "-t", "=s:", "@laatmux_branch", ";"}
	if !slices.Equal(full, want) {
		t.Errorf("full spec: %q, want %q", full, want)
	}
	partial := strings.Join(tagArgs("s", Spec{Host: peer.Host{Name: "vm"}, Key: "k", Branch: "b"}), " ")
	if strings.Contains(partial, "@laatmux_repo") || !strings.Contains(partial, "@laatmux_branch b") || !strings.Contains(partial, "@laatmux_host vm") {
		t.Errorf("partial spec wrote an empty source or dropped the rest: %q", partial)
	}
	if plain := strings.Join(tagArgs("s", Spec{Host: peer.Host{Name: "vm"}, Source: "src"}), " "); strings.Contains(plain, "@laatmux_repo") {
		t.Errorf("plain attachment got identity tags: %q", plain)
	}
}

func TestAttachHintSelectsDefaultServer(t *testing.T) {
	if got := AttachHint("vm/proj/x"); got != `tmux -L default attach-session -t '=vm/proj/x'` {
		t.Fatalf("AttachHint = %s", got)
	}
}
