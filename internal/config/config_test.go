package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/laat/laatmux/internal/tmux"
)

func TestServersDefaultAndParsed(t *testing.T) {
	got, err := (Config{}).Servers()
	if err != nil || len(got) != 1 || got[0] != tmux.LaatmuxServer {
		t.Fatalf("default = %v, %v", got, err)
	}
	got, err = (Config{TmuxServers: []string{"laatmux", "default"}}).Servers()
	if err != nil || len(got) != 2 || got[0] != tmux.LaatmuxServer || got[1] != (tmux.Server{}) {
		t.Fatalf("parsed = %v, %v", got, err)
	}
	if _, err := ParseServers([]string{"default", ""}); err == nil {
		t.Fatal("duplicate default accepted")
	}
}

func TestLoadReadsTmuxServers(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	os.WriteFile(p, []byte("hosts:\n  - name: mac\n  - name: box\n    ssh: box\ntmux_servers: [laatmux, default]\n"), 0o600)
	t.Setenv("LAATMUX_CONFIG", p)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hosts) != 2 || c.Hosts[1].SSH != "box" {
		t.Fatalf("hosts: %+v", c.Hosts)
	}
	srv, err := c.Servers()
	if err != nil || len(srv) != 2 || srv[1].Label() != "default" {
		t.Fatalf("servers: %v %v", srv, err)
	}
}
