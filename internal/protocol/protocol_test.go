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
