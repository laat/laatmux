package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
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

// A root with a byte tmux 3.4 prints escaped, a tab, a newline, a C1
// control character, a U+2063 or a % is encoded after a % in its key,
// so the key has none of them and parses back to the root; any other
// root is kept as given after a /.
// A key with a / is never decoded, so a key an earlier build wrote for
// a root with a % in it still gives that root.
func TestSessionKeyEncodesRoot(t *testing.T) {
	for root, want := range map[string]string{
		"/w/proj/fix/v1.2":                     "env//w/proj/fix/v1.2",
		`/w/a\b$x#y;`:                          `env//w/a\b$x#y;`,
		"/w/bl\u00e5b\u00e6r/\U0001f600\ufffd": "env//w/bl\u00e5b\u00e6r/\U0001f600\ufffd",
		"/w/a\x01b":                            "env%/w/a%01b",
		"/w/tab\tx":                            "env%/w/tab%09x",
		"/w/nl\nx":                             "env%/w/nl%0ax",
		"/w/del\x7f":                           "env%/w/del%7f",
		"/w/a\xffb":                            "env%/w/a%ffb",
		"/w/c1\u0085x":                         "env%/w/c1%c2%85x",
		"/w/sep\u2063\u2063x":                  "env%/w/sep%e2%81%a3%e2%81%a3x",
		"/w/100%":                              "env%/w/100%25",
		"/w/a%01b":                             "env%/w/a%2501b",
		"/w/\xe2\x82":                          "env%/w/%e2%82",
		"/w/nb\u00a0x\u2064y\u2028":            "env//w/nb\u00a0x\u2064y\u2028",
	} {
		key := SessionKey("env", root)
		if key != want {
			t.Errorf("SessionKey(%q) = %q, want %q", root, key, want)
		}
		if env, back := SplitSessionKey(key); env != "env" || back != root {
			t.Errorf("SplitSessionKey(%q) = %q, %q, want %q", key, env, back, root)
		}
	}
	// Every byte alone, and every code point, round trips through a key
	// that has no control character, no byte that is not UTF-8 and no
	// U+2063.
	check := func(root string) {
		key := SessionKey("env", root)
		if env, back := SplitSessionKey(key); env != "env" || back != root {
			t.Errorf("SessionKey(%q) = %q, split %q %q", root, key, env, back)
		}
		if !utf8.ValidString(key) || strings.IndexFunc(key, func(r rune) bool { return unicode.IsControl(r) || r == '\u2063' }) >= 0 {
			t.Errorf("SessionKey(%q) = %q", root, key)
		}
	}
	for c := 0; c < 256; c++ {
		check("/w/" + string([]byte{byte(c)}))
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		check("/w/" + string(r))
	}
	// A key with a / has its root as given, a %01 in it too; one with a
	// % has it decoded, a % before no two hex digits kept.
	for key, root := range map[string]string{
		"env//w/a%01b":  "/w/a%01b",
		"env//w/a\x01b": "/w/a\x01b",
		"env%/w/a%01b":  "/w/a\x01b",
		"env%/w/a%1":    "/w/a%1",
		"env%/w/a%zzb":  "/w/a%zzb",
		"env%/w/a%FFb":  "/w/a\xffb",
		"env":           "",
	} {
		env, back := SplitSessionKey(key)
		if back != root || env != "env" {
			t.Errorf("SplitSessionKey(%q) = %q, %q, want %q", key, env, back, root)
		}
	}
}
