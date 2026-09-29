package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/tmux"
)

// A fake ssh on PATH scripted through an env var: exit code, stderr, delay.
func fakeSSH(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "ssh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCheckSessionDistinguishesFailures(t *testing.T) {
	h := client.Host{Name: "vm", SSH: "vm"}
	cases := []struct {
		name, script, want string
	}{
		{"absent", `printf "cannot find session: =x\n" >&2; exit 1`, "vm/x: no such session"},
		{"refused", `printf "ssh: connect to host vm port 22: Connection refused\n" >&2; exit 255`, "vm: ssh failed: ssh: connect to host vm port 22: Connection refused"},
		{"no server", `printf "no server running on /tmp/tmux-1001/laatmux\n" >&2; exit 1`, "vm/x: no such session"},
		{"ok", `exit 0`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeSSH(t, c.script)
			err := checkSession(context.Background(), h, "x")
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

func TestCheckSessionTimesOut(t *testing.T) {
	fakeSSH(t, `sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := checkSession(ctx, client.Host{Name: "vm", SSH: "vm"}, "x")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("preflight did not honour the deadline")
	}
	_ = errors.New
}

// Issue 3, option B: only the managed server is attached; the local default
// server is switched to; a remote default server or any other server is
// refused as unmanaged.
func TestJumpMode(t *testing.T) {
	mac := client.Host{Name: "mac"}
	vm := client.Host{Name: "vm", SSH: "vm"}
	cases := []struct {
		h    client.Host
		srv  string
		want jumpKind
		err  string
	}{
		{mac, "laatmux", jumpAttach, ""},
		{vm, "laatmux", jumpAttach, ""},
		{mac, "default", jumpSwitch, ""},
		{vm, "default", 0, "vm/x: on vm's default tmux server"},
		{mac, "work", 0, "tmux server work is not managed"},
		{vm, "/tmp/sock", 0, "tmux server /tmp/sock is not managed"},
	}
	for _, c := range cases {
		got, err := jumpMode(c.h, tmux.Parse(c.srv), "x")
		switch {
		case c.err == "" && (err != nil || got != c.want):
			t.Errorf("%s --server %s: got %v, %v", c.h.Name, c.srv, got, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s --server %s: got %v, want %q", c.h.Name, c.srv, err, c.err)
		}
	}
}

// A worktree row with no home session whose agent runs elsewhere is
// jumped to through the agent, not answered with the add hint: here an
// agent on a remote host's default server, which jump refuses as such.
func TestJumpRowWorktreeThroughAgent(t *testing.T) {
	cfg := config.Config{Hosts: []config.Host{{Host: client.Host{Name: "vm", SSH: "vm"}}}}
	w := protocol.Worktree{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/w/a"}
	a := protocol.Agent{ID: "venv/default/%1", EnvironmentID: "venv", Server: "default", Session: "notes", WorktreeID: w.ID}
	err := jumpRow(context.Background(), cfg, rows.Row{Host: "vm", Name: "proj/a", Worktree: &w, Agent: &a})
	if err == nil || !strings.Contains(err.Error(), "only observes") {
		t.Fatalf("jump through the agent: %v", err)
	}
	err = jumpRow(context.Background(), cfg, rows.Row{Host: "vm", Name: "proj/a", Worktree: &w})
	if err == nil || !strings.Contains(err.Error(), "has no managed session") {
		t.Fatalf("no agent: %v", err)
	}
}

// A worktree row with no home session whose agent is in a managed
// session attaches through the worktree's own workspace session, keyed
// by the worktree, so it never collides with the name that session has.
func TestRowSpecWorktreeThroughManagedAgent(t *testing.T) {
	h := config.Host{Host: client.Host{Name: "vm", SSH: "vm"}}
	cfg := config.Config{Hosts: []config.Host{h}}
	w := protocol.Worktree{ID: "venv/worktree//w/a", EnvironmentID: "venv", Repo: "proj", Branch: "a", Root: "/w/a", Source: "git@example.com:o/proj.git"}
	a := protocol.Agent{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "proj/a", WorktreeID: w.ID}
	spec, session, err := rowSpec(cfg, h, rows.Row{Host: "vm", Name: "proj/a", Worktree: &w, Agent: &a})
	if err != nil || session != "" || spec.Key != "venv//w/a" || spec.Managed != "proj/a" || spec.Name != "vm/proj/a" || spec.Branch != "a" {
		t.Fatalf("spec %+v session %q err %v", spec, session, err)
	}
	// With the home back the spec is the same session's.
	w.Session = "proj/a"
	home, _, err := rowSpec(cfg, h, rows.Row{Host: "vm", Name: "proj/a", Worktree: &w, Agent: &a})
	if err != nil || home != spec {
		t.Fatalf("home %+v, without %+v, err %v", home, spec, err)
	}
}
