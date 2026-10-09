package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/source"
)

// The decision table: what stays whatever the host reads (the main
// checkout, a detached HEAD, a branch laatmux cannot carry, an open
// local session, a listing that cannot tell what runs in it), what
// stays for want of facts (a host without prune, a read that failed, a
// branch changed since the listing), and over the facts and the branch
// record: dirty stays, and so do a locked worktree and one with
// submodules, which git keeps; clean with nothing beyond the default
// branch goes, its ignored files said; clean and ahead goes only with
// its PR merged into the default branch at HEAD; anything else ahead
// stays, said with whether it is pushed and its PR.
func TestPruneDecide(t *testing.T) {
	w := protocol.Worktree{Repo: "proj", Branch: "fix", Root: "/w/proj/fix", Source: "git@x:o/proj.git"}
	facts := func(change func(*protocol.RootFacts)) *protocol.RootFacts {
		f := &protocol.RootFacts{Root: w.Root, Branch: "fix", Head: "abc", Base: "origin/main"}
		if change != nil {
			change(f)
		}
		return f
	}
	ahead := func(n int) func(*protocol.RootFacts) { return func(f *protocol.RootFacts) { f.Ahead = n } }
	prOn := func(state string, draft bool, head, base string) *protocol.BranchStatus {
		return &protocol.BranchStatus{HeadOID: head, PR: &protocol.PullRequest{Number: 7, State: state, Draft: draft, Base: base}}
	}
	pr := func(state string, draft bool, head string) *protocol.BranchStatus {
		return prOn(state, draft, head, "main")
	}
	with := func(change func(*protocol.Worktree)) protocol.Worktree {
		c := w
		change(&c)
		return c
	}
	cases := []struct {
		name   string
		in     pruneInput
		remove bool
		reason string
	}{
		{"main checkout", pruneInput{Worktree: with(func(w *protocol.Worktree) { w.Main = true }), Facts: facts(nil)}, false, "the main checkout"},
		{"detached", pruneInput{Worktree: with(func(w *protocol.Worktree) { w.Branch = "" }), Facts: facts(nil)}, false, "detached HEAD"},
		{"display only", pruneInput{Worktree: with(func(w *protocol.Worktree) { w.BranchDisplayOnly = true }), Facts: facts(nil)}, false, "a branch laatmux cannot carry; rm --root removes it"},
		{"local session", pruneInput{Worktree: w, Local: &protocol.Session{Name: "vm/proj/fix"}, Facts: facts(nil)}, false, "local session vm/proj/fix is open"},
		{"unseen", pruneInput{Worktree: w, Unseen: "local sessions not listed: boom", Facts: facts(nil)}, false, "local sessions not listed: boom"},
		{"no capability", pruneInput{Worktree: w, NoFacts: "the daemon on vm, v1, has no prune"}, false, "the daemon on vm, v1, has no prune"},
		{"read failed", pruneInput{Worktree: w, Facts: &protocol.RootFacts{Root: w.Root, Error: "git status: timeout"}}, false, "git status: timeout"},
		{"branch changed", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Branch = "other" })}, false, "the branch is other now"},
		{"detached since", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Branch = "" })}, false, "the branch is detached now"},
		{"dirty", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Changed = 3 })}, false, "dirty: 3 changed files"},
		{"dirty, one file", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Changed = 1 })}, false, "dirty: 1 changed file"},
		{"dirty and merged", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Changed = 1; f.Ahead = 2 }), Branch: pr("merged", false, "abc")}, false, "dirty: 1 changed file"},
		{"in use", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.InUse = "add a1, which has no outcome yet" })}, false, "in use, by add a1, which has no outcome yet"},
		{"in use and dirty", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.InUse, f.Changed = "a run", 2 })}, false, "in use, by a run"},
		{"dirty and locked", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Changed, f.Locked = 1, true })}, false, "dirty: 1 changed file"},
		{"locked", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Locked, f.LockReason = true, "keep me" })}, false, "locked: keep me"},
		{"locked, no reason", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Locked = true })}, false, "locked"},
		{"submodules", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Submodules = true })}, false, "has submodules, which git removes only by force; rm --force removes it"},
		{"no base", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Base = "" })}, false, "no default branch to compare with: no origin/HEAD, main or master"},
		{"the default branch", pruneInput{Worktree: with(func(w *protocol.Worktree) { w.Branch = "main" }), Facts: facts(func(f *protocol.RootFacts) { f.Branch = "main" })}, false, "the default branch"},
		{"nothing ahead", pruneInput{Worktree: w, Facts: facts(nil)}, true, "clean, no commits beyond origin/main"},
		{"nothing ahead, PR open", pruneInput{Worktree: w, Facts: facts(nil), Branch: pr("open", false, "abc")}, true, "clean, no commits beyond origin/main"},
		{"nothing ahead, ignored files", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Ignored = 3 })}, true, "clean, 3 ignored files, no commits beyond origin/main"},
		{"nothing ahead, ignored ones of each", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Ignored, f.IgnoredDirs = 1, 2 })}, true, "clean, 1 ignored file and 2 ignored directories, no commits beyond origin/main"},
		{"merged at HEAD", pruneInput{Worktree: w, Facts: facts(ahead(2)), Branch: pr("merged", false, "abc")}, true, "clean, PR #7 merged"},
		{"merged at HEAD, an ignored directory", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Ahead, f.IgnoredDirs = 2, 1 }), Branch: pr("merged", false, "abc")}, true, "clean, 1 ignored directory, PR #7 merged"},
		{"merged into another branch", pruneInput{Worktree: w, Facts: facts(ahead(2)), Branch: prOn("merged", false, "abc", "feature")}, false, "2 commits ahead of origin/main; PR #7 is merged into feature, not main"},
		{"merged, base not known", pruneInput{Worktree: w, Facts: facts(ahead(2)), Branch: prOn("merged", false, "abc", "")}, false, "2 commits ahead of origin/main; PR #7 is merged, into a branch not known yet"},
		{"merged into a local default", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Ahead, f.Base = 2, "master" }), Branch: prOn("merged", false, "abc", "master")}, true, "clean, PR #7 merged"},
		{"merged, HEAD moved on", pruneInput{Worktree: w, Facts: facts(ahead(3)), Branch: pr("merged", false, "old")}, false, "3 commits ahead of origin/main; PR #7 is merged, but HEAD is not its last commit"},
		{"merged, last commit unknown", pruneInput{Worktree: w, Facts: facts(ahead(1)), Branch: pr("merged", false, "")}, false, "1 commit ahead of origin/main; PR #7 is merged, its last commit not known"},
		{"ahead, not on origin", pruneInput{Worktree: w, Facts: facts(ahead(1))}, false, "1 commit ahead of origin/main, not on origin"},
		{"ahead, pushed, open", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Ahead, f.OnOrigin, f.Pushed = 2, true, true }), Branch: pr("open", false, "abc")}, false, "2 commits ahead of origin/main, pushed, PR #7 open"},
		{"ahead, partly pushed, draft", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Ahead, f.OnOrigin = 2, true }), Branch: pr("open", true, "abc")}, false, "2 commits ahead of origin/main, not all pushed, draft PR #7"},
		{"ahead, closed", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Ahead, f.OnOrigin, f.Pushed = 2, true, true }), Branch: pr("closed", false, "abc")}, false, "2 commits ahead of origin/main, pushed, PR #7 closed"},
		{"ahead, no PR record", pruneInput{Worktree: w, Facts: facts(ahead(2)), Branch: &protocol.BranchStatus{HeadOID: "abc"}}, false, "2 commits ahead of origin/main, not on origin"},
	}
	for _, c := range cases {
		remove, reason := decide(c.in)
		if remove != c.remove || reason != c.reason {
			t.Errorf("%s: %v %q, want %v %q", c.name, remove, reason, c.remove, c.reason)
		}
	}
}

