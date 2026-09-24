package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
	"github.com/laat/laatmux/internal/worktree"
)

// idleScreen is a claude pane with its prompt box on screen: the
// detector says idle, visible.
var idleScreen = []string{
	"⏺ Ready.",
	"",
	"───────────────────────────────────────────────────────────",
	"❯ ",
	"───────────────────────────────────────────────────────────",
	"  F 5.1 laatmux (main) │ ctx 5%",
}

// taskDaemon is an add daemon whose managed server shows screen in
// every pane and whose process table has a verified claude, polled in
// the background so a delivery sees fresh observations.
func taskDaemon(t *testing.T, screen []string, agents map[string][]string) (*Daemon, *fakeServer, *worktree.Store, string) {
	t.Helper()
	store, remote := newStore(t)
	ft := &fakeServer{screen: screen}
	if agents == nil {
		agents = map[string][]string{"claude": {"claude"}}
	}
	d := New(Config{
		EnvironmentID: "env", Host: "box",
		Targets: []Target{{Label: "laatmux", Tmux: ft, Managed: true}},
		Procs:   &fakeProcs{tables: []procTable{{procs: []procs.Proc{shell, claude}}}},
		Store:   store, Agents: agents,
		Commands: t.TempDir(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	t.Cleanup(func() { cancel(); <-stopped })
	go func() {
		defer close(stopped)
		for ctx.Err() == nil {
			if d.poll(ctx) == nil {
				d.markDiscovered(&d.panesDiscovered)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return d, ft, store, remote
}

func shortWait(t *testing.T, d time.Duration) {
	t.Helper()
	was := readyWait
	readyWait = d
	t.Cleanup(func() { readyWait = was })
}

// readEntry reads the journal file for id.
func readEntry(t *testing.T, d *Daemon, id string) entry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(d.journal.dir, FileName(id)))
	if err != nil {
		t.Fatal(err)
	}
	var e entry
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func progressWith(ps []protocol.Message, stage, state string) (protocol.Message, bool) {
	for _, p := range ps {
		if p.Stage == stage && p.State == state {
			return p, true
		}
	}
	return protocol.Message{}, false
}

// The placeholder path: the prompt is one argument of the command, the
// launch is the delivery, the start line names the placeholder, and
// the journal has the transitions and no prompt.
func TestAddArgvPrompt(t *testing.T) {
	d, ft, store, remote := taskDaemon(t, nil, map[string][]string{"claude": {"claude", "--flag", PromptPlaceholder}})
	pc := conn(t, d)
	const secret = "make the sidebar\nfollow the current row"
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: secret, SubmittedAt: time.Now()})
	res, progress := result(t, pc, "c1")
	if !res.OK || res.Prompt != protocol.DeliveryDelivered || res.Error != "" || res.Branch != "task" || res.Listing == nil || res.Listing.Revision != 1 {
		t.Fatalf("result %+v", res)
	}
	root := store.Dirs.Worktree("proj", "task")
	if res.Root != root || res.Session != "proj/task" {
		t.Fatalf("result %+v", res)
	}
	if len(ft.cmds) != 1 || strings.Join(ft.cmds[0], " ") != "claude --flag "+secret {
		t.Fatalf("cmds %q", ft.cmds)
	}
	start, ok := progressWith(progress, protocol.StageAgent, protocol.StateStart)
	if !ok || !strings.Contains(start.Detail, PromptPlaceholder) || strings.Contains(start.Detail, "sidebar") {
		t.Fatalf("agent start %+v", start)
	}
	alloc, ok := progressWith(progress, protocol.StageAllocate, protocol.StateSkip)
	if !ok || alloc.Branch != "task" || alloc.Root != root || !strings.Contains(alloc.Detail, "given") {
		t.Fatalf("allocate %+v", alloc)
	}
	for _, p := range progress {
		if strings.Contains(p.Detail, "sidebar") {
			t.Fatalf("prompt in progress: %+v", p)
		}
	}
	b, _ := os.ReadFile(filepath.Join(d.journal.dir, FileName("c1")))
	if strings.Contains(string(b), "sidebar") {
		t.Fatalf("prompt in the journal:\n%s", b)
	}
	e := readEntry(t, d, "c1")
	if e.Launch != launchLaunched || !e.ArgvPrompt || e.Delivery != protocol.DeliveryDelivered || e.PaneID != "%1" || e.ServerPID != 5 || e.Result == nil || !e.Result.OK || e.TerminalAt.IsZero() || !e.HasPrompt || !e.Allocated {
		t.Fatalf("entry %+v", e)
	}
	// A resend under the id, once the memory has let it go, is answered
	// from the journal: nothing runs.
	d.forgetDone("c1")
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: secret, SubmittedAt: time.Now()})
	again, ps := result(t, pc, "c1")
	if !again.OK || again.Prompt != protocol.DeliveryDelivered || len(ps) != 0 || len(ft.cmds) != 1 {
		t.Fatalf("resend %+v %d", again, len(ps))
	}
	// A cmd without the placeholder and no prompt gets the argument
	// removed; with the placeholder and no prompt, removed too.
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c2", Repo: remote, Branch: "plain", AgentName: "claude"})
	if res, _ := result(t, pc, "c2"); !res.OK || res.Prompt != protocol.DeliveryNone || strings.Join(ft.cmds[1], " ") != "claude --flag" {
		t.Fatalf("no prompt: %+v cmds %q", res, ft.cmds)
	}
}

// The typed path: the prompt is pasted once the detector sees the
// prompt box with a verified agent in the recorded pane, the identity
// is bound, and the buffer is named for the add.
func TestAddTypedPrompt(t *testing.T) {
	d, ft, _, remote := taskDaemon(t, idleScreen, nil)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "do the thing"})
	res, progress := result(t, pc, "c1")
	if !res.OK || res.Prompt != protocol.DeliveryDelivered || res.Error != "" {
		t.Fatalf("result %+v", res)
	}
	if len(ft.pastes) != 1 || ft.pastes[0].pane != "%1" || ft.pastes[0].text != "do the thing" || !strings.HasPrefix(ft.pastes[0].buffer, attemptBufferPrefix) {
		t.Fatalf("pastes %+v", ft.pastes)
	}
	if !hasProgress(progress, protocol.StageAgent, protocol.StateDone, "prompt delivered") {
		t.Fatalf("progress %+v", progress)
	}
	e := readEntry(t, d, "c1")
	if e.Identity == nil || e.Identity.PID != claude.PID || e.Typing || e.Delivery != protocol.DeliveryDelivered || e.ArgvPrompt {
		t.Fatalf("entry %+v", e)
	}
}

