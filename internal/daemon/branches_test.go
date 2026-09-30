package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/github"
	"github.com/laat/laatmux/internal/protocol"
)

// fakeGH answers a branch query with a state per branch name: a PR with
// checks in that state, "gone" for no ref, "fail" for no answer; err
// fails the whole call.
type fakeGH struct {
	mu     sync.Mutex
	states map[string]string
	err    error
	calls  int
}

func (f *fakeGH) run(ctx context.Context, host, query string, vars map[string]string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	data := map[string]any{}
	for k, v := range vars {
		if !strings.HasPrefix(k, "b") {
			continue
		}
		switch st := f.states[v]; st {
		case "gone":
			data[k] = map[string]any{"url": "u", "ref": nil, "pullRequests": map[string]any{"nodes": []any{}}}
		case "fail":
			data[k] = nil
		default:
			data[k] = map[string]any{"url": "https://github.com/o/r", "ref": map[string]any{"target": map[string]any{"oid": "h-" + v,
				"statusCheckRollup": map[string]any{"id": "R", "state": st, "contexts": map[string]any{
					"checkRunCountsByState": []any{map[string]any{"state": st, "count": 1}}, "statusContextCountsByState": []any{}}}}},
				"pullRequests": map[string]any{"nodes": []any{}}}
		}
	}
	return json.Marshal(map[string]any{"data": data})
}

const ghSource = "git@github.com:o/r.git"

// branchDaemon is a merging daemon with a remote host vm listing
// worktrees of the branches given, and a merged subscriber.
func branchDaemon(t *testing.T, dir string, gh *fakeGH, branches ...string) (*Daemon, *subscriber) {
	t.Helper()
	hosts := &hostsList{hosts: []client.Host{{Name: "vm", SSH: "vm"}}}
	d := New(Config{EnvironmentID: "menv", Hosts: hosts.get, GitHub: gh.run, Branches: filepath.Join(dir, "branches.json")})
	s := &subscriber{ch: make(chan protocol.Message, 256), merged: true}
	d.mu.Lock()
	d.reconcileHostsLocked(hosts.hosts)
	for _, b := range branches {
		root := "/w/" + b
		d.mhosts["vm"].worktrees["venv/worktree/"+root] = protocol.Worktree{ID: "venv/worktree/" + root, EnvironmentID: "venv", Source: ghSource, Branch: b, Root: root}
	}
	d.msubs[s] = struct{}{}
	d.mu.Unlock()
	return d, s
}

func (d *Daemon) fetchNow(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	set := d.branchSetLocked()
	d.mu.Unlock()
	d.fetchBranches(context.Background(), set)
}

func drainBranches(s *subscriber) (ups []protocol.BranchStatus, removes []protocol.BranchKey, msgs []protocol.Message) {
	for {
		select {
		case m := <-s.ch:
			msgs = append(msgs, m)
			if m.BranchStatus != nil {
				ups = append(ups, *m.BranchStatus)
			}
			if m.BranchStatusKey != nil {
				removes = append(removes, *m.BranchStatusKey)
			}
		default:
			return
		}
	}
}

func bkey(branch string) protocol.BranchKey {
	return protocol.BranchKey{Source: config.SourceKey(ghSource), Branch: branch}
}

// A fetch upserts each branch's record, keyed by the source key; one
// that changes nothing upserts nothing; pending_since holds for a head
// and restarts for a new one; a branch GitHub does not have is removed;
// one without an answer keeps its record, stale.
func TestBranchesFetch(t *testing.T) {
	gh := &fakeGH{states: map[string]string{"a": "SUCCESS", "b": "IN_PROGRESS", "c": "FAILURE"}}
	d, s := branchDaemon(t, t.TempDir(), gh, "a", "b", "c")
	d.fetchNow(t)
	ups, _, _ := drainBranches(s)
	if len(ups) != 3 {
		t.Fatalf("upserts: %+v", ups)
	}
	var since time.Time
	for _, u := range ups {
		if u.BranchKey == bkey("b") {
			since = u.Checks.PendingSince
		}
	}
	if since.IsZero() {
		t.Fatal("pending without pending_since")
	}
	d.fetchNow(t)
	if ups, _, _ := drainBranches(s); len(ups) != 0 {
		t.Errorf("an unchanged fetch upserted: %+v", ups)
	}

	gh.mu.Lock()
	gh.states["a"], gh.states["c"] = "gone", "fail"
	gh.mu.Unlock()
	d.fetchNow(t)
	ups, removes, _ := drainBranches(s)
	if len(removes) != 1 || removes[0] != bkey("a") {
		t.Errorf("removes: %+v", removes)
	}
	if len(ups) != 1 || ups[0].BranchKey != bkey("c") || !ups[0].Stale || ups[0].Checks.State != protocol.ChecksFailure {
		t.Errorf("a failed answer: %+v", ups)
	}
	d.mu.Lock()
	b := d.branches[branchKeyString(bkey("b"))]
	d.mu.Unlock()
	if !b.Status.Checks.PendingSince.Equal(since) {
		t.Error("pending_since moved for the same head")
	}
}

