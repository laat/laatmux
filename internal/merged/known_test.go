package merged

import (
	"slices"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
)

// knownFixture is a config of three hosts, vm first, box second though
// it sorts first, with two entries of its own, and a merged state in
// which box and vm have sent their repositories, pc, a daemon with
// worktrees but without repos, has records alone, and lap, a host of
// the stream the config does not list, has answered without worktrees.
func knownFixture(t *testing.T) (config.Config, *State) {
	t.Helper()
	cfg, err := config.Parse([]byte(`hosts:
  - name: vm
    ssh: vm
  - name: box
    ssh: box
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
			// box first in the stream and by name; vm first in the
			// config.
			{Name: "box", SSH: "box", EnvironmentID: "benv", Connected: true, Listed: true, Repos: set(
				protocol.Checkout{Repo: "notes", Source: "https://github.com/laat/notes.git", Root: "/b/notes"},
				// A second clone of notes on box, labelled apart.
				protocol.Checkout{Repo: "notes2", Source: "git@github.com:laat/notes.git", Root: "/b/notes2"},
				protocol.Checkout{Repo: "tools", Source: "git@github.com:b/tools.git", Root: "/b/tools"})},
			{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Listed: true, Repos: set(
				protocol.Checkout{Repo: "laatmux", Source: "https://github.com/laat/laatmux", Root: "/v/laatmux"},
				protocol.Checkout{Repo: "notes-vm", Source: "git@github.com:laat/notes.git", Root: "/v/notes-vm"},
				protocol.Checkout{Repo: "tools", Source: "git@github.com:a/tools.git", Root: "/v/tools"})},
			{Name: "pc", SSH: "pc", EnvironmentID: "penv", Connected: true, Listed: true, Capabilities: []string{protocol.CapStatus, protocol.CapWorktrees}},
			{Name: "lap", EnvironmentID: "lenv", Connected: true, Listed: true, Capabilities: []string{protocol.CapStatus, protocol.CapMerged}},
		},
		Worktrees: []protocol.Worktree{
			// Two clones of pcrepo on pc, each with a worktree, labelled
			// apart.
			{ID: "penv/worktree//p/w/pcrepo/x", EnvironmentID: "penv", Repo: "pcrepo", Source: "git@github.com:laat/pcrepo.git", Branch: "x", Root: "/p/w/pcrepo/x"},
			{ID: "penv/worktree//p/w/pcrepo2/y", EnvironmentID: "penv", Repo: "pcrepo2", Source: "git@github.com:laat/pcrepo.git", Branch: "y", Root: "/p/w/pcrepo2/y"},
			// old's main checkout, and a worktree of it whose root sorts
			// before the checkout's.
			{ID: "penv/checkout//p/old", EnvironmentID: "penv", Repo: "old", Source: "git@github.com:laat/old.git", Branch: "main", Root: "/p/old", Main: true},
			{ID: "penv/worktree//a/w/old/z", EnvironmentID: "penv", Repo: "old", Source: "git@github.com:laat/old.git", Branch: "z", Root: "/a/w/old/z"},
			// box's record of a set it sent: nothing new.
			{ID: "benv/worktree//b/w/notes/y", EnvironmentID: "benv", Repo: "notes", Source: "git@github.com:laat/notes.git", Branch: "y", Root: "/b/w/notes/y"},
		},
	})
	return cfg, m
}

// The known set is the config's entries in its order, as the config has
// them, then the sources the hosts have and the config does not list,
// by name: each by the label of the first host in the config's order
// that has it, whatever order the stream or the names have the hosts
// in, and from a host without a set, by its records, a main
// checkout's with its root, every label its records give a source. A
// record of a source its host's set has adds nothing. Every checkout
// is found, the hosts in the config's order, a host's second clone
// after its first; a configured source found by a host is found under
// the config's name. A host without a set is unread, but one that has
// answered without worktrees, which has none.
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
		"notes-vm git@github.com:laat/notes.git vm:notes-vm:/v/notes-vm box:notes:/b/notes box:notes2:/b/notes2",
		"old git@github.com:laat/old.git pc:old:/p/old",
		"pcrepo git@github.com:laat/pcrepo.git pc:pcrepo: pc:pcrepo2:",
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
	// Every name a repository goes by is taken.
	var taken []string
	for _, r := range k.Taken() {
		taken = append(taken, r.Name)
	}
	if want := "lmx laatmux kept notes-vm notes notes2 old pcrepo pcrepo2 tools tools"; strings.Join(taken, " ") != want {
		t.Errorf("taken %q, want %q", strings.Join(taken, " "), want)
	}
}

// A name is the config's entry of that name first, as the config finds
// it, whatever a host labels; else the one source a host labels so, on
// any host, under the known set's name; two sources one name labels
// are the one the host given labels so, else an error naming both. A
// source in any form finds its repository, the config's entry first.
// Find takes a name before a source.
func TestKnownLookups(t *testing.T) {
	cfg, m := knownFixture(t)
	k := m.Known(cfg)
	for _, c := range []struct{ name, host, want string }{
		{"lmx", "", "lmx git@github.com:laat/laatmux.git"},
		// vm's label for the configured laatmux.
		{"laatmux", "", "lmx git@github.com:laat/laatmux.git"},
		{"notes-vm", "", "notes-vm git@github.com:laat/notes.git"},
		// box's labels for notes, which the known set names vm's way.
		{"notes", "", "notes-vm git@github.com:laat/notes.git"},
		{"notes2", "", "notes-vm git@github.com:laat/notes.git"},
		{"pcrepo2", "", "pcrepo git@github.com:laat/pcrepo.git"},
		{"old", "", "old git@github.com:laat/old.git"},
		// tools by the host that labels it so.
		{"tools", "box", "tools git@github.com:b/tools.git"},
		{"tools", "vm", "tools git@github.com:a/tools.git"},
	} {
		r, ok, err := k.ByName(c.name, c.host)
		if got := r.Name + " " + r.Source; !ok || err != nil || got != c.want {
			t.Errorf("ByName(%s, %s) = %q %v %v, want %q", c.name, c.host, got, ok, err, c.want)
		}
	}
	for _, host := range []string{"", "pc"} {
		if _, ok, err := k.ByName("tools", host); ok || err == nil || err.Error() != `"tools" is the label of git@github.com:a/tools.git and git@github.com:b/tools.git on different hosts; --host takes the host's, and a name for one of them under repos in the config tells them apart` {
			t.Errorf("tools on %q: %v %v", host, ok, err)
		}
	}
	if _, ok, err := k.ByName("nope", ""); ok || err != nil {
		t.Errorf("nope: %v %v", ok, err)
	}
	for _, c := range []struct{ src, want string }{
		{"https://github.com/laat/laatmux.git", "lmx"},
		{"https://github.com/laat/notes", "notes-vm"},
		{"git@github.com:b/tools.git", "tools"},
	} {
		if r, ok := k.BySource(c.src); !ok || r.Name != c.want {
			t.Errorf("BySource(%s) = %+v %v", c.src, r, ok)
		}
	}
	if r, ok, err := k.Find("git@github.com:a/tools.git", ""); !ok || err != nil || r.Source != "git@github.com:a/tools.git" {
		t.Errorf("Find by source: %+v %v %v", r, ok, err)
	}
	if err := k.Unknown("nope"); err.Error() != `unknown repository "nope": not checked out on any host and not configured (checkouts not read from pc); known: lmx, kept, notes-vm, old, pcrepo, tools, tools` {
		t.Errorf("unknown: %v", err)
	}
	if err := ConfigOnly(cfg).Unknown("nope"); err.Error() != `unknown repository "nope": not configured, and no local daemon answered for the hosts' checkouts; configured: lmx, kept` {
		t.Errorf("unknown, config only: %v", err)
	}
}