// A pane that is not ready within the wait gets nothing: the prompt is
// not delivered, with the reason, and the add is still ok.
func TestAddTypedNotReady(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	d, ft, _, remote := taskDaemon(t, []string{"loading"}, nil)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "do the thing"})
	res, _ := result(t, pc, "c1")
	if !res.OK || res.Prompt != protocol.DeliveryNotDelivered || !strings.Contains(res.Error, "not ready within") || !strings.Contains(res.Error, "prompt box") {
		t.Fatalf("result %+v", res)
	}
	if len(ft.pastes) != 0 {
		t.Fatalf("pasted %+v", ft.pastes)
	}
	// A buffer that could not be loaded is not delivered; a paste or an
	// Enter that failed may have run, so it is unknown.
	ft.set(func() {
		ft.screen = idleScreen
		ft.pasteErr = &tmux.PasteError{Step: "load", Err: errors.New("no space")}
	})
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c2", Repo: remote, Branch: "two", AgentName: "claude", Prompt: "second"})
	if res, _ := result(t, pc, "c2"); !res.OK || res.Prompt != protocol.DeliveryNotDelivered || !strings.Contains(res.Error, "paste refused") {
		t.Fatalf("load refused: %+v", res)
	}
	for _, step := range []string{"paste", "enter"} {
		ft.set(func() { ft.pasteErr = &tmux.PasteError{Step: step, Err: errors.New("gone")} })
		id := "c-" + step
		pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: id, Repo: remote, Branch: "b-" + step, AgentName: "claude", Prompt: "third"})
		if res, _ := result(t, pc, id); !res.OK || res.Prompt != protocol.DeliveryUnknown || !strings.Contains(res.Error, "may have reached") {
			t.Fatalf("%s refused: %+v", step, res)
		}
	}
}

// A session already in the root is adopted for the result, and the
// prompt is not delivered: session existed. Without a prompt that is
// none, as before.
func TestAddSessionExisted(t *testing.T) {
	d, ft, _, remote := taskDaemon(t, nil, nil)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude"})
	if res, _ := result(t, pc, "c1"); !res.OK || res.Prompt != protocol.DeliveryNone {
		t.Fatalf("first %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c2", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "again"})
	res, _ := result(t, pc, "c2")
	if !res.OK || res.Prompt != protocol.DeliveryNotDelivered || res.Error != "session existed" || res.Session != "proj/task" || len(ft.cmds) != 1 {
		t.Fatalf("second %+v", res)
	}
	if e := readEntry(t, d, "c2"); e.Launch != launchNone || e.PaneID != "" {
		t.Fatalf("entry %+v", e)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c3", Repo: remote, Branch: "task", AgentName: "claude"})
	if res, _ := result(t, pc, "c3"); !res.OK || res.Prompt != protocol.DeliveryNone || res.Error != "" {
		t.Fatalf("third %+v", res)
	}
}

// Generated names are allocated after the fetch: the first free of the
// proposal and its numbered forms against the branches, the worktrees
// and the journal's unfinished entries, written before the branch is
// made, and a resend under a known id keeps its name.
func TestAddGenerated(t *testing.T) {
	d, _, store, remote := taskDaemon(t, nil, nil)
	pc := conn(t, d)
	for i, want := range []string{"task", "task-2"} {
		id := "g" + string(rune('1'+i))
		pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: id, Repo: remote, Branch: "task", Generated: true, AgentName: "claude"})
		res, progress := result(t, pc, id)
		if !res.OK || res.Branch != want || res.Root != store.Dirs.Worktree("proj", want) {
			t.Fatalf("%s: %+v", id, res)
		}
		alloc, ok := progressWith(progress, protocol.StageAllocate, protocol.StateDone)
		if !ok || alloc.Branch != want || alloc.Root != res.Root {
			t.Fatalf("%s: allocate %+v", id, alloc)
		}
	}
	// A name reserved by an unfinished entry is skipped; the entry's
	// own resend takes it, with allocate skipped.
	if err := d.journal.create(entry{ID: "g3", Source: remote, Repo: "proj", Branch: "task-3", Generated: true, Allocated: true, Stage: protocol.StageWorktree, FirstSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "g4", Repo: remote, Branch: "task", Generated: true, AgentName: "claude"})
	if res, _ := result(t, pc, "g4"); !res.OK || res.Branch != "task-4" {
		t.Fatalf("g4: %+v", res)
	}
	// Before the resend, a follow says interrupted at the stage.
	pc.Write(protocol.Message{Type: protocol.TypeFollow, ID: "g3"})
	if res, _ := result(t, pc, "g3"); res.OK || res.Error != protocol.ErrInterrupted || res.Stage != protocol.StageWorktree || res.Branch != "task-3" {
		t.Fatalf("follow g3: %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "g3", Repo: remote, Branch: "other-proposal", Generated: true, AgentName: "claude"})
	res, progress := result(t, pc, "g3")
	if !res.OK || res.Branch != "task-3" {
		t.Fatalf("g3 resend: %+v", res)
	}
	if alloc, ok := progressWith(progress, protocol.StageAllocate, protocol.StateSkip); !ok || !strings.Contains(alloc.Detail, "allocated before") {
		t.Fatalf("g3 allocate %+v", alloc)
	}
	// A branch that occupies the proposal's ref namespace takes the
	// name too: task/sub rules out task.
	checkout, _, _ := store.Checkout(context.Background(), store.Repos[0])
	if out, err := exec.Command("git", "-C", checkout, "branch", "other/sub", "origin/HEAD").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "g6", Repo: remote, Branch: "other", Generated: true, AgentName: "claude"})
	if res, _ := result(t, pc, "g6"); !res.OK || res.Branch != "other-2" {
		t.Fatalf("g6: %+v", res)
	}
	// A proposal git refuses is refused at resolve.
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "g5", Repo: remote, Branch: "bad..name", Generated: true, AgentName: "claude"})
	if res, _ := result(t, pc, "g5"); res.OK || res.Stage != protocol.StageResolve {
		t.Fatalf("g5: %+v", res)
	}
}

