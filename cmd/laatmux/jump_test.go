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
