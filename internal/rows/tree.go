package rows

import (
	"path"
	"sort"
	"strings"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/source"
	"github.com/laat/laatmux/internal/tmux"
)

// The two views: the tree, the repositories with their worktrees and
// what runs in each, and the agent view, one tile per agent in sort
// order. Both are built from one join of the same input; a node is a
// Row with a kind, a depth and an id, so the view's selection, anchors
// and rendering treat it as one.

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
func RepoNode(src string) string { return "repo/" + source.Key(src) }

// LabelRepoNode is the id of a repository line known by a host's label
// alone, from an older host that reports no source.
func LabelRepoNode(name string) string { return "repo/label\x00" + name }

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
	byKey    map[string]*protocol.Session
	byAttach map[string]*protocol.Session
	byName   map[string]*protocol.Session
}

func newJoin(in Input) *join {
	j := &join{in: in, hosts: map[string]Host{}, byEnv: map[string]string{},
		byKey: map[string]*protocol.Session{}, byAttach: map[string]*protocol.Session{}, byName: map[string]*protocol.Session{}}
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

// attached reports whether the viewer is in a plain attachment with the
// tag, a host's name and a managed session's: by the viewer's own
// session's tag. Two attachments can carry one tag, set by hand or by
// an older build; byAttach keeps the last listed, a jump
// (workspace.Find) the first, and the viewer may be in either.
func (j *join) attached(tag string) bool {
	v := j.byName[j.in.Current]
	return j.in.Current != "" && v != nil && !v.Workspace() && v.Attach == tag
}

// agentLocal is the local session an agent stands for on its own: the
// plain attachment to its managed session, the viewer's when it is in
// one (attached), so that the viewer is found in it by its name; or the
// observed session on this machine's default server.
func (j *join) agentLocal(host string, a *protocol.Agent) *protocol.Session {
	switch {
	case a.Server == protocol.ServerLaatmux:
		tag := host + "/" + a.Session
		if j.attached(tag) {
			return j.byName[j.in.Current]
		}
		return j.byAttach[tag]
	case j.hosts[host].Local && a.Server == protocol.ServerDefault:
		if l := j.byName[a.Session]; l != nil {
			return l
		}
		return &protocol.Session{Name: a.Session}
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
		if b, ok := in.Branches[protocol.BranchKey{Source: source.Key(w.Source), Branch: w.Branch}]; ok {
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
		case w.Session != "" && a.Server == protocol.ServerLaatmux && a.Session == w.Session:
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return before(out[a], out[b]) })
	return out
}

// Tree is the tree view: repositories by name, their main checkouts in
// use first, with the agents in plain sessions on the default server in
// each, then their worktrees by branch with their agents, panes and runs
// under them, tasks and orphaned sessions where they belong, and other
// sessions last. Depth is the
// node's level, Children how many nodes are under a foldable one.
func Tree(in Input) []Row {
	b := newBuilder(sorted(in))
	b.tasks()
	b.worktrees()
	b.looseTasks()
	b.orphans()
	b.nameRepos()
	out := b.repoLines()
	b.attachedHome(out)
	b.visitors(out)
	out = b.otherSessions(out)
	markViewer(out, in.Current)
	return out
}

// sorted is the input with its agents and worktrees in one order
// whatever the maps they came from: the agents by host, session, start
// and id, which the tree's children and the tiles' suffixes count on;
// the worktrees by host and root, so a repository this machine has no
// label for is named by the first worktree naming it. Copies: the
// caller's slices are its own.
func sorted(in Input) Input {
	in.Agents = append([]protocol.Agent(nil), in.Agents...)
	sort.SliceStable(in.Agents, func(a, b int) bool {
		x, y := &in.Agents[a], &in.Agents[b]
		if x.EnvironmentID != y.EnvironmentID {
			return x.EnvironmentID < y.EnvironmentID
		}
		if x.Session != y.Session {
			return x.Session < y.Session
		}
		return before(x, y)
	})
	in.Worktrees = append([]protocol.Worktree(nil), in.Worktrees...)
	sort.SliceStable(in.Worktrees, func(a, b int) bool {
		x, y := &in.Worktrees[a], &in.Worktrees[b]
		if x.EnvironmentID != y.EnvironmentID {
			return x.EnvironmentID < y.EnvironmentID
		}
		return x.Root < y.Root
	})
	return in
}

// repo is a repository line in the making: its node key, its name, its
// source and the groups under it.
type repo struct {
	key, name string
	source    string  // "" for a repository known by a host's label alone
	nodes     [][]Row // one worktree line with its children each
}

// builder is the tree in the making: the passes in order, each reading
// what the ones before it placed.
type builder struct {
	in    Input
	j     *join
	repos map[string]*repo
	// used marks the agents placed under a worktree or a task, so the
	// other sessions are the rest; seenKey the workspace keys a worktree
	// or a task neither gone nor failed accounts for, so the orphans are
	// the rest;
	// placed the tasks a worktree line took, so the loose ones are the
	// rest.
	used    map[*protocol.Agent]bool
	seenKey map[string]bool
	placed  map[int]bool
	// taskRows is one row per pending task; standing holds, by worktree
	// id, the tasks that stand for a listed worktree, newest first.
	taskRows []Row
	standing map[string][]int
	// otherOrphans are the orphaned sessions with no source tag, for
	// other sessions.
	otherOrphans []Row
}

func newBuilder(in Input) *builder {
	return &builder{
		in: in, j: newJoin(in), repos: map[string]*repo{},
		used: map[*protocol.Agent]bool{}, seenKey: map[string]bool{}, placed: map[int]bool{},
		standing: map[string][]int{},
	}
}

// repoOf is the repository line for a source, or for a host's label
// when the record carries no source; the first name given sticks.
func (b *builder) repoOf(src, name string) *repo {
	key := source.Key(src)
	if src == "" {
		key = strings.TrimPrefix(LabelRepoNode(name), "repo/")
	}
	r := b.repos[key]
	if r == nil {
		r = &repo{key: key, name: name, source: src}
		b.repos[key] = r
	}
	if r.name == "" {
		r.name = name
	}
	return r
}

// tasks makes the task rows and finds which stand for a listed
// worktree, by the worktree id; the newest submitted of those owns the
// worktree's children.
func (b *builder) tasks() {
	in, j := b.in, b.j
	b.taskRows = make([]Row, len(in.Pendings))
	for i := range in.Pendings {
		p := &in.Pendings[i]
		h, configured := j.hosts[p.Host]
		b.taskRows[i] = Row{Kind: KindTask, Host: p.Host, Name: p.Repo + "/" + p.Branch, Pending: p, Removed: !configured,
			Replaced: configured && h.EnvironmentID != "" && p.EnvironmentID != "" && h.EnvironmentID != p.EnvironmentID}
		if alias := b.taskRows[i].Alias(); alias != "" {
			b.standing[alias] = append(b.standing[alias], i)
		}
		if p.EnvironmentID != "" && p.Root != "" && !p.Gone && !(p.Done && !p.OK) {
			b.seenKey[protocol.SessionKey(p.EnvironmentID, p.Root)] = true
		}
	}
	for _, idx := range b.standing {
		sort.SliceStable(idx, func(x, y int) bool { return newer(b.taskRows[idx[x]].Pending, b.taskRows[idx[y]].Pending) })
	}
}

// worktrees makes one group per listed worktree: the line with its
// agents, panes and runs under it, or the newest standing task in the
// line's place and the other standing tasks after its children.
func (b *builder) worktrees() {
	in, j := b.in, b.j
	for i := range in.Worktrees {
		w := &in.Worktrees[i]
		host := j.byEnv[w.EnvironmentID]
		line := Row{Kind: KindWorktree, Node: w.ID, Host: host, Worktree: w, hostRepo: in.HostRepos[w.ID]}
		if w.Branch == "" {
			line.Name = w.Repo + " (detached) " + w.Root
		} else {
			line.Name = w.Repo + "/" + w.Branch
		}
		key := protocol.SessionKey(w.EnvironmentID, w.Root)
		b.seenKey[key] = true
		agents := j.worktreeAgents(w)
		// The line's agent is the one its jump goes through: in the home
		// session; with the home lost, the one laatmux made at the root,
		// whose session the workspace session attaches to then (Home);
		// or one on a default server. The home, as Home has it, is what
		// the children take the workspace session by.
		line.Agent = rowAgent(agents, w)
		home := w.Session
		if home == "" && line.Agent != nil && line.Agent.Server == protocol.ServerLaatmux {
			home = line.Agent.Session
		}
		var children []Row
		for _, a := range agents {
			b.used[a] = true
			// The agent's own local session: the workspace session for
			// one in the home session, else the attachment to its
			// session, or its session on this machine's default server.
			// A managed one in another line's home session takes that
			// line's workspace session once the tree is in order
			// (visitors).
			c := Row{Kind: KindAgent, Node: a.ID, Host: host, Name: a.Session, Worktree: w, Agent: a}
			if a.Server == protocol.ServerLaatmux && home != "" && a.Session == home {
				c.Local = j.byKey[key]
			}
			if c.Local == nil {
				c.Local = j.agentLocal(host, a)
			}
			if c.Local == nil {
				c.Local = j.byKey[key]
			}
			children = append(children, c)
		}
		children = append(children, b.panes(w, host)...)
		children = append(children, b.runs(w, host)...)
		// The worktree's own session: its workspace session, the one
		// with its key, whenever that exists, which S and z act on even
		// where enter goes to a homeless line's agent in a plain session.
		// Without one, the session its agent on this machine's default
		// server stands for, unless that is a workspace session, which is
		// a line's by its key alone: an agent here in a window of another
		// worktree's workspace session leaves that session to that
		// worktree's line. The viewer in the session enter goes to, when
		// it is no workspace session, is on the line by its own session
		// either way, and on the standing tasks that take its place.
		// The settled state is a workspace session's only, not one set
		// by hand on a plain session or attachment. The most pressing
		// agent is kept apart, for the folded line's icon.
		line.Local = j.byKey[key]
		if w.Session == "" && line.Agent != nil && line.Agent.Server == protocol.ServerDefault {
			if l := j.agentLocal(host, line.Agent); l != nil && !l.Workspace() {
				if line.Local == nil {
					line.Local = l
				}
				if in.Current != "" && l.Name == in.Current {
					line.Current, line.Own = true, true
				}
			}
		}
		line.Settled = line.Local != nil && line.Local.Workspace() && line.Local.Settled
		for k := range children {
			c := &children[k]
			c.Settled, c.Depth = line.Settled, 2
			j.finish(c)
		}
		line.Worst = pressing(children)
		line.Children, line.Depth = len(children), 1
		j.finish(&line)
		if line.Agent != nil {
			// The line's own status is its jump agent's; the icon is
			// the worst's.
			line.Dim = line.Dim && (line.Worst == nil || line.Worst.Dim)
		}
		rp := b.repoOf(w.Source, w.Repo)
		if idx := b.standing[w.ID]; len(idx) > 0 {
			// The newest standing task takes the line's place and its
			// children; the others follow as lines of their own.
			owner := &b.taskRows[idx[0]]
			owner.Worktree, owner.Agent, owner.Local, owner.Worst, owner.Children, owner.Depth = w, line.Agent, line.Local, line.Worst, len(children), 1
			owner.Current, owner.Own, owner.hostRepo = line.Current, line.Own, line.hostRepo
			j.finish(owner)
			for _, k := range idx[1:] {
				b.taskRows[k].Worktree, b.taskRows[k].hostRepo, b.taskRows[k].Local, b.taskRows[k].Depth, b.taskRows[k].Current, b.taskRows[k].Own = w, line.hostRepo, line.Local, 1, line.Current, line.Own
				j.finish(&b.taskRows[k])
			}
			group := append([]Row{*owner}, children...)
			for _, k := range idx[1:] {
				group = append(group, b.taskRows[k])
			}
			rp.nodes = append(rp.nodes, group)
			for _, k := range idx {
				b.placed[k] = true
			}
			continue
		}
		rp.nodes = append(rp.nodes, append([]Row{line}, children...))
	}
}

// panes is the worktree's pane rows, by session, window and pane.
func (b *builder) panes(w *protocol.Worktree, host string) []Row {
	var panes []Row
	for k := range b.in.Panes {
		if p := &b.in.Panes[k]; p.WorktreeID == w.ID && p.EnvironmentID == w.EnvironmentID {
			panes = append(panes, Row{Kind: KindPane, Node: p.ID, Host: host, Name: p.Command, Worktree: w, Pane: p})
		}
	}
	sort.SliceStable(panes, func(x, y int) bool {
		pa, pb := panes[x].Pane, panes[y].Pane
		if pa.Session != pb.Session {
			return pa.Session < pb.Session
		}
		if pa.Window != pb.Window {
			return pa.Window < pb.Window
		}
		return pa.PaneID < pb.PaneID
	})
	return panes
}

// runs is the worktree's run rows, oldest first.
func (b *builder) runs(w *protocol.Worktree, host string) []Row {
	var runs []Row
	for k := range b.in.Runs {
		if r := &b.in.Runs[k]; r.WorktreeID == w.ID && r.EnvironmentID == w.EnvironmentID {
			runs = append(runs, Row{Kind: KindRun, Node: r.ID, Host: host, Name: strings.Join(r.Cmd, " "), Worktree: w, Run: r})
		}
	}
	sort.SliceStable(runs, func(x, y int) bool {
		ra, rb := runs[x].Run, runs[y].Run
		if !ra.StartedAt.Equal(rb.StartedAt) {
			return ra.StartedAt.Before(rb.StartedAt)
		}
		return ra.ID < rb.ID
	})
	return runs
}

// looseTasks places the tasks no worktree line took: before the host
// lists the worktree, or unable to become it. A standing one holds the
// add's agent in its session whose pane starts at the root; the newest
// of several owns it.
func (b *builder) looseTasks() {
	in, j := b.in, b.j
	var loose []int
	for i := range b.taskRows {
		if !b.placed[i] {
			loose = append(loose, i)
		}
	}
	sort.SliceStable(loose, func(x, y int) bool { return newer(b.taskRows[loose[x]].Pending, b.taskRows[loose[y]].Pending) })
	for _, i := range loose {
		t := &b.taskRows[i]
		p := t.Pending
		if t.stands() && p.EnvironmentID != "" && p.Root != "" {
			t.Local = j.byKey[protocol.SessionKey(p.EnvironmentID, p.Root)]
		}
		var children []Row
		if t.stands() && p.EnvironmentID != "" && p.Session != "" && p.Root != "" {
			for k := range in.Agents {
				a := &in.Agents[k]
				if !b.used[a] && a.EnvironmentID == p.EnvironmentID && a.Server == protocol.ServerLaatmux && a.Session == p.Session && a.Cwd == p.Root {
					b.used[a] = true
					// The task's workspace session, or the attachment
					// to the add's session the viewer may be in.
					local := t.Local
					if local == nil {
						local = j.agentLocal(t.Host, a)
					}
					c := Row{Kind: KindAgent, Node: a.ID, Host: t.Host, Name: a.Session, Agent: a, Local: local, Depth: 2}
					j.finish(&c)
					children = append(children, c)
					break
				}
			}
		}
		if len(children) > 0 {
			t.Agent = children[0].Agent
		}
		t.Worst = pressing(children)
		t.Children, t.Depth = len(children), 1
		j.finish(t)
		rp := b.repoOf(p.Source, p.Repo)
		rp.nodes = append(rp.nodes, append([]Row{*t}, children...))
	}
}

// orphans places the orphaned sessions, workspace sessions no worktree,
// nor a task neither gone nor failed, accounts for: under their
// repository by the source tag, or kept for other sessions without one.
func (b *builder) orphans() {
	in, j := b.in, b.j
	for i := range in.Locals {
		l := &in.Locals[i]
		if !l.Workspace() || b.seenKey[l.Key] {
			continue
		}
		env, _ := protocol.SplitSessionKey(l.Key)
		if !j.up(env) {
			continue
		}
		line := Row{Kind: KindWorktree, Node: "session/" + l.Name, Host: j.byEnv[env], Name: l.Name, Local: l, Orphaned: true, Settled: l.Settled, Depth: 1}
		j.finish(&line)
		if l.Source != "" {
			rp := b.repoOf(l.Source, "")
			rp.nodes = append(rp.nodes, []Row{line})
		} else {
			b.otherOrphans = append(b.otherOrphans, line)
		}
	}
}

// nameRepos names a repository known by a source tag alone: its label
// from the worktrees is missing; the source's last element stands in,
// a forge source's path's, without .git.
func (b *builder) nameRepos() {
	for _, rp := range b.repos {
		if rp.name == "" {
			p := rp.source
			if _, fp, ok := source.Forge(rp.source); ok {
				p = fp
			}
			rp.name = path.Base(strings.TrimSuffix(p, ".git"))
		}
	}
}

// repoLines is the repositories by name, each line followed by its
// groups: the main checkouts first, then worktrees by branch, tasks and
// orphaned lines among them by their name, tasks of one name the newest
// first.
func (b *builder) repoLines() []Row {
	var out []Row
	keys := make([]string, 0, len(b.repos))
	for k := range b.repos {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(x, y int) bool {
		if b.repos[keys[x]].name != b.repos[keys[y]].name {
			return b.repos[keys[x]].name < b.repos[keys[y]].name
		}
		return keys[x] < keys[y]
	})
	for _, k := range keys {
		rp := b.repos[k]
		sort.SliceStable(rp.nodes, func(x, y int) bool {
			if ma, mb := rp.nodes[x][0].mainCheckout(), rp.nodes[y][0].mainCheckout(); ma != mb {
				return ma
			}
			la, _ := rp.nodes[x][0].Labels()
			lb, _ := rp.nodes[y][0].Labels()
			if la != lb {
				return la < lb
			}
			// A worktree line before a task line beside it. Of two task
			// lines, the newest first, as of the tasks standing for one
			// worktree: before the listing the newest holds the add's
			// agent, and the session the tasks share is its (HomeLine).
			if ka, kb := rp.nodes[x][0].Kind, rp.nodes[y][0].Kind; ka != kb {
				return ka == KindWorktree
			}
			if pa, pb := rp.nodes[x][0].Pending, rp.nodes[y][0].Pending; pa != nil && pb != nil {
				return newer(pa, pb)
			}
			return rp.nodes[x][0].ID() < rp.nodes[y][0].ID()
		})
		out = append(out, Row{Kind: KindRepo, Node: "repo/" + k, Name: rp.name, Children: len(rp.nodes)})
		for _, group := range rp.nodes {
			out = append(out, group...)
		}
	}
	return out
}

// mainCheckout reports whether a line is a main checkout's.
func (r Row) mainCheckout() bool {
	return r.Kind == KindWorktree && r.Worktree != nil && r.Worktree.Main
}

// attachedHome marks the lines whose home the viewer's plain attachment
// is attached to, whatever runs there: an agent of the line, a visitor,
// or none. The one HomeLine picks, as Enter and the view's LineFor go
// by, is the viewer's by its own session, as following wants it: a
// worktree's line; a task's standing for it, whose home is the task's
// session where the worktree has none and no root agent laatmux made;
// or a task's before the listing. Another line with that home, a
// homeless worktree whose root agent was moved by hand into the
// session, say, stays the viewer's, not Own: the viewer sits with what
// runs there for it, and the scope keeps it, as before; but one line is
// the viewer's by its own session, so following does not take whichever
// is first in the tree's order. A line with no home at all is the
// viewer's, and Own, through an attachment to the session its worktree
// is named after while that session is no other line's: the session
// add made for the worktree, which the host stops calling its home once
// a pane of another worktree is in it, is still the worktree's for the
// viewer. The tasks standing for one worktree are one line here, as for
// the workspace session they share: all are the viewer's when one is,
// by its own Home or name, which for a task with no agent of its own may
// not be the line's, and the newest, which holds the children and which
// following goes to, is Own when HomeLine picks any of them. The line
// HomeLine picks takes the mark whichever of a machine's two names the
// attachment is tagged with and the line carries (homeLine). Not for no
// host, as HomeLine has it, which wants the lines in the tree's order,
// so this runs once they are.
func (b *builder) attachedHome(out []Row) {
	if b.in.Current == "" {
		return
	}
	// lead is the first line of a depth-1 line's group: the line itself,
	// or for a task beside the one standing for a worktree, that one.
	lead := make([]int, len(out))
	first := -1
	for i := range out {
		n := &out[i]
		if n.Depth != 1 {
			continue
		}
		if first < 0 || n.Pending == nil || n.Worktree == nil || n.Worktree != out[first].Worktree {
			first = i
		}
		lead[i] = first
	}
	marked := map[int]bool{} // the groups the viewer is on through the attachment, by lead
	for i := range out {
		n := &out[i]
		home, named := n.Home(), false
		if home == "" && n.Depth == 1 {
			home, named = n.named(), true
		}
		if home == "" || !b.attachedTo(n, home) {
			continue
		}
		l := b.homeLine(out, n, home)
		if !named || l >= 0 && lead[l] == lead[i] {
			marked[lead[i]] = true
		}
		if l >= 0 {
			marked[lead[l]], out[lead[l]].Own = true, true
		}
	}
	for i := range out {
		if out[i].Depth == 1 && marked[lead[i]] {
			out[i].Current = true
		}
	}
}

// attachedTo reports whether the viewer is in a plain attachment to a
// line's session: by the line's host name, or by the name its machine's
// records are listed under where the config gives the machine two, as a
// task submitted through the other keeps its own (Host). Never by no
// host.
func (b *builder) attachedTo(n *Row, home string) bool {
	for _, host := range []string{n.Host, b.listed(n)} {
		if host != "" && b.j.attached(host+"/"+home) {
			return true
		}
	}
	return false
}

// homeLine is the line HomeLine picks for a line's session as the
// view's LineFor asks for it, by the name the line's machine's records
// are listed under, so one line is Own whichever name the others carry;
// by the line's own name where no line under the listed name has the
// session as its home or is named after it, a task submitted through
// another name of the machine.
func (b *builder) homeLine(out []Row, n *Row, home string) int {
	if l := HomeLine(out, b.listed(n), home); l >= 0 {
		return l
	}
	return HomeLine(out, n.Host, home)
}

// listed is the name a line's machine's records are listed under, by
// its worktree's or its task's environment; "" for none.
func (b *builder) listed(n *Row) string {
	switch {
	case n.Worktree != nil:
		return b.j.byEnv[n.Worktree.EnvironmentID]
	case n.Pending != nil:
		return b.j.byEnv[n.Pending.EnvironmentID]
	}
	return ""
}

// visitors gives a managed agent of a worktree that runs in another
// line's home session, `cd ../y && claude` in a split of that line's
// session, that line's workspace session as its own: its pane jump
// lands there, by the line the view's LineFor finds (HomeLine), and the
// viewer in that session sits with it, which marks its own line as the
// viewer's (markViewer). The viewer in a plain attachment to the
// agent's session is on its line all the same, as it was when the
// attachment was the agent's own, and sits with it: its tile is Own
// (attached), as is the line whose home the session is, by that
// attachment (attachedHome). Either way the agent's line is the
// viewer's through a visitor, not Own: following stays on the line
// whose session the viewer is in. A line with no workspace session
// leaves the agent the session worktrees gave it, also where the line
// holds a plain session instead: a task standing for a homeless
// worktree carries the session of the worktree's agent on this
// machine's default server, which the agent's pane is not in. An agent
// of the line's own worktree in the session its line has by name alone
// (HomeLine) is no visitor: it takes the workspace session as one in
// the home does, and the viewer in an attachment to the session is on
// the line by that (attachedHome), not through the agent. HomeLine wants
// the lines in the tree's order, so this runs once they are.
func (b *builder) visitors(out []Row) {
	line := -1
	for i := range out {
		c := &out[i]
		if c.Depth <= 1 {
			line = -1
			if c.Depth == 1 {
				line = i
			}
			continue
		}
		a := c.Agent
		if line < 0 || c.Kind != KindAgent || c.Worktree == nil || a.Server != protocol.ServerLaatmux || a.Session == out[line].Home() {
			continue
		}
		l := HomeLine(out, c.Host, a.Session)
		if l < 0 || out[l].Local == nil || !out[l].Local.Workspace() {
			continue
		}
		if l != line && b.j.attached(c.Host+"/"+a.Session) {
			out[line].Current, c.attached = true, true
		}
		c.Local = out[l].Local
	}
}

// otherSessions appends the other sessions group: managed agents in no
// worktree, then observed ones, then the orphaned sessions with no
// source tag; nothing when there are none.
func (b *builder) otherSessions(out []Row) []Row {
	in, j := b.in, b.j
	var others []Row
	for pass := 0; pass < 2; pass++ {
		for i := range in.Agents {
			a := &in.Agents[i]
			managed := a.Server == protocol.ServerLaatmux
			if b.used[a] || managed != (pass == 0) {
				continue
			}
			host := j.byEnv[a.EnvironmentID]
			c := Row{Kind: KindAgent, Node: a.ID, Host: host, Name: a.Session, Agent: a, Local: j.agentLocal(host, a), Depth: 1}
			// An agent observed in a window of a workspace session is one
			// of that workspace's agents, though no line takes it as a
			// child (its worktree on another host, say): it shows the
			// session's settled state as the line's children do. So is a
			// managed agent in a line's home session, its directory in no
			// worktree, whose pane jump lands in the line's workspace
			// session: it shows that session's state, which z on it
			// toggles, and the viewer in that session sits with it, as
			// with the line's children in the home session, so its row
			// stays in sight when settled and z can undo itself. The line
			// is the one the view's LineFor finds (HomeLine). A task's
			// line carries the session but not the state, and z refuses
			// its agent of no worktree as the task's: none. So does a
			// line holding a plain session instead, a homeless worktree's
			// with no workspace session whose agent is on this machine's
			// default server, which the agent's pane is not in.
			ws := c.Local
			if managed {
				if l := HomeLine(out, host, a.Session); l >= 0 {
					ws = out[l].Local
					if out[l].Pending != nil || ws != nil && !ws.Workspace() {
						ws = nil
					}
					c.Current = ws != nil && in.Current != "" && ws.Name == in.Current
				}
			}
			c.Settled = ws != nil && ws.Workspace() && ws.Settled
			j.finish(&c)
			others = append(others, c)
		}
	}
	others = append(others, b.otherOrphans...)
	if len(others) > 0 {
		out = append(out, Row{Kind: KindGroup, Node: NodeOther, Name: "other sessions", Children: len(others)})
		out = append(out, others...)
	}
	return out
}

// markViewer marks the viewer's lines: a line of its own, an orphaned
// session or a session in other sessions, is the viewer's when its
// session is, or a managed agent's there already through a line's
// workspace session (otherSessions); a worktree or task line is when the
// viewer sits with one of its agents, through the workspace session or
// an attachment, as following wants it. A line is Own by its own
// session alone, not through an agent of it.
func markViewer(out []Row, current string) {
	line := -1
	for i := range out {
		r := &out[i]
		mine := current != "" && r.Local != nil && r.Local.Name == current
		switch {
		case r.Depth <= 1:
			line = -1
			if r.Depth == 1 && (r.Kind == KindWorktree || r.Kind == KindTask) {
				line = i
			}
			// A line may be the viewer's already, through an attachment
			// to its home session beside its workspace session.
			r.Current = mine || r.Current
			r.Own = mine || r.Own
		case mine && line >= 0:
			out[line].Current = true
		}
	}
}

// pressing is the most pressing of a line's agent children, finished,
// in the note's precedence: blocked, done, working, idle, stale; nil
// without agents.
func pressing(children []Row) *Row {
	var best *Row
	for i := range children {
		c := &children[i]
		if c.Agent == nil || c.Agent.Liveness == protocol.Gone {
			// A gone agent's last activity says nothing now.
			continue
		}
		if best == nil || c.Rank() < best.Rank() || c.Rank() == best.Rank() && activityRank(c) < activityRank(best) {
			best = c
		}
	}
	if best == nil {
		return nil
	}
	cc := *best
	return &cc
}

// activityRank orders agents of one rank by what they do: blocked,
// working, then the rest, so a settled worktree's working agent is its
// most pressing over an idle one.
func activityRank(r *Row) int {
	switch r.Agent.Activity {
	case protocol.Blocked:
		return 0
	case protocol.Working:
		return 1
	}
	return 2
}

// Wants is a live agent that is blocked, working or done: what opens a
// line's first fold.
func (r Row) Wants() bool {
	if r.Agent == nil || r.Pending != nil || r.Agent.Liveness == protocol.Gone {
		return false
	}
	return r.Done || r.Agent.Activity == protocol.Blocked || r.Agent.Activity == protocol.Working
}

// Home is the managed session a depth-1 line's workspace session
// attaches to: the worktree's home session; with the home lost, the
// session of the agent laatmux made at its root; a task's before the
// host lists the worktree, or while it lists it without a home; "" for
// a line with none.
func (r Row) Home() string {
	home, _ := r.home()
	return home
}

// home is Home, and whether the session is the line's own: the
// worktree's home or the task's session, not the one the agent laatmux
// made at the root is in with the home lost, which may be another
// line's own.
func (r Row) home() (session string, own bool) {
	switch {
	case r.Depth != 1:
		return "", false
	case r.Worktree != nil && r.Worktree.Session != "":
		return r.Worktree.Session, true
	case r.Worktree != nil && r.Agent != nil && r.Agent.Server == protocol.ServerLaatmux:
		// A standing task's root agent in the task's session leaves
		// the session the task's own.
		return r.Agent.Session, r.stands() && r.Pending.Session == r.Agent.Session
	case r.stands() && r.Pending.EnvironmentID != "" && r.Pending.Root != "":
		// A task's session, before the host lists the worktree or
		// while it lists one without a home.
		return r.Pending.Session, true
	}
	return "", false
}

// HomeLine is the index in the tree of the depth-1 line on a host whose
// workspace session attaches to a managed session, the line whose Home
// it is, or, with no home at all, the line of the worktree the session
// is named after. Of several, the first in the tree's order whose own
// session it is; then the first of a worktree the session is named after
// (namedAfter), whose root agent is in it with the home lost or which
// has no home at all; then the first. A worktree's root agent moved by
// hand into another worktree's session takes the home from both, the
// session's panes no longer all in one root, and the session stays the
// one it is named after. So does a pane of another worktree alone, `cd
// ../y && claude` in a split with claude gone from the root: the
// session add made for the worktree is still its home for the viewer,
// though the host's home, every pane inside the root, is gone. -1 for
// none, and for no host: records no configured host claims may be of
// different machines whose sessions share a name. The view's LineFor
// finds the line by it, a managed agent of no worktree in other
// sessions takes the line's state by it, one of another worktree in the
// session the line's workspace session (visitors), and the viewer in a
// plain attachment to the session is on the line by its own session
// (attachedHome).
func HomeLine(tree []Row, host, session string) int {
	if host == "" || session == "" {
		return -1
	}
	named, first := -1, -1
	for i := range tree {
		n := &tree[i]
		if n.Depth != 1 || n.Host != host {
			continue
		}
		home, own := n.home()
		switch {
		case home != session && home != "":
		case home == session && own:
			return i
		case named < 0 && n.namedAfter(session):
			named = i
		case home == session && first < 0:
			first = i
		}
	}
	if named >= 0 {
		return named
	}
	return first
}

// namedAfter reports whether a managed session has the name add gives
// the line's worktree's (named).
func (r Row) namedAfter(session string) bool {
	return session != "" && session == r.named()
}

// named is the name add gives the managed session of the line's
// worktree, tmux.SessionName of the host's label, which this machine's
// configuration may name otherwise, and the branch. "" for a detached
// worktree, which add does not make, for a main checkout, which add
// makes no session for, and for a line of none.
func (r Row) named() string {
	w := r.Worktree
	if w == nil || w.Branch == "" || w.Main {
		return ""
	}
	return tmux.SessionName(firstOf(r.hostRepo, w.Repo), w.Branch)
}

// Agents is the agent view, from the tree Tree built of the input: the
// tasks first, the newest first, then one tile per agent in the sort
// order; the stale agents and those of settled workspaces, unless
// pressing or the viewer's own, in the Stale fold. Tiles that share a
// primary label, several agents of one worktree say, are numbered in
// the tree's order. Of the input it reads the viewer's session, the
// sort order and the stale fold setting.
func Agents(in Input, tree []Row) Rows {
	var rows []Row
	viewer := map[string]bool{} // the worktree and task lines the viewer is on
	for _, n := range tree {
		if !n.Current {
			continue
		}
		if n.Worktree != nil {
			viewer["w:"+n.Worktree.ID] = true
		}
		if n.Pending != nil {
			viewer["t:"+n.Pending.ID] = true
		}
	}
	var owner string // the task or worktree line the agents below are under
	for _, n := range tree {
		switch n.Kind {
		case KindTask:
			owner = "t:" + n.Pending.ID
			t := n
			t.Kind, t.Node, t.Depth, t.Children = KindTile, n.Pending.ID, 0, 0
			// Own as its line is.
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
			// A node in other sessions is the viewer's as the tree marks
			// it: by its own session, or by the workspace session of the
			// line whose home a managed agent's session is.
			t.Current = n.Depth == 1 && n.Current
			if n.Depth == 2 {
				t.Current = viewer[owner]
			}
			// A tile in the viewer's own session is the viewer's wherever
			// its node sits, and Own, as is a node's in other sessions
			// that is the viewer's, which is in the viewer's session by
			// its own or through the workspace session of the line whose
			// home its session is, and a visitor's the viewer sits with
			// through a plain attachment to its session. Not one in a
			// session a line is marked through: a line marked through one
			// of its children or an attachment stands for its own
			// session, which the viewer is not in.
			t.Own = n.Depth == 1 && n.Current || n.attached || t.Local != nil && in.Current != "" && t.Local.Name == in.Current
			t.Current = t.Current || t.Own
			rows = append(rows, t)
		}
	}
	// Tiles that share a primary label, in the tree's order.
	count := map[string]int{}
	for i := range rows {
		if rows[i].Agent != nil && rows[i].Pending == nil {
			p, _ := rows[i].Labels()
			count[p]++
		}
	}
	seen := map[string]int{}
	for i := range rows {
		if rows[i].Agent == nil || rows[i].Pending != nil {
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
