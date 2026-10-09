package merged

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/source"
	"github.com/laat/laatmux/internal/tmux"
)

// Known is the repositories this machine knows: the checkouts every
// host has discovered, merged by source, and the config's entries. An
// entry of the config's for a source is the repository as the config
// has it, its name, copy rules and setup; a source no entry has is
// known by the label its host gave its checkout, the first host's in
// the config's order where hosts label it differently. A host's
// checkouts are its daemon's set (protocol.CapRepos), and where its
// daemon sends none, those its records name.
type Known struct {
	// Repos is the config's entries in its order, then the sources it
	// does not list, by name.
	Repos []KnownRepo
	// Discovered is that the hosts' checkouts were read: false for the
	// config's entries alone, no local daemon answering. Unread is the
	// hosts no set of checkouts has come from, in the config's order:
	// paused, down since the local daemon started, or with a daemon
	// without repos, whose repositories are known by its records alone.
	Discovered bool
	Unread     []string
}

// KnownRepo is a repository of the known set: as the config has it, or
// a source and its label; Configured is that the config lists it, and
// Found the hosts' checkouts of it, the hosts in the config's order, a
// host's in the order its scan finds them.
type KnownRepo struct {
	config.Repo
	Configured bool
	Found      []Found
}

// Found is a host's checkout of a repository: the label the host gives
// it and its directory there, "" where only a worktree record of the
// host's names the source.
type Found struct {
	Host, Label, Root string
}

// hostCheckouts is what a host has said of its checkouts: its set, when
// one has come, or that it has answered without worktrees and has none
// (read), and the checkouts its records name.
type hostCheckouts struct {
	read bool
	set  []protocol.Checkout
	recs []protocol.Checkout
}

// ConfigOnly is the known set of the config's entries alone, for a
// client no local daemon answers.
func ConfigOnly(cfg config.Config) Known { return known(cfg, nil) }

// Known is the known set of the config's entries and the checkouts the
// state's hosts have discovered: each host's set, and the sources its
// records name that the set does not have, all of them for a host
// without a set, a main checkout's with its directory.
func (m *State) Known(cfg config.Config) Known {
	m.mu.Lock()
	hosts := make(map[string]*hostCheckouts, len(m.hosts))
	for n, h := range m.hosts {
		// A host that has answered without worktrees has no checkouts
		// to read.
		hosts[n] = &hostCheckouts{read: h.EnvID != "" && !h.Worktrees}
		if h.Repos != nil {
			hosts[n].read, hosts[n].set = true, h.Repos.Checkouts
		}
	}
	type rec struct {
		host string
		w    protocol.Worktree
	}
	var recs []rec
	for id, w := range m.worktrees {
		recs = append(recs, rec{m.byHost[id], w})
	}
	m.mu.Unlock()
	// The records in a fixed order, the main checkouts first, which
	// have the checkout's directory, so a host's first label and root
	// for a source are the same every time.
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].w.Main != recs[j].w.Main {
			return recs[i].w.Main
		}
		return recs[i].w.Root < recs[j].w.Root
	})
	for _, r := range recs {
		h, ok := hosts[r.host]
		if !ok || r.w.Source == "" || r.w.Repo == "" {
			continue
		}
		c := protocol.Checkout{Repo: r.w.Repo, Source: r.w.Source}
		if r.w.Main {
			c.Root = r.w.Root
		}
		h.recs = append(h.recs, c)
	}
	k := known(cfg, hosts)
	k.Discovered = true
	return k
}

// known merges the config's entries and the checkouts the hosts have,
// the hosts taken in the config's order and then by name.
func known(cfg config.Config, hosts map[string]*hostCheckouts) Known {
	var k Known
	at := map[string]int{} // by source key
	for _, r := range cfg.Repos {
		at[source.Key(r.Source)] = len(k.Repos)
		k.Repos = append(k.Repos, KnownRepo{Repo: r, Configured: true})
	}
	configured := len(k.Repos)
	var order []string
	inConfig := map[string]bool{}
	for _, h := range cfg.Hosts {
		order = append(order, h.Name)
		inConfig[h.Name] = true
	}
	var others []string
	for h := range hosts {
		if !inConfig[h] {
			others = append(others, h)
		}
	}
	sort.Strings(others)
	for _, host := range append(order, others...) {
		h, ok := hosts[host]
		if !ok {
			continue
		}
		if !h.read {
			k.Unread = append(k.Unread, host)
		}
		// The sources the host's set has, by key, which its records add
		// nothing to; and the labels its records have added, by key and
		// label, one each, a second clone's under its own.
		inSet, labelled := map[string]bool{}, map[string]bool{}
		add := func(c protocol.Checkout, record bool) {
			key := source.Key(c.Source)
			switch {
			case c.Source == "":
				return
			case !record:
				inSet[key] = true
			case inSet[key] || labelled[key+"\x00"+c.Repo]:
				return
			default:
				labelled[key+"\x00"+c.Repo] = true
			}
			f := Found{Host: host, Label: c.Repo, Root: c.Root}
			i, ok := at[key]
			if !ok {
				at[key] = len(k.Repos)
				k.Repos = append(k.Repos, KnownRepo{Repo: config.Repo{Source: c.Source, Name: c.Repo}})
				i = len(k.Repos) - 1
			}
			if !slices.Contains(k.Repos[i].Found, f) {
				k.Repos[i].Found = append(k.Repos[i].Found, f)
			}
		}
		for _, c := range h.set {
			add(c, false)
		}
		for _, c := range h.recs {
			add(c, true)
		}
	}
	rest := k.Repos[configured:]
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].Name != rest[j].Name {
			return rest[i].Name < rest[j].Name
		}
		return rest[i].Source < rest[j].Source
	})
	return k
}

