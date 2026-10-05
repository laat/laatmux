package daemon

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

// Attribution. The host sees both git and tmux, so it says which worktree
// each pane belongs to: the one whose root contains the pane's path, the
// longest such root when checkouts nest. A pane laatmux made has its
// path recorded in @laatmux_cwd; any other pane is where its process is,
// pane_current_path. Paths are compared on path separators, so /w/foo-2
// is not inside /w/foo, against each root as git registered it and with
// its symlinks resolved; a pane's path is resolved too, off the poll, so
// a pane on a hung mount never holds the poll up. A pane under no listed
// root, the main checkout's included, belongs to no worktree.
//
// Panes are polled far more often than git lists worktrees. An agent
// record takes its worktree at every observation, and when a listing
// changes the set of roots every pane is attributed again, so a
// worktree listed after its pane was seen gains the pane without
// waiting for the pane to change.

// The resolved-path cache: its size, how many resolutions may be in
// flight, and how long a resolution stands before it is made again, in
// the background, the old one standing meanwhile.
const (
	maxResolved  = 4096
	maxResolving = 64
	resolveTTL   = time.Minute
)

// resolution is a path with its symlinks resolved, and when that was.
type resolution struct {
	real string
	at   time.Time
}

// root is a listed worktree root and its path with symlinks resolved.
type root struct {
	root string // as git registered it, the worktree record's
	real string
}

// panePath is the path a pane is attributed by: the recorded one of a
// pane laatmux made, the current one otherwise.
func panePath(p tmux.Pane) string {
	if p.Managed && p.Cwd != "" {
		return p.Cwd
	}
	return p.CurrentPath
}

// resolve is the path with its symlinks resolved as last seen, and the
// path cleaned until it has been: the file system is asked on a
// goroutine of its own, so a path on a hung mount costs that goroutine
// and never the poll, and the answer is there by a later poll. A path
// that does not resolve, gone or unreadable, is taken as it is.
func (d *Daemon) resolve(path string) string {
	if path == "" {
		return ""
	}
	clean := filepath.Clean(path)
	d.resolveMu.Lock()
	defer d.resolveMu.Unlock()
	r, ok := d.resolved[path]
	if (!ok || time.Since(r.at) > resolveTTL) && !d.resolving[path] && len(d.resolving) < maxResolving {
		d.resolving[path] = true
		go func() {
			real := clean
			if rr, err := filepath.EvalSymlinks(clean); err == nil {
				real = rr
			}
			d.resolveMu.Lock()
			defer d.resolveMu.Unlock()
			delete(d.resolving, path)
			if _, ok := d.resolved[path]; !ok && len(d.resolved) >= maxResolved {
				d.evictResolvedLocked()
			}
			d.resolved[path] = resolution{real: real, at: time.Now()}
		}()
	}
	if ok {
		return r.real
	}
	return clean
}

// evictResolvedLocked makes room in the full cache: the entries no poll
// has asked for in a while go, those of the panes there are now being
// refreshed within a TTL; failing that, one entry goes. Never all, which
// would have every path answered cleaned for a poll, and a symlinked
// pane lose its worktree for it. Called with resolveMu held.
func (d *Daemon) evictResolvedLocked() {
	for p, r := range d.resolved {
		if time.Since(r.at) > 3*resolveTTL {
			delete(d.resolved, p)
		}
	}
	for p := range d.resolved {
		if len(d.resolved) < maxResolved {
			return
		}
		delete(d.resolved, p)
	}
}

// resolveNow resolves a path on the caller's goroutine: a listed root,
// which git has just read.
func resolveNow(path string) string {
	clean := filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		return real
	}
	return clean
}

// inside reports whether path is dir or below it, on path separators.
func inside(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// resolveRoots is the roots of a listing with their resolved paths,
// longest first, so the first root that contains a path is the deepest.
func resolveRoots(roots []string) []root {
	out := make([]root, 0, len(roots))
	for _, r := range roots {
		out = append(out, root{root: r, real: resolveNow(r)})
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].real) > len(out[j].real) })
	return out
}

func sameRoots(a, b []root) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// setRootsLocked takes the roots of a listing, and attributes every
// pane again when they differ from the last. Called with d.mu held,
// after the worktree records are published, so a pane's new worktree is
// in the stream before the pane names it.
func (d *Daemon) setRootsLocked(roots []root, now time.Time) {
	if sameRoots(roots, d.roots) {
		return
	}
	d.roots = roots
	d.reattributeLocked(now)
}

// worktreeOfLocked is the id of the worktree whose root contains the
// resolved path, "" when none does. Called with d.mu held.
func (d *Daemon) worktreeOfLocked(path string) string {
	if path == "" {
		return ""
	}
	for _, r := range d.roots {
		if inside(path, r.real) || inside(path, r.root) {
			return d.worktreeID(r.root)
		}
	}
	return ""
}

// within reports whether path is inside root, as written or resolved;
// resolve is Daemon.resolve.
func within(path, root string, resolve func(string) string) bool {
	p := resolve(path)
	return inside(p, filepath.Clean(root)) || inside(p, resolve(root))
}