// gh missing or logged out is the daemon's github_error, in the snapshot
// and an upsert, cleared by github_ok; the records it had stay, stale.
func TestBranchesGitHubError(t *testing.T) {
	gh := &fakeGH{states: map[string]string{"a": "SUCCESS"}}
	d, s := branchDaemon(t, t.TempDir(), gh, "a")
	d.fetchNow(t)
	drainBranches(s)
	gh.mu.Lock()
	gh.err = github.ErrNoGH
	gh.mu.Unlock()
	d.fetchNow(t)
	ups, _, msgs := drainBranches(s)
	sawErr := false
	for _, m := range msgs {
		sawErr = sawErr || m.GitHubError == github.ErrNoGH.Error()
	}
	if !sawErr || len(ups) != 1 || !ups[0].Stale {
		t.Errorf("error: %+v", msgs)
	}
	d.mu.Lock()
	snap := d.mergedSnapshotLocked()
	d.mu.Unlock()
	if snap.GitHubError == "" || len(snap.BranchStatuses) != 1 {
		t.Errorf("snapshot: %q %+v", snap.GitHubError, snap.BranchStatuses)
	}
	gh.mu.Lock()
	gh.err = nil
	gh.mu.Unlock()
	d.fetchNow(t)
	_, _, msgs = drainBranches(s)
	cleared := false
	for _, m := range msgs {
		cleared = cleared || m.GitHubOK
	}
	if !cleared {
		t.Error("github_ok not sent")
	}
}

// The answers outlive the daemon; past five minutes they are stale; a
// branch no worktree has had for a day is dropped; a source that is not
// on a forge, or a detached worktree, is not asked about.
func TestBranchesAgeAndKeep(t *testing.T) {
	dir := t.TempDir()
	gh := &fakeGH{states: map[string]string{"a": "SUCCESS"}}
	d, _ := branchDaemon(t, dir, gh, "a")
	d.mu.Lock()
	d.mhosts["vm"].worktrees["venv/worktree//w/x"] = protocol.Worktree{ID: "venv/worktree//w/x", EnvironmentID: "venv", Source: "/srv/repo.git", Branch: "x", Root: "/w/x"}
	d.mhosts["vm"].worktrees["venv/worktree//w/d"] = protocol.Worktree{ID: "venv/worktree//w/d", EnvironmentID: "venv", Source: ghSource, Root: "/w/d"}
	set := d.branchSetLocked()
	d.mu.Unlock()
	if len(set) != 1 {
		t.Fatalf("set: %+v", set)
	}
	d.fetchNow(t)

	d2, s2 := branchDaemon(t, dir, gh)
	d2.mu.Lock()
	snap := d2.mergedSnapshotLocked()
	d2.ageBranchesLocked(map[string]branchQuery{}, time.Now().Add(6*time.Minute))
	e := d2.branches[branchKeyString(bkey("a"))]
	stale := e != nil && e.Status.Stale
	// Before the hosts have listed, an empty set is no proof of
	// absence: kept however old.
	d2.ageBranchesLocked(map[string]branchQuery{}, time.Now().Add(25*time.Hour))
	_, kept := d2.branches[branchKeyString(bkey("a"))]
	// A snapshot from a host whose git listing failed is no listing.
	d2.mu.Unlock()
	d2.applyRemote(context.Background(), d2.mhosts["vm"], protocol.Message{Type: protocol.TypeSnapshot, ListingError: "git failed"})
	d2.mu.Lock()
	d2.ageBranchesLocked(map[string]branchQuery{}, time.Now().Add(25*time.Hour))
	if _, ok := d2.branches[branchKeyString(bkey("a"))]; !ok {
		t.Error("dropped after a failed listing")
	}
	d2.mu.Unlock()
	d2.applyRemote(context.Background(), d2.mhosts["vm"], protocol.Message{Type: protocol.TypeSnapshot, Listing: &protocol.Listing{Generation: 1}})
	d2.mu.Lock()
	d2.ageBranchesLocked(map[string]branchQuery{}, time.Now().Add(25*time.Hour))
	_, still := d2.branches[branchKeyString(bkey("a"))]
	d2.mu.Unlock()
	if len(snap.BranchStatuses) != 1 || !stale || !kept || still {
		t.Errorf("kept %d, stale %v, kept before the listing %v, still there after a day %v", len(snap.BranchStatuses), stale, kept, still)
	}
	if _, removes, _ := drainBranches(s2); len(removes) != 1 {
		t.Errorf("removes after a day: %+v", removes)
	}
}