// The candidates are the worktree lines ls shows with no session and
// nothing under them, on a listed host, of the repository given: not
// one with an agent, a home session, a pane or a run, not a main
// checkout, not a task's line, not one on a host still connecting.
func TestPruneCandidates(t *testing.T) {
	const src, other = "git@x:o/proj.git", "git@x:o/other.git"
	wt := func(env, branch, src string) protocol.Worktree {
		return protocol.Worktree{ID: env + "/worktree//w/" + branch, EnvironmentID: env, Repo: "proj", Branch: branch, Root: "/w/" + branch, Source: src}
	}
	withSession := wt("menv", "session", src)
	withSession.Session = "proj/session"
	main := wt("menv", "main", src)
	main.ID, main.Main = "menv/checkout//r/proj", true
	detached := wt("menv", "", src)
	detached.ID, detached.Root = "menv/worktree//w/d", "/w/d"
	elsewhere := wt("menv", "elsewhere", other)
	elsewhere.Repo = "other"
	m := merged.New()
	m.Apply(protocol.Message{Type: protocol.TypeSnapshot,
		Hosts: []protocol.HostStatus{
			{Name: "mac", EnvironmentID: "menv", Connected: true, Listed: true, Capabilities: []string{protocol.CapStatus, protocol.CapWorktrees, protocol.CapAttribution}},
			{Name: "vm", SSH: "vm", EnvironmentID: "venv", Connected: true, Capabilities: []string{protocol.CapStatus, protocol.CapWorktrees, protocol.CapAttribution}},
		},
		Worktrees: []protocol.Worktree{wt("menv", "plain", src), wt("menv", "agent", src), withSession, wt("menv", "pane", src), wt("menv", "run", src),
			main, detached, wt("menv", "task", src), elsewhere, wt("venv", "unlisted", src)},
		Agents: []protocol.Agent{{ID: "menv/default/%1", EnvironmentID: "menv", Server: "default", Session: "s", PaneID: "%1", Activity: protocol.Idle, Liveness: protocol.Alive, WorktreeID: "menv/worktree//w/agent"}},
		Panes:  []protocol.Pane{{ID: "menv/pane/default/%2", EnvironmentID: "menv", Server: "default", Session: "s", PaneID: "%2", Command: "zsh", WorktreeID: "menv/worktree//w/pane"}},
		Runs:   []protocol.Run{{ID: "menv/run/r1", EnvironmentID: "menv", Root: "/w/run", WorktreeID: "menv/worktree//w/run", Cmd: []string{"make"}}},
		Pendings: []protocol.Pending{{ID: "t1", Host: "mac", EnvironmentID: "menv", Source: src, Repo: "proj", Branch: "task", Root: "/w/task",
			Taken: true, Done: true, OK: true, Prompt: protocol.DeliveryNotDelivered}},
	})
	s := m.Status("")
	listed := func(h string) bool { return h == "mac" }
	names := func(items []pruneItem) []string {
		var out []string
		for _, it := range items {
			out = append(out, it.name+"@"+it.host)
		}
		slices.Sort(out)
		return out
	}
	cfg := config.Config{Repos: []config.Repo{{Name: "mine", Source: src}}}
	candidates := func(flag string) []string {
		t.Helper()
		match, ok := matchRepo(cfg, flag, s.Input.Worktrees)
		if !ok {
			t.Fatalf("--repo %q unknown", flag)
		}
		return names(pruneCandidates(rows.Tree(s.Input), listed, match))
	}
	if got := candidates(""); !slices.Equal(got, []string{"other/elsewhere@mac", "proj (detached) /w/d@mac", "proj/plain@mac"}) {
		t.Errorf("candidates: %q", got)
	}
	// By this machine's name for a repository, by the source, and, for
	// one this machine's config does not list, by a host's label or
	// its source.
	for _, flag := range []string{"mine", src} {
		if got := candidates(flag); !slices.Equal(got, []string{"proj (detached) /w/d@mac", "proj/plain@mac"}) {
			t.Errorf("candidates of %s: %q", flag, got)
		}
	}
	for _, flag := range []string{"other", other} {
		if got := candidates(flag); !slices.Equal(got, []string{"other/elsewhere@mac"}) {
			t.Errorf("candidates of %s: %q", flag, got)
		}
	}
	if _, ok := matchRepo(cfg, "nope", s.Input.Worktrees); ok {
		t.Error("--repo nope known")
	}
}

