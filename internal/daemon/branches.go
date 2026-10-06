package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/github"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/source"
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

// branchPaging is how long a branch whose forks' PRs held none of the
// repository's own is not paged again.
const branchPaging = time.Hour

// branchQuery is one branch the loop asks about, with where.
type branchQuery struct {
	key  string
	bk   protocol.BranchKey
	host string
	b    github.Branch
}

func branchKeyString(k protocol.BranchKey) string { return k.Source + "\n" + k.Branch }

// branches is the merging daemon's branch records and the state of
// asking GitHub about them. It is used under the daemon's lock, mu,
// which the records are published under: the methods with the Locked
// suffix are called with it held, the others take it. fetch runs a
// round beside the daemon's loop and takes the lock around each step.
type branches struct {
	mu      *sync.Mutex             // the daemon's
	entries map[string]*branchEntry // by key
	// githubErr is why GitHub cannot be read, joined from hostErrs,
	// gh's failure by host; lastErr is the file's last write error,
	// logged once.
	githubErr string
	hostErrs  map[string]string
	lastErr   string
	// listed is that every host has listed once since the daemon
	// started, from when a branch missing from the set is known gone.
	listed bool
	// errs are the per-branch errors of the last round, logged, and
	// roundErrs those of the round under way; pagedNone is when a
	// branch's forks' pages held none of its own.
	errs      map[string]bool
	roundErrs map[string]bool
	pagedNone map[string]time.Time
	// cfg is the daemon's: GitHub, Branches, GitHubHosts and Logger.
	cfg *Config
	// publish puts a message on the merged stream; called with mu held.
	publish func(protocol.Message)
}

// newBranches is the tracker over the kept answers in cfg.Branches; a
// file that does not read starts over.
func newBranches(cfg *Config, mu *sync.Mutex, publish func(protocol.Message)) *branches {
	entries, err := openBranches(cfg.Branches)
	if err != nil {
		cfg.Logger.Printf("branches: %v; starting over", err)
		entries = map[string]*branchEntry{}
	}
	return &branches{mu: mu, entries: entries, cfg: cfg, publish: publish}
}

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

// saveLocked writes the kept answers whole, through a temporary file.
func (b *branches) saveLocked() {
	data, err := json.Marshal(struct {
		Entries map[string]*branchEntry `json:"entries"`
	}{b.entries})
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(b.cfg.Branches), 0o700); err == nil {
			err = home.WriteAtomic(b.cfg.Branches, data)
		}
	}
	if err != nil {
		logOnce(b.cfg.Logger, &b.lastErr, "branches: %v", err)
	} else {
		b.lastErr = ""
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
		host, path, ok := source.Forge(w.Source)
		if !ok || !d.branches.githubHost(host) {
			return
		}
		owner, repo, ok := strings.Cut(path, "/")
		if !ok || strings.Contains(repo, "/") {
			return
		}
		bk := protocol.BranchKey{Source: source.Key(w.Source), Branch: w.Branch}
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
func (b *branches) githubHost(host string) bool {
	if strings.EqualFold(host, "github.com") {
		return true
	}
	return slices.ContainsFunc(b.cfg.GitHubHosts, func(h string) bool { return strings.EqualFold(h, host) })
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
		d.branches.ageLocked(set, now, d.hostsListedLocked())
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
			d.branches.fetch(rctx, set)
			done <- struct{}{}
		}()
	}
}

