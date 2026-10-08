package main

import (
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/home"
)

// laatmux repos says where each repository lands on each host: a host
// with several repos directories has a line saying each is looked in
// for a checkout and a clone is made in the first, which is the
// checkout path the repository's line names; a host with one keeps the
// lines it had.
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
	var b strings.Builder
	writeRepos(&b, cfg, last)
	want := `copy, every worktree on this machine: .envrc
checkouts on mac: found in ~/code  ~/code/group, cloned into ~/code

laatmux  git@github.com:laat/laatmux.git
  mac    ~/code/laatmux  ~/worktrees/laatmux/<branch>  last used, agent claude
  box    ~/src/laatmux   ~/src/worktrees/laatmux/<branch>
  bare   no repos and worktrees configured, cannot add
`
	if b.String() != want {
		t.Errorf("repos:\n%s\nwant:\n%s", b.String(), want)
	}
}