// A launch the daemon died in is unknown: the resend materializes the
// worktree, launches nothing, and says so.
func TestAddLaunchInterrupted(t *testing.T) {
	d, ft, store, remote := taskDaemon(t, nil, nil)
	root := store.Dirs.Worktree("proj", "task")
	if err := d.journal.create(entry{ID: "i1", Source: remote, Repo: "proj", Branch: "task", Allocated: true, HasPrompt: true, Root: root, Stage: protocol.StageAgent, Launch: launchLaunching, Session: "proj/task", FirstSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "i1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "p"})
	res, progress := result(t, pc, "i1")
	if !res.OK || res.Prompt != protocol.DeliveryUnknown || !strings.Contains(res.Error, "restarted during the launch") || res.Session != "" || len(ft.cmds) != 0 {
		t.Fatalf("result %+v cmds %q", res, ft.cmds)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("worktree not materialized")
	}
	if !hasProgress(progress, protocol.StageAgent, protocol.StateSkip, "daemon restarted") {
		t.Fatalf("progress %+v", progress)
	}
	// Without a prompt the launch is not repeated either: the agent may
	// have run and exited.
	if err := d.journal.create(entry{ID: "i0", Source: remote, Repo: "proj", Branch: "zero", Allocated: true, Root: store.Dirs.Worktree("proj", "zero"), Stage: protocol.StageAgent, Launch: launchLaunching, Session: "proj/zero", FirstSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "i0", Repo: remote, Branch: "zero", AgentName: "claude"})
	if res, _ := result(t, pc, "i0"); !res.OK || res.Prompt != protocol.DeliveryNone || res.Session != "" || !strings.Contains(res.Error, "restarted during the launch") || len(ft.cmds) != 0 {
		t.Fatalf("i0: %+v cmds %q", res, ft.cmds)
	}
	// A resend without the prompt of an add that had one is refused.
	if err := d.journal.create(entry{ID: "i2", Source: remote, Repo: "proj", Branch: "two", Allocated: true, HasPrompt: true, Stage: protocol.StageFetch, FirstSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "i2", Repo: remote, Branch: "two", AgentName: "claude"})
	if res, _ := result(t, pc, "i2"); res.OK || res.Stage != protocol.StageResolve || !strings.Contains(res.Error, "must carry it") {
		t.Fatalf("i2: %+v", res)
	}
	// Launched before, typed path, no attempt yet: the resend delivers.
	ft.set(func() { ft.screen = idleScreen })
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "i3", Repo: remote, Branch: "three", AgentName: "claude"})
	first, _ := result(t, pc, "i3")
	if !first.OK {
		t.Fatal(first.Error)
	}
	if err := d.journal.create(entry{ID: "i4", Source: remote, Repo: "proj", Branch: "three", Allocated: true, HasPrompt: true, Root: first.Root, Stage: protocol.StageAgent, Launch: launchLaunched, Session: first.Session, PaneID: first.PaneID, ServerPID: 5, FirstSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "i4", Repo: remote, Branch: "three", AgentName: "claude", Prompt: "late"})
	if res, _ := result(t, pc, "i4"); !res.OK || res.Prompt != protocol.DeliveryDelivered || res.Session != first.Session || len(ft.pastes) != 1 || ft.pastes[0].text != "late" {
		t.Fatalf("i4: %+v pastes %+v", res, ft.pastes)
	}
}

// rm marks the journal's entries at the root removed: a follow and a
// resend are answered removed, a prompt message too, and the listing
// revision steps.
func TestRmMarksRemoved(t *testing.T) {
	d, _, _, remote := taskDaemon(t, nil, nil)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude"})
	res, _ := result(t, pc, "c1")
	if !res.OK {
		t.Fatal(res.Error)
	}
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "task", Root: res.Root, Force: true})
	if rres, _ := result(t, pc, "r1"); !rres.OK {
		t.Fatal(rres.Error)
	}
	d.mu.Lock()
	rev := d.revision
	d.mu.Unlock()
	if rev != 2 {
		t.Fatalf("revision %d", rev)
	}
	pc.Write(protocol.Message{Type: protocol.TypeFollow, ID: "c1"})
	if f, _ := result(t, pc, "c1"); f.OK || f.Error != protocol.ErrRemoved || f.Root != res.Root {
		t.Fatalf("follow %+v", f)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude"})
	if f, ps := result(t, pc, "c1"); f.OK || f.Error != protocol.ErrRemoved || len(ps) != 0 {
		t.Fatalf("resend %+v", f)
	}
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c1", Attempt: 1, Prompt: "x"})
	if f, _ := result(t, pc, "c1"); f.OK || f.Error != protocol.ErrRemoved {
		t.Fatalf("prompt %+v", f)
	}
	pc.Write(protocol.Message{Type: protocol.TypeFollow, ID: "c1", Attempt: 1})
	if f, _ := result(t, pc, "c1"); f.OK || f.Error != protocol.ErrRemoved {
		t.Fatalf("follow with attempt %+v", f)
	}
}

// A resend is the recorded add: one naming another repository under
// the same id is refused before anything runs.
func TestResendKeepsRepository(t *testing.T) {
	d, _, _, remote := taskDaemon(t, nil, nil)
	if err := d.journal.create(entry{ID: "x1", Source: "git@x:o/elsewhere.git", Repo: "elsewhere", Branch: "b", Allocated: true, Stage: protocol.StageFetch, FirstSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "x1", Repo: remote, Branch: "b", AgentName: "claude"})
	if res, ps := result(t, pc, "x1"); res.OK || res.Stage != protocol.StageResolve || !strings.Contains(res.Error, "was submitted for") || len(ps) != 0 {
		t.Fatalf("%+v %d", res, len(ps))
	}
}

