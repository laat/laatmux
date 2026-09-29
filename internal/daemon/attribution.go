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
// pane_current_path. Paths are compared with symlinks resolved, as git
// registers roots, and on path separators, so /w/foo-2 is not inside
// /w/foo. A pane under no listed root, the main checkout's included,
// belongs to no worktree.
//
// Panes are polled far more often than git lists worktrees. An agent
// record takes its worktree at every observation, and when a listing
// changes the set of roots every pane is attributed again, so a
// worktree listed after its pane was seen gains the pane without
// waiting for the pane to change.

// maxResolved bounds the resolved-path cache between listings.
const maxResolved = 4096

// root is a listed worktree root and its path with symlinks resolved.
type root struct {
	root string // as git registered it, the worktree record's
	real string
}

// panePath is the path a pane is attributed by: the recorded one of a
// pane laatmux made, the current one otherwise.
func panePath(p tmux.Pane) string { return firstNonEmpty(p.Cwd, p.CurrentPath) }

// resolve cleans a path and resolves its symlinks, through a cache the
// worktree listing clears, so a poll costs no file system call per pane
// and a changed symlink is seen within a listing. A path that does not
// resolve, gone or unreadable, is taken as it is.
func (d *Daemon) resolve(path string) string {
	if path == "" {
		return ""
	}
	d.resolveMu.Lock()
	r, ok := d.resolved[path]
	d.resolveMu.Unlock()
	if ok {
		return r
	}
	r = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(r); err == nil {
		r = real
	}
	d.resolveMu.Lock()
	if len(d.resolved) >= maxResolved {
		clear(d.resolved)
	}
	d.resolved[path] = r
	d.resolveMu.Unlock()
	return r
}

// forgetResolved empties the resolved-path cache.
func (d *Daemon) forgetResolved() {
	d.resolveMu.Lock()
	clear(d.resolved)
	d.resolveMu.Unlock()
}

// inside reports whether path is dir or below it, on path separators.
func inside(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// resolveRoots is the roots of a listing with their resolved paths,
// longest first, so the first root that contains a path is the deepest.
func (d *Daemon) resolveRoots(roots []string) []root {
	out := make([]root, 0, len(roots))
	for _, r := range roots {
		out = append(out, root{root: r, real: d.resolve(r)})
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
		if inside(path, r.real) {
			return d.worktreeID(r.root)
		}
	}
	return ""
}

// homeSessions maps a root to its home session on the managed server:
// the session with a pane laatmux made at the root, all of whose panes
// are inside it. A split for a shell or a test watcher keeps the home;
// a pane that has gone elsewhere takes it away, so rm, which kills the
// home session, never takes a pane outside the worktree with it. Two
// sessions on one root is not a state add creates; the lexically first
// name wins so the record is stable. resolve is Daemon.resolve.
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
			real := resolve(p.Cwd)
			all := true
			for _, q := range ps {
				if !inside(resolve(panePath(q)), real) {
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
	if wid == "" {
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
	d.seq++
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Seq: d.seq, Pane: &rec})
}

// dropPaneLocked removes a pane's record, if it has one: the pane has
// left every worktree, is gone, or has an agent now.
func (d *Daemon) dropPaneLocked(key string) {
	if _, had := d.paneRecs[key]; !had {
		return
	}
	delete(d.paneRecs, key)
	d.seq++
	d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, Seq: d.seq, PaneRecordID: d.paneRecordID(key)})
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
			d.seq++
			d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Seq: d.seq, Agent: &a})
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
	d.seq++
	d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Seq: d.seq, Run: &rec})
}

// runEndedLocked removes a run's record, if it was published. Called
// with d.mu held.
func (d *Daemon) runEndedLocked(r *runJob) {
	id := d.runRecordID(r.id)
	if _, ok := d.runRecs[id]; !ok {
		return
	}
	delete(d.runRecs, id)
	d.seq++
	d.broadcastLocked(protocol.Message{Type: protocol.TypeRemove, Seq: d.seq, RunID: id})
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
