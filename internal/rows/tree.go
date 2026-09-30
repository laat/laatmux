package rows

import (
	"path"
	"sort"
	"strings"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/workspace"
)

// The two views, as milestone five's note settles them: the tree, the
// repositories with their worktrees and what runs in each, and the agent
// view, one tile per agent in sort order. Both are built from one join
// of the same input; a node is a Row with a kind, a depth and an id, so
// the view's selection, anchors and rendering treat it as one.

// Kind is what a row or node is.
type Kind int

const (
	KindTile     Kind = iota // the agent view's tile: an agent, or a task
	KindRepo                 // a repository line
	KindWorktree             // a worktree line, or an orphaned session's
	KindTask                 // a pending task's line
	KindAgent                // an agent under a worktree, or in other sessions
	KindPane                 // a pane with no agent under a worktree
	KindRun                  // a run under a worktree
	KindGroup                // the other-sessions group
	KindFold                 // the agent view's stale fold
)

// Node ids the tree gives what has no record of its own.
const (
	NodeOther = "group/other"
	NodeStale = "fold/stale"
)

// RepoNode is the id of a repository's node: repo/ and the source key.
func RepoNode(source string) string { return "repo/" + config.SourceKey(source) }

// Foldable reports whether the node folds: a repository, or a worktree
// or task line holding children.
func (r Row) Foldable() bool {
	switch r.Kind {
	case KindRepo, KindFold:
		return true
	case KindWorktree, KindTask:
		return r.Children > 0
	}
	return false
}

// Selectable by number: tiles and worktree lines, not folds, groups or
// repository lines.
func (r Row) Numbered() bool {
	return r.Kind == KindTile || r.Kind == KindWorktree || r.Kind == KindTask
}

// join is the input read once: the hosts by name and environment, the
// local sessions by key, attach tag and name, and the records grouped.
type join struct {
	in       Input
	hosts    map[string]Host
	byEnv    map[string]string
	byKey    map[string]*workspace.Local
	byAttach map[string]*workspace.Local
	byName   map[string]*workspace.Local
}

func newJoin(in Input) *join {
	j := &join{in: in, hosts: map[string]Host{}, byEnv: map[string]string{},
		byKey: map[string]*workspace.Local{}, byAttach: map[string]*workspace.Local{}, byName: map[string]*workspace.Local{}}
	names := make([]string, 0, len(in.Hosts))
	for _, h := range in.Hosts {
		j.hosts[h.Name] = h
		names = append(names, h.Name)
	}
	sort.Strings(names)
	for i := len(names) - 1; i >= 0; i-- {
		if h := j.hosts[names[i]]; h.EnvironmentID != "" {
			j.byEnv[h.EnvironmentID] = h.Name
		}
	}
	for i := range in.Locals {
		l := &in.Locals[i]
		j.byName[l.Name] = l
		if l.Workspace() {
			j.byKey[l.Key] = l
		} else if l.Attach != "" {
			j.byAttach[l.Attach] = l
		}
	}
	return j
}

func (j *join) attributes(env string) bool {
	h, ok := j.hosts[j.byEnv[env]]
	return ok && h.Attribution
}

// up is a host that can say a worktree is gone.
func (j *join) up(env string) bool {
	h, ok := j.hosts[j.byEnv[env]]
	return ok && h.Connected && h.Listed && h.Worktrees && env != ""
}

// agentLocal is the local session an agent stands for on its own: the
// plain attachment to its managed session, or the observed session on
// this machine's default server.
func (j *join) agentLocal(host string, a *protocol.Agent) *workspace.Local {
	switch {
	case Server(*a) == tmux.LaatmuxServer.Label():
		return j.byAttach[host+"/"+a.Session]
	case j.hosts[host].Local && Server(*a) == tmux.DefaultServer.Label():
		if l := j.byName[a.Session]; l != nil {
			return l
		}
		return &workspace.Local{Name: a.Session}
	}
	return nil
}