// A change the journal could not write reaches no reader: the live
// entry shares nothing with the copy the change was applied to.
func TestJournalUpdateIsAtomic(t *testing.T) {
	dir := t.TempDir()
	j, err := openJournal(dir, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	id := protocol.Identity{PID: 1}
	if err := j.create(entry{ID: "a", Attempts: []attempt{{N: 1, State: attemptAttempting}}, Identity: &id}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	_, err = j.update("a", func(e *entry) {
		e.Attempts[0].State = protocol.DeliveryDelivered
		e.Identity.PID = 2
		e.Attempts = append(e.Attempts, attempt{N: 2})
	})
	if err == nil {
		t.Fatal("update wrote into a read-only directory")
	}
	got, _ := j.get("a")
	if got.Attempts[0].State != attemptAttempting || got.Identity.PID != 1 || len(got.Attempts) != 1 {
		t.Fatalf("live entry changed: %+v", got)
	}
	// A copy handed out is not the live entry either.
	got.Attempts[0].State = "x"
	if again, _ := j.get("a"); again.Attempts[0].State != attemptAttempting {
		t.Fatal("a copy from get reached the journal")
	}
}

// An add older than the retention, or from too far in the future, is
// refused; a follow for an id the journal never saw is unknown command.
func TestSubmissionExpired(t *testing.T) {
	d, _, _, remote := taskDaemon(t, nil, nil)
	pc := conn(t, d)
	for _, at := range []time.Time{time.Now().Add(-31 * 24 * time.Hour), time.Now().Add(2 * 24 * time.Hour)} {
		pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "old", Repo: remote, Branch: "task", AgentName: "claude", SubmittedAt: at})
		if res, ps := result(t, pc, "old"); res.OK || res.Error != protocol.ErrSubmissionExpired || len(ps) != 0 {
			t.Fatalf("%s: %+v", at, res)
		}
		d.mu.Lock()
		delete(d.cmds, "old")
		d.mu.Unlock()
	}
	if _, ok := d.journal.get("old"); ok {
		t.Fatal("a refused add was journaled")
	}
	pc.Write(protocol.Message{Type: protocol.TypeFollow, ID: "never"})
	if res, _ := result(t, pc, "never"); res.Error != protocol.ErrUnknownCommand {
		t.Fatalf("follow %+v", res)
	}
}

