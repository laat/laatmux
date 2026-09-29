package daemon

import (
	"context"
	"encoding/json"
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
	d2.mhosts["vm"].status.Listed = true
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
		return nil, ctx.Err()
	}
	d.mu.Lock()
	d.cfg.GitHub = slow
	// Stale a little after the first tick, which starts the round: only
	// a loop not held by the round marks it.
	for _, e := range d.branches {
		e.Status.FetchedAt = time.Now().Add(-branchStale + branchTick + branchTick/2)
	}
	d.mu.Unlock()
	go d.runBranches(ctx)
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
