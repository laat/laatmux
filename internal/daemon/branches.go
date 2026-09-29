package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/github"
	"github.com/laat/laatmux/internal/protocol"
)

// PR and checks: the merging daemon reads, through gh on this machine,
// the PR and check state of every branch the merged stream has a
// worktree of, keyed by source and branch, since a pushed branch is the
// same on every host. It asks every branchEvery while a merged subscriber
// is there, and at once when the set of branches changes; the answers
// are kept in a file with the time they were fetched, so a restart has
// them at once. See milestone five's note, PR and checks, on the laptop.
const (
	branchTick  = 2 * time.Second
	branchEvery = 30 * time.Second
	// branchStale is the age past which an answer is drawn stale.
	branchStale = 5 * time.Minute
	// branchForget is how long an entry is kept once no worktree in the
	// stream has its branch.
	branchForget = 24 * time.Hour
)

// branchEntry is one branch's kept state: the record, when a worktree
// last had the branch, and the head whose checks were first seen
// pending when.
type branchEntry struct {
	Status       protocol.BranchStatus `json:"status"`
	LastSeen     time.Time             `json:"last_seen"`
	PendingOID   string                `json:"pending_oid,omitempty"`
	PendingSince time.Time             `json:"pending_since,omitzero"`
}

// branchQuery is one branch the loop asks about, with where.
type branchQuery struct {
	key  string
	bk   protocol.BranchKey
	host string
	b    github.Branch
}

func branchKeyString(k protocol.BranchKey) string { return k.Source + "\n" + k.Branch }