// The prompt message: an attempt is delivered when the pane is ready,
// a repeat of its number is answered from the record without a paste,
// out-of-order numbers are refused, follow finds attempts, and an id
// the journal lacks is recovery expired.
func TestPromptMessage(t *testing.T) {
	shortWait(t, 300*time.Millisecond)
	d, ft, _, remote := taskDaemon(t, []string{"loading"}, nil)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "do it"})
	if res, _ := result(t, pc, "c1"); !res.OK || res.Prompt != protocol.DeliveryNotDelivered {
		t.Fatalf("add %+v", res)
	}
	ft.set(func() { ft.screen = idleScreen })
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c1", Attempt: 1, Prompt: "do it"})
	res, _ := result(t, pc, "c1")
	if !res.OK || res.Attempt != 1 || res.Prompt != protocol.DeliveryDelivered || len(ft.pastes) != 1 || !strings.HasSuffix(ft.pastes[0].buffer, "-1") {
		t.Fatalf("attempt 1: %+v pastes %+v", res, ft.pastes)
	}
	e := readEntry(t, d, "c1")
	if len(e.Attempts) != 1 || e.Attempts[0].State != protocol.DeliveryDelivered || e.Delivery != protocol.DeliveryDelivered {
		t.Fatalf("entry %+v", e)
	}
	// The command is remembered for a while; evict it so the repeat
	// reaches the journal.
	d.mu.Lock()
	delete(d.cmds, promptKey("c1", 1))
	d.mu.Unlock()
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c1", Attempt: 1, Prompt: "do it"})
	if res, _ := result(t, pc, "c1"); !res.OK || res.Prompt != protocol.DeliveryDelivered || len(ft.pastes) != 1 {
		t.Fatalf("repeat: %+v pastes %d", res, len(ft.pastes))
	}
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c1", Attempt: 3, Prompt: "do it"})
	if res, _ := result(t, pc, "c1"); res.OK || !strings.Contains(res.Error, "not the next") {
		t.Fatalf("attempt 3: %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypeFollow, ID: "c1", Attempt: 1})
	if res, _ := result(t, pc, "c1"); !res.OK || res.Attempt != 1 || res.Prompt != protocol.DeliveryDelivered {
		t.Fatalf("follow 1: %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypeFollow, ID: "c1", Attempt: 2})
	if res, _ := result(t, pc, "c1"); res.OK || res.Error != protocol.ErrUnknownAttempt {
		t.Fatalf("follow 2: %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "gone", Attempt: 1, Prompt: "x"})
	if res, _ := result(t, pc, "gone"); res.OK || res.Error != protocol.ErrRecoveryExpired {
		t.Fatalf("gone: %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypeFollow, ID: "gone", Attempt: 1})
	if res, _ := result(t, pc, "gone"); res.OK || res.Error != protocol.ErrRecoveryExpired {
		t.Fatalf("follow gone: %+v", res)
	}
	// A second attempt on a replaced agent is refused as such: the
	// bound identity is not the one in the pane.
	d.journal.update("c1", func(e *entry) { e.Identity.PID = 999 })
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c1", Attempt: 2, Prompt: "do it"})
	if res, _ := result(t, pc, "c1"); !res.OK || res.Prompt != protocol.DeliveryNotDelivered || !strings.Contains(res.Error, "session replaced") || len(ft.pastes) != 1 {
		t.Fatalf("attempt 2: %+v", res)
	}
}

// A prompt message for an entry without a target adopts the managed
// session in the root when it is the only one and has a verified agent,
// and says no agent to deliver to otherwise.
func TestPromptAdopts(t *testing.T) {
	d, ft, _, remote := taskDaemon(t, idleScreen, nil)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude"})
	first, _ := result(t, pc, "c1")
	if !first.OK {
		t.Fatal(first.Error)
	}
	// session existed: the add records no target.
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c2", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "hello"})
	if res, _ := result(t, pc, "c2"); !res.OK || res.Error != "session existed" {
		t.Fatalf("c2 %+v", res)
	}
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c2", Attempt: 1, Prompt: "hello"})
	if res, _ := result(t, pc, "c2"); !res.OK || res.Prompt != protocol.DeliveryDelivered || len(ft.pastes) != 1 || ft.pastes[0].pane != first.PaneID {
		t.Fatalf("adopt: %+v pastes %+v", res, ft.pastes)
	}
	if e := readEntry(t, d, "c2"); e.PaneID != first.PaneID || e.Session != first.Session || e.Identity == nil {
		t.Fatalf("entry %+v", e)
	}
	// No session in the root: nothing to adopt.
	ft.KillSession(context.Background(), first.Session)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c3", Repo: remote, Branch: "two", AgentName: "claude"})
	third, _ := result(t, pc, "c3")
	ft.KillSession(context.Background(), third.Session)
	if err := d.journal.create(entry{ID: "c4", Source: remote, Repo: "proj", Branch: "two", Allocated: true, HasPrompt: true, Root: third.Root, Result: &protocol.Message{OK: true}, TerminalAt: time.Now(), FirstSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c4", Attempt: 1, Prompt: "hello"})
	if res, _ := result(t, pc, "c4"); !res.OK || res.Prompt != protocol.DeliveryNotDelivered || !strings.Contains(res.Error, "no agent to deliver to") {
		t.Fatalf("c4: %+v", res)
	}
}

// Listings are stamped: the snapshot carries the generation and the
// revision the listing was read at, and a result's barrier is passed by
// the listing after it and not by one before.
func TestListingStamp(t *testing.T) {
	d, _, _, remote := taskDaemon(t, nil, nil)
	ctx := context.Background()
	d.pollWorktrees(ctx)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeSubscribe})
	snap, err := pc.Read()
	if err != nil || snap.Type != protocol.TypeSnapshot || snap.Listing == nil || snap.Listing.Generation != d.generation || snap.Listing.Revision != 0 {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	before := *snap.Listing
	pc2 := conn(t, d)
	pc2.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude"})
	res, _ := result(t, pc2, "c1")
	if !res.OK || res.Listing == nil || res.Listing.Revision != 1 {
		t.Fatalf("result %+v", res)
	}
	if before.Satisfies(*res.Listing) {
		t.Fatal("a listing before the add satisfies its barrier")
	}
	d.pollWorktrees(ctx)
	d.mu.Lock()
	after := d.listing
	d.mu.Unlock()
	if !after.Satisfies(*res.Listing) {
		t.Fatalf("listing %+v does not satisfy %+v", after, res.Listing)
	}
	if (protocol.Listing{Generation: d.generation + 1, Revision: 0}).Satisfies(*res.Listing) != true {
		t.Fatal("a later generation does not satisfy")
	}
	// The stamp is published on the stream too.
	for {
		m, err := pc.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.Type == protocol.TypeUpsert && m.Listing != nil && m.Listing.Revision == 1 {
			break
		}
	}
}

// A daemon that starts sweeps the attempt buffers and resolves what the
// last one died in: a paste in progress is unknown.
func TestJournalStartup(t *testing.T) {
	dir := t.TempDir()
	e := entry{ID: "x", Source: "s", Repo: "proj", Branch: "b", HasPrompt: true, Typing: true, Attempts: []attempt{{N: 1, State: attemptAttempting}}, FirstSeen: time.Now()}
	b, _ := json.Marshal(e)
	os.WriteFile(filepath.Join(dir, FileName("x")), b, 0o600)
	os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{"), 0o600)
	store, _ := newStore(t)
	ft := &fakeServer{}
	d := New(Config{Targets: []Target{{Label: "laatmux", Tmux: ft, Managed: true}}, Store: store, Commands: dir})
	if !protocol.Has(d.capabilities(), protocol.CapTask) {
		t.Fatal("no task capability")
	}
	got, ok := d.journal.get("x")
	if !ok || got.Typing || got.Delivery != protocol.DeliveryUnknown || got.Attempts[0].State != protocol.DeliveryUnknown {
		t.Fatalf("entry %+v", got)
	}
	// An attempt open before the paste was written is not delivered.
	waiting := entry{ID: "w", Source: "s", Repo: "proj", Branch: "b", HasPrompt: true, Attempts: []attempt{{N: 1, State: attemptAttempting}}, FirstSeen: time.Now()}
	b, _ = json.Marshal(waiting)
	os.WriteFile(filepath.Join(dir, FileName("w")), b, 0o600)
	j, err := openJournal(dir, d.cfg.Logger)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := j.get("w"); got.Delivery != protocol.DeliveryNotDelivered || got.Attempts[0].State != protocol.DeliveryNotDelivered {
		t.Fatalf("waiting entry %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go d.Run(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for {
		ft.mu.Lock()
		n := len(ft.buffers)
		ft.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if len(ft.buffers) != 1 || ft.buffers[0] != attemptBufferPrefix {
		t.Fatalf("buffers %v", ft.buffers)
	}
	// The sweep deletes what has been terminal for the retention and
	// keeps the rest; the junk file is not laatmux's to delete.
	d.journal.create(entry{ID: "old", Result: &protocol.Message{OK: true}, TerminalAt: time.Now().Add(-31 * 24 * time.Hour)})
	d.journal.create(entry{ID: "new", Result: &protocol.Message{OK: true}, TerminalAt: time.Now()})
	d.journal.sweep(time.Now())
	if _, ok := d.journal.get("old"); ok {
		t.Fatal("old entry kept")
	}
	if _, ok := d.journal.get("new"); !ok {
		t.Fatal("new entry swept")
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.json")); err != nil {
		t.Fatal("junk deleted")
	}
	// Ids that are not file names are hashed; safe ones are used as is.
	if FileName("add-1-2") != "add-1-2.json" || !strings.HasPrefix(FileName("../x"), "h-") || !strings.HasPrefix(FileName(".hidden"), "h-") {
		t.Fatalf("fileName %s %s", FileName("add-1-2"), FileName("../x"))
	}
	// A daemon without a journal directory has no task capability and
	// refuses a prompt at resolve.
	d2 := New(Config{Targets: []Target{{Label: "laatmux", Tmux: &fakeServer{}, Managed: true}}, Store: store})
	if protocol.Has(d2.capabilities(), protocol.CapTask) {
		t.Fatal("task capability without a journal")
	}
	pc := conn(t, d2)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: store.Repos[0].Source, Branch: "b", Cmd: []string{"true"}, Prompt: "p"})
	if res, _ := result(t, pc, "c1"); res.OK || res.Stage != protocol.StageResolve || !strings.Contains(res.Error, "task capability") {
		t.Fatalf("no task: %+v", res)
	}
}

// A tmux error that echoes the command line has the prompt replaced by
// the placeholder; a failure after new-session was submitted is unknown
// on the argv path, a failure before it is not delivered.
func TestLaunchErrorRedacted(t *testing.T) {
	d, ft, _, remote := taskDaemon(t, nil, map[string][]string{"claude": {"claude", PromptPlaceholder}})
	ft.newErr = &tmux.SubmittedError{Err: errors.New("tmux new-session -d claude 'the secret' ; set-option: failed")}
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "the secret"})
	res, progress := result(t, pc, "c1")
	if res.OK || res.Stage != protocol.StageAgent || res.Prompt != protocol.DeliveryUnknown || strings.Contains(res.Error, "secret") || !strings.Contains(res.Error, PromptPlaceholder) {
		t.Fatalf("submitted: %+v", res)
	}
	for _, p := range progress {
		if strings.Contains(p.Detail, "secret") {
			t.Fatalf("prompt in progress %+v", p)
		}
	}
	if e := readEntry(t, d, "c1"); strings.Contains(e.DeliveryError, "secret") || e.Delivery != protocol.DeliveryUnknown || e.Result == nil || e.Result.OK {
		t.Fatalf("entry %+v", e)
	}
	ft.newErr = errors.New("tmux: cwd: no such directory")
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c2", Repo: remote, Branch: "two", AgentName: "claude", Prompt: "the secret"})
	if res, _ := result(t, pc, "c2"); res.OK || res.Prompt != protocol.DeliveryNotDelivered || !strings.Contains(res.Error, "cwd") {
		t.Fatalf("plain: %+v", res)
	}
}

func TestWithPrompt(t *testing.T) {
	argv, ok := withPrompt([]string{"claude", PromptPlaceholder, "--x"}, "hi there")
	if !ok || strings.Join(argv, "|") != "claude|hi there|--x" {
		t.Fatalf("%v %v", argv, ok)
	}
	argv, ok = withPrompt([]string{"claude", PromptPlaceholder}, "")
	if !ok || strings.Join(argv, "|") != "claude" {
		t.Fatalf("%v %v", argv, ok)
	}
	argv, ok = withPrompt([]string{"claude"}, "hi")
	if ok || strings.Join(argv, "|") != "claude" {
		t.Fatalf("%v %v", argv, ok)
	}
}

// An rm during the typed delivery wait, with the repository lock
// released, wins: the add's result is removed, not a success over the
// tombstone, and the worktree is gone.
func TestRmDuringDeliveryWait(t *testing.T) {
	shortWait(t, 2*time.Second)
	d, _, _, remote := taskDaemon(t, []string{"loading"}, nil)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "p"})
	var root string
	for {
		m, err := pc.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.Type == protocol.TypeProgress && m.Stage == protocol.StageAllocate {
			root = m.Root
		}
		if m.Type == protocol.TypeProgress && strings.HasPrefix(m.Detail, "typing the prompt") {
			break
		}
	}
	pc2 := conn(t, d)
	pc2.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "task", Root: root, Force: true})
	if res, _ := result(t, pc2, "r1"); !res.OK {
		t.Fatal(res.Error)
	}
	res, _ := result(t, pc, "c1")
	if res.OK || res.Error != protocol.ErrRemoved {
		t.Fatalf("add after rm: %+v", res)
	}
	if e := readEntry(t, d, "c1"); !e.Removed || e.Result != nil {
		t.Fatalf("entry %+v", e)
	}
	pc.Write(protocol.Message{Type: protocol.TypeFollow, ID: "c1"})
	if f, _ := result(t, pc, "c1"); f.OK || f.Error != protocol.ErrRemoved {
		t.Fatalf("follow %+v", f)
	}
	// A resumed add that finds its entry removed under the repository
	// lock remakes nothing.
	if err := d.journal.create(entry{ID: "c2", Source: remote, Repo: "proj", Branch: "gone", Allocated: true, Root: root, Stage: protocol.StageWorktree, Removed: true, TerminalAt: time.Now(), FirstSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c2", Repo: remote, Branch: "gone", AgentName: "claude"})
	if f, ps := result(t, pc, "c2"); f.OK || f.Error != protocol.ErrRemoved || len(ps) != 0 {
		t.Fatalf("resend of removed: %+v %d", f, len(ps))
	}
}

// A journal that cannot be written stops the add before its side
// effects: no launch without launching on disk.
func TestJournalWriteGatesLaunch(t *testing.T) {
	d, ft, _, remote := taskDaemon(t, nil, nil)
	pc := conn(t, d)
	// The directory is made read-only once the entry exists, so the
	// creation succeeds and the transitions fail.
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "p"})
	for {
		m, err := pc.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.Type == protocol.TypeProgress && m.Stage == protocol.StageClone {
			break
		}
	}
	if err := os.Chmod(d.journal.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(d.journal.dir, 0o700) })
	res, _ := result(t, pc, "c1")
	if res.OK || len(ft.cmds) != 0 {
		t.Fatalf("launched without a journal: %+v cmds %q", res, ft.cmds)
	}
	if !strings.Contains(res.Error, "journal") {
		t.Fatalf("error %q", res.Error)
	}
}

// A prompt message whose root is another repository's worktree now, or
// another branch's, is refused as replaced: nothing is adopted there.
func TestPromptRefusesReplacedWorktree(t *testing.T) {
	d, _, _, remote := taskDaemon(t, idleScreen, nil)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "p"})
	first, _ := result(t, pc, "c1")
	if !first.OK {
		t.Fatal(first.Error)
	}
	d.journal.update("c1", func(e *entry) { e.Source = "elsewhere" })
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c1", Attempt: 1, Prompt: "p"})
	if res, _ := result(t, pc, "c1"); !res.OK || res.Prompt != protocol.DeliveryNotDelivered || !strings.Contains(res.Error, "worktree replaced") {
		t.Fatalf("%+v", res)
	}
	d.journal.update("c1", func(e *entry) { e.Source, e.Branch = remote, "renamed" })
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c1", Attempt: 2, Prompt: "p"})
	if res, _ := result(t, pc, "c1"); !res.OK || res.Prompt != protocol.DeliveryNotDelivered || !strings.Contains(res.Error, "now on branch task") {
		t.Fatalf("%+v", res)
	}
}