// Configs is the known repositories as the config would have them, for
// the task form's picker.
func (k Known) Configs() []config.Repo {
	out := make([]config.Repo, len(k.Repos))
	for i, r := range k.Repos {
		out[i] = r.Repo
	}
	return out
}

// Taken is the known repositories under every name they go by, each
// one's and every label a host gives it, for NewRepo: a pasted source
// named as a host labels another would clone into that host's
// checkout's directory.
func (k Known) Taken() []config.Repo {
	var out []config.Repo
	for _, r := range k.Repos {
		out = append(out, r.Repo)
		for _, f := range r.Found {
			if f.Label != r.Name {
				out = append(out, config.Repo{Source: r.Source, Name: f.Label})
			}
		}
	}
	return out
}

// ByName finds a repository by a name: the config's entry of that
// name, as config.Config.RepoByName finds it, else the one source a
// host labels so, by the name the known set has for it. A name that is
// the label of two sources, on two hosts, is the one host labels so
// when host is given and labels one so; else it is an error naming
// both.
func (k Known) ByName(name, host string) (config.Repo, bool, error) {
	for _, r := range k.Repos {
		if r.Configured && r.Name == name {
			return r.Repo, true, nil
		}
	}
	labels := func(r KnownRepo, host string) bool {
		return slices.ContainsFunc(r.Found, func(f Found) bool { return f.Label == name && (host == "" || f.Host == host) })
	}
	var named, on []KnownRepo
	for _, r := range k.Repos {
		if labels(r, "") {
			named = append(named, r)
		}
		if host != "" && labels(r, host) {
			on = append(on, r)
		}
	}
	switch {
	case len(named) == 0:
		return config.Repo{}, false, nil
	case len(named) == 1:
		return named[0].Repo, true, nil
	case len(on) == 1:
		return on[0].Repo, true, nil
	}
	srcs := make([]string, len(named))
	for i, r := range named {
		srcs[i] = tmux.Printable(r.Source)
	}
	return config.Repo{}, false, fmt.Errorf("%q is the label of %s on different hosts; --host takes the host's, and a name for one of them under repos in the config tells them apart", name, strings.Join(srcs, " and "))
}

// BySource finds a repository by its source, in any of the forms
// source.Same takes as one, the config's entry before a host's
// checkout.
func (k Known) BySource(src string) (config.Repo, bool) {
	for _, r := range k.Repos {
		if r.Source == src {
			return r.Repo, true
		}
	}
	for _, r := range k.Repos {
		if source.Same(r.Source, src) {
			return r.Repo, true
		}
	}
	return config.Repo{}, false
}

// Find finds a repository by name, else by source, the order
// config.Config.Repo takes them in; host is ByName's.
func (k Known) Find(nameOrSource, host string) (config.Repo, bool, error) {
	if r, ok, err := k.ByName(nameOrSource, host); ok || err != nil {
		return r, ok, err
	}
	r, ok := k.BySource(nameOrSource)
	return r, ok, nil
}

// Missing says what a repository the known set does not have is: one
// no host has a checkout of and the config does not list, with the
// hosts whose checkouts were not read, or, without the hosts'
// checkouts, one the config does not list.
func (k Known) Missing() string {
	switch {
	case !k.Discovered:
		return "not configured, and no local daemon answered for the hosts' checkouts"
	case len(k.Unread) > 0:
		return "not checked out on any host and not configured (checkouts not read from " + strings.Join(k.Unread, ", ") + ")"
	}
	return "not checked out on any host and not configured"
}

// List is the known repositories' names for an error that lists them,
// "known: a, b", or "configured: a, b" without the hosts' checkouts.
func (k Known) List() string {
	what := "known"
	if !k.Discovered {
		what = "configured"
	}
	if len(k.Repos) == 0 {
		return what + ": none"
	}
	names := make([]string, len(k.Repos))
	for i, r := range k.Repos {
		names[i] = r.Name
	}
	return what + ": " + strings.Join(names, ", ")
}

// Unknown is the error for a name the known set does not have, with
// what it has.
func (k Known) Unknown(name string) error {
	return fmt.Errorf("unknown repository %q: %s; %s", name, k.Missing(), k.List())
}