// homeSessions maps a root to its home session on the managed server:
// the session with a pane laatmux made at the root, all of whose panes
// are inside it, which jump attaches to. A split for a shell or a test
// watcher keeps the home; a pane that has gone elsewhere takes it away,
// the session being no longer the worktree's alone. rm kills every
// managed session with a pane made at the root all the same, so no
// agent is left in a removed directory. Two sessions on one root is not
// a state add creates; the lexically first name wins so the record is
// stable. resolve is Daemon.resolve.
func homeSessions(panes []tmux.Pane, resolve func(string) string) map[string]string {
	bySession := map[string][]tmux.Pane{}
	for _, p := range panes {
		bySession[p.Session] = append(bySession[p.Session], p)
	}
	homes := map[string]string{}
	for session, ps := range bySession {
		for _, p := range ps {
			if !p.Managed || p.Cwd == "" {
				continue
			}
			if cur, ok := homes[p.Cwd]; ok && cur <= session {
				continue
			}
			all := true
			for _, q := range ps {
				if !within(panePath(q), p.Cwd, resolve) {
					all = false
					break
				}
			}
			if all {
				homes[p.Cwd] = session
			}
		}
	}
	return homes
}

// paneRecordID is <environment_id>/pane/<server>/<pane_id>.
func (d *Daemon) paneRecordID(key string) string { return d.cfg.EnvironmentID + "/pane/" + key }

// runRecordID is <environment_id>/run/<command id>.
func (d *Daemon) runRecordID(id string) string { return d.cfg.EnvironmentID + "/run/" + id }

// publishPaneLocked publishes the pane record of a pane with no agent,
// from its last observation: upserted when it is inside a worktree and
// what the record says has changed, removed when it has left every
// worktree. Called with d.mu held.
func (d *Daemon) publishPaneLocked(key string, st *paneState, now time.Time) {
	wid := d.worktreeOfLocked(st.path)
	if wid == "" || st.pane.Own {
		// Outside every worktree, or laatmux's own: a sidebar pane, or
		// the attach pane of a workspace session.
		d.dropPaneLocked(key)
		return
	}
	p := st.pane
	rec := protocol.Pane{
		ID:            d.paneRecordID(key),
		EnvironmentID: d.cfg.EnvironmentID,
		Server:        st.target.Label,
		Session:       p.Session,
		Window:        p.WindowIndex,
		PaneID:        p.ID,
		Command:       p.CurrentCommand,
		PID:           p.PID,
		Cwd:           panePath(p),
		WorktreeID:    wid,
		UpdatedAt:     now,
	}
	if prev, had := d.paneRecs[key]; had && samePane(prev, rec) {
		return
	}
	d.paneRecs[key] = rec
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Pane: &rec})
}

// dropPaneLocked removes a pane's record, if it has one: the pane has
// left every worktree, is gone, or has an agent now.
func (d *Daemon) dropPaneLocked(key string) {
	if _, had := d.paneRecs[key]; !had {
		return
	}
	delete(d.paneRecs, key)
	d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, PaneRecordID: d.paneRecordID(key)})
}

func samePane(a, b protocol.Pane) bool {
	return a.Session == b.Session && a.Window == b.Window && a.Command == b.Command &&
		a.PID == b.PID && a.Cwd == b.Cwd && a.WorktreeID == b.WorktreeID
}

// reattributeLocked attributes every observed pane again, after a
// listing that changed the roots: agent records take their new
// worktree, and panes without an agent gain or lose their records.
// Called with d.mu held.
func (d *Daemon) reattributeLocked(now time.Time) {
	for key, st := range d.panes {
		if !st.observed {
			continue
		}
		if a, ok := d.agents[key]; ok {
			wid := d.worktreeOfLocked(st.path)
			if a.WorktreeID == wid {
				continue
			}
			a.WorktreeID, a.UpdatedAt = wid, now
			d.agents[key] = a
			d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &a})
			continue
		}
		if st.bare {
			d.publishPaneLocked(key, st, now)
		}
	}
}

// runStarted publishes a run whose process has started.
func (d *Daemon) runStarted(r *runJob, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec := protocol.Run{
		ID:            d.runRecordID(r.id),
		EnvironmentID: d.cfg.EnvironmentID,
		Root:          r.root,
		WorktreeID:    d.worktreeID(r.root),
		Cmd:           append([]string(nil), r.argv...),
		StartedAt:     at,
	}
	d.runRecs[rec.ID] = rec
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Run: &rec})
}

// runEndedLocked removes a run's record, if it was published. Called
// with d.mu held.
func (d *Daemon) runEndedLocked(r *runJob) {
	id := d.runRecordID(r.id)
	if _, ok := d.runRecs[id]; !ok {
		return
	}
	delete(d.runRecs, id)
	d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, RunID: id})
}

// paneRecsLocked and runRecsLocked are the published records, for a
// snapshot. Called with d.mu held.
func (d *Daemon) paneRecsLocked() []protocol.Pane {
	out := make([]protocol.Pane, 0, len(d.paneRecs))
	for _, p := range d.paneRecs {
		out = append(out, p)
	}
	return out
}

func (d *Daemon) runRecsLocked() []protocol.Run {
	out := make([]protocol.Run, 0, len(d.runRecs))
	for _, r := range d.runRecs {
		out = append(out, r)
	}
	return out
}