// A delivery that waited for the root's lock reads the entry again
// when it gets it: a tombstone written meanwhile, or a replacement at
// the root, is refused, and nothing is adopted or pasted.
func TestDeliveryRereadsAfterLock(t *testing.T) {
	d, ft, _, remote := taskDaemon(t, idleScreen, nil)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude"})
	first, _ := result(t, pc, "c1")
	if !first.OK {
		t.Fatal(first.Error)
	}
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c2", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "p"})
	if res, _ := result(t, pc, "c2"); !res.OK || res.Error != "session existed" {
		t.Fatalf("c2 %+v", res)
	}
	// The lock is held while the attempt arrives, and the entry is
	// tombstoned meanwhile, as rm would under the same lock.
	unlock := d.lockDeliveries(first.Root)
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c2", Attempt: 1, Prompt: "p"})
	time.Sleep(200 * time.Millisecond)
	if _, err := d.journal.markRemoved(first.Root, time.Now()); err != nil {
		t.Fatal(err)
	}
	unlock()
	if res, _ := result(t, pc, "c2"); !res.OK || res.Prompt != protocol.DeliveryNotDelivered || res.Error != "worktree removed" || len(ft.pastes) != 0 {
		t.Fatalf("attempt after tombstone: %+v pastes %+v", res, ft.pastes)
	}
	if e := readEntry(t, d, "c2"); e.PaneID != "" || len(e.Attempts) != 1 || e.Attempts[0].State != protocol.DeliveryNotDelivered {
		t.Fatalf("entry %+v", e)
	}
}