// ageLocked marks when each branch was last in the stream, marks
// answers stale past branchStale, and drops the entries of branches no
// worktree has had for branchForget. hostsListed is whether every host
// has a listing now.
func (b *branches) ageLocked(set map[string]branchQuery, now time.Time, hostsListed bool) {
	changed := false
	for k := range set {
		if e := b.entries[k]; e != nil {
			e.LastSeen = now
		}
	}
	// A branch is known gone only once every host has listed: after a
	// restart, before a subscription has reached them, the set is
	// empty, and the kept answers stand.
	if hostsListed {
		b.listed = true
	}
	listed := b.listed
	for k, e := range b.entries {
		switch {
		case listed && set[k].key == "" && now.Sub(e.LastSeen) > branchForget:
			bk := e.Status.BranchKey
			delete(b.entries, k)
			b.publish(protocol.Message{Type: protocol.TypeRemove, BranchStatusKey: &bk})
			changed = true
		case !e.Status.Stale && now.Sub(e.Status.FetchedAt) > branchStale:
			e.Status.Stale = true
			st := e.Status
			b.publish(protocol.Message{Type: protocol.TypeUpsert, BranchStatus: &st})
			changed = true
		}
	}
	if changed {
		b.saveLocked()
	}
}

// fetch asks GitHub about the set, host by host, and applies the
// answers.
func (b *branches) fetch(ctx context.Context, set map[string]branchQuery) {
	byHost := map[string][]branchQuery{}
	for _, q := range set {
		byHost[q.host] = append(byHost[q.host], q)
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	// A host no branch is on any more has no failure to say.
	b.mu.Lock()
	for h := range b.hostErrs {
		if _, asked := byHost[h]; !asked {
			delete(b.hostErrs, h)
		}
	}
	b.mu.Unlock()
	// Every host's status first, each within its share of the round,
	// so a slow host starves no other; the failing checks' names after.
	type answer struct {
		qs      []branchQuery
		results []github.Result
	}
	answers := make([]answer, 0, len(hosts))
	// gh's own failures by host, "" for a host that answered.
	hostErrs := map[string]string{}
	b.mu.Lock()
	b.roundErrs = map[string]bool{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.errs, b.roundErrs = b.roundErrs, nil
		b.mu.Unlock()
	}()
	deadline, bounded := ctx.Deadline()
	for n, host := range hosts {
		qs := byHost[host]
		sort.Slice(qs, func(i, j int) bool { return qs[i].key < qs[j].key })
		bs := make([]github.Branch, len(qs))
		b.mu.Lock()
		for i, q := range qs {
			bs[i] = q.b
			if time.Since(b.pagedNone[q.key]) < branchPaging {
				bs[i].NoPaging = true
			}
		}
		b.mu.Unlock()
		hctx, cancel := ctx, context.CancelFunc(func() {})
		if bounded {
			hctx, cancel = context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(hosts)-n))
		}
		results, err := github.Fetch(hctx, b.cfg.GitHub, host, bs)
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
				// The names kept are filled in; none is looked up.
				known := b.knownFailing(a.qs)
				github.FillFailing(ctx, b.cfg.GitHub, a.qs[0].host, a.results, known)
				b.apply(a.qs, a.results, known)
			}
			// The login failures of the hosts asked are said; the hosts
			// not reached keep what was said of them.
			b.publishGitHubErr(hostErrs)
			return
		}
		hostErrs[host] = ""
		if errors.Is(err, github.ErrNoGH) || errors.Is(err, github.ErrLoggedOut) {
			// gh missing, or logged out of a host the config trusts,
			// is the daemon's reason for showing nothing there.
			hostErrs[host] = err.Error()
		}
		answers = append(answers, answer{qs, results})
	}
	for _, a := range answers {
		known := b.knownFailing(a.qs)
		github.FillFailing(ctx, b.cfg.GitHub, a.qs[0].host, a.results, known)
		b.apply(a.qs, a.results, known)
	}
	b.publishGitHubErr(hostErrs)
}

