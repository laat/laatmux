package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/protocol"
)

// laatmux repos prints the known set: the config's entries, then the
// repositories the hosts have discovered, each marked configured,
// discovered on which hosts, or both, and where it lands on each host,
// a checkout the host has as the host found it. A host with several
// repos directories has a line saying each is looked in for a
// checkout and a clone is made in the first, which is the checkout
// path a repository's line names where the host has none; a host no
// set of checkouts came from is named before the list.
func TestWriteRepos(t *testing.T) {
	cfg, err := config.Parse([]byte(`hosts:
  - name: mac
    repos: [~/code, ~/code/group]
    worktrees: ~/worktrees
  - name: box
    ssh: box
    repos: ~/src
    worktrees: ~/src/worktrees
  - ssh: bare
copy: [.envrc]
repos:
  - git@github.com:laat/laatmux.git
`))
	if err != nil {
		t.Fatal(err)
	}
	var last home.Last
	last.Set("git@github.com:laat/laatmux.git", home.LastRepo{Host: "mac", Agent: "claude"})
	st := merged.New()
	set := func(cos ...protocol.Checkout) *protocol.RepoSet { return &protocol.RepoSet{Checkouts: cos} }
	st.Apply(protocol.Message{Type: protocol.TypeSnapshot, Hosts: []protocol.HostStatus{
		{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Repos: set(
			protocol.Checkout{Repo: "laatmux", Source: "git@github.com:laat/laatmux.git", Root: "/u/code/laatmux"},
			protocol.Checkout{Repo: "notes", Source: "git@github.com:laat/notes.git", Root: "/u/code/group/notes"},
			// A second clone: the line has the first's.
			protocol.Checkout{Repo: "notes2", Source: "git@github.com:laat/notes.git", Root: "/u/code/notes2"})},
		{Name: "box", SSH: "box", EnvironmentID: "benv", Connected: true, Listed: true, Repos: set(
			protocol.Checkout{Repo: "notes", Source: "https://github.com/laat/notes", Root: "/home/u/src/notes"})},
		{Name: "bare", SSH: "bare", Error: "ssh: no route"},
	}})
	var b strings.Builder
	writeRepos(&b, cfg, st.Known(cfg), last)
	want := `copy, every worktree on this machine: .envrc
checkouts on mac: found in ~/code  ~/code/group, cloned into ~/code
checkouts not read from bare: their repositories show where the config or a worktree has them

laatmux  git@github.com:laat/laatmux.git  configured, discovered on mac
  mac    /u/code/laatmux                  ~/worktrees/laatmux/<branch>  last used, agent claude
  box    ~/src/laatmux                    ~/src/worktrees/laatmux/<branch>
  bare   no repos and worktrees configured, cannot add

notes   git@github.com:laat/notes.git  discovered on mac, box
  mac   /u/code/group/notes            ~/worktrees/notes/<branch>
  box   /home/u/src/notes              ~/src/worktrees/notes/<branch>
  bare  no repos and worktrees configured, cannot add
`
	if b.String() != want {
		t.Errorf("repos:\n%s\nwant:\n%s", b.String(), want)
	}
	// A host's origin is shown as tmux.Printable shows it.
	b.Reset()
	odd := "git@x:o/a\x1b[31m.git"
	st.Apply(protocol.Message{Type: protocol.TypeUpsert, HostStatus: &protocol.HostStatus{Name: "box", SSH: "box", EnvironmentID: "benv", Connected: true, Listed: true,
		Repos: set(protocol.Checkout{Repo: "odd", Source: odd, Root: "/home/u/src/odd"})}})
	writeRepos(&b, cfg, st.Known(cfg), last)
	if got := b.String(); !strings.Contains(got, " "+strconv.Quote(odd)+"  discovered on box\n") || strings.Contains(got, "\x1b") {
		t.Errorf("an origin with an ESC:\n%s", got)
	}
	// No local daemon answering: the config's entries alone, said so.
	b.Reset()
	writeRepos(&b, cfg, merged.ConfigOnly(cfg), home.Last{})
	if got := b.String(); !strings.Contains(got, "the hosts' checkouts not read, no local daemon answered: the config's repos alone\n") ||
		!strings.Contains(got, "laatmux  git@github.com:laat/laatmux.git  configured\n") || strings.Contains(got, "notes") {
		t.Errorf("config alone:\n%s", got)
	}
}