// A tombstone that cannot be written fails rm, after git has removed
// the worktree, so the caller knows the journal still says otherwise.
func TestRmReportsTombstoneFailure(t *testing.T) {
	d, _, _, remote := taskDaemon(t, nil, nil)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude"})
	res, _ := result(t, pc, "c1")
	if !res.OK {
		t.Fatal(res.Error)
	}
	if err := os.Chmod(d.journal.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(d.journal.dir, 0o700) })
	pc.Write(protocol.Message{Type: protocol.TypeRm, ID: "r1", Repo: remote, Branch: "task", Root: res.Root, Force: true})
	if rres, _ := result(t, pc, "r1"); rres.OK || !strings.Contains(rres.Error, "journal") {
		t.Fatalf("rm %+v", rres)
	}
	if _, err := os.Stat(res.Root); err == nil {
		t.Fatal("worktree kept")
	}
}

// An attempt the journal cannot write is not taken: the answer says so
// and the journal has no attempt, so the same number is sent again.
func TestPromptAttemptNotRecorded(t *testing.T) {
	d, _, _, remote := taskDaemon(t, idleScreen, nil)
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude"})
	if res, _ := result(t, pc, "c1"); !res.OK {
		t.Fatal(res.Error)
	}
	d.journal.update("c1", func(e *entry) {
		e.HasPrompt, e.Delivery, e.DeliveryError = true, protocol.DeliveryNotDelivered, "session existed"
	})
	if err := os.Chmod(d.journal.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(d.journal.dir, 0o700) })
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c1", Attempt: 1, Prompt: "p"})
	if res, _ := result(t, pc, "c1"); res.OK || !strings.HasPrefix(res.Error, protocol.ErrAttemptNotRecorded) {
		t.Fatalf("%+v", res)
	}
	if e, _ := d.journal.get("c1"); len(e.Attempts) != 0 {
		t.Fatalf("attempt recorded: %+v", e.Attempts)
	}
	os.Chmod(d.journal.dir, 0o700)
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c1", Attempt: 1, Prompt: "p"})
	if res, _ := result(t, pc, "c1"); !res.OK || res.Prompt != protocol.DeliveryDelivered {
		t.Fatalf("retry %+v", res)
	}
}

// The server instance comes from new-session itself, not from a
// listing after it.
func TestLaunchRecordsServerFromNewSession(t *testing.T) {
	d, ft, _, remote := taskDaemon(t, nil, nil)
	ft.set(func() { ft.server = 77 })
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude"})
	if res, _ := result(t, pc, "c1"); !res.OK {
		t.Fatal(res.Error)
	}
	if e := readEntry(t, d, "c1"); e.ServerPID != 77 {
		t.Fatalf("entry %+v", e)
	}
}

