package merged

import (
	"slices"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
)

// knownFixture is a config of three hosts, box first, with two entries
// of its own, and a merged state in which box and vm have sent their
// repositories and pc, a daemon without repos, has records alone.
func knownFixture(t *testing.T) (config.Config, *State) {
	t.Helper()
	cfg, err := config.Parse([]byte(`hosts:
  - name: box
    ssh: box
  - name: vm
    ssh: vm
  - name: pc
    ssh: pc
repos:
  - source: git@github.com:laat/laatmux.git
    name: lmx
    setup: [make]
  - git@github.com:laat/kept.git
`))
	if err != nil {
		t.Fatal(err)
	}
	set := func(cos ...protocol.Checkout) *protocol.RepoSet { return &protocol.RepoSet{Checkouts: cos} }
	m := New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts: []protocol.HostStatus{
			// vm first in the stream; box first in the config.
			{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Repos: set(
				protocol.Checkout{Repo: "laatmux", Source: "https://github.com/laat/laatmux", Root: "/v/laatmux"},
				protocol.Checkout{Repo: "notes-vm", Source: "git@github.com:laat/notes.git", Root: "/v/notes-vm"},
				protocol.Checkout{Repo: "tools", Source: "git@github.com:a/tools.git", Root: "/v/tools"})},
			{Name: "box", SSH: "box", EnvironmentID: "benv", Connected: true, Listed: true, Repos: set(
				protocol.Checkout{Repo: "notes", Source: "https://github.com/laat/notes.git", Root: "/b/notes"},
				// A second clone of notes on box, labelled apart.
				protocol.Checkout{Repo: "notes2", Source: "git@github.com:laat/notes.git", Root: "/b/notes2"},
				protocol.Checkout{Repo: "tools", Source: "git@github.com:b/tools.git", Root: "/b/tools"})},
			{Name: "pc", SSH: "pc", EnvironmentID: "penv", Connected: true, Listed: true},
		},
		Worktrees: []protocol.Worktree{
			{ID: "penv/worktree//p/w/pcrepo/x", EnvironmentID: "penv", Repo: "pcrepo", Source: "git@github.com:laat/pcrepo.git", Branch: "x", Root: "/p/w/pcrepo/x"},
			{ID: "penv/checkout//p/old", EnvironmentID: "penv", Repo: "old", Source: "git@github.com:laat/old.git", Branch: "main", Root: "/p/old", Main: true},
			// box's record of a set it sent: nothing new.
			{ID: "benv/worktree//b/w/notes/y", EnvironmentID: "benv", Repo: "notes", Source: "git@github.com:laat/notes.git", Branch: "y", Root: "/b/w/notes/y"},
		},
	})
	return cfg, m
}

// The known set is the config's entries in its order, as the config has
// them, then the sources the hosts have and the config does not list,
// by name: each by the label of the first host in the config's order
// that has it, whatever the order the stream has the hosts in, and
// from a host without a set, by its records, a main checkout's with its
// root. Every checkout is found, the hosts in the config's order, a
// host's second clone after its first; a record of a source the
// host's set has adds nothing; a configured source found by a host is
// found under the config's name.
func TestKnown(t *testing.T) {
	cfg, m := knownFixture(t)
	k := m.Known(cfg)
	if !k.Discovered || !slices.Equal(k.Unread, []string{"pc"}) {
		t.Fatalf("discovered %v unread %v", k.Discovered, k.Unread)
	}
	var got []string
	for _, r := range k.Repos {
		line := r.Name + " " + r.Source
		if r.Configured {
			line += " configured"
		}
		for _, f := range r.Found {
			line += " " + f.Host + ":" + f.Label + ":" + f.Root
		}
		got = append(got, line)
	}
	want := []string{
		"lmx git@github.com:laat/laatmux.git configured vm:laatmux:/v/laatmux",
		"kept git@github.com:laat/kept.git configured",
		"notes https://github.com/laat/notes.git box:notes:/b/notes box:notes2:/b/notes2 vm:notes-vm:/v/notes-vm",
		"old git@github.com:laat/old.git pc:old:/p/old",
		"pcrepo git@github.com:laat/pcrepo.git pc:pcrepo:",
		"tools git@github.com:a/tools.git vm:tools:/v/tools",
		"tools git@github.com:b/tools.git box:tools:/b/tools",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("known:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r := k.Repos[0]; r.Name != "lmx" || len(r.Setup) != 1 {
		t.Errorf("the config's entry: %+v", r)
	}
	// The config's entries alone, without a daemon.
	c := ConfigOnly(cfg)
	if c.Discovered || len(c.Repos) != 2 || c.Repos[0].Name != "lmx" || len(c.Repos[0].Found) != 0 {
		t.Errorf("config only: %+v", c)
	}
}

// A name is the config's entry of that name first, as the config finds
// it, whatever a host labels; else the one source a host labels so, on
// any host, under the known set's name; two sources one name answers
// to are an error naming both. A source in any form finds its
// repository, the config's entry first. Find takes a name before a
// source.
func TestKnownLookups(t *testing.T) {
	cfg, m := knownFixture(t)
	k := m.Known(cfg)
	for _, c := range []struct{ name, want string }{
		{"lmx", "lmx git@github.com:laat/laatmux.git"},
		// vm's label for the configured laatmux.
		{"laatmux", "lmx git@github.com:laat/laatmux.git"},
		{"notes", "notes https://github.com/laat/notes.git"},
		// vm's label for notes, which the known set names box's way.
		{"notes-vm", "notes https://github.com/laat/notes.git"},
		{"pcrepo", "pcrepo git@github.com:laat/pcrepo.git"},
		{"old", "old git@github.com:laat/old.git"},
	} {
		r, ok, err := k.ByName(c.name)
		if got := r.Name + " " + r.Source; !ok || err != nil || got != c.want {
			t.Errorf("ByName(%s) = %q %v %v, want %q", c.name, got, ok, err, c.want)
		}
	}
	if _, ok, err := k.ByName("tools"); ok || err == nil || err.Error() != `"tools" names git@github.com:a/tools.git and git@github.com:b/tools.git, on different hosts; name the repository by its source` {
		t.Errorf("tools: %v %v", ok, err)
	}
	if r, ok, err := k.ByName("notes2"); !ok || err != nil || r.Name != "notes" {
		t.Errorf("a second clone's label: %+v %v %v", r, ok, err)
	}
	if _, ok, err := k.ByName("nope"); ok || err != nil {
		t.Errorf("nope: %v %v", ok, err)
	}
	for _, c := range []struct{ src, want string }{
		{"https://github.com/laat/laatmux.git", "lmx"},
		{"git@github.com:laat/notes", "notes"},
		{"git@github.com:b/tools.git", "tools"},
	} {
		if r, ok := k.BySource(c.src); !ok || r.Name != c.want {
			t.Errorf("BySource(%s) = %+v %v", c.src, r, ok)
		}
	}
	if r, ok, err := k.Find("git@github.com:a/tools.git"); !ok || err != nil || r.Source != "git@github.com:a/tools.git" {
		t.Errorf("Find by source: %+v %v %v", r, ok, err)
	}
	if err := k.Unknown("nope"); err.Error() != `unknown repository "nope": not checked out on any host and not configured; known: lmx, kept, notes, old, pcrepo, tools, tools` {
		t.Errorf("unknown: %v", err)
	}
	if err := ConfigOnly(cfg).Unknown("nope"); err.Error() != `unknown repository "nope": not configured, and no local daemon answered for the hosts' checkouts; configured: lmx, kept` {
		t.Errorf("unknown, config only: %v", err)
	}
}