// finish fills what every row has from its records: the host axis, done
// and stale, the branch's PR, and dim.
func (j *join) finish(r *Row) {
	in := j.in
	h, known := j.hosts[r.Host]
	r.HostDown = !known || !h.Connected
	if w := r.Worktree; w != nil && w.Branch != "" && w.Source != "" {
		if b, ok := in.Branches[protocol.BranchKey{Source: config.SourceKey(w.Source), Branch: w.Branch}]; ok {
			r.Branch = &b
		}
	}
	if a := r.Agent; a != nil && r.Pending == nil && a.Liveness != protocol.Gone && a.Activity == protocol.Idle {
		r.Done = in.Attention[a.ID].Done()
		r.Stale = !r.Done && in.StaleAfter > 0 && in.Now.Sub(a.ActivityAt) > in.StaleAfter
	}
	r.Dim = r.Agent == nil || r.Agent.Liveness == protocol.Gone || r.HostDown || r.Orphaned ||
		r.Settled && !r.Pressing() || r.Stale && in.DimStale
	if r.Pending != nil {
		r.Dim, r.Settled = false, false
	}
}

// worktreeAgents are the agents of a worktree: by the worktree id from
// a host with attribution, from any session and server; else by the
// worktree's home session. By start time.
func (j *join) worktreeAgents(w *protocol.Worktree) []*protocol.Agent {
	var out []*protocol.Agent
	for i := range j.in.Agents {
		a := &j.in.Agents[i]
		if a.EnvironmentID != w.EnvironmentID {
			continue
		}
		switch {
		case j.attributes(w.EnvironmentID):
			if a.WorktreeID == w.ID {
				out = append(out, a)
			}
		case w.Session != "" && Server(*a) == tmux.LaatmuxServer.Label() && a.Session == w.Session:
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return before(out[a], out[b]) })
	return out
}