// openBranches reads the kept answers; a missing file is none.
func openBranches(path string) (map[string]*branchEntry, error) {
	out := map[string]*branchEntry{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Entries map[string]*branchEntry `json:"entries"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	for k, e := range f.Entries {
		if e != nil {
			out[k] = e
		}
	}
	return out, nil
}

// saveBranchesLocked writes the kept answers whole, through a temporary
// file.
func (d *Daemon) saveBranchesLocked() {
	b, err := json.Marshal(struct {
		Entries map[string]*branchEntry `json:"entries"`
	}{d.branches})
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(d.cfg.Branches), 0o700); err == nil {
			tmp := d.cfg.Branches + ".tmp"
			if err = os.WriteFile(tmp, b, 0o600); err == nil {
				err = os.Rename(tmp, d.cfg.Branches)
			}
		}
	}
	if err != nil {
		d.logOnce(&d.lastBranchesErr, "branches: %v", err)
	} else {
		d.lastBranchesErr = ""
	}
}

// branchSetLocked is every branch a worktree in the merged stream has,
// of a source on a forge, by key.
func (d *Daemon) branchSetLocked() map[string]branchQuery {
	out := map[string]branchQuery{}
	add := func(w protocol.Worktree) {
		if w.Source == "" || w.Branch == "" {
			return
		}
		host, path, ok := config.Forge(w.Source)
		if !ok {
			return
		}
		owner, repo, ok := strings.Cut(path, "/")
		if !ok || strings.Contains(repo, "/") {
			return
		}
		bk := protocol.BranchKey{Source: config.SourceKey(w.Source), Branch: w.Branch}
		out[branchKeyString(bk)] = branchQuery{key: branchKeyString(bk), bk: bk, host: strings.ToLower(host),
			b: github.Branch{Owner: owner, Repo: repo, Branch: w.Branch}}
	}
	if d.localHostLocked() != nil {
		for _, w := range d.worktrees {
			add(w)
		}
	}
	for _, mh := range d.mhosts {
		for _, w := range mh.worktrees {
			add(w)
		}
	}
	return out
}

func sameKeys(a map[string]branchQuery, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// runBranches keeps the branch records until ctx is done.
func (d *Daemon) runBranches(ctx context.Context) {
	t := time.NewTicker(branchTick)
	defer t.Stop()
	var asked map[string]bool
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		d.mu.Lock()
		set := d.branchSetLocked()
		d.ageBranchesLocked(set, now)
		due := len(d.msubs) > 0 && (!sameKeys(set, asked) || now.Sub(last) >= branchEvery)
		d.mu.Unlock()
		if !due {
			continue
		}
		asked = map[string]bool{}
		for k := range set {
			asked[k] = true
		}
		last = now
		d.fetchBranches(ctx, set)
	}
}

// ageBranchesLocked marks when each branch was last in the stream, marks
// answers stale past branchStale, and drops the entries of branches no
// worktree has had for branchForget.
func (d *Daemon) ageBranchesLocked(set map[string]branchQuery, now time.Time) {
	changed := false
	for k := range set {
		if e := d.branches[k]; e != nil {
			e.LastSeen = now
		}
	}
	for k, e := range d.branches {
		switch {
		case set[k].key == "" && now.Sub(e.LastSeen) > branchForget:
			bk := e.Status.BranchKey
			delete(d.branches, k)
			d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, BranchStatusKey: &bk})
			changed = true
		case !e.Status.Stale && now.Sub(e.Status.FetchedAt) > branchStale:
			e.Status.Stale = true
			st := e.Status
			d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, BranchStatus: &st})
			changed = true
		}
	}
	if changed {
		d.saveBranchesLocked()
	}
}

// fetchBranches asks GitHub about the set, host by host, and applies the
// answers.
func (d *Daemon) fetchBranches(ctx context.Context, set map[string]branchQuery) {
	byHost := map[string][]branchQuery{}
	for _, q := range set {
		byHost[q.host] = append(byHost[q.host], q)
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	ghErr := ""
	for _, host := range hosts {
		qs := byHost[host]
		sort.Slice(qs, func(i, j int) bool { return qs[i].key < qs[j].key })
		bs := make([]github.Branch, len(qs))
		for i, q := range qs {
			bs[i] = q.b
		}
		results, err := github.Fetch(ctx, d.cfg.GitHub, host, bs)
		if ctx.Err() != nil {
			return
		}
		if err != nil && ghErr == "" && (host == "github.com" || errors.Is(err, github.ErrNoGH)) {
			// gh missing, or logged out of github.com, is the daemon's
			// reason for showing nothing; another host that is not
			// GitHub, or not logged in, is only not shown.
			ghErr = err.Error()
		}
		d.applyBranches(qs, results)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case ghErr != "" && ghErr != d.githubErr:
		d.githubErr = ghErr
		d.cfg.Logger.Printf("github: %s", ghErr)
		d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, GitHubError: ghErr})
	case ghErr == "" && d.githubErr != "":
		d.githubErr = ""
		d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, GitHubOK: true})
	}
}

// applyBranches takes one host's answers: a branch GitHub does not have
// drops its entry; a failed answer keeps the last one, marked stale; an
// answer upserts the record when a value changed.
func (d *Daemon) applyBranches(qs []branchQuery, results []github.Result) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for i, q := range qs {
		r := results[i]
		e := d.branches[q.key]
		switch {
		case r.NoRef:
			if e != nil {
				delete(d.branches, q.key)
				bk := q.bk
				d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, BranchStatusKey: &bk})
			}
		case r.Err != nil:
			if e != nil && !e.Status.Stale {
				e.Status.Stale = true
				st := e.Status
				d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, BranchStatus: &st})
			}
		default:
			if e == nil {
				e = &branchEntry{}
				d.branches[q.key] = e
			}
			e.LastSeen = now
			st := protocol.BranchStatus{BranchKey: q.bk, FetchedAt: now, HeadOID: r.HeadOID, ChecksURL: r.ChecksURL, PR: r.PR, Checks: r.Checks}
			if c := st.Checks; c != nil && c.State == protocol.ChecksPending {
				if e.PendingOID != r.HeadOID {
					e.PendingOID, e.PendingSince = r.HeadOID, now
				}
				c.PendingSince = e.PendingSince
			} else {
				e.PendingOID, e.PendingSince = "", time.Time{}
			}
			same := sameBranchStatus(e.Status, st)
			e.Status = st
			if !same {
				d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, BranchStatus: &st})
			}
		}
	}
	d.saveBranchesLocked()
}

// sameBranchStatus compares two records but for when they were fetched.
func sameBranchStatus(a, b protocol.BranchStatus) bool {
	a.FetchedAt, b.FetchedAt = time.Time{}, time.Time{}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// branchStatusesLocked is every kept record, for a merged snapshot.
func (d *Daemon) branchStatusesLocked() []protocol.BranchStatus {
	out := make([]protocol.BranchStatus, 0, len(d.branches))
	for _, e := range d.branches {
		out = append(out, e.Status)
	}
	sort.Slice(out, func(i, j int) bool {
		return branchKeyString(out[i].BranchKey) < branchKeyString(out[j].BranchKey)
	})
	return out
}
