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
// root belongs to no worktree. An agent in a main checkout, under no
// worktree root inside it, belongs to the checkout's record when its
// pane is on the default server and not laatmux's own, the user's plain
// session there, or in the checkout's home session on the managed
// server; the record is published while an agent is so (see
// syncMainsLocked), and first, so no agent names a record a subscriber
// has not had. Any other pane in a main checkout belongs to none, and a
// pane with no agent there has no pane record.
//
// Panes are polled far more often than git lists worktrees. An agent
// record takes its worktree at every observation, and when a listing
// changes the set of roots every pane is attributed again, so a
// worktree listed after its pane was seen gains the pane without
// waiting for the pane to change.

// root is a listed worktree root and its path with symlinks resolved,
// or with main a main checkout's directory, of which only an agent in a
// plain session on the default server or in the checkout's home session
// is (attributeLocked); with unread
// too, one whose HEAD could not be read, which has no record: no pane is
// its, nor a worktree's around it.
type root struct {
	root   string // as git registered it, the worktree record's
	real   string
	main   bool
	unread bool
}

// panePath is the path a pane is attributed by: the recorded one of a
// pane laatmux made, the current one otherwise.
func panePath(p tmux.Pane) string {
	if p.Managed && p.Cwd != "" {
		return p.Cwd
	}
	return p.CurrentPath
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

// resolveRoots is the roots of a listing with their resolved paths, the
// worktrees' and the main checkouts', given by root and unread, longest
// first, so the first root that contains a path is the deepest.
func resolveRoots(roots []string, mains []root) []root {
	out := make([]root, 0, len(roots)+len(mains))
	for _, r := range roots {
		out = append(out, root{root: r, real: resolveNow(r)})
	}
	for _, r := range mains {
		r.real, r.main = resolveNow(r.root), true
		out = append(out, r)
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

// worktreeOfLocked is the id of the worktree whose root is the deepest
// to contain the resolved path, "" when none does. A main checkout's
// directory is such a root too: its id where main takes the checkout's
// root, else "", not the id of a worktree around the checkout, since the
// pane is in the checkout; "" too for one unread, which has no record.
// A nil main takes none. Called with d.mu held.
func (d *Daemon) worktreeOfLocked(path string, main func(root string) bool) string {
	if path == "" {
		return ""
	}
	for _, r := range d.roots {
		if !inside(path, r.real) && !inside(path, r.root) {
			continue
		}
		if r.main && (main == nil || r.unread || !main(r.root)) {
			return ""
		}
		return d.recordID(r)
	}
	return ""
}

// attributeLocked is the worktree an observed pane's agent belongs to:
// the deepest listed root its path is in, a main checkout's directory
// only for a live, named agent, claude or codex, in a pane that is not
// laatmux's own: on the default server, the user's plain session there,
// or on the managed server in the checkout's home session (homeSessions),
// the shell session a jump made at the root, say, where the user started
// it. Any other session new made in a main checkout keeps a row of its
// own; a shell, an identified pane with no named agent or one left after
// its agent quit, puts no checkout in use, nor keeps one, so a record
// never follows a shell's cd; a pane record is never a main checkout's
// (publishPaneLocked). Called with d.mu held.
func (d *Daemon) attributeLocked(st *paneState, a protocol.Agent) string {
	if a.Agent == "" || a.Liveness == protocol.Gone || st.pane.Own {
		return d.worktreeOfLocked(st.path, nil)
	}
	return d.worktreeOfLocked(st.path, func(root string) bool {
		if st.target.Managed {
			return st.pane.Session != "" && d.managedRoots[root] == st.pane.Session
		}
		return st.target.Label == protocol.ServerDefault
	})
}

// within reports whether path is inside root, as written or resolved;
// resolve is d.paths.resolve.
func within(path, root string, resolve func(string) string) bool {
	p := resolve(path)
	return inside(p, filepath.Clean(root)) || inside(p, resolve(root))
}

// homeSessions maps a root to its home session on the managed server:
// the session with a pane laatmux made at the root, all of whose panes
// are inside it, which jump attaches to: a worktree's root or a main
// checkout's, whose home a jump makes with a shell. A split for a shell
// or a test watcher keeps the home; a pane that has gone elsewhere takes
// it away, the session being no longer the worktree's alone. rm kills
// every managed session with a pane made at the root all the same, so
// no agent is left in a removed directory. Two sessions on one root is
// not a state add creates; the lexically first name wins so the record
// is stable. resolve is d.paths.resolve.
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
	wid := d.worktreeOfLocked(st.path, nil)
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
		Managed:       p.Managed,
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
		a.PID == b.PID && a.Cwd == b.Cwd && a.WorktreeID == b.WorktreeID && a.Managed == b.Managed
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
			wid := d.attributeLocked(st, a)
			if a.WorktreeID == wid {
				continue
			}
			a.WorktreeID, a.UpdatedAt = wid, now
			d.syncMainsLocked(now, wid)
			d.agents[key] = a
			d.broadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Agent: &a})
			continue
		}
		if st.bare {
			d.publishPaneLocked(key, st, now)
		}
	}
	d.syncMainsLocked(now, "")
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