// Tree is the tree view: repositories by name, their worktrees by branch
// with their agents, panes and runs under them, tasks and orphaned
// sessions where they belong, and other sessions last. Depth is the
// node's level, Children how many nodes are under a foldable one.
func Tree(in Input) []Row {
	j := newJoin(in)
	type repo struct {
		key, name string
		nodes     [][]Row // one worktree line with its children each
	}
	repos := map[string]*repo{}
	repoOf := func(source, name string) *repo {
		key := config.SourceKey(source)
		if source == "" {
			key = "label\x00" + name
		}
		r := repos[key]
		if r == nil {
			r = &repo{key: key, name: name}
			repos[key] = r
		}
		if r.name == "" {
			r.name = name
		}
		return r
	}
	used := map[*protocol.Agent]bool{}
	seenKey := map[string]bool{}
	placed := map[int]bool{}

	// The tasks: which stand for a listed worktree, by the worktree id.
	standing := map[string][]int{}
	tasks := make([]Row, len(in.Pendings))
	for i := range in.Pendings {
		p := &in.Pendings[i]
		h, configured := j.hosts[p.Host]
		tasks[i] = Row{Kind: KindTask, Host: p.Host, Name: p.Repo + "/" + p.Branch, Pending: p, Removed: !configured,
			Replaced: configured && h.EnvironmentID != "" && p.EnvironmentID != "" && h.EnvironmentID != p.EnvironmentID}
		if alias := tasks[i].Alias(); alias != "" {
			standing[alias] = append(standing[alias], i)
		}
		if p.EnvironmentID != "" && p.Root != "" && !p.Gone && !(p.Done && !p.OK) {
			seenKey[workspace.Key(p.EnvironmentID, p.Root)] = true
		}
	}
	// The newest submitted task owns a worktree's children.
	for _, idx := range standing {
		sort.SliceStable(idx, func(a, b int) bool {
			pa, pb := tasks[idx[a]].Pending, tasks[idx[b]].Pending
			if !pa.SubmittedAt.Equal(pb.SubmittedAt) {
				return pa.SubmittedAt.After(pb.SubmittedAt)
			}
			return pa.ID < pb.ID
		})
	}

	for i := range in.Worktrees {
		w := &in.Worktrees[i]
		host := j.byEnv[w.EnvironmentID]
		line := Row{Kind: KindWorktree, Node: w.ID, Host: host, Worktree: w}
		if w.Branch == "" {
			line.Name = w.Repo + " (detached) " + w.Root
		} else {
			line.Name = w.Repo + "/" + w.Branch
		}
		key := workspace.Key(w.EnvironmentID, w.Root)
		seenKey[key] = true
		agents := j.worktreeAgents(w)
		var children []Row
		for _, a := range agents {
			used[a] = true
			c := Row{Kind: KindAgent, Node: a.ID, Host: host, Name: a.Session, Worktree: w, Agent: a, Local: j.byKey[key]}
			if c.Local == nil {
				c.Local = j.agentLocal(host, a)
			}
			children = append(children, c)
		}
		var panes []Row
		for k := range in.Panes {
			if p := &in.Panes[k]; p.WorktreeID == w.ID && p.EnvironmentID == w.EnvironmentID {
				panes = append(panes, Row{Kind: KindPane, Node: p.ID, Host: host, Name: p.Command, Worktree: w, Pane: p})
			}
		}
		sort.SliceStable(panes, func(a, b int) bool {
			pa, pb := panes[a].Pane, panes[b].Pane
			if pa.Session != pb.Session {
				return pa.Session < pb.Session
			}
			if pa.Window != pb.Window {
				return pa.Window < pb.Window
			}
			return pa.PaneID < pb.PaneID
		})
		children = append(children, panes...)
		var runs []Row
		for k := range in.Runs {
			if r := &in.Runs[k]; r.WorktreeID == w.ID && r.EnvironmentID == w.EnvironmentID {
				runs = append(runs, Row{Kind: KindRun, Node: r.ID, Host: host, Name: strings.Join(r.Cmd, " "), Worktree: w, Run: r})
			}
		}
		sort.SliceStable(runs, func(a, b int) bool {
			ra, rb := runs[a].Run, runs[b].Run
			if !ra.StartedAt.Equal(rb.StartedAt) {
				return ra.StartedAt.Before(rb.StartedAt)
			}
			return ra.ID < rb.ID
		})
		children = append(children, runs...)
		// The worktree's own session: the home session's workspace
		// session, or the one its agent on this machine's default server
		// stands for.
		if w.Session == "" && len(agents) > 0 {
			if a := rowAgent(agents, w); a != nil && Server(*a) == tmux.DefaultServer.Label() {
				line.Local = j.agentLocal(host, a)
			}
		}
		if line.Local == nil {
			line.Local = j.byKey[key]
		}
		line.Settled = line.Local != nil && line.Local.Settled
		// The most pressing agent, for the folded line's icon.
		line.Agent = pressing(children, j)
		for k := range children {
			c := &children[k]
			c.Settled, c.Depth = line.Settled, 2
			j.finish(c)
		}
		line.Children, line.Depth = len(children), 1
		j.finish(&line)
		rp := repoOf(w.Source, w.Repo)
		if idx := standing[w.ID]; len(idx) > 0 {
			// The newest standing task takes the line's place and its
			// children; the others follow as lines of their own.
			owner := &tasks[idx[0]]
			owner.Worktree, owner.Agent, owner.Local, owner.Children, owner.Depth = w, line.Agent, line.Local, len(children), 1
			for _, k := range idx[1:] {
				tasks[k].Worktree, tasks[k].Local, tasks[k].Depth = w, line.Local, 1
			}
			group := append([]Row{*owner}, children...)
			for _, k := range idx[1:] {
				group = append(group, tasks[k])
			}
			rp.nodes = append(rp.nodes, group)
			for _, k := range idx {
				placed[k] = true
			}
			continue
		}
		rp.nodes = append(rp.nodes, append([]Row{line}, children...))
	}
	// The tasks left: before the host lists the worktree, or unable to
	// become it. A standing one holds the add's agent in its session
	// whose pane starts at the root; the newest of several owns it.
	var loose []int
	for i := range tasks {
		if !placed[i] {
			loose = append(loose, i)
		}
	}
	sort.SliceStable(loose, func(a, b int) bool {
		pa, pb := tasks[loose[a]].Pending, tasks[loose[b]].Pending
		if !pa.SubmittedAt.Equal(pb.SubmittedAt) {
			return pa.SubmittedAt.After(pb.SubmittedAt)
		}
		return pa.ID < pb.ID
	})
	for _, i := range loose {
		t := &tasks[i]
		p := t.Pending
		if t.stands() && p.EnvironmentID != "" && p.Root != "" {
			t.Local = j.byKey[workspace.Key(p.EnvironmentID, p.Root)]
		}
		var children []Row
		if t.stands() && p.EnvironmentID != "" && p.Session != "" && p.Root != "" {
			for k := range in.Agents {
				a := &in.Agents[k]
				if !used[a] && a.EnvironmentID == p.EnvironmentID && Server(*a) == tmux.LaatmuxServer.Label() && a.Session == p.Session && a.Cwd == p.Root {
					used[a] = true
					c := Row{Kind: KindAgent, Node: a.ID, Host: t.Host, Name: a.Session, Agent: a, Local: t.Local, Depth: 2}
					j.finish(&c)
					children = append(children, c)
					break
				}
			}
		}
		t.Agent = pressing(children, j)
		t.Children, t.Depth = len(children), 1
		j.finish(t)
		rp := repoOf(p.Source, p.Repo)
		rp.nodes = append(rp.nodes, append([]Row{*t}, children...))
	}
	// Orphaned sessions: under their repository by the source tag, or
	// in other sessions.
	var otherOrphans []Row
	for i := range in.Locals {
		l := &in.Locals[i]
		if !l.Workspace() || seenKey[l.Key] {
			continue
		}
		env, _ := workspace.SplitKey(l.Key)
		if !j.up(env) {
			continue
		}
		line := Row{Kind: KindWorktree, Node: "session/" + l.Name, Host: j.byEnv[env], Name: l.Name, Local: l, Orphaned: true, Settled: l.Settled, Depth: 1}
		j.finish(&line)
		if l.Source != "" {
			rp := repoOf(l.Source, "")
			rp.nodes = append(rp.nodes, []Row{line})
		} else {
			otherOrphans = append(otherOrphans, line)
		}
	}
	// A repository named by a source tag alone: its label from the
	// worktrees is missing; the tag's last element stands in.
	for _, rp := range repos {
		if rp.name == "" {
			if _, p, ok := config.Forge(strings.TrimPrefix(rp.key, "forge\x00")); ok {
				rp.name = path.Base(p)
			} else {
				rp.name = path.Base(strings.TrimSuffix(strings.TrimPrefix(rp.key, "exact\x00"), ".git"))
			}
		}
	}

	var out []Row
	keys := make([]string, 0, len(repos))
	for k := range repos {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		if repos[keys[a]].name != repos[keys[b]].name {
			return repos[keys[a]].name < repos[keys[b]].name
		}
		return keys[a] < keys[b]
	})
	for _, k := range keys {
		rp := repos[k]
		// Worktrees by branch; tasks and orphaned lines among them by
		// their name.
		sort.SliceStable(rp.nodes, func(a, b int) bool {
			la, _ := rp.nodes[a][0].Labels()
			lb, _ := rp.nodes[b][0].Labels()
			if la != lb {
				return la < lb
			}
			// A worktree line before a task line beside it.
			if ka, kb := rp.nodes[a][0].Kind, rp.nodes[b][0].Kind; ka != kb {
				return ka == KindWorktree
			}
			return rp.nodes[a][0].ID() < rp.nodes[b][0].ID()
		})
		out = append(out, Row{Kind: KindRepo, Node: "repo/" + k, Name: rp.name, Children: len(rp.nodes)})
		for _, group := range rp.nodes {
			out = append(out, group...)
		}
	}
	// Other sessions: managed agents in no worktree, then observed ones,
	// then the orphaned sessions with no source tag.
	var others []Row
	for pass := 0; pass < 2; pass++ {
		for i := range in.Agents {
			a := &in.Agents[i]
			managed := Server(*a) == tmux.LaatmuxServer.Label()
			if used[a] || managed != (pass == 0) {
				continue
			}
			host := j.byEnv[a.EnvironmentID]
			c := Row{Kind: KindAgent, Node: a.ID, Host: host, Name: a.Session, Agent: a, Local: j.agentLocal(host, a), Depth: 1}
			j.finish(&c)
			others = append(others, c)
		}
	}
	others = append(others, otherOrphans...)
	if len(others) > 0 {
		out = append(out, Row{Kind: KindGroup, Node: NodeOther, Name: "other sessions", Children: len(others)})
		out = append(out, others...)
	}
	for i := range out {
		r := &out[i]
		r.Current = in.Current != "" && r.Local != nil && r.Local.Name == in.Current && r.Kind != KindAgent && r.Kind != KindPane && r.Kind != KindRun
		if r.Kind == KindAgent && r.Depth == 1 && r.Local != nil && r.Local.Name == in.Current {
			// A session line in other sessions is the viewer's own.
			r.Current = true
		}
	}
	return out
}