// A client from before branches passes over the records: its envelope
// has no keys for them, and a remove of one names nothing it knows.
func TestBranchesOldEnvelope(t *testing.T) {
	type oldMessage struct {
		Type       string `json:"type"`
		AgentID    string `json:"agent_id,omitempty"`
		WorktreeID string `json:"worktree_id,omitempty"`
		Branch     string `json:"branch,omitempty"`
		HostName   string `json:"host_name,omitempty"`
	}
	k := bkey("a")
	for _, m := range []protocol.Message{
		{Type: protocol.TypeSnapshot, BranchStatuses: []protocol.BranchStatus{{BranchKey: k}}, GitHubError: "x"},
		{Type: protocol.TypeUpsert, BranchStatus: &protocol.BranchStatus{BranchKey: k}},
		{Type: protocol.TypeRemove, BranchStatusKey: &k},
		{Type: protocol.TypeUpsert, GitHubOK: true},
	} {
		b, _ := json.Marshal(m)
		var old oldMessage
		if err := json.Unmarshal(b, &old); err != nil || old.AgentID != "" || old.WorktreeID != "" || old.Branch != "" || old.HostName != "" {
			t.Errorf("%s: %+v %v", b, old, err)
		}
	}
}

// Only github.com and the hosts the config trusts are asked about: a
// source on another host never reaches gh, whose token would go there.
func TestBranchesHostAllowList(t *testing.T) {
	gh := &fakeGH{states: map[string]string{}}
	d, _ := branchDaemon(t, t.TempDir(), gh)
	d.mu.Lock()
	for i, src := range []string{"git@gitlab.com:o/r.git", "git@ghe.example.com:o/r.git", "https://github.com/o/r"} {
		root := fmt.Sprintf("/w/%d", i)
		d.mhosts["vm"].worktrees["venv/worktree/"+root] = protocol.Worktree{ID: "venv/worktree/" + root, EnvironmentID: "venv", Source: src, Branch: "b", Root: root}
	}
	set := d.branchSetLocked()
	d.cfg.GitHubHosts = []string{"GHE.example.com"}
	trusted := d.branchSetLocked()
	d.mu.Unlock()
	if len(set) != 1 || len(trusted) != 2 {
		t.Errorf("github.com alone: %d; with the enterprise host: %d", len(set), len(trusted))
	}
	for _, q := range set {
		if q.host != "github.com" {
			t.Errorf("asked %s", q.host)
		}
	}
}