// A poll whose process check failed vouches for no agent: the pane is
// not ready on it, whatever the last check said, so a prompt box left
// by an agent that exited gets nothing.
func TestDeliveryNeedsLivenessThisPoll(t *testing.T) {
	shortWait(t, 600*time.Millisecond)
	store, remote := newStore(t)
	// Nothing is idle while claude is found; by the time the prompt box
	// shows, every process check fails.
	ft := &fakeServer{screen: []string{"loading"}}
	tables := []procTable{{procs: []procs.Proc{shell, claude}}, {err: errors.New("proc table unreadable")}}
	d := New(Config{
		EnvironmentID: "env", Host: "box",
		Targets: []Target{{Label: "laatmux", Tmux: ft, Managed: true}},
		Procs:   &fakeProcs{tables: tables},
		Store:   store, Agents: map[string][]string{"claude": {"claude"}},
		Commands: t.TempDir(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	t.Cleanup(func() { cancel(); <-stopped })
	go func() {
		defer close(stopped)
		for ctx.Err() == nil {
			if d.poll(ctx) == nil {
				d.markDiscovered(&d.panesDiscovered)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "p"})
	for {
		m, err := pc.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.Type == protocol.TypeProgress && strings.HasPrefix(m.Detail, "typing the prompt") {
			break
		}
	}
	time.Sleep(100 * time.Millisecond) // the finding poll has happened
	ft.set(func() { ft.screen = idleScreen })
	res, _ := result(t, pc, "c1")
	if !res.OK || res.Prompt != protocol.DeliveryNotDelivered || !strings.Contains(res.Error, "no verified agent") || len(ft.pastes) != 0 {
		t.Fatalf("%+v pastes %+v", res, ft.pastes)
	}
}

// Readiness is judged on the observation's own time: one made inside
// the agent's startup grace does not become ready by time passing, a
// later one does.
func TestReadyJudgesObservationTime(t *testing.T) {
	store, _ := newStore(t)
	ft := &fakeServer{}
	d := New(Config{Targets: []Target{{Label: "laatmux", Tmux: ft, Managed: true}}, Store: store, Commands: t.TempDir()})
	start := time.Now().Add(-10 * time.Second)
	id := procs.Identity{Agent: "claude", PID: 7, Start: start, Comm: "claude"}
	since := start
	st := &paneState{target: d.managed, obs: observation{at: start.Add(time.Second), session: "s", serverPID: 5, verified: true, identity: id, idle: true}}
	d.panes[paneKey("laatmux", "%1")] = st
	e := &entry{Session: "s", PaneID: "%1", ServerPID: 5}
	if _, why, _ := d.ready(e, since); !strings.Contains(why, "startup grace") {
		t.Fatalf("pre-grace observation: %q", why)
	}
	st.obs.at = start.Add(startupGrace + time.Second)
	if _, why, _ := d.ready(e, since); why != "" {
		t.Fatalf("post-grace observation: %q", why)
	}
	st.obs.at = since
	if _, why, _ := d.ready(e, since); !strings.Contains(why, "since the wait began") {
		t.Fatalf("stale observation: %q", why)
	}
	// One observation serves one paste: after a paste into the pane the
	// next delivery needs a newer one.
	st.obs.at = start.Add(startupGrace + time.Second)
	d.pasted[paneKey("laatmux", "%1")] = st.obs.at.Add(time.Millisecond)
	if _, why, _ := d.ready(e, since); !strings.Contains(why, "since the last paste") {
		t.Fatalf("observation before the paste: %q", why)
	}
	st.obs.at = st.obs.at.Add(time.Second)
	if _, why, _ := d.ready(e, since); why != "" {
		t.Fatalf("observation after the paste: %q", why)
	}
}

// Two deliveries to one pane never share an observation: with one
// observation made while both wait, one pastes on it and the other
// waits for a newer one, which never comes, and says so; on a new
// observation its next attempt is delivered.
func TestDeliveriesDoNotShareObservation(t *testing.T) {
	shortWait(t, 600*time.Millisecond)
	store, remote := newStore(t)
	ft := &fakeServer{screen: idleScreen}
	d := New(Config{
		EnvironmentID: "env", Host: "box",
		Targets: []Target{{Label: "laatmux", Tmux: ft, Managed: true}},
		Procs:   &fakeProcs{tables: []procTable{{procs: []procs.Proc{shell, claude}}}},
		Store:   store, Agents: map[string][]string{"claude": {"claude"}},
		Commands: t.TempDir(),
	})
	ctx := context.Background()
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude"})
	first, _ := result(t, pc, "c1")
	if !first.OK {
		t.Fatal(first.Error)
	}
	for _, id := range []string{"c2", "c3"} {
		pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: id, Repo: remote, Branch: "task", AgentName: "claude", Prompt: id})
		if res, _ := result(t, pc, id); !res.OK || res.Error != "session existed" {
			t.Fatalf("%s: %+v", id, res)
		}
	}
	// The first poll identifies the agent; the delivery needs a
	// verified observation, which the second poll gives, made while
	// both attempts wait.
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	d.markDiscovered(&d.panesDiscovered)
	// awaitWaits returns once n deliveries have begun their wait, so
	// the observation published next is after every since.
	awaitWaits := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; {
			d.mu.Lock()
			w := d.waits
			d.mu.Unlock()
			if w >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d deliveries waiting, want %d", w, n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	pc2, pc3 := conn(t, d), conn(t, d)
	pc2.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c2", Attempt: 1, Prompt: "c2"})
	pc3.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c3", Attempt: 1, Prompt: "c3"})
	awaitWaits(2)
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	r2, _ := result(t, pc2, "c2")
	r3, _ := result(t, pc3, "c3")
	delivered, waited := 0, 0
	for _, r := range []protocol.Message{r2, r3} {
		switch {
		case r.OK && r.Prompt == protocol.DeliveryDelivered:
			delivered++
		case r.OK && r.Prompt == protocol.DeliveryNotDelivered && strings.Contains(r.Error, "since the last paste"):
			waited++
		default:
			t.Fatalf("%+v", r)
		}
	}
	if delivered != 1 || waited != 1 || len(ft.pastes) != 1 {
		t.Fatalf("delivered %d waited %d pastes %d", delivered, waited, len(ft.pastes))
	}
	// A new observation, and the next attempt of the one that waited
	// is delivered.
	loser := "c3"
	if r3.Prompt == protocol.DeliveryDelivered {
		loser = "c2"
	}
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: loser, Attempt: 2, Prompt: loser})
	awaitWaits(3)
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if res, _ := result(t, pc, loser); !res.OK || res.Prompt != protocol.DeliveryDelivered || len(ft.pastes) != 2 {
		t.Fatalf("%s on a new observation: %+v", loser, res)
	}
}

// A daemon stopping starts no paste and waits for one in flight, so
// the buffer is gone before the process ends.
func TestStopWaitsForPaste(t *testing.T) {
	d, ft, _, remote := taskDaemon(t, idleScreen, nil)
	release := make(chan struct{})
	ft.set(func() { ft.pasteHold = release })
	pc := conn(t, d)
	pc.Write(protocol.Message{Type: protocol.TypeAdd, ID: "c1", Repo: remote, Branch: "task", AgentName: "claude", Prompt: "p"})
	for {
		m, err := pc.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.Type == protocol.TypeProgress && strings.HasPrefix(m.Detail, "typing the prompt") {
			break
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		n := d.pasting
		d.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopped := make(chan struct{})
	go func() {
		d.StopRuns(context.Background())
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("StopRuns returned during the paste")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	<-stopped
	if res, _ := result(t, pc, "c1"); !res.OK || res.Prompt != protocol.DeliveryDelivered {
		t.Fatalf("%+v", res)
	}
	// After the stop no paste starts.
	pc.Write(protocol.Message{Type: protocol.TypePrompt, ID: "c1", Attempt: 1, Prompt: "p"})
	if res, _ := result(t, pc, "c1"); !res.OK || res.Prompt != protocol.DeliveryNotDelivered || !strings.Contains(res.Error, "shutting down") {
		t.Fatalf("after stop: %+v", res)
	}
}