// pressing is the most pressing of a worktree's agents, in the note's
// precedence: blocked, done, working, idle, stale; nil without agents.
func pressing(children []Row, j *join) *protocol.Agent {
	var best *Row
	for i := range children {
		c := children[i]
		if c.Agent == nil {
			continue
		}
		j.finish(&c)
		if best == nil || c.Rank() < best.Rank() {
			cc := c
			best = &cc
		}
	}
	if best == nil {
		return nil
	}
	return best.Agent
}

// Agents is the agent view: the tasks first, the newest first, then one
// tile per agent in the sort order; the stale agents and those of
// settled workspaces, unless pressing or the viewer's own, in the Stale
// fold. Tiles that share a primary label, several agents of one worktree
// say, are numbered in the tree's order.
func Agents(in Input) Rows {
	tree := Tree(in)
	var rows []Row
	viewer := map[string]bool{} // worktree ids and session names the viewer is in
	for _, n := range tree {
		if n.Current && n.Local != nil {
			if n.Worktree != nil {
				viewer["w:"+n.Worktree.ID] = true
			}
			if n.Pending != nil {
				viewer["t:"+n.Pending.ID] = true
			}
			viewer["s:"+n.Local.Name] = true
		}
	}
	var owner string // the task or worktree line the agents below are under
	for _, n := range tree {
		switch n.Kind {
		case KindTask:
			owner = "t:" + n.Pending.ID
			t := n
			t.Kind, t.Node, t.Depth, t.Children = KindTile, n.Pending.ID, 0, 0
			t.Current = viewer["t:"+n.Pending.ID]
			rows = append(rows, t)
		case KindWorktree:
			if n.Worktree != nil {
				owner = "w:" + n.Worktree.ID
			} else {
				owner = ""
			}
		case KindAgent:
			// The tile's id is the agent's, as the node's is: a worktree
			// with two agents is two tiles.
			t := n
			t.Kind, t.Node, t.Depth = KindTile, n.Agent.ID, 0
			t.Current = false
			if n.Depth == 2 {
				t.Current = viewer[owner]
			}
			if t.Local != nil && viewer["s:"+t.Local.Name] {
				t.Current = true
			}
			rows = append(rows, t)
		}
	}
	// Tiles that share a primary label, in the tree's order.
	count := map[string]int{}
	for i := range rows {
		if rows[i].Agent != nil {
			p, _ := rows[i].Labels()
			count[p]++
		}
	}
	seen := map[string]int{}
	for i := range rows {
		if rows[i].Agent == nil {
			continue
		}
		p, _ := rows[i].Labels()
		if count[p] > 1 {
			seen[p]++
			rows[i].Suffix = "(" + itoa(seen[p]) + ")"
		}
	}
	sort.SliceStable(rows, func(a, b int) bool { return less(rows[a], rows[b], in.Sort) })
	var out Rows
	for _, r := range rows {
		switch {
		case r.Current:
			out.Main = append(out.Main, r)
		case r.Settled && !r.Pressing(), r.Stale && in.CollapseStale:
			out.Stale = append(out.Stale, r)
		default:
			out.Main = append(out.Main, r)
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