// prune end to end against a daemon that is the merging one and the
// host's: the plan names each worktree with no session with why it
// goes or stays; the facts are asked only for those the host decides,
// on the host's environment; -n removes nothing, nor does a no to the
// question or no terminal to ask in; --yes removes the ones the plan
// removes through rm, each held to the commit read and to nothing
// running there since and, with --branches, its branch with it; a
// local session opened since the plan keeps its worktree. A local
// session listing that failed, or records without attribution, keep
// every worktree, since what runs in them is not known.
func TestPrune(t *testing.T) {
	const src = "git@x:o/proj.git"
	wt := func(branch string) protocol.Worktree {
		return protocol.Worktree{ID: "lenv/worktree//w/proj/" + branch, EnvironmentID: "lenv", Repo: "proj", Branch: branch, Root: "/w/proj/" + branch, Source: src}
	}
	busy := wt("busy")
	busy.Session = "proj/busy"
	facts := map[string]protocol.RootFacts{
		"/w/proj/fresh":  {Branch: "fresh", Head: "f1", Base: "origin/main", Ignored: 2},
		"/w/proj/merged": {Branch: "merged", Head: "m1", Base: "origin/main", Ahead: 2, OnOrigin: true, Pushed: true},
		"/w/proj/wip":    {Branch: "wip", Head: "w1", Base: "origin/main", Changed: 2},
		"/w/proj/ahead":  {Branch: "ahead", Head: "a1", Base: "origin/main", Ahead: 1},
	}
	var mu sync.Mutex
	var asked [][]string
	var rms []protocol.Message
	refuse, sessionsErr := "", ""
	caps := []string{protocol.CapStatus, protocol.CapMerged, protocol.CapFollow, protocol.CapWorktrees, protocol.CapAttribution, protocol.CapRm, protocol.CapPrune}
	hostCaps := caps
	startFakeDaemon(t, caps, func(pc *protocol.Conn, m protocol.Message) bool {
		mu.Lock()
		defer mu.Unlock()
		switch m.Type {
		case protocol.TypeSubscribe:
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1, SessionsError: sessionsErr,
				Hosts:     []protocol.HostStatus{{Name: "mac", EnvironmentID: "lenv", Connected: true, Listed: true, Version: "fake", Capabilities: hostCaps}},
				Worktrees: []protocol.Worktree{wt("fresh"), wt("merged"), wt("wip"), wt("ahead"), wt("open"), busy},
				Sessions:  []protocol.Session{{Name: "mac/proj/open", Key: "lenv//w/proj/open", Host: "mac", Source: src, Branch: "open"}},
				BranchStatuses: []protocol.BranchStatus{{BranchKey: protocol.BranchKey{Source: source.Key(src), Branch: "merged"}, HeadOID: "m1",
					PR: &protocol.PullRequest{Number: 9, State: "merged", Base: "main"}}},
			})
		case protocol.TypeFacts:
			asked = append(asked, m.Roots)
			res := protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true}
			for _, root := range m.Roots {
				f := facts[root]
				f.Root = root
				res.Facts = append(res.Facts, f)
			}
			pc.Write(res)
		case protocol.TypeRm:
			rms = append(rms, m)
			if m.Root == refuse {
				pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, Error: "HEAD moved"})
				break
			}
			if m.DeleteBranch {
				pc.Write(protocol.Message{Type: protocol.TypeProgress, ID: m.ID, N: 1, Stage: protocol.StageBranch, State: protocol.StateDone, Detail: "deleted branch " + m.Branch})
			}
			pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Root: m.Root})
		}
		return true
	})
	cfgPath := filepath.Join(os.Getenv("LAATMUX_HOME"), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("hosts:\n  - name: mac\n    repos: /r\n    worktrees: /w\nrepos:\n  - "+src+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", cfgPath)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	was, wasLocals := confirm, pruneLocals
	t.Cleanup(func() { confirm, pruneLocals = was, wasLocals })
	var prompts []string
	answer := func(yes bool, err error) {
		confirm = func(_ context.Context, prompt string) (bool, error) {
			prompts = append(prompts, prompt)
			return yes, err
		}
	}
	run := func(args ...string) (string, error) {
		t.Helper()
		f, ferr := os.Create(filepath.Join(t.TempDir(), "stdout"))
		if ferr != nil {
			t.Fatal(ferr)
		}
		stdout := os.Stdout
		os.Stdout = f
		err := cmdPrune(context.Background(), args)
		os.Stdout = stdout
		f.Close()
		out, _ := os.ReadFile(f.Name())
		return string(out), err
	}
	sent := func() ([][]string, []protocol.Message) {
		mu.Lock()
		defer mu.Unlock()
		a, r := asked, rms
		asked, rms = nil, nil
		return a, r
	}
	plan := "" +
		"keep    proj/ahead (mac)                     1 commit ahead of origin/main, not on origin\n" +
		"remove  proj/fresh (mac)                     clean, 2 ignored files, no commits beyond origin/main\n" +
		"remove  proj/merged (mac)                    clean, PR #9 merged\n" +
		"keep    proj/open (mac)                      local session mac/proj/open is open\n" +
		"keep    proj/wip (mac)                       dirty: 2 changed files\n"

	answer(true, nil)
	out, err := run("-n")
	a, r := sent()
	if err != nil || out != plan || len(r) != 0 || len(prompts) != 0 {
		t.Fatalf("-n: %v %d rms, prompts %q\n%s", err, len(r), prompts, out)
	}
	if len(a) != 1 || !slices.Equal(a[0], []string{"/w/proj/ahead", "/w/proj/fresh", "/w/proj/merged", "/w/proj/wip"}) {
		t.Errorf("facts asked for %q", a)
	}

	answer(false, nil)
	out, err = run()
	if _, r = sent(); err != nil || out != plan+"nothing removed\n" || len(r) != 0 || len(prompts) != 1 || prompts[0] != "remove 2 worktrees? [y/N] " {
		t.Fatalf("answered no: %v %d rms, prompts %q\n%s", err, len(r), prompts, out)
	}
	answer(false, errors.New("not asking without a terminal"))
	if _, err = run("--branches"); err == nil || !strings.Contains(err.Error(), "without a terminal") {
		t.Fatalf("no terminal: %v", err)
	}
	if _, r = sent(); len(r) != 0 || prompts[1] != "remove 2 worktrees with their local branches? [y/N] " {
		t.Fatalf("no terminal: %d rms, prompts %q", len(r), prompts)
	}

	prompts = nil
	out, err = run("--yes", "--branches")
	_, r = sent()
	if err != nil || len(prompts) != 0 {
		t.Fatalf("--yes: %v, prompts %q\n%s", err, prompts, out)
	}
	if len(r) != 2 || r[0].Root != "/w/proj/fresh" || r[0].Head != "f1" || r[0].Branch != "fresh" || r[0].Repo != src || !r[0].Unused || !r[0].DeleteBranch ||
		r[1].Root != "/w/proj/merged" || r[1].Head != "m1" || !r[1].Unused || !r[1].DeleteBranch {
		t.Fatalf("rms %+v", r)
	}
	want := plan +
		"branch    done  deleted branch fresh\nremoved proj/fresh on mac (/w/proj/fresh)\n" +
		"branch    done  deleted branch merged\nremoved proj/merged on mac (/w/proj/merged)\n"
	if out != want {
		t.Errorf("--yes output:\n%s\nwant:\n%s", out, want)
	}

	// A removal refused is said, the others go on, and the command
	// fails with the count.
	mu.Lock()
	refuse = "/w/proj/fresh"
	mu.Unlock()
	out, err = run("--yes")
	if _, r = sent(); err == nil || err.Error() != "1 of 2 worktrees not removed" || len(r) != 2 || r[1].DeleteBranch {
		t.Fatalf("a refusal: %v, rms %+v", err, r)
	}
	if !strings.HasSuffix(out, "proj/fresh on mac not removed: HEAD moved\nremoved proj/merged on mac (/w/proj/merged)\n") {
		t.Errorf("a refusal:\n%s", out)
	}
	mu.Lock()
	refuse = ""
	mu.Unlock()

	// A local session opened for one since the plan keeps it, and is
	// said; nothing is sent for it.
	pruneLocals = func(context.Context) ([]protocol.Session, error) {
		return []protocol.Session{{Name: "mac/proj/merged", Key: "lenv//w/proj/merged"}}, nil
	}
	out, err = run("--yes")
	if _, r = sent(); err == nil || err.Error() != "1 of 2 worktrees not removed" || len(r) != 1 || r[0].Root != "/w/proj/fresh" {
		t.Fatalf("a local session since: %v, rms %+v", err, r)
	}
	if !strings.HasSuffix(out, "proj/merged on mac not removed: local session mac/proj/merged is open since the plan\n") {
		t.Errorf("a local session since:\n%s", out)
	}
	pruneLocals = func(context.Context) ([]protocol.Session, error) { return nil, errors.New("tmux: boom") }
	if out, err = run("--yes"); err == nil || !strings.Contains(out, "proj/fresh on mac not removed: local sessions not listed: tmux: boom\n") {
		t.Errorf("local sessions not listed before the removal: %v\n%s", err, out)
	}
	if _, r = sent(); len(r) != 0 {
		t.Errorf("sent with the local sessions not listed: %+v", r)
	}
	pruneLocals = wasLocals

	// What runs in the worktrees cannot be told: every one stays, and
	// the host is not asked.
	for _, k := range []struct {
		sessionsErr string
		caps        []string
		reason      string
	}{
		{"tmux: boom", caps, "local sessions not listed: tmux: boom"},
		{"", slices.DeleteFunc(slices.Clone(caps), func(c string) bool { return c == protocol.CapAttribution }),
			"the daemon on mac, fake, does not say which worktree each agent is in; laatmux upgrade mac installs one that does"},
	} {
		mu.Lock()
		sessionsErr, hostCaps = k.sessionsErr, k.caps
		mu.Unlock()
		out, err = run("--yes")
		a, r = sent()
		if err != nil || len(a) != 0 || len(r) != 0 || !strings.Contains(out, "keep    proj/fresh (mac)                     "+k.reason+"\n") || !strings.HasSuffix(out, "nothing to remove\n") {
			t.Errorf("%s: %v, facts %q, rms %+v\n%s", k.reason, err, a, r, out)
		}
	}
	mu.Lock()
	sessionsErr, hostCaps = "", caps
	mu.Unlock()

	if _, err := run("--yes", "--repo", "nope"); err == nil || !strings.Contains(err.Error(), `unknown repository "nope"`) {
		t.Errorf("--repo nope: %v", err)
	}
	if _, err := run("--host", "nope"); err == nil || !strings.Contains(err.Error(), `unknown host "nope"`) {
		t.Errorf("--host nope: %v", err)
	}
	if _, err := run("extra"); err == nil || !strings.Contains(err.Error(), "usage: laatmux prune") {
		t.Errorf("extra argument: %v", err)
	}
}

// The environment guards: facts asked of a host that answers as
// another machine than the one its records are of keep every worktree
// there, said, with nothing sent; and an rm, its facts read, goes only
// to the environment the records are of, refused before it is sent.
func TestPruneEnvironment(t *testing.T) {
	const src = "git@x:o/proj.git"
	var mu sync.Mutex
	var sent []protocol.Message
	caps := []string{protocol.CapStatus, protocol.CapMerged, protocol.CapFollow, protocol.CapWorktrees, protocol.CapAttribution, protocol.CapRm, protocol.CapPrune}
	startFakeDaemonAs(t, "lenv", caps, func(pc *protocol.Conn, m protocol.Message) bool {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, m)
		switch m.Type {
		case protocol.TypeSubscribe:
			// The host's records are of another machine than the one
			// that answers a dial now; vm is down.
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1,
				Hosts: []protocol.HostStatus{{Name: "mac", EnvironmentID: "oenv", Connected: true, Listed: true, Version: "fake", Capabilities: caps},
					{Name: "vm", SSH: "box", Error: "ssh: unreachable"}},
				Worktrees: []protocol.Worktree{{ID: "oenv/worktree//w/proj/fresh", EnvironmentID: "oenv", Repo: "proj", Branch: "fresh", Root: "/w/proj/fresh", Source: src}},
			})
		case protocol.TypeFacts:
			// Answered, so a guard that let the question through would
			// show in the plan rather than as a wait.
			pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true,
				Facts: []protocol.RootFacts{{Root: "/w/proj/fresh", Branch: "fresh", Head: "f1", Base: "origin/main"}}})
		case protocol.TypeRm:
			pc.Write(protocol.Message{Type: protocol.TypeResult, ID: m.ID, OK: true, Root: m.Root})
		}
		return true
	})
	cfgPath := filepath.Join(os.Getenv("LAATMUX_HOME"), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("hosts:\n  - name: mac\n    repos: /r\n    worktrees: /w\n  - name: vm\n    ssh: box\nrepos:\n  - "+src+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAATMUX_CONFIG", cfgPath)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	run := func(args ...string) (string, error) {
		t.Helper()
		f, ferr := os.Create(filepath.Join(t.TempDir(), "stdout"))
		if ferr != nil {
			t.Fatal(ferr)
		}
		stdout := os.Stdout
		os.Stdout = f
		err := cmdPrune(context.Background(), append([]string{"--yes"}, args...))
		os.Stdout = stdout
		f.Close()
		out, _ := os.ReadFile(f.Name())
		return string(out), err
	}
	commands := func() []string {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for _, m := range sent {
			if m.Type != protocol.TypeSubscribe && m.Type != protocol.TypeHello {
				out = append(out, m.Type)
			}
		}
		sent = nil
		return out
	}

	out, err := run()
	if err != nil || !strings.Contains(out, "keep    proj/fresh (mac)                     mac: answers as environment lenv, not oenv its worktrees were listed for\n") {
		t.Errorf("facts from another machine: %v\n%s", err, out)
	}
	if got := commands(); len(got) != 0 {
		t.Errorf("sent %q", got)
	}
	// --host by the ssh alias names the host the stream has by its
	// name: vm, which is down, said, and mac not looked at.
	out, err = run("--host", "box")
	if err != nil || out != "vm  DOWN  ssh: unreachable; its worktrees are not looked at\nno worktree without a session to look at\n" {
		t.Errorf("--host box: %v\n%s", err, out)
	}

	was := hostFacts
	t.Cleanup(func() { hostFacts = was })
	hostFacts = func(_ context.Context, _ peer.Host, env string, roots []string) ([]protocol.RootFacts, error) {
		return []protocol.RootFacts{{Root: roots[0], Branch: "fresh", Head: "f1", Base: "origin/main"}}, nil
	}
	out, err = run()
	if err == nil || !strings.Contains(out, "proj/fresh on mac not removed: mac: answers as environment lenv, not oenv the request was resolved for\n") {
		t.Errorf("rm to another machine: %v\n%s", err, out)
	}
	if got := commands(); len(got) != 0 {
		t.Errorf("sent %q", got)
	}
}

// The question is asked only on a terminal: from a pipe it is an error
// naming --yes, with nothing read.
func TestPruneConfirmNeedsTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	w.WriteString("y\n")
	stdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = stdin }()
	if ok, err := confirm(context.Background(), "remove? "); ok || err == nil || !strings.Contains(err.Error(), "give --yes") {
		t.Errorf("confirm from a pipe: %v %v", ok, err)
	}
}
