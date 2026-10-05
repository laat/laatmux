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
//	sidebar:
//	  width: 40               # columns or N%; unset: 10%, clamped to 25..50
//	  layout: tiles           # tiles or compact; default tiles
//	icons: emoji              # emoji, nerdfont or ascii; default emoji
//	status_icons: {waiting: "?"}  # per status: working, waiting, done, stale
//	agent_icons: {claude: {icon: CC, color: "#d97757"}}
//	theme:
//	  mode: auto              # auto, dark or light; default auto
//	  custom: {accent: "#b48ead"}   # palette colours over the defaults
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/palette"
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
	// DefaultAgentName is the agent add starts when neither --agent nor the
	// repository's last-used agent says: agents is a map and has no
	// order, and without this the first add for every repository asks
	// for the flag. It must name a configured agent.
	DefaultAgentName string `yaml:"default_agent"`
	// Repos is the known set of repositories. Load fills in derived names.
	Repos []Repo `yaml:"repos"`
	// Copy is this machine's own copy rules for every worktree it makes:
	// files personal to the machine, gitignored, that no committed
	// .laatmux.yaml should name. Each entry is a path relative to the
	// repository root or a glob over the main checkout; see CheckCopy.
	Copy []string `yaml:"copy"`
	// Sidebar is the sidebar pane on this machine's tmux.
	Sidebar Sidebar `yaml:"sidebar"`
	// GitHubHosts are GitHub Enterprise hosts whose sources' PRs and
	// checks are read through gh, beside github.com, which always is.
	// A source on a host not listed is never sent to gh, whose token
	// for it would go there.
	GitHubHosts []string `yaml:"github_hosts"`
	// Icons is the icon set the views draw statuses with: emoji, the
	// default, nerdfont or ascii. StatusIcons overrides single icons by
	// status; "" keeps the set's. AgentIcons overrides the agent icons
	// by agent name.
	Icons       string               `yaml:"icons"`
	StatusIcons map[string]string    `yaml:"status_icons"`
	AgentIcons  map[string]AgentIcon `yaml:"agent_icons"`
	// Theme is the views' colours: the mode picks the dark or the light
	// defaults, auto by asking the terminal, and Custom sets palette
	// colours over them.
	Theme Theme `yaml:"theme"`
}

// AgentIcon is an agent's icon and its colour, `#rrggbb` or 0 to 255.
type AgentIcon struct {
	Icon  string `yaml:"icon"`
	Color string `yaml:"color"`
}

// Theme is the theme section.
type Theme struct {
	Mode   string            `yaml:"mode"`
	Custom map[string]string `yaml:"custom"`
}

// The icon sets and the statuses an icon can be set for.
var (
	IconSets     = []string{"emoji", "nerdfont", "ascii"}
	IconStatuses = []string{"working", "waiting", "done", "stale"}
)

// Sidebar configures the sidebar pane: its width in columns and which
// layout it starts in; and for the sidebar and the dashboard alike, the
// order of the rows and what stale is. Zero values are the defaults.
type Sidebar struct {
	// Width is columns or N% of the window; unset is 10% clamped to
	// 25..50 columns, and an explicit width is not clamped. Position is
	// left or top; Height, for top, the strip's lines, 3 by default;
	// Horizontal.ItemWidth the strip's chip width, 24 by default.
	Width      string     `yaml:"width"`
	Position   string     `yaml:"position"`
	Height     int        `yaml:"height"`
	Horizontal Horizontal `yaml:"horizontal"`
	Layout     string     `yaml:"layout"`
	// View is the view a sidebar pane starts in: agents or tree; Scope
	// what it shows, all, session or project, the file's last choice
	// over it.
	View  string `yaml:"view"`
	Scope string `yaml:"scope"`
	// Sort is priority, recency or window.
	Sort string `yaml:"sort"`
	// DimStale draws a stale row dim; CollapseStale folds the stale rows
	// with the settled ones. Both default to true. StaleAfter is how long
	// an agent is idle before it is stale, an hour by default.
	DimStale      *bool  `yaml:"dim_stale"`
	CollapseStale *bool  `yaml:"collapse_stale"`
	StaleAfter    string `yaml:"stale_after"`
	// Templates are the lines the views draw with, the defaults where
	// unset; the view parses them and shows an error in a bad one's
	// place rather than the config failing.
	Templates Templates `yaml:"templates"`
	// JumpKeys binds M-1..M-9 in tmux's root table to the sidebar's
	// jump, off by default: bound there they take the keys from every
	// pane.
	JumpKeys bool `yaml:"jump_keys"`
}

