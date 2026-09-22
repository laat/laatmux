// Package config reads the global config: ~/.config/laatmux/config.yaml.
//
//	hosts:
//	  - name: laptop          # label; omit ssh for this machine
//	    repos: ~/code         # main checkouts live in <repos>/<name>
//	    worktrees: ~/worktrees  # worktrees live in <worktrees>/<name>/<branch>
//	  - name: box
//	    ssh: box              # ssh alias, ControlMaster assumed
//	    bin: ~/.local/bin/laatmux   # optional, default "laatmux" on PATH
//	    repos: ~/src
//	    worktrees: ~/src/worktrees
//	tmux_servers: [laatmux, default]  # what this machine's daemon watches
//	agents:
//	  claude:
//	    cmd: [claude]
//	repos:                    # known sources; a name is derived, see repos.go
//	  - git@github.com:laat/laatmux.git
//	  - source: https://github.com/laat/other.git
//	    name: other
//
// hosts, agents and repos are read by clients; tmux_servers and the local
// host's directories by the daemon on the machine the file lives on. Each
// host's daemon reads its own file, so the laptop's config cannot change
// what a remote daemon watches or where it puts checkouts.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/tmux"
	"gopkg.in/yaml.v3"
)

// Host is one configured environment: how to reach it and where it keeps
// checkouts and worktrees.
type Host struct {
	client.Host `yaml:",inline"`
	// Repos is the directory of main checkouts on the host; a checkout is
	// <Repos>/<name>. Worktrees is the directory of worktrees; a worktree
	// is <Worktrees>/<name>/<branch>. Both are as written in the file, so
	// "~" is expanded only on the host itself; see Dirs.Expand.
	Repos     string `yaml:"repos"`
	Worktrees string `yaml:"worktrees"`
}

// Dirs is a host's checkout and worktree directories.
type Dirs struct {
	Repos, Worktrees string
}

// Dirs returns the host's directories, or an error naming the host when it
// has none: such a host cannot add.
func (h Host) Dirs() (Dirs, error) {
	if h.Repos == "" || h.Worktrees == "" {
		return Dirs{}, fmt.Errorf("host %s has no repos and worktrees directories configured", h.Name)
	}
	return Dirs{Repos: h.Repos, Worktrees: h.Worktrees}, nil
}

// CanAdd reports whether the host has both directories.
func (h Host) CanAdd() bool { return h.Repos != "" && h.Worktrees != "" }

// Checkout is where a clone of the named repository lands.
func (d Dirs) Checkout(name string) string { return d.Repos + "/" + name }

// Worktree is where a new worktree for the branch lands.
func (d Dirs) Worktree(name, branch string) string {
	return d.Worktrees + "/" + name + "/" + branch
}

// Expand returns the directories with a leading "~" replaced by this
// machine's home directory. Only meaningful on the host the entry is for.
func (d Dirs) Expand() Dirs {
	return Dirs{Repos: ExpandHome(d.Repos), Worktrees: ExpandHome(d.Worktrees)}
}

// ExpandHome replaces a leading "~" or "~/" with the user's home directory.
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(h, p[1:])
}

// Agent is a launchable agent. Cmd is the argv laatmux builds the tmux
// command from; sandboxing and wrappers are the command's business.
type Agent struct {
	Cmd []string `yaml:"cmd"`
}

type Config struct {
	Hosts []Host `yaml:"hosts"`
	// TmuxServers are the tmux servers the daemon on this machine polls,
	// as Parse reads them. Empty means the managed laatmux server only.
	// Only the laatmux server is configured or created on; the rest are
	// observed read-only.
	TmuxServers []string         `yaml:"tmux_servers"`
	Agents      map[string]Agent `yaml:"agents"`
	// Repos is the known set of repositories. Load fills in derived names.
	Repos []Repo `yaml:"repos"`
}

// Servers resolves TmuxServers, or the default when it is empty.
func (c Config) Servers() ([]tmux.Server, error) {
	return ParseServers(c.TmuxServers)
}

// ParseServers turns server specs into servers, rejecting duplicates. An
// empty list is the managed laatmux server alone.
func ParseServers(specs []string) ([]tmux.Server, error) {
	if len(specs) == 0 {
		return []tmux.Server{tmux.LaatmuxServer}, nil
	}
	seen := map[string]bool{}
	out := make([]tmux.Server, 0, len(specs))
	for _, v := range specs {
		s := tmux.Parse(v)
		if seen[s.Label()] {
			return nil, fmt.Errorf("tmux_servers: %s listed twice", s.Label())
		}
		seen[s.Label()] = true
		out = append(out, s)
	}
	return out, nil
}

