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
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/source"
)

// The decision table: what stays whatever the host reads (the main
// checkout, a detached HEAD, a branch laatmux cannot carry, an open
// local session), what stays for want of facts (a host without prune,
// a read that failed, a branch changed since the listing), and over
// the facts and the branch record: dirty stays; clean with nothing
// beyond the default branch goes; clean and ahead goes only with its
// PR merged at HEAD; anything else ahead stays, said with whether it
// is pushed and its PR.
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
	pr := func(state string, draft bool, head string) *protocol.BranchStatus {
		return &protocol.BranchStatus{HeadOID: head, PR: &protocol.PullRequest{Number: 7, State: state, Draft: draft}}
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
		{"no capability", pruneInput{Worktree: w, NoFacts: "the daemon on vm, v1, has no prune"}, false, "the daemon on vm, v1, has no prune"},
		{"read failed", pruneInput{Worktree: w, Facts: &protocol.RootFacts{Root: w.Root, Error: "git status: timeout"}}, false, "git status: timeout"},
		{"branch changed", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Branch = "other" })}, false, "the branch is other now"},
		{"detached since", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Branch = "" })}, false, "the branch is detached now"},
		{"dirty", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Changed = 3 })}, false, "dirty: 3 changed files"},
		{"dirty, one file", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Changed = 1 })}, false, "dirty: 1 changed file"},
		{"dirty and merged", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Changed = 1; f.Ahead = 2 }), Branch: pr("merged", false, "abc")}, false, "dirty: 1 changed file"},
		{"no base", pruneInput{Worktree: w, Facts: facts(func(f *protocol.RootFacts) { f.Base = "" })}, false, "no default branch to compare with: no origin/HEAD, main or master"},
		{"the default branch", pruneInput{Worktree: with(func(w *protocol.Worktree) { w.Branch = "main" }), Facts: facts(func(f *protocol.RootFacts) { f.Branch = "main" })}, false, "the default branch"},
		{"nothing ahead", pruneInput{Worktree: w, Facts: facts(nil)}, true, "clean, no commits beyond origin/main"},
		{"nothing ahead, PR open", pruneInput{Worktree: w, Facts: facts(nil), Branch: pr("open", false, "abc")}, true, "clean, no commits beyond origin/main"},
		{"merged at HEAD", pruneInput{Worktree: w, Facts: facts(ahead(2)), Branch: pr("merged", false, "abc")}, true, "clean, PR #7 merged"},
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
	if got := names(pruneCandidates(rows.Tree(s.Input), listed, config.Repo{})); !slices.Equal(got, []string{"other/elsewhere@mac", "proj (detached) /w/d@mac", "proj/plain@mac"}) {
		t.Errorf("candidates: %q", got)
	}
	if got := names(pruneCandidates(rows.Tree(s.Input), listed, config.Repo{Name: "proj", Source: src})); !slices.Equal(got, []string{"proj (detached) /w/d@mac", "proj/plain@mac"}) {
		t.Errorf("candidates of proj: %q", got)
	}
}

// prune end to end against a daemon that is the merging one and the
// host's: the plan names each worktree with no session with why it
// goes or stays; the facts are asked only for those the host decides,
// on the host's environment; -n removes nothing, nor does a no to the
// question or no terminal to ask in; --yes removes the ones the plan
// removes through rm, each held to the commit read and, with
// --branches, its branch with it.
func TestPrune(t *testing.T) {
	const src = "git@x:o/proj.git"
	wt := func(branch string) protocol.Worktree {
		return protocol.Worktree{ID: "lenv/worktree//w/proj/" + branch, EnvironmentID: "lenv", Repo: "proj", Branch: branch, Root: "/w/proj/" + branch, Source: src}
	}
	busy := wt("busy")
	busy.Session = "proj/busy"
	facts := map[string]protocol.RootFacts{
		"/w/proj/fresh":  {Branch: "fresh", Head: "f1", Base: "origin/main"},
		"/w/proj/merged": {Branch: "merged", Head: "m1", Base: "origin/main", Ahead: 2, OnOrigin: true, Pushed: true},
		"/w/proj/wip":    {Branch: "wip", Head: "w1", Base: "origin/main", Changed: 2},
		"/w/proj/ahead":  {Branch: "ahead", Head: "a1", Base: "origin/main", Ahead: 1},
	}
	var mu sync.Mutex
	var asked [][]string
	var rms []protocol.Message
	refuse := ""
	caps := []string{protocol.CapStatus, protocol.CapMerged, protocol.CapFollow, protocol.CapWorktrees, protocol.CapRm, protocol.CapPrune}
	startFakeDaemon(t, caps, func(pc *protocol.Conn, m protocol.Message) bool {
		mu.Lock()
		defer mu.Unlock()
		switch m.Type {
		case protocol.TypeSubscribe:
			pc.Write(protocol.Message{Type: protocol.TypeSnapshot, Seq: 1,
				Hosts:     []protocol.HostStatus{{Name: "mac", EnvironmentID: "lenv", Connected: true, Listed: true, Version: "fake", Capabilities: caps}},
				Worktrees: []protocol.Worktree{wt("fresh"), wt("merged"), wt("wip"), wt("ahead"), wt("open"), busy},
				Sessions:  []protocol.Session{{Name: "mac/proj/open", Key: "lenv//w/proj/open", Host: "mac", Source: src, Branch: "open"}},
				BranchStatuses: []protocol.BranchStatus{{BranchKey: protocol.BranchKey{Source: source.Key(src), Branch: "merged"}, HeadOID: "m1",
					PR: &protocol.PullRequest{Number: 9, State: "merged"}}},
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
			pc.Write(protocol.Message{Type: protocol.TypeProgress, ID: m.ID, N: 1, Stage: protocol.StageBranch, State: protocol.StateDone, Detail: "deleted branch " + m.Branch})
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
	was := confirm
	t.Cleanup(func() { confirm = was })
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
		"remove  proj/fresh (mac)                     clean, no commits beyond origin/main\n" +
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
	if len(r) != 2 || r[0].Root != "/w/proj/fresh" || r[0].Head != "f1" || r[0].Branch != "fresh" || r[0].Repo != src || !r[0].DeleteBranch ||
		r[1].Root != "/w/proj/merged" || r[1].Head != "m1" || !r[1].DeleteBranch {
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
	if !strings.HasSuffix(out, "proj/fresh on mac not removed: HEAD moved\nbranch    done  deleted branch merged\nremoved proj/merged on mac (/w/proj/merged)\n") {
		t.Errorf("a refusal:\n%s", out)
	}
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
