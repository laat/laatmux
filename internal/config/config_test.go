package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/tmux"
)

func TestServersDefaultAndParsed(t *testing.T) {
	got, err := (Config{}).Servers()
	if err != nil || len(got) != 1 || got[0] != tmux.LaatmuxServer {
		t.Fatalf("default = %v, %v", got, err)
	}
	got, err = (Config{TmuxServers: []string{"laatmux", "default"}}).Servers()
	if err != nil || len(got) != 2 || got[0] != tmux.LaatmuxServer || got[1] != tmux.DefaultServer {
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

func TestLoadMissingFile(t *testing.T) {
	t.Setenv("LAATMUX_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hosts) != 1 || !c.Hosts[0].Local() || c.Hosts[0].CanAdd() {
		t.Fatalf("hosts: %+v", c.Hosts)
	}
}

const full = `
hosts:
  - name: mac
    repos: ~/code
    worktrees: ~/worktrees
  - name: vm
    ssh: vm
    repos: ~/src
    worktrees: ~/src/worktrees
  - ssh: bare
tmux_servers: [laatmux, default]
agents:
  claude:
    cmd: [claude]
  claude-safe:
    cmd: [claude-safe, --flag]
repos:
  - git@github.com:laat/laatmux.git
  - https://github.com/laat/other.git
`

func TestParseFull(t *testing.T) {
	c, err := Parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	mac, ok := c.Local()
	if !ok || mac.Name != "mac" || mac.Repos != "~/code" || mac.Worktrees != "~/worktrees" {
		t.Fatalf("local: %+v %v", mac, ok)
	}
	vm, ok := c.Find("vm")
	if !ok || vm.SSH != "vm" || !vm.CanAdd() {
		t.Fatalf("vm: %+v %v", vm, ok)
	}
	bare, ok := c.Find("bare")
	if !ok || bare.Name != "bare" || bare.CanAdd() {
		t.Fatalf("bare: %+v %v", bare, ok)
	}
	if _, err := bare.Dirs(); err == nil || !strings.Contains(err.Error(), "bare") {
		t.Fatalf("bare dirs error should name the host: %v", err)
	}
	d, err := vm.Dirs()
	if err != nil {
		t.Fatal(err)
	}
	if d.Checkout("laatmux") != "~/src/laatmux" || d.Worktree("laatmux", "fix/v1.2") != "~/src/worktrees/laatmux/fix/v1.2" {
		t.Fatalf("paths: %s %s", d.Checkout("laatmux"), d.Worktree("laatmux", "fix/v1.2"))
	}
	if got := c.AgentNames(); strings.Join(got, ",") != "claude,claude-safe" {
		t.Fatalf("agents: %v", got)
	}
	if c.Agents["claude-safe"].Cmd[1] != "--flag" {
		t.Fatalf("cmd: %v", c.Agents["claude-safe"].Cmd)
	}
	if len(c.Repos) != 2 || c.Repos[0].Name != "laatmux" || c.Repos[1].Name != "other" {
		t.Fatalf("repos: %+v", c.Repos)
	}
	if r, ok := c.Repo("other"); !ok || r.Source != "https://github.com/laat/other.git" {
		t.Fatalf("repo by name: %+v %v", r, ok)
	}
	if r, ok := c.Repo("git@github.com:laat/laatmux.git"); !ok || r.Name != "laatmux" {
		t.Fatalf("repo by source: %+v %v", r, ok)
	}
}

// A bare local source can equal another entry's name. The name wins,
// whichever order the list is in.
func TestRepoLookupNameBeforeSource(t *testing.T) {
	a := "repos:\n  - source: foo\n    name: local-foo\n  - https://example.com/org/foo.git\n"
	b := "repos:\n  - https://example.com/org/foo.git\n  - source: foo\n    name: local-foo\n"
	for _, in := range []string{a, b} {
		c, err := Parse([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if r, ok := c.Repo("foo"); !ok || r.Source != "https://example.com/org/foo.git" {
			t.Errorf("%q: Repo(foo) = %+v %v, want the entry named foo", in, r, ok)
		}
		if r, ok := c.RepoBySource("foo"); !ok || r.Name != "local-foo" {
			t.Errorf("%q: RepoBySource(foo) = %+v %v", in, r, ok)
		}
		if r, ok := c.RepoByName("local-foo"); !ok || r.Source != "foo" {
			t.Errorf("%q: RepoByName(local-foo) = %+v %v", in, r, ok)
		}
		if _, ok := c.RepoByName("nope"); ok {
			t.Errorf("%q: RepoByName(nope) found", in)
		}
	}
}

func TestExpandDirs(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	d := Dirs{Repos: "~/code", Worktrees: "/abs/wt"}.Expand()
	if d.Repos != "/home/u/code" || d.Worktrees != "/abs/wt" {
		t.Fatalf("expand: %+v", d)
	}
	if ExpandHome("~") != "/home/u" || ExpandHome("~user/x") != "~user/x" {
		t.Fatalf("expand ~: %q %q", ExpandHome("~"), ExpandHome("~user/x"))
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"hosts:\n  - ssh: my.box\n":                                            "ssh alias",
		"hosts:\n  - name: my.box\n":                                           "name \"my.box\" is not a valid label",
		"repos:\n  - 123\n":                                                    "source must be a string, got int",
		"repos:\n  - true\n":                                                   "source must be a string, got bool",
		"repos:\n  - source: 123\n    name: x\n":                               "source must be a string, got int",
		"repos:\n  - source: a/x\n    name: 123\n":                             "name must be a string, got int",
		"repos:\n  - source: [a]\n":                                            "source must be a string, got seq",
		"repos:\n  - name: x\n":                                                "has no source",
		"hosts:\n  - name: a\n  - name: a\n    ssh: a\n":                       "listed twice",
		"hosts:\n  - name: a\n  - name: b\n":                                   "more than one entry without ssh",
		"hosts:\n  - name: a\n    repos: ~/code\n":                             "repos but not worktrees",
		"hosts:\n  - name: a\n    worktrees: ~/wt\n":                           "worktrees but not repos",
		"hosts:\n  - name: a\n    repos: code\n    worktrees: ~/wt\n":          "repos \"code\" must be absolute or start with ~",
		"hosts:\n  - name: a\n    repos: /c\n    worktrees: ./wt\n":            "worktrees \"./wt\" must be absolute or start with ~",
		"agents:\n  my.agent:\n    cmd: [x]\n":                                 "not a valid label",
		"agents:\n  claude: {}\n":                                              "has no cmd",
		"repos:\n  - a/b\n  - a/b\n":                                           "listed twice",
		"repos:\n  - source: a/x\n    name: x\n  - source: b/x\n    name: x\n": "both get the name x",
		"repos:\n  - a/x\n  - source: b/y\n    name: x\n":                      "both get the name x",
		"repos:\n  - git@github.com:laat/foo.js.git\n":                         "set an explicit name",
		"repos:\n  - source: a/x\n    name: bad.name\n":                        "not a valid label",
		"repos:\n  - source: \"\"\n":                                           "has no source",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", in, err, want)
		}
	}
}

func TestValidLabel(t *testing.T) {
	for _, ok := range []string{"a", "A-z_09", "claude-safe"} {
		if !ValidLabel(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "a.b", "a/b", "a:b", "a%b", "a b", "ø"} {
		if ValidLabel(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSourceParts(t *testing.T) {
	cases := []struct{ src, org, name string }{
		{"git@github.com:laat/laatmux.git", "laat", "laatmux"},
		{"https://github.com/laat/other.git", "laat", "other"},
		{"https://github.com/laat/other", "laat", "other"},
		{"ssh://git@host:2222/org/name.git/", "org", "name"},
		{"file:///home/u/code/repo", "code", "repo"},
		{"/home/u/code/repo", "code", "repo"},
		{"~/code/repo.git", "code", "repo"},
		{"host:repo.git", "", "repo"},
		{"repo", "", "repo"},
	}
	for _, c := range cases {
		org, name := sourceParts(c.src)
		if org != c.org || name != c.name {
			t.Errorf("%s: got %q %q, want %q %q", c.src, org, name, c.org, c.name)
		}
	}
}

func TestDeriveNames(t *testing.T) {
	names := func(srcs ...string) []string {
		repos := make([]Repo, len(srcs))
		for i, s := range srcs {
			repos[i] = Repo{Source: s}
		}
		if err := deriveNames(repos); err != nil {
			t.Fatalf("%v: %v", srcs, err)
		}
		out := make([]string, len(repos))
		for i, r := range repos {
			out[i] = r.Name
		}
		return out
	}
	// Rule 1: plain names when unique, even without an org.
	if got := names("git@github.com:laat/laatmux.git", "bare"); got[0] != "laatmux" || got[1] != "bare" {
		t.Fatalf("rule 1: %v", got)
	}
	// Rule 2: org prefix on collision.
	if got := names("git@github.com:laat/laatmux.git", "git@github.com:acme/laatmux.git"); got[0] != "laat-laatmux" || got[1] != "acme-laatmux" {
		t.Fatalf("rule 2: %v", got)
	}
	// Rule 3: hash suffix when the org-prefixed names still collide, and
	// for a collision with no org to prefix.
	a, b := "git@github.com:laat/laatmux.git", "https://github.com/laat/laatmux.git"
	got := names(a, b)
	if got[0] != "laat-laatmux-"+sourceHash(a) || got[1] != "laat-laatmux-"+sourceHash(b) {
		t.Fatalf("rule 3 org collision: %v", got)
	}
	got = names("host:x.git", "git@github.com:laat/x.git")
	if got[0] != "x-"+sourceHash("host:x.git") || got[1] != "laat-x" {
		t.Fatalf("rule 3 no org: %v", got)
	}
	if len(sourceHash(a)) != 6 || sourceHash(a) == sourceHash(b) {
		t.Fatalf("hash: %s %s", sourceHash(a), sourceHash(b))
	}
	// Stable across runs and orderings of the list.
	if x, y := names(a, b), names(b, a); x[0] != y[1] || x[1] != y[0] {
		t.Fatalf("order dependent: %v %v", x, y)
	}
	// Rule 4: explicit names win and are left out of derivation, so the
	// remaining entry keeps its plain name.
	repos := []Repo{{Source: a, Name: "main", Explicit: true}, {Source: b}}
	if err := deriveNames(repos); err != nil {
		t.Fatal(err)
	}
	if repos[0].Name != "main" || repos[1].Name != "laatmux" {
		t.Fatalf("rule 4: %+v", repos)
	}
}

func TestDefaultHost(t *testing.T) {
	c, err := Parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if h, err := c.DefaultHost("vm", "mac"); err != nil || h.Name != "vm" {
		t.Fatalf("flag: %+v %v", h, err)
	}
	if _, err := c.DefaultHost("nope", ""); err == nil {
		t.Fatal("unknown flag host accepted")
	}
	if h, err := c.DefaultHost("", "vm"); err != nil || h.Name != "vm" {
		t.Fatalf("last: %+v %v", h, err)
	}
	// A last-used host that cannot add, or is gone, is passed over.
	if h, err := c.DefaultHost("", "bare"); err != nil || h.Name != "mac" {
		t.Fatalf("last without dirs: %+v %v", h, err)
	}
	if h, err := c.DefaultHost("", "gone"); err != nil || h.Name != "mac" {
		t.Fatalf("last gone: %+v %v", h, err)
	}
	// No local dirs and two remote candidates: an error naming them.
	c2, _ := Parse([]byte("hosts:\n  - name: mac\n  - name: a\n    ssh: a\n    repos: r\n    worktrees: w\n  - name: b\n    ssh: b\n    repos: r\n    worktrees: w\n"))
	if _, err := c2.DefaultHost("", ""); err == nil || !strings.Contains(err.Error(), "a, b") {
		t.Fatalf("candidates: %v", err)
	}
	// One remote candidate: chosen.
	c3, _ := Parse([]byte("hosts:\n  - name: mac\n  - name: a\n    ssh: a\n    repos: r\n    worktrees: w\n"))
	if h, err := c3.DefaultHost("", ""); err != nil || h.Name != "a" {
		t.Fatalf("only: %+v %v", h, err)
	}
	c4, _ := Parse([]byte("hosts:\n  - name: mac\n"))
	if _, err := c4.DefaultHost("", ""); err == nil {
		t.Fatal("no candidates accepted")
	}
}

func TestDefaultAgent(t *testing.T) {
	c, err := Parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if n, a, err := c.DefaultAgent("claude-safe", "claude"); err != nil || n != "claude-safe" || a.Cmd[0] != "claude-safe" {
		t.Fatalf("flag: %s %+v %v", n, a, err)
	}
	if _, _, err := c.DefaultAgent("nope", ""); err == nil || !strings.Contains(err.Error(), "claude, claude-safe") {
		t.Fatalf("unknown flag: %v", err)
	}
	if n, _, err := c.DefaultAgent("", "claude"); err != nil || n != "claude" {
		t.Fatalf("last: %s %v", n, err)
	}
	if _, _, err := c.DefaultAgent("", "gone"); err == nil || !strings.Contains(err.Error(), "--agent") {
		t.Fatalf("two agents, no default: %v", err)
	}
	c1, _ := Parse([]byte("agents:\n  codex:\n    cmd: [codex]\n"))
	if n, _, err := c1.DefaultAgent("", ""); err != nil || n != "codex" {
		t.Fatalf("only: %s %v", n, err)
	}
	// A configured default: after the flag and the last-used agent,
	// before the refusal; one that names no agent fails to load.
	cd, err := Parse([]byte("agents:\n  claude:\n    cmd: [claude]\n  cc-safe:\n    cmd: [claude-safe]\ndefault_agent: claude\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n, _, err := cd.DefaultAgent("", ""); err != nil || n != "claude" {
		t.Fatalf("default: %s %v", n, err)
	}
	if n, _, err := cd.DefaultAgent("", "cc-safe"); err != nil || n != "cc-safe" {
		t.Fatalf("last over default: %s %v", n, err)
	}
	if n, _, err := cd.DefaultAgent("cc-safe", "claude"); err != nil || n != "cc-safe" {
		t.Fatalf("flag over both: %s %v", n, err)
	}
	if n, _, err := cd.DefaultAgent("", "gone"); err != nil || n != "claude" {
		t.Fatalf("stale last, default: %s %v", n, err)
	}
	if _, err := Parse([]byte("agents:\n  claude:\n    cmd: [claude]\ndefault_agent: nope\n")); err == nil || !strings.Contains(err.Error(), "default_agent") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("default naming no agent: %v", err)
	}
	c0, _ := Parse(nil)
	if _, _, err := c0.DefaultAgent("", ""); err == nil {
		t.Fatal("no agents accepted")
	}
}

func TestLoadSetup(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadSetup(dir)
	if err != nil || len(s.Copy) != 0 || len(s.Setup) != 0 {
		t.Fatalf("missing file: %+v %v", s, err)
	}
	os.WriteFile(filepath.Join(dir, SetupFile), []byte("copy: [.envrc, config/.env.local]\nsetup: [\"pnpm install\", \"make -C sub gen\"]\n"), 0o600)
	s, err = LoadSetup(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Copy, ",") != ".envrc,config/.env.local" || strings.Join(s.Setup, ";") != "pnpm install;make -C sub gen" {
		t.Fatalf("parsed: %+v", s)
	}
	for in, want := range map[string]string{
		"copy: [/etc/passwd]\n": "absolute",
		"copy: [../secret]\n":   "leaves the repository root",
		"copy: [a/../../x]\n":   "leaves the repository root",
		"setup: [\"  \"]\n":     "is empty",
		"copy: {not: a list}\n": SetupFile,
	} {
		os.WriteFile(filepath.Join(dir, SetupFile), []byte(in), 0o600)
		if _, err := LoadSetup(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", in, err, want)
		}
	}
}

// The sidebar section: defaults when absent, validated when present.
func TestSidebarConfig(t *testing.T) {
	c, err := Parse([]byte("hosts:\n  - name: mac\n"))
	if err != nil || c.Sidebar.Columns() != DefaultSidebarWidth || c.Sidebar.Layout != "" {
		t.Fatalf("defaults: %+v %v", c.Sidebar, err)
	}
	c, err = Parse([]byte("sidebar:\n  width: 40\n  layout: compact\n"))
	if err != nil || c.Sidebar.Columns() != 40 || c.Sidebar.Layout != "compact" {
		t.Fatalf("set: %+v %v", c.Sidebar, err)
	}
	for _, bad := range []string{"sidebar:\n  width: 5\n", "sidebar:\n  width: -1\n", "sidebar:\n  layout: wide\n"} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