// Templates are the views' line templates: the tile's lines (nil is
// the default three; an empty line in the list is a line removed), the
// compact line, the top layout's item, and the tree's lines by node.
type Templates struct {
	Tiles   []string      `yaml:"tiles"`
	Compact string        `yaml:"compact"`
	Top     Lines         `yaml:"top"`
	Tree    TreeTemplates `yaml:"tree"`
}

// Lines is a list of template lines that a config may write as one
// string, as `top` was before it took several.
type Lines []string

// UnmarshalYAML reads a list, or a scalar as a list of one.
func (l *Lines) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		if s == "" {
			// As before it took several lines: the default.
			*l = nil
			return nil
		}
		*l = Lines{s}
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*l = list
	return nil
}

// TreeTemplates are the tree's lines by node kind.
type TreeTemplates struct {
	Repo     string `yaml:"repo"`
	Worktree string `yaml:"worktree"`
	Agent    string `yaml:"agent"`
	Pane     string `yaml:"pane"`
	Run      string `yaml:"run"`
}

// Sort orders.
var SortOrders = []string{"priority", "recency", "window"}

// DefaultStaleAfter is how long an agent is idle before it is stale.
const DefaultStaleAfter = time.Hour

// Stale is the stale settings with the defaults filled in.
func (s Sidebar) Stale() (after time.Duration, dim, collapse bool) {
	after = DefaultStaleAfter
	if d, err := time.ParseDuration(s.StaleAfter); err == nil && s.StaleAfter != "" {
		after = d
	}
	return after, s.DimStale == nil || *s.DimStale, s.CollapseStale == nil || *s.CollapseStale
}

// Horizontal is the top strip's own settings.
type Horizontal struct {
	ItemWidth int `yaml:"item_width"`
}

// The strip's defaults: its height and its chips' width.
const (
	DefaultSidebarHeight    = 3
	DefaultSidebarItemWidth = 24
)

// DefaultSidebarWidth is the sidebar's width when the config sets none
// and the window's width is not known.
const DefaultSidebarWidth = 35

// Columns is the sidebar's width in a window of the given width, 0 for
// one not known: the configured columns; N% of the window; or unset,
// 10% of the window clamped to 25..50 columns, the default 35 with the
// window not known.
func (s Sidebar) Columns(windowWidth int) int {
	cols, pct, set := parseSize(s.Width)
	switch {
	case !set && windowWidth <= 0:
		return DefaultSidebarWidth
	case !set:
		return min(max(windowWidth/10, 25), 50)
	case pct > 0 && windowWidth <= 0:
		return DefaultSidebarWidth
	case pct > 0:
		return max(windowWidth*pct/100, 1)
	}
	return cols
}

// parseSize reads columns or N%; set is false for "" and for 0, the
// zero value, which is the default as it was when the width was a
// number.
func parseSize(s string) (cols, pct int, set bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, 0, false
	}
	if strings.HasSuffix(s, "%") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "%"))
		if err != nil {
			return 0, 0, true
		}
		return 0, n, true
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, 0, true
	}
	return n, 0, true
}

// Lines is the strip's height, for position top.
func (s Sidebar) Lines() int {
	if s.Height <= 0 {
		return DefaultSidebarHeight
	}
	return s.Height
}

// ItemWidth is the strip's chip width.
func (s Sidebar) ItemWidth() int {
	if s.Horizontal.ItemWidth <= 0 {
		return DefaultSidebarItemWidth
	}
	return s.Horizontal.ItemWidth
}