// A round of queries that waits on GitHub does not hold the aging: the
// loop marks answers stale while the round runs.
func TestBranchesSlowRound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	block := make(chan struct{})
	gh := &fakeGH{states: map[string]string{"a": "SUCCESS"}}
	d, _ := branchDaemon(t, t.TempDir(), gh, "a")
	d.fetchNow(t)
	slow := func(ctx context.Context, host, q string, vars map[string]string) ([]byte, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil, errors.New("slow")
	}
	d.mu.Lock()
	d.cfg.GitHub = slow
	// Stale a little after the first tick, which starts the round: only
	// a loop not held by the round marks it.
	for _, e := range d.branches {
		e.Status.FetchedAt = time.Now().Add(-branchStale + branchTick + branchTick/2)
	}
	d.mu.Unlock()
	exited := make(chan struct{})
	go func() {
		d.runBranches(ctx)
		close(exited)
	}()
	// The loop, and the round it waits on, end before the test does:
	// the round writes into the test's directory.
	defer func() {
		cancel()
		<-exited
	}()
	deadline := time.Now().Add(4 * branchTick)
	for {
		d.mu.Lock()
		stale := d.branches[branchKeyString(bkey("a"))].Status.Stale
		d.mu.Unlock()
		if stale {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not marked stale while a round waited")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(block)
}

// An answer kept from before a restart is as old as it is: stale at once
// when past the stale time.
func TestBranchesStaleAfterRestart(t *testing.T) {
	dir := t.TempDir()
	gh := &fakeGH{states: map[string]string{"a": "SUCCESS"}}
	d, _ := branchDaemon(t, dir, gh, "a")
	d.fetchNow(t)
	d.mu.Lock()
	for _, e := range d.branches {
		e.Status.FetchedAt = time.Now().Add(-time.Hour)
	}
	d.saveBranchesLocked()
	d.mu.Unlock()
	d2, _ := branchDaemon(t, dir, gh)
	d2.mu.Lock()
	snap := d2.mergedSnapshotLocked()
	d2.mu.Unlock()
	if len(snap.BranchStatuses) != 1 || !snap.BranchStatuses[0].Stale {
		t.Errorf("%+v", snap.BranchStatuses)
	}
}

// A failing check's name is asked for again once it is old: on the same
// rollup a rerun can move the failure to another check.
func TestBranchesFailingNameLifetime(t *testing.T) {
	d, _ := branchDaemon(t, t.TempDir(), &fakeGH{})
	k := branchKeyString(bkey("a"))
	d.mu.Lock()
	d.branches[k] = &branchEntry{FailingKey: "R x", FailingAt: time.Now().Add(-time.Minute),
		Status: protocol.BranchStatus{BranchKey: bkey("a"), Checks: &protocol.Checks{State: protocol.ChecksFailure, Failing: "lint"}}}
	d.mu.Unlock()
	q := []branchQuery{{key: k}}
	if known := d.knownFailing(q); known["R x"] != "lint" {
		t.Errorf("a recent name not known: %v", known)
	}
	d.mu.Lock()
	d.branches[k].FailingAt = time.Now().Add(-2 * branchStale)
	d.mu.Unlock()
	if known := d.knownFailing(q); len(known) != 0 {
		t.Errorf("an old name still known: %v", known)
	}
}

// A slow host holds only its share of the round: the next host is still
// asked, and answered.
func TestBranchesSlowHostShare(t *testing.T) {
	gh := &fakeGH{states: map[string]string{"b": "SUCCESS"}}
	d, s := branchDaemon(t, t.TempDir(), gh)
	d.mu.Lock()
	d.cfg.GitHubHosts = []string{"aaa.example.com"}
	d.mhosts["vm"].worktrees["venv/worktree//w/a"] = protocol.Worktree{ID: "venv/worktree//w/a", EnvironmentID: "venv", Source: "git@aaa.example.com:o/r.git", Branch: "a", Root: "/w/a"}
	d.mhosts["vm"].worktrees["venv/worktree//w/b"] = protocol.Worktree{ID: "venv/worktree//w/b", EnvironmentID: "venv", Source: ghSource, Branch: "b", Root: "/w/b"}
	fast := d.cfg.GitHub
	d.cfg.GitHub = func(ctx context.Context, host, q string, vars map[string]string) ([]byte, error) {
		if host == "aaa.example.com" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return fast(ctx, host, q, vars)
	}
	set := d.branchSetLocked()
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	d.fetchBranches(ctx, set)
	ups, _, _ := drainBranches(s)
	if len(ups) != 1 || ups[0].BranchKey != bkey("b") {
		t.Errorf("the second host: %+v", ups)
	}
}

// A round that runs out on its last host still applies what the hosts
// before it answered.
func TestBranchesSlowHostLast(t *testing.T) {
	gh := &fakeGH{states: map[string]string{"b": "SUCCESS"}}
	d, s := branchDaemon(t, t.TempDir(), gh)
	d.mu.Lock()
	d.cfg.GitHubHosts = []string{"zzz.example.com"}
	d.mhosts["vm"].worktrees["venv/worktree//w/z"] = protocol.Worktree{ID: "venv/worktree//w/z", EnvironmentID: "venv", Source: "git@zzz.example.com:o/r.git", Branch: "z", Root: "/w/z"}
	d.mhosts["vm"].worktrees["venv/worktree//w/b"] = protocol.Worktree{ID: "venv/worktree//w/b", EnvironmentID: "venv", Source: ghSource, Branch: "b", Root: "/w/b"}
	fast := d.cfg.GitHub
	d.cfg.GitHub = func(ctx context.Context, host, q string, vars map[string]string) ([]byte, error) {
		if host == "zzz.example.com" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return fast(ctx, host, q, vars)
	}
	set := d.branchSetLocked()
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	d.fetchBranches(ctx, set)
	ups, _, _ := drainBranches(s)
	if len(ups) != 1 || ups[0].BranchKey != bkey("b") {
		t.Errorf("the host answered before the slow one: %+v", ups)
	}
	// A login failure on the host answered first is said though the
	// round ran out on the last.
	gh.mu.Lock()
	gh.err = fmt.Errorf("%w to github.com", github.ErrLoggedOut)
	gh.mu.Unlock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	d.fetchBranches(ctx2, set)
	_, _, msgs := drainBranches(s)
	said := false
	for _, m := range msgs {
		said = said || strings.Contains(m.GitHubError, "not logged in")
	}
	if !said {
		t.Errorf("the login failure was not said: %+v", msgs)
	}
}

// A host with no worktrees to list, a daemon without the capability,
// does not hold the forgetting for the others.
func TestBranchesListedWithoutWorktrees(t *testing.T) {
	hosts := &hostsList{hosts: []client.Host{{Name: "vm", SSH: "vm"}, {Name: "old", SSH: "old"}}}
	d := New(Config{EnvironmentID: "menv", Hosts: hosts.get, GitHub: (&fakeGH{}).run, Branches: filepath.Join(t.TempDir(), "b.json")})
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reconcileHostsLocked(hosts.hosts)
	d.mhosts["vm"].listed = true
	d.mhosts["old"].status.Connected, d.mhosts["old"].status.Capabilities = true, []string{protocol.CapStatus}
	if !d.hostsListedLocked() {
		t.Error("a host without worktrees holds the forgetting")
	}
	d.mhosts["old"].status.Capabilities = append(d.mhosts["old"].status.Capabilities, protocol.CapWorktrees)
	if d.hostsListedLocked() {
		t.Error("a host with worktrees not yet listed does not hold it")
	}
}

// A round that runs out of time keeps the failing names it knows, and a
// branch whose forks' pages held nothing of its own is not paged again
// within the hour.
func TestBranchesTimeoutKeepsNames(t *testing.T) {
	gh := &fakeGH{states: map[string]string{"c": "FAILURE"}}
	d, s := branchDaemon(t, t.TempDir(), gh, "c")
	d.fetchNow(t)
	drainBranches(s)
	d.mu.Lock()
	e := d.branches[branchKeyString(bkey("c"))]
	e.Status.Checks.Failing, e.FailingAt = "lint", time.Now()
	d.pagedNone = map[string]time.Time{branchKeyString(bkey("c")): time.Now()}
	d.mu.Unlock()
	var paging bool
	slow := func(ctx context.Context, host, q string, vars map[string]string) ([]byte, error) {
		if b, ok := vars["b0"]; ok && b == "c" {
			// The status answers; then the deadline passes.
			out, err := gh.run(ctx, host, q, vars)
			<-ctx.Done()
			return out, err
		}
		paging = true
		return nil, ctx.Err()
	}
	d.mu.Lock()
	d.cfg.GitHub = slow
	set := d.branchSetLocked()
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	d.fetchBranches(ctx, set)
	d.mu.Lock()
	failing := d.branches[branchKeyString(bkey("c"))].Status.Checks.Failing
	d.mu.Unlock()
	if failing != "lint" || paging {
		t.Errorf("failing %q, paged %v", failing, paging)
	}
}

// The paging memory: kept for a branch GitHub does not have, so an
// unpushed patch-1 is not paged every round; ended by a PR found, so one
// pushed off the first page later is still paged for.
func TestBranchesPagingMemory(t *testing.T) {
	d, _ := branchDaemon(t, t.TempDir(), &fakeGH{})
	q := []branchQuery{{key: "k", bk: bkey("patch-1"), host: "github.com"}}
	d.applyBranches(q, []github.Result{{NoRef: true, PagedNone: true}}, nil)
	d.mu.Lock()
	_, kept := d.pagedNone["k"]
	d.mu.Unlock()
	if !kept {
		t.Fatal("no ref lost the paging memory")
	}
	d.applyBranches(q, []github.Result{{HeadOID: "h", PR: &protocol.PullRequest{Number: 1, State: "open"}}}, nil)
	d.mu.Lock()
	_, still := d.pagedNone["k"]
	d.mu.Unlock()
	if still {
		t.Error("a PR found did not end the paging memory")
	}
}

// A round that runs out before a host with a login failure said before
// keeps that failure said, while a host asked and fine clears its own.
func TestBranchesHostErrorsKept(t *testing.T) {
	d, s := branchDaemon(t, t.TempDir(), &fakeGH{})
	d.publishGitHubErr(map[string]string{"github.com": "gh is not logged in to github.com", "ghe.example.com": "gh is not logged in to ghe.example.com"})
	d.publishGitHubErr(map[string]string{"github.com": ""})
	_, _, msgs := drainBranches(s)
	last := ""
	for _, m := range msgs {
		if m.GitHubError != "" {
			last = m.GitHubError
		}
	}
	if last != "gh is not logged in to ghe.example.com" {
		t.Errorf("after github.com answered: %q", last)
	}
}