// Path is the config file location. LAATMUX_CONFIG overrides.
func Path() string {
	if v := os.Getenv("LAATMUX_CONFIG"); v != "" {
		return v
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		h, _ := os.UserHomeDir()
		base = filepath.Join(h, ".config")
	}
	return filepath.Join(base, "laatmux", "config.yaml")
}

// Load reads and validates the config. A missing file yields one local
// host named after the machine.
func Load() (Config, error) {
	b, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return Parse(nil)
		}
		return Config{}, err
	}
	return Parse(b)
}

// Parse reads config from bytes and validates it. Host names, agent keys
// and repository names are labels: they end up in session names, ids and
// directory names, so anything but A-Z a-z 0-9 _ - is rejected.
func Parse(b []byte) (Config, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if len(c.Hosts) == 0 {
		c.Hosts = []Host{{Host: client.Host{Name: localName()}}}
	}
	if err := c.validateHosts(); err != nil {
		return c, err
	}
	if err := c.validateAgents(); err != nil {
		return c, err
	}
	if err := deriveNames(c.Repos); err != nil {
		return c, err
	}
	return c, nil
}

func (c *Config) validateHosts() error {
	seen := map[string]int{}
	locals := 0
	for i := range c.Hosts {
		h := &c.Hosts[i]
		from := "name"
		if h.Name == "" {
			if h.SSH == "" {
				h.Name = localName()
				from = "hostname"
			} else {
				h.Name = h.SSH
				from = "ssh alias"
			}
		}
		if !ValidLabel(h.Name) {
			return fmt.Errorf("hosts: %q from %s is not a valid label (%s); set an explicit name", h.Name, from, labelChars)
		}
		if j, dup := seen[h.Name]; dup {
			return fmt.Errorf("hosts: %s listed twice (entries %d and %d)", h.Name, j+1, i+1)
		}
		seen[h.Name] = i
		if h.Local() {
			locals++
		}
		if (h.Repos == "") != (h.Worktrees == "") {
			return fmt.Errorf("hosts: %s has %s but not %s; set both or neither", h.Name,
				pick(h.Repos != "", "repos", "worktrees"), pick(h.Repos != "", "worktrees", "repos"))
		}
	}
	if locals > 1 {
		return errors.New("hosts: more than one entry without ssh; only one host is this machine")
	}
	return nil
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func (c *Config) validateAgents() error {
	for name, a := range c.Agents {
		if !ValidLabel(name) {
			return fmt.Errorf("agents: %q is not a valid label (%s)", name, labelChars)
		}
		if len(a.Cmd) == 0 || a.Cmd[0] == "" {
			return fmt.Errorf("agents: %s has no cmd", name)
		}
	}
	return nil
}

// AgentNames lists configured agents, sorted.
func (c Config) AgentNames() []string {
	names := make([]string, 0, len(c.Agents))
	for n := range c.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

const labelChars = "A-Z a-z 0-9 _ -"

// ValidLabel reports whether s is a host name, repository name or agent
// key: one or more of A-Z a-z 0-9 _ -. With / . : and % excluded, a
// /-joined name parses unambiguously from the left and only branches need
// encoding.
func ValidLabel(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case 'A' <= b && b <= 'Z', 'a' <= b && b <= 'z', '0' <= b && b <= '9', b == '_', b == '-':
		default:
			return false
		}
	}
	return true
}

func localName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "local"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// Find returns the host by name, or the local host when name is "".
func (c Config) Find(name string) (Host, bool) {
	for _, h := range c.Hosts {
		if name == "" && h.Local() {
			return h, true
		}
		if h.Name == name || (h.SSH != "" && h.SSH == name) {
			return h, true
		}
	}
	if name == "" {
		return Host{Host: client.Host{Name: localName()}}, true
	}
	return Host{}, false
}

// Local returns the entry for this machine, the one without ssh. The daemon
// finds its own directories there.
func (c Config) Local() (Host, bool) {
	for _, h := range c.Hosts {
		if h.Local() {
			return h, true
		}
	}
	return Host{}, false
}