// Top reports whether the sidebar is a strip along the top.
func (s Sidebar) Top() bool { return s.Position == "top" }

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
	for _, e := range c.Copy {
		if err := CheckCopy(e); err != nil {
			return c, fmt.Errorf("copy: %w", err)
		}
	}
	for _, r := range c.Repos {
		for _, e := range r.Copy {
			if err := CheckCopy(e); err != nil {
				return c, fmt.Errorf("repos: %s: copy: %w", r.Source, err)
			}
		}
		for i, cmd := range r.Setup {
			if strings.TrimSpace(cmd) == "" {
				return c, fmt.Errorf("repos: %s: setup: entry %d is empty", r.Source, i+1)
			}
		}
	}
	if cols, pct, set := parseSize(c.Sidebar.Width); set {
		switch {
		case pct > 0 && pct <= 100 && cols == 0:
		case pct == 0 && cols >= 10:
		default:
			return c, fmt.Errorf("sidebar: width %q must be at least 10 columns, or 1%% to 100%%", c.Sidebar.Width)
		}
	}
	switch c.Sidebar.Position {
	case "", "left", "top":
	default:
		return c, fmt.Errorf("sidebar: position %q is not left or top", c.Sidebar.Position)
	}
	if c.Sidebar.Height < 0 || c.Sidebar.Horizontal.ItemWidth < 0 {
		return c, fmt.Errorf("sidebar: height and item_width must be positive")
	}
	switch c.Sidebar.Layout {
	case "", "tiles", "compact":
	default:
		return c, fmt.Errorf("sidebar: layout %q is not tiles or compact", c.Sidebar.Layout)
	}
	switch c.Sidebar.View {
	case "", "agents", "tree":
	default:
		return c, fmt.Errorf("sidebar: view %q is not agents or tree", c.Sidebar.View)
	}
	switch c.Sidebar.Scope {
	case "", "all", "session", "project":
	default:
		return c, fmt.Errorf("sidebar: scope %q is not all, session or project", c.Sidebar.Scope)
	}
	if c.Sidebar.Sort != "" && !slices.Contains(SortOrders, c.Sidebar.Sort) {
		return c, fmt.Errorf("sidebar: sort %q is not one of %s", c.Sidebar.Sort, strings.Join(SortOrders, ", "))
	}
	if c.Sidebar.StaleAfter != "" {
		if d, err := time.ParseDuration(c.Sidebar.StaleAfter); err != nil || d <= 0 {
			return c, fmt.Errorf("sidebar: stale_after %q is not a positive duration, 1h or 30m say", c.Sidebar.StaleAfter)
		}
	}
	if err := c.validateLook(); err != nil {
		return c, err
	}
	return c, nil
}

// validateLook checks the icons and the theme, so a view never starts on
// a config it cannot draw with.
func (c *Config) validateLook() error {
	if c.Icons != "" && !slices.Contains(IconSets, c.Icons) {
		return fmt.Errorf("icons: %q is not one of %s", c.Icons, strings.Join(IconSets, ", "))
	}
	for k := range c.StatusIcons {
		if !slices.Contains(IconStatuses, k) {
			return fmt.Errorf("status_icons: %q is not one of %s", k, strings.Join(IconStatuses, ", "))
		}
	}
	for name, a := range c.AgentIcons {
		if a.Color != "" {
			if _, err := palette.Parse(a.Color); err != nil {
				return fmt.Errorf("agent_icons: %s: %w", name, err)
			}
		}
	}
	if !palette.ValidMode(c.Theme.Mode) {
		return fmt.Errorf("theme: mode %q is not auto, dark or light", c.Theme.Mode)
	}
	if _, err := palette.New(true, c.Theme.Custom); err != nil {
		return err
	}
	return nil
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
			if from == "name" {
				return fmt.Errorf("hosts: name %q is not a valid label (%s)", h.Name, labelChars)
			}
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
		// The daemon has no meaningful working directory, and git
		// registers absolute paths, so a relative directory could never
		// match what git reports.
		for _, kv := range [][2]string{{"repos", h.Repos}, {"worktrees", h.Worktrees}} {
			if kv[1] != "" && !filepath.IsAbs(kv[1]) && kv[1] != "~" && !strings.HasPrefix(kv[1], "~/") {
				return fmt.Errorf("hosts: %s: %s %q must be absolute or start with ~", h.Name, kv[0], kv[1])
			}
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
	if c.DefaultAgentName != "" {
		if _, ok := c.Agents[c.DefaultAgentName]; !ok {
			return fmt.Errorf("default_agent: %q is not a configured agent (configured: %s)", c.DefaultAgentName, c.agentList())
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
