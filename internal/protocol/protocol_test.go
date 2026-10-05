package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

// An attention record travels under keys of its own: a client from
// before attention decodes the messages as nothing it knows, and passes
// over them; a remove of one is not an agent's.
func TestAttentionOldEnvelope(t *testing.T) {
	// The envelope's record keys as they were before attention.
	type oldMessage struct {
		Type       string     `json:"type"`
		Agents     []Agent    `json:"agents,omitempty"`
		Agent      *Agent     `json:"agent,omitempty"`
		AgentID    string     `json:"agent_id,omitempty"`
		Worktrees  []Worktree `json:"worktrees,omitempty"`
		Worktree   *Worktree  `json:"worktree,omitempty"`
		WorktreeID string     `json:"worktree_id,omitempty"`
		PendingID  string     `json:"pending_id,omitempty"`
		HostName   string     `json:"host_name,omitempty"`
	}
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for _, m := range []Message{
		{Type: TypeSnapshot, Attentions: []Attention{{AgentID: "e/laatmux/%1", FinishedAt: at}}},
		{Type: TypeUpsert, Attention: &Attention{AgentID: "e/laatmux/%1", FinishedAt: at, SeenAt: at}},
		{Type: TypeRemove, AttentionID: "e/laatmux/%1"},
	} {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var old oldMessage
		if err := json.Unmarshal(b, &old); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if old.Agents != nil || old.Agent != nil || old.AgentID != "" || old.Worktrees != nil || old.Worktree != nil ||
			old.WorktreeID != "" || old.PendingID != "" || old.HostName != "" {
			t.Errorf("%s decodes as a record an older client knows: %+v", b, old)
		}
	}
}

// The git object is a field of the worktree record: a client from before
// git-status decodes the record as it knew it, and a merging daemon from
// before drops the object as it re-encodes the record.
func TestGitStatusOldEnvelope(t *testing.T) {
	type oldWorktree struct {
		ID      string `json:"id"`
		Branch  string `json:"branch"`
		Root    string `json:"root"`
		Session string `json:"session,omitempty"`
	}
	type oldMessage struct {
		Type     string       `json:"type"`
		Worktree *oldWorktree `json:"worktree,omitempty"`
	}
	no := false
	m := Message{Type: TypeUpsert, Worktree: &Worktree{ID: "e/worktree//w", Branch: "b", Root: "/w",
		Git: &GitStatus{Base: "origin/main", Committed: [2]int{1, 2}, Conflict: &no}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var old oldMessage
	if err := json.Unmarshal(b, &old); err != nil || old.Worktree == nil || old.Worktree.Root != "/w" {
		t.Fatalf("%s: %+v %v", b, old, err)
	}
	re, _ := json.Marshal(old)
	var back Message
	if err := json.Unmarshal(re, &back); err != nil || back.Worktree == nil || back.Worktree.Git != nil {
		t.Errorf("forwarded by an older merging daemon: %s", re)
	}
	// Same ignores ChangedAt and compares the conflict's value.
	yes, no2 := true, false
	a := GitStatus{Conflict: &yes, ChangedAt: time.Now()}
	if !a.Same(GitStatus{Conflict: &yes}) || a.Same(GitStatus{Conflict: &no2}) || a.Same(GitStatus{}) {
		t.Error("Same")
	}
}

// A session key is the environment id and the root, and parses from
// the left: the root may hold a slash, the id never does.
func TestSessionKeyRoundTrip(t *testing.T) {
	key := SessionKey("3fa9c1d2e4b5a6f7", "/home/u/src/worktrees/proj/fix/v1.2")
	env, root := SplitSessionKey(key)
	if env != "3fa9c1d2e4b5a6f7" || root != "/home/u/src/worktrees/proj/fix/v1.2" {
		t.Fatalf("SplitSessionKey(%q) = %q, %q", key, env, root)
	}
	if !(Session{Key: key}).Workspace() || !(Session{Key: key}).Laatmux() || (Session{Attach: "vm/proj/x"}).Workspace() || !(Session{Attach: "vm/proj/x"}).Laatmux() || (Session{Name: "notes"}).Laatmux() {
		t.Fatal("a workspace has a key; laatmux's sessions have a key or an attach tag")
	}
}
