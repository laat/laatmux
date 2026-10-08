package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
)

// A foreground add of a repository new to the config appends it to the
// config's repos once the host's add has succeeded, and says so; one
// the host refuses leaves the file as it was, and so does an add of a
// listed repository.
func TestRunRemembers(t *testing.T) {
	caps := []string{protocol.CapStatus, protocol.CapAdd, protocol.CapFollow}
	f := startFake(t, 0, protocol.Message{EnvironmentID: "env", Capabilities: caps})
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("LAATMUX_CONFIG", cfgPath)
	const before = "# mine\nrepos:\n  - git@x:o/a.git\n"
	if err := os.WriteFile(cfgPath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	add := Add{Host: config.Host{Host: peer.Host{Name: "local"}}, Repo: config.Repo{Source: "git@x:o/p.git", Name: "p"}, Branch: "x", Agent: "claude", Remember: true}
	f.answer = func(m protocol.Message) protocol.Message {
		return protocol.Message{Type: protocol.TypeResult, ID: m.ID, Stage: protocol.StageWorktree, Error: "branch x is checked out in the main checkout /r/p"}
	}
	if _, err := add.Run(context.Background(), Discard{}); err == nil {
		t.Fatal("a refused add succeeded")
	}
	if b, _ := os.ReadFile(cfgPath); string(b) != before {
		t.Fatalf("a refused add changed the config:\n%s", b)
	}
	f.answer = nil
	var notes []string
	add.Run(context.Background(), noter{fn: func(s string) { notes = append(notes, s) }})
	if b, _ := os.ReadFile(cfgPath); string(b) != before+"  - git@x:o/p.git\n" {
		t.Fatalf("config after the add:\n%s", b)
	}
	if len(notes) == 0 || notes[0] != "git@x:o/p.git added to the config's repos as p" {
		t.Fatalf("notes %q", notes)
	}
	add.Remember = false
	add.Repo = config.Repo{Source: "git@x:o/q.git", Name: "q"}
	add.Run(context.Background(), Discard{})
	if b, _ := os.ReadFile(cfgPath); strings.Contains(string(b), "q.git") {
		t.Fatalf("an add without remember changed the config:\n%s", b)
	}
	// The name the host placed the checkout under is the name appended,
	// or none: a repository of the same last element listed since the
	// name was chosen takes it, and the append fails, never listing the
	// source under another name than its directory's.
	const named = "repos:\n  - git@x:laat/scripts.git\n"
	if err := os.WriteFile(cfgPath, []byte(named), 0o600); err != nil {
		t.Fatal(err)
	}
	notes = nil
	add.Remember, add.Repo = true, config.Repo{Source: "git@x:nrkno/scripts.git", Name: "scripts"}
	add.Run(context.Background(), noter{fn: func(s string) { notes = append(notes, s) }})
	if b, _ := os.ReadFile(cfgPath); string(b) != named {
		t.Fatalf("appended under another name:\n%s", b)
	}
	if len(notes) == 0 || !strings.HasPrefix(notes[0], "git@x:nrkno/scripts.git not added to the config's repos: ") || !strings.Contains(notes[0], "both get the name scripts") {
		t.Fatalf("notes %q", notes)
	}
}

// Submit sends remember with the add to a daemon that has the
// capability, with the entry the daemon appends; one without it is
// refused before anything is sent, naming the older build.
func TestSubmitRemember(t *testing.T) {
	caps := []string{protocol.CapStatus, protocol.CapMerged, protocol.CapRelay}
	add := Add{Host: config.Host{Host: peer.Host{Name: "vm", SSH: "vm"}}, Repo: config.Repo{Source: "git@x:o/p.git", Name: "p"}, Branch: "task", Agent: "claude", Remember: true}
	f := startFake(t, 0, protocol.Message{EnvironmentID: "env", Capabilities: caps})
	if _, err := add.Submit(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot add git@x:o/p.git to the config's repos, an older build") {
		t.Fatalf("without remember: %v", err)
	}
	if got := f.commands(); len(got) != 0 {
		t.Fatalf("sent without remember: %+v", got)
	}
	f = startFake(t, 0, protocol.Message{EnvironmentID: "env", Capabilities: append(caps, protocol.CapRemember)})
	if _, err := add.Submit(context.Background()); err != nil {
		t.Fatal(err)
	}
	add.Remember = false
	if _, err := add.Submit(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.commands()
	if len(got) != 2 || !got[0].Remember || got[0].RepoEntry == nil || got[0].RepoEntry.Source != "git@x:o/p.git" || got[0].RepoEntry.Name != "p" || got[1].Remember {
		t.Fatalf("commands %+v", got)
	}
}

// Dismiss passes on what the daemon says went with the record, the
// append of a repository new to the config, for the dismiss's message;
// a refusal says nothing dropped.
func TestDismissDropped(t *testing.T) {
	f := startFake(t, 0, protocol.Message{EnvironmentID: "env", Capabilities: []string{protocol.CapStatus, protocol.CapMerged, protocol.CapRelay}})
	const dropped = "the append of git@x:o/p.git to the config's repos as p is dropped (yaml: bad); add it to the config by hand, or paste the source again"
	f.answer = func(m protocol.Message) protocol.Message {
		if m.ID == "running" {
			return protocol.Message{Type: protocol.TypeResult, ID: m.ID, Error: "the add is still running"}
		}
		return protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Detail: dropped}
	}
	if got, err := Dismiss(context.Background(), "held"); err != nil || got != dropped {
		t.Fatalf("dismiss: %q %v", got, err)
	}
	if got, err := Dismiss(context.Background(), "running"); err == nil || got != "" {
		t.Fatalf("refused: %q %v", got, err)
	}
}