// publishGitHubErr takes the hosts asked this round, with gh's failure
// for each or "", and says the failures of every host, or clears them
// with github_ok. A host not asked keeps its last.
func (b *branches) publishGitHubErr(hostErrs map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hostErrs == nil {
		b.hostErrs = map[string]string{}
	}
	for h, e := range hostErrs {
		if e == "" {
			delete(b.hostErrs, h)
		} else {
			b.hostErrs[h] = e
		}
	}
	var msgs []string
	for _, e := range b.hostErrs {
		msgs = append(msgs, e)
	}
	sort.Strings(msgs)
	msgs = slices.Compact(msgs) // gh missing names no host: once
	ghErr := strings.Join(msgs, "; ")
	switch {
	case ghErr != "" && ghErr != b.githubErr:
		b.githubErr = ghErr
		b.cfg.Logger.Printf("github: %s", ghErr)
		b.publish(protocol.Message{Type: protocol.TypeUpsert, GitHubError: ghErr})
	case ghErr == "" && b.githubErr != "":
		b.githubErr = ""
		b.publish(protocol.Message{Type: protocol.TypeUpsert, GitHubOK: true})
	}
}

// knownFailing is the failing names found for the branches' rollups
// within branchStale: a rollup's failing check can change on the same
// commit, a rerun say, so a name is asked for again once it is old.
func (b *branches) knownFailing(qs []branchQuery) map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	known := map[string]string{}
	now := time.Now().Round(0)
	for _, q := range qs {
		e := b.entries[q.key]
		if e != nil && e.FailingKey != "" && e.Status.Checks != nil && e.Status.Checks.Failing != "" && now.Sub(e.FailingAt) < branchStale {
			known[e.FailingKey] = e.Status.Checks.Failing
		}
	}
	return known
}

// apply takes one host's answers: a branch GitHub does not have drops
// its entry; a failed answer keeps the last one, marked stale; an
// answer upserts the record when a value changed.
func (b *branches) apply(qs []branchQuery, results []github.Result, known map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// The wall clock: a laptop that slept has its answers aged by the
	// sleep too.
	now := time.Now().Round(0)
	for i, q := range qs {
		r := results[i]
		e := b.entries[q.key]
		// The pages held none of the branch's own PR: not paged again
		// for a while, whether the branch is there or not; one found
		// ends that, so a PR pushed off the first page is still found.
		switch {
		case r.PagedNone:
			if b.pagedNone == nil {
				b.pagedNone = map[string]time.Time{}
			}
			b.pagedNone[q.key] = now
		case r.Err == nil && r.PR != nil:
			delete(b.pagedNone, q.key)
		}
		switch {
		case r.NoRef:
			if e != nil {
				delete(b.entries, q.key)
				bk := q.bk
				b.publish(protocol.Message{Type: protocol.TypeRemove, BranchStatusKey: &bk})
			}
		case r.Err != nil:
			// A branch GitHub answered with an error, a repository it
			// cannot resolve or an organisation's SSO say, is logged
			// when the round before did not have it; gh's own failures
			// are github_error's.
			if !errors.Is(r.Err, github.ErrNoGH) && !errors.Is(r.Err, github.ErrLoggedOut) {
				msg := q.host + ": " + r.Err.Error()
				if !b.errs[msg] {
					b.cfg.Logger.Printf("github: %s", msg)
				}
				if b.roundErrs != nil {
					b.roundErrs[msg] = true
				}
			}
			if e != nil && !e.Status.Stale {
				e.Status.Stale = true
				st := e.Status
				b.publish(protocol.Message{Type: protocol.TypeUpsert, BranchStatus: &st})
			}
		default:
			if e == nil {
				e = &branchEntry{}
				b.entries[q.key] = e
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
				b.publish(protocol.Message{Type: protocol.TypeUpsert, BranchStatus: &st})
			}
		}
	}
	b.saveLocked()
}

// sameBranchStatus compares two records but for when they were fetched.
func sameBranchStatus(a, b protocol.BranchStatus) bool {
	a.FetchedAt, b.FetchedAt = time.Time{}, time.Time{}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// statusesLocked is every kept record, for a merged snapshot.
func (b *branches) statusesLocked() []protocol.BranchStatus {
	out := make([]protocol.BranchStatus, 0, len(b.entries))
	for _, e := range b.entries {
		out = append(out, e.Status)
	}
	sort.Slice(out, func(i, j int) bool {
		return branchKeyString(out[i].BranchKey) < branchKeyString(out[j].BranchKey)
	})
	return out
}
