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
	// FailingKey is the checks' rollup and counts, whose failing
	// check's name is not asked for again while it holds.
	FailingKey string `json:"failing_key,omitempty"`
	// FailingAt is when the failing check's name was last asked for.
	FailingAt time.Time `json:"failing_at,omitzero"`
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
	now := time.Now()
	for k, e := range f.Entries {
		if e != nil {
			// An answer from before the restart is as old as it is.
			e.Status.Stale = e.Status.Stale || now.Sub(e.Status.FetchedAt) > branchStale
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
		if !ok || !d.githubHost(host) {
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

// githubHost reports whether a source's host is one gh is asked about:
// github.com, or a GitHub Enterprise host the config names.
func (d *Daemon) githubHost(host string) bool {
	if strings.EqualFold(host, "github.com") {
		return true
	}
	for _, h := range d.cfg.GitHubHosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// hostsListedLocked reports whether every configured host has a listing
// of this connection's, so a branch missing from the set is missing
// from the hosts, not only not yet heard of. Once it has held since the
// daemon started, a host down later does not stop the forgetting.
func (d *Daemon) hostsListedLocked() bool {
	if len(d.mhosts) == 0 {
		return false
	}
	// A listing that succeeded, not only a snapshot: a host whose git
	// failed sends one with no worktrees.
	// A host with no worktrees, this machine without a store or a
	// daemon without the capability, has no branches to wait for.
	for _, mh := range d.mhosts {
		switch {
		case mh.host.Local() && d.cfg.Store != nil && !d.listed:
			return false
		case !mh.host.Local() && !mh.listed && (!mh.status.Connected || protocol.Has(mh.status.Capabilities, protocol.CapWorktrees)):
			return false
		}
	}
	return true
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

// branchRound bounds one round of queries, every host's chunks and
// failing names: a slow GitHub delays the next round, never the aging.
const branchRound = 2 * time.Minute

// runBranches keeps the branch records until ctx is done. A round of
// queries runs beside the loop, one at a time, so the records keep
// aging while it waits on GitHub.
func (d *Daemon) runBranches(ctx context.Context) {
	t := time.NewTicker(branchTick)
	defer t.Stop()
	var asked map[string]bool
	var last time.Time
	done := make(chan struct{}, 1)
	running := false
	for {
		select {
		case <-ctx.Done():
			// A round under way writes the file when it ends: it ends
			// before the loop does.
			if running {
				<-done
			}
			return
		case <-t.C:
		case <-done:
			running = false
			continue
		}
		now := time.Now().Round(0) // the wall clock, sleep included
		d.mu.Lock()
		set := d.branchSetLocked()
		d.ageBranchesLocked(set, now)
		due := !running && len(d.msubs) > 0 && (!sameKeys(set, asked) || now.Sub(last) >= branchEvery)
		d.mu.Unlock()
		if !due {
			continue
		}
		asked = map[string]bool{}
		for k := range set {
			asked[k] = true
		}
		last, running = now, true
		go func() {
			rctx, cancel := context.WithTimeout(ctx, branchRound)
			defer cancel()
			d.fetchBranches(rctx, set)
			done <- struct{}{}
		}()
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
	// A branch is known gone only once every host has listed: after a
	// restart, before a subscription has reached them, the set is
	// empty, and the kept answers stand.
	if d.hostsListedLocked() {
		d.branchesListed = true
	}
	listed := d.branchesListed
	for k, e := range d.branches {
		switch {
		case listed && set[k].key == "" && now.Sub(e.LastSeen) > branchForget:
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
	// Every host's status first, each within its share of the round,
	// so a slow host starves no other; the failing checks' names after.
	type answer struct {
		qs      []branchQuery
		results []github.Result
	}
	answers := make([]answer, 0, len(hosts))
	var ghErrs []string
	d.mu.Lock()
	d.roundErrs = map[string]bool{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.branchErrs, d.roundErrs = d.roundErrs, nil
		d.mu.Unlock()
	}()
	deadline, bounded := ctx.Deadline()
	for n, host := range hosts {
		qs := byHost[host]
		sort.Slice(qs, func(i, j int) bool { return qs[i].key < qs[j].key })
		bs := make([]github.Branch, len(qs))
		for i, q := range qs {
			bs[i] = q.b
		}
		hctx, cancel := ctx, context.CancelFunc(func() {})
		if bounded {
			hctx, cancel = context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(hosts)-n))
		}
		results, err := github.Fetch(hctx, d.cfg.GitHub, host, bs)
		cancel()
		if ctx.Err() != nil {
			// The round is out of time: what it has is applied, the
			// hosts it did not reach kept, stale, and no names asked.
			answers = append(answers, answer{qs, results})
			for _, rest := range hosts[n+1:] {
				rqs := byHost[rest]
				failed := make([]github.Result, len(rqs))
				for i := range failed {
					failed[i].Err = ctx.Err()
				}
				answers = append(answers, answer{rqs, failed})
			}
			for _, a := range answers {
				d.applyBranches(a.qs, a.results, nil)
			}
			return
		}
		if errors.Is(err, github.ErrNoGH) || errors.Is(err, github.ErrLoggedOut) {
			// gh missing, or logged out of a host the config trusts,
			// is the daemon's reason for showing nothing there.
			if msg := err.Error(); len(ghErrs) == 0 || ghErrs[len(ghErrs)-1] != msg {
				ghErrs = append(ghErrs, msg)
			}
		}
		answers = append(answers, answer{qs, results})
	}
	for _, a := range answers {
		known := d.knownFailing(a.qs)
		github.FillFailing(ctx, d.cfg.GitHub, a.qs[0].host, a.results, known)
		d.applyBranches(a.qs, a.results, known)
	}
	ghErr := strings.Join(ghErrs, "; ")
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

// knownFailing is the failing names found for the branches' rollups
// within branchStale: a rollup's failing check can change on the same
// commit, a rerun say, so a name is asked for again once it is old.
func (d *Daemon) knownFailing(qs []branchQuery) map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	known := map[string]string{}
	now := time.Now().Round(0)
	for _, q := range qs {
		e := d.branches[q.key]
		if e != nil && e.FailingKey != "" && e.Status.Checks != nil && e.Status.Checks.Failing != "" && now.Sub(e.FailingAt) < branchStale {
			known[e.FailingKey] = e.Status.Checks.Failing
		}
	}
	return known
}

// applyBranches takes one host's answers: a branch GitHub does not have
// drops its entry; a failed answer keeps the last one, marked stale; an
// answer upserts the record when a value changed.
func (d *Daemon) applyBranches(qs []branchQuery, results []github.Result, known map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// The wall clock: a laptop that slept has its answers aged by the
	// sleep too.
	now := time.Now().Round(0)
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
			// A branch GitHub answered with an error, a repository it
			// cannot resolve or an organisation's SSO say, is logged
			// when the round before did not have it; gh's own failures
			// are github_error's.
			if !errors.Is(r.Err, github.ErrNoGH) && !errors.Is(r.Err, github.ErrLoggedOut) {
				msg := q.host + ": " + r.Err.Error()
				if !d.branchErrs[msg] {
					d.cfg.Logger.Printf("github: %s", msg)
				}
				if d.roundErrs != nil {
					d.roundErrs[msg] = true
				}
			}
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
			if _, cached := known[r.FailingKey()]; !cached {
				e.FailingAt = now // a name found now, or none needed
			}
			e.LastSeen, e.FailingKey = now, r.FailingKey()
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
