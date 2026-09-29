package daemon

import (
	"context"
	"encoding/json"
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
	d2.ageBranchesLocked(map[string]branchQuery{}, time.Now().Add(25*time.Hour))
	_, gone := d2.branches[branchKeyString(bkey("a"))]
	d2.mu.Unlock()
	if len(snap.BranchStatuses) != 1 || !stale || gone {
		t.Errorf("kept %d, stale %v, still there after a day %v", len(snap.BranchStatuses), stale, gone)
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
