package view

import (
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
	"github.com/laat/laatmux/internal/term"
)

// The parser: tokens, styles and the fill; an unknown token, an unclosed
// brace or style, an unknown style item or colour, and a second fill
// are errors naming the column.
func TestParseTemplate(t *testing.T) {
	for _, src := range []string{
		"", "plain text", "{primary}", "{stripe} {status_icon} {primary} {pane_suffix}{fill}{elapsed}",
		"#[fg=accent,bold]{repo}#[default] x #[bg=#112233,dim,nobold,nodim]{host}", "#[fg=colour42]{idx}",
		DefaultTile1, DefaultTile2, DefaultTile3, DefaultCompact, DefaultTop1, DefaultTop2, DefaultTop3, DefaultRepo, DefaultWorktree, DefaultAgent, DefaultPane, DefaultRun,
	} {
		if _, err := ParseTemplate(src); err != nil {
			t.Errorf("%q: %v", src, err)
		}
	}
	for _, c := range []struct{ src, want string }{
		{"{primary} {nope}", "unknown token {nope} at column 11"},
		{"{primary", "unclosed { at column 1"},
		{"a #[fg=accent", "unclosed #[ at column 3"},
		{"#[blink]{primary}", `unknown style "blink" at column 1`},
		{"#[fg=mauve]{primary}", `unknown colour "mauve" at column 1`},
		{"{fill}{primary}{fill}", "a second {fill} at column 16"},
	} {
		_, err := ParseTemplate(c.src)
		if err == nil || err.Error() != c.want {
			t.Errorf("%q: %v, want %s", c.src, err, c.want)
		}
	}
	if c := Compile("tiles[0]", "{nope}", ""); c.Err != "template error: unknown token {nope} at column 1 in tiles[0]" {
		t.Errorf("compile: %q", c.Err)
	}
	if c := Compile("compact", "", DefaultCompact); c.Err != "" || c.src != DefaultCompact {
		t.Errorf("the default for an empty source: %+v", c)
	}
	// The default set parses whole, and a nil tiles list is the three.
	d := DefaultTemplates()
	if len(d.Tiles) != 3 || len(d.Top) != 3 {
		t.Fatalf("default tiles: %d top: %d", len(d.Tiles), len(d.Top))
	}
	for _, c := range append(append([]Compiled{d.Compact, d.Tree.Repo, d.Tree.Worktree, d.Tree.Agent, d.Tree.Pane, d.Tree.Run}, d.Tiles...), d.Top...) {
		if c.Err != "" {
			t.Error(c.Err)
		}
	}
}

// tokenRow is a row with something for every token: a worktree with
// git stats and a PR, an agent with a title, numbered (2).
func tokenRow(now time.Time) rows.Row {
	yes := true
	w := &protocol.Worktree{ID: "venv/worktree//r/fix-ls", EnvironmentID: "venv", Repo: "laatmux", Source: "git@github.com:laat/laatmux.git", Branch: "fix-ls", Root: "/r/fix-ls", Session: "laatmux/fix-ls",
		Git: &protocol.GitStatus{Base: "origin/main", Committed: [2]int{46, 11}, Uncommitted: [2]int{28, 3}, Dirty: true, Ahead: 2, Behind: 1, Conflict: &yes, Rebasing: true}}
	a := &protocol.Agent{ID: "venv/laatmux/%1", EnvironmentID: "venv", Server: "laatmux", Session: "laatmux/fix-ls", Window: 2, PaneID: "%1", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now.Add(-2 * time.Minute), Liveness: protocol.Alive, Managed: true, Title: "✳ Permission to run pnpm test"}
	return rows.Row{Kind: rows.KindTile, Host: "vm", Name: "laatmux/fix-ls", Node: a.ID, Worktree: w, Agent: a, Suffix: "(2)", Children: 1,
		Branch: &protocol.BranchStatus{PR: &protocol.PullRequest{Number: 52, State: "open"}, Checks: &protocol.Checks{State: protocol.ChecksFailure, Passed: 3, Total: 5, Failing: "test (macos-latest)"}}}
}

// Every token on the tile row, and what the tree-only ones give there.
func TestTokens(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	m := &Model{Now: now, LocalHost: "mac", Width: 80, AgentIcons: map[string]AgentIcon{"claude": {Icon: "CL"}}, JumpKeys: true}
	r := tokenRow(now)
	for _, c := range []struct{ token, want string }{
		{"primary", "fix-ls"}, {"secondary", "laatmux"}, {"branch", "fix-ls"}, {"repo", "laatmux"}, {"host", "vm"},
		{"session", "laatmux/fix-ls"}, {"window", "laatmux/fix-ls:2"}, {"window_index", "2"}, {"pane_title", "Permission to run pnpm test"}, {"pane_suffix", "(2)"},
		{"status_icon", "💬"}, {"status_label", "waiting"}, {"agent_icon", "CL"}, {"agent_label", "claude"}, {"elapsed", "2:00"}, {"stripe", "▌"},
		{"git_stats", "R +46 -11 ✎ +28 -3"}, {"git_committed", "+46 -11"}, {"git_uncommitted", "✎ +28 -3"}, {"git_ahead", "↑2"}, {"git_behind", "↓1"},
		{"git_dirty", "✎"}, {"git_conflict", "!"}, {"git_rebase", "R"}, {"git_branch", "origin/main"}, {"git_sync", "! ↑2 ↓1"},
		{"pr_number", "#52"}, {"pr_checks", "× 3/5"}, {"pr_state", "●"}, {"pr_detail", "test (macos-latest)"}, {"idx", "2"}, {"jump_key", "M-2"},
		{"repo_count", ""}, {"fold", ""}, {"worst_status", ""}, {"child_count", ""}, {"command", ""}, {"indent", ""},
	} {
		tm, err := ParseTemplate("{" + c.token + "}")
		if err != nil {
			t.Fatal(err)
		}
		got := strings.TrimSpace(Text([]Line{{Spans: m.line(Compiled{Template: tm}, r, 80, 2)}}))
		if got != c.want {
			t.Errorf("{%s} = %q, want %q", c.token, got, c.want)
		}
	}
	// The agent's own icon colour, and the default icon for an agent
	// the config leaves alone.
	m.AgentIcons = nil
	tm, _ := ParseTemplate("{agent_icon}")
	if sp := m.line(Compiled{Template: tm}, r, 80, 2); len(sp) != 1 || sp[0].Text != "CC" || sp[0].Fg != "#d97757" {
		t.Errorf("the default agent icon: %+v", sp)
	}
	// On the viewer's own row the pane's suffix is the viewer's label,
	// as the primary label is: on a dim line both keep their colour and
	// are not faint.
	cur := r
	cur.Current = true
	tm, _ = ParseTemplate("{primary} {pane_suffix}")
	dark, _ := palette.New(true, nil)
	kept := "\x1b[1m" + dark.SGR(palette.CurrentWorktreeFg, false)
	if got := ANSI(Line{Dim: true, Spans: m.line(Compiled{Template: tm}, cur, 80, 2)}, dark); !strings.Contains(got, kept+"fix-ls") || !strings.Contains(got, kept+"(2)") || strings.Contains(got, "\x1b[2") {
		t.Errorf("the viewer's label and suffix on a dim line: %q", got)
	}
	// Tree tokens on tree nodes.
	in := treeInput(now)
	tree := &Model{Now: now, LocalHost: "mac", View: ViewTree, Width: 80, Height: 30}
	tree.SetTree(rows.Tree(in))
	tree.SetRows(rows.Agents(in, rows.Tree(in)))
	tree.Render() // the first folds decided: agents-config open, its agent working
	at := func(id string) rows.Row { return tree.Tree[tree.indexOf(id)] }
	for _, c := range []struct {
		id, token, want string
	}{
		{rows.RepoNode("git@github.com:laat/laatmux.git"), "repo_count", "4"}, {rows.RepoNode("git@github.com:laat/laatmux.git"), "fold", ""}, {rows.RepoNode("git@github.com:laat/laatmux.git"), "primary", "laatmux"},
		{"venv/worktree//r/agents-config", "fold", "▾"}, {"venv/worktree//r/agents-config", "child_count", "4"}, {"venv/worktree//r/agents-config", "worst_status", ""},
		{"venv/worktree//r/agents-config", "indent", ""}, {"venv/worktree//r/agents-config", "status_label", ""},
		{"venv/pane/laatmux/%7", "command", "zsh"}, {"venv/pane/laatmux/%7", "indent", ""}, {"venv/pane/laatmux/%7", "session", "laatmux/agents-config"},
		{"venv/run/r1", "command", "make test"}, {"venv/run/r1", "elapsed", "0:42"},
		{"venv/laatmux/%2", "pane_title", "Reading the config"}, {"venv/laatmux/%2", "status_label", "idle"}, {"venv/laatmux/%2", "stripe", ""},
		{"session/mac/laatmux/gone", "status_label", "worktree gone"}, {"session/mac/laatmux/gone", "primary", "mac/laatmux/gone"},
	} {
		tm, _ := ParseTemplate("{" + c.token + "}")
		got := strings.TrimSpace(Text([]Line{{Spans: tree.line(Compiled{Template: tm}, at(c.id), 80, 0)}}))
		if got != c.want {
			t.Errorf("%s {%s} = %q, want %q", c.id, c.token, got, c.want)
		}
	}
	// The pane's indent is its depth; a folded line shows the worst
	// agent's icon.
	tm, _ = ParseTemplate("{indent}|")
	if got := Text([]Line{{Spans: tree.line(Compiled{Template: tm}, at("venv/pane/laatmux/%7"), 80, 0)}}); !strings.HasPrefix(got, "    |") {
		t.Errorf("a pane's indent: %q", got)
	}
	// A worktree line's host is the worktree's, not its agent's server;
	// a tile's carries the server.
	in.Worktrees[1].Session = ""
	in.Agents[3].Server, in.Agents[3].Managed = "default", false
	tree.SetTree(rows.Tree(in))
	tree.SetRows(rows.Agents(in, rows.Tree(in)))
	tm, _ = ParseTemplate("({host})")
	if got := Text([]Line{{Spans: tree.line(Compiled{Template: tm}, at("venv/worktree//r/auto-layout"), 80, 0)}}); got != "(vm)\n" {
		t.Errorf("a worktree line's host: %q", got)
	}
	for _, r := range tree.Rows.Main {
		if r.ID() == "venv/laatmux/%8" {
			if got := Text([]Line{{Spans: tree.line(Compiled{Template: tm}, r, 80, 0)}}); got != "(vm/default)\n" {
				t.Errorf("a tile's host: %q", got)
			}
		}
	}
	in = treeInput(now)
	tree.SetTree(rows.Tree(in))
	tree.SetRows(rows.Agents(in, rows.Tree(in)))
	// {idx} through Render counts as the digits do.
	tree.SetTemplates(CompileTemplates(nil, "", nil, "", "{indent}{fold}{idx}:{primary}", "{indent}{idx}:{agent_label}", "", ""))
	tree.Height = 30
	text := Text(tree.Render())
	for i, want := range []string{"1:batch-processing", "2:agents-config", "3:auto-layout", "4:fix-sidebar", "5:mac/laatmux/gone"} {
		if !strings.Contains(text, want) {
			t.Errorf("row %d not numbered %q:\n%s", i+1, want, text)
		}
	}
	if strings.Contains(text, "1:claude") || strings.Contains(text, "2:claude") {
		t.Errorf("an agent line numbered:\n%s", text)
	}
	tree.SetTemplates(DefaultTemplates())
	tree.Select("venv/worktree//r/agents-config")
	tree.Handle(term.Key{Rune: 'h'})
	tm, _ = ParseTemplate("{worst_status}")
	if got := strings.TrimSpace(Text([]Line{{Spans: tree.line(Compiled{Template: tm}, at("venv/worktree//r/agents-config"), 80, 0)}})); got == "" {
		t.Error("a folded line has no worst status")
	}
}

// Styles hold until the next one and leave a token's own colours; a
// dim token with no colour of its own, a stale one among them, takes
// the background alone; a background is drawn; the
// fill puts the right part against the edge; an empty token takes its
// separator with it; a template that did not parse draws its error.
func TestTemplateStyles(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	m := &Model{Now: now, LocalHost: "mac"}
	r := tokenRow(now)
	render := func(src string, w int) string {
		t.Helper()
		tm, err := ParseTemplate(src)
		if err != nil {
			t.Fatal(err)
		}
		return Debug([]Line{{Spans: m.line(Compiled{Template: tm}, r, w, 0)}})
	}
	if got := render("#[fg=accent,bold]{secondary}#[default] {pr_number} #[bg=#112233]{host}", 40); got != "...|«⟨accent:laatmux⟩» «⟨success:#52⟩» ‹⟦#112233:vm⟧›\n" {
		t.Errorf("styles: %q", got)
	}
	if got := render("{primary}{fill}{elapsed}", 20); got != "...|fix-ls          2:00\n" {
		t.Errorf("fill: %q", got)
	}
	// The fill's padding takes the style in force.
	if got := render("#[bg=#112233]{primary}{fill}{elapsed}", 14); got != "...|⟦#112233:fix-ls⟧⟦#112233:    ⟧⟦#112233:2:00⟧\n" {
		t.Errorf("a styled fill: %q", got)
	}
	// The partial mark on the uncommitted count, as in the stats.
	r.Worktree.Git.UncommittedPartial = true
	if got := Text([]Line{{Spans: m.line(mustParse(t, "{git_uncommitted} | {git_stats}"), r, 60, 2)}}); got != "✎ +28+ -3 | R +46 -11 ✎ +28+ -3\n" {
		t.Errorf("the partial mark: %q", got)
	}
	r.Worktree.Git.UncommittedPartial = false
	r.Suffix = ""
	if got := render("a {pane_suffix}  b  {repo_count} c", 40); got != "...|a b  c\n" {
		t.Errorf("empty tokens: %q", got)
	}
	if got := render("{stripe}    {repo_count}{fill}{repo_count} {repo_count}", 20); got != "...|⟨accent:▌⟩\n" {
		t.Errorf("all empty: %q", got)
	}
	// A stale answer marks the pair once, on the checks; the number
	// alone carries it. Dim and plain: the open PR's bold goes too.
	r.Branch.Stale = true
	if got := render("{pr_number} {pr_checks}", 40); got != "...|‹#52› ‹×›‹ 3/5›‹?›\n" {
		t.Errorf("a stale pair: %q", got)
	}
	if got := render("{pr_number}", 40); got != "...|‹#52›‹?›\n" {
		t.Errorf("a stale number without the checks drawn: %q", got)
	}
	if got := render("{pr_number}{fill}{pr_checks} {host}", 5); got != "...|‹#52›‹?›\n" {
		t.Errorf("a stale number with the checks dropped: %q", got)
	}
	// Of a one-digit number and the check mark with the stale mark,
	// equally wide, the number goes first: the checks keep the mark,
	// where the number would take it and grow past the room.
	r.Branch.Checks = &protocol.Checks{State: protocol.ChecksSuccess}
	r.Branch.PR.Number = 7
	if got := render(DefaultTile3, 13); got != "...|⟨accent:▌⟩    Perm… ‹✓›‹?›\n" {
		t.Errorf("the number before the checks at 13: %q", got)
	}
	if got := render(DefaultTile3, 12); got != "...|⟨accent:▌⟩    Per… ‹✓›‹?›\n" {
		t.Errorf("the number before the checks at 12: %q", got)
	}
	if got := render("{pane_title}{fill}{pr_number}", 8); got != "...|Per… ‹#7›‹?›\n" {
		t.Errorf("the number alone with its mark: %q", got)
	}
	// Whatever the width, a PR number drawn on a stale row has its mark,
	// on the checks or on itself; the mark takes the template's
	// background.
	for _, src := range []string{DefaultTile3, DefaultWorktree, "{fill}{pr_number} {pr_checks}", "#[bg=#112233]{primary}{fill}{pr_number} {pr_checks}", "{host} {pr_number}", "{pr_number} {pr_checks} {host}", "{pr_checks} {pr_number}{fill}{host}"} {
		for _, st := range []string{protocol.ChecksSuccess, protocol.ChecksFailure, protocol.ChecksPending} {
			r.Branch.Checks = &protocol.Checks{State: st, Passed: 3, Total: 5}
			for w := 3; w <= 40; w++ {
				got := Text([]Line{{Spans: m.line(mustParse(t, src), r, w, 0)}})
				if strings.Contains(got, "#7") && !strings.Contains(got, "?") {
					t.Errorf("%q at %d: %q without the stale mark", src, w, got)
				}
			}
		}
	}
	r.Branch.Checks = &protocol.Checks{State: protocol.ChecksSuccess}
	if got := render("{fill}{pr_number} {pr_checks}", 5); got != "...|‹#7› ‹✓›‹?›\n" {
		t.Errorf("the number's mark not counted while the checks stand: %q", got)
	}
	if got := render("#[bg=#112233]{primary}{fill}{pr_number} {pr_checks}", 6); got != "...|⟦#112233:fi…⟧⟦#112233: ⟧‹⟦#112233:✓⟧›‹⟦#112233:?⟧›\n" {
		t.Errorf("the mark styled: %q", got)
	}
	if got := render("#[bg=#112233]{pr_number}{fill}{pr_checks} {host}", 4); got != "...|‹⟦#112233:#7⟧›‹⟦#112233:?⟧›\n" {
		t.Errorf("the mark put back styled: %q", got)
	}
	// Stale wins over a style: a stale token takes the style's
	// background, not its colour or bold, which would make it read
	// fresh; the put-back ? looks as the number's own.
	for _, c := range []struct{ src, want string }{
		{"#[bold]{pr_number}", "...|‹#7›‹?›\n"},
		{"#[fg=accent]{pr_number}", "...|‹#7›‹?›\n"},
		{"#[fg=accent,bold,bg=#112233]{pr_number}", "...|‹⟦#112233:#7⟧›‹⟦#112233:?⟧›\n"},
		{"#[fg=accent,bold,bg=#112233]{pr_number}{fill}{pr_checks} {host}", "...|‹⟦#112233:#7⟧›‹⟦#112233:?⟧›\n"},
	} {
		if got := render(c.src, 4); got != c.want {
			t.Errorf("stale under %q: %q, want %q", c.src, got, c.want)
		}
	}
	own := m.line(mustParse(t, "#[fg=accent,bold,bg=#112233]{pr_number}"), r, 4, 0)
	back := m.line(mustParse(t, "#[fg=accent,bold,bg=#112233]{pr_number}{fill}{pr_checks} {host}"), r, 4, 0)
	if len(own) != 2 || len(back) != 2 || own[1] != back[1] {
		t.Errorf("the put-back mark %+v, the number's own %+v", back, own)
	}
	// Every PR token of a stale answer, at every width, the shrunk
	// checks and the put-back mark among them.
	r.Branch.FetchedAt = now.Add(-time.Minute)
	for _, c := range []struct {
		ch   *protocol.Checks
		want string
	}{
		{&protocol.Checks{State: protocol.ChecksFailure, Passed: 3, Total: 5, Failing: "test (macos-latest)"}, "● #7 × 3/5? test (macos-latest)"},
		{&protocol.Checks{State: protocol.ChecksPending, Passed: 3, Total: 5, PendingSince: now.Add(-5 * time.Minute)}, "● #7 ⠋ 3/5? 4:00"},
	} {
		r.Branch.Checks = c.ch
		staleStyled(t, m, r, "#[fg=accent,bold,bg=#112233]{pr_state} {pr_number} {pr_checks} {pr_detail}", c.want)
	}
	r.Branch.PR.Number = 52
	r.Branch.Checks = nil
	if got := render("{pr_number} {pr_checks}", 40); got != "...|‹#52›‹?›\n" {
		t.Errorf("a stale number alone: %q", got)
	}
	r = tokenRow(now)
	// A run of spaces a style splits still goes with an empty token.
	r.Suffix = ""
	if got := render("a {pane_suffix} #[fg=accent] b", 20); got != "...|a ⟨accent:b⟩\n" {
		t.Errorf("a split run: %q", got)
	}
	if got := render("a #[fg=accent] {pane_suffix}", 20); got != "...|a\n" {
		t.Errorf("a split run before: %q", got)
	}
	r = tokenRow(now)
	// A dim token with no colour of its own takes a style's background
	// alone, as a stale one does: a remote host and a draft's number on
	// a chip are not drawn in the chip's colour or bold, which would
	// read as a local host and an open PR; the literal between them is.
	r.Branch.PR.Draft = true
	if got := render("#[fg=#000000,bg=#ffff00]{host} {pr_number}", 40); got != "...|‹⟦#ffff00:vm⟧›⟦#ffff00:⟨#000000: ⟩⟧‹⟦#ffff00:#52⟧›\n" {
		t.Errorf("a remote host and a draft on a chip: %q", got)
	}
	r.Branch.PR.Draft = false
	for _, c := range []struct{ src, local, want string }{
		{"#[fg=accent]{host}", "mac", "...|‹vm›\n"},
		{"#[fg=accent]{host}", "vm", "...|⟨accent:vm⟩\n"},
		{"#[bold]{host}", "mac", "...|‹vm›\n"},
		{"#[bold]{host}", "vm", "...|«vm»\n"},
	} {
		m.LocalHost = c.local
		if got := render(c.src, 40); got != c.want {
			t.Errorf("%q with %s local: %q, want %q", c.src, c.local, got, c.want)
		}
	}
	m.LocalHost = "mac"
	// A dim token's own colour wins over dim: a closed PR's red and the
	// committed counts' green take the style's background and bold as
	// any coloured token does.
	r.Branch.PR.State = "closed"
	if got := render("#[fg=#000000,bg=#ffff00,bold]{pr_number} {git_committed}", 40); got != "...|‹«⟦#ffff00:⟨danger:#52⟩⟧»›«⟦#ffff00:⟨#000000: ⟩⟧»‹«⟦#ffff00:⟨success:+46⟩⟧»›«⟦#ffff00:⟨#000000: ⟩⟧»‹«⟦#ffff00:⟨danger:-11⟩⟧»›\n" {
		t.Errorf("own colours on a chip: %q", got)
	}
	r = tokenRow(now)
	// A fresh base takes a style's colour and bold, as a label does.
	if got := render("#[fg=accent,bold]{git_branch}", 20); got != "...|«⟨accent:origin/main⟩»\n" {
		t.Errorf("a fresh base styled: %q", got)
	}
	// The single git tokens are stale as the stats are.
	r.Worktree.Git.Stale = true
	if got := render("{git_rebase} {git_conflict} {git_ahead}", 20); got != "...|‹R› ‹!› ‹↑2›\n" {
		t.Errorf("stale git tokens: %q", got)
	}
	// Stale wins over a style, as on the PR tokens.
	if got := render("#[fg=accent]{git_stats}", 40); got != "...|‹R›‹ ›‹+46›‹ ›‹-11›‹ ›‹✎›‹ ›‹+28›‹ ›‹-3›\n" {
		t.Errorf("stale stats under a colour: %q", got)
	}
	r.Worktree.Git.Base = "origin/feature-long-base"
	staleStyled(t, m, r, "#[fg=accent,bold,bg=#112233]{git_stats} {git_sync} {git_rebase} {git_conflict}", "R +46 -11 ✎ +28 -3 →feature-lo… ! ↑2 ↓1 R !")
	// The base whole and cut, as {git_sync} draws it on the same row.
	staleStyled(t, m, r, "#[fg=accent,bold,bg=#112233]{git_sync} {git_branch}", "→feature-lo… ! ↑2 ↓1 origin/feature-long-base")
	// Every git token alone, one added later among them.
	for name := range tokens {
		if !strings.HasPrefix(name, "git_") {
			continue
		}
		src := "#[fg=accent,bold,bg=#112233]{" + name + "}"
		want := strings.TrimSpace(Text([]Line{{Spans: m.line(mustParse(t, src), r, 60, 0)}}))
		if want == "" {
			t.Errorf("{%s} draws nothing on the token row", name)
			continue
		}
		staleStyled(t, m, r, src, want)
	}
	r = tokenRow(now)
	// fg=default clears the colour, as tmux spells it.
	if got := render("#[fg=accent]a#[fg=default]b", 10); got != "...|⟨accent:a⟩b\n" {
		t.Errorf("fg=default: %q", got)
	}
	c := Compile("tiles[1]", "{nope}", "")
	if got := Debug([]Line{{Spans: m.line(c, r, 60, 2)}}); got != "...|⟨danger:template error: unknown token {nope} at column 1 in tiles[1]⟩\n" {
		t.Errorf("an error drawn: %q", got)
	}
}

// Dim text with no colour of its own on a template's background is
// drawn in the dim colour that reads on it, not faint in the
// terminal's: on the lightest backgrounds the dark theme's, on the
// darkest the light theme's, whatever the theme, and on those between
// the background half way to white or to black, for a remote host, a
// draft's number and a stale token under a chip's colour, and dim
// literal text, alike; a palette name through the theme, an indexed
// colour by xterm's; faint on a colour 0 to 15. Where no template
// background is drawn, under the selection's band, on a dim line, on a
// strip chip's band and without colours, the line is drawn as it is
// without one.
func TestDimOnBackground(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	m := &Model{Now: now, LocalHost: "mac"}
	dark, _ := palette.New(true, nil)
	light, _ := palette.New(false, nil)
	guessed := dark
	guessed.Guessed = true
	darkDim, lightDim := dark.SGR("#565f89", false), dark.SGR("#8990b3", false)
	yellow, black := dark.SGR("#ffff00", true), dark.SGR("#000000", false)
	draw := func(r rows.Row, src string, l Line, th palette.Theme) string {
		t.Helper()
		l.Spans = m.line(mustParse(t, src), r, 40, 0)
		return ANSI(l, th)
	}
	r := tokenRow(now)
	r.Branch.PR.Draft = true
	want := darkDim + yellow + "vm\x1b[0m" + yellow + " \x1b[0m" + darkDim + yellow + "#52\x1b[0m" + yellow + " \x1b[0m" + darkDim + yellow + "x\x1b[0m\x1b[0m"
	for _, th := range []palette.Theme{dark, light, guessed} {
		if got := draw(r, "#[bg=#ffff00]{host} {pr_number} #[dim]x", Line{}, th); got != want {
			t.Errorf("a host, a draft and dim text on yellow:\n%q, want\n%q", got, want)
		}
	}
	// Stale under a chip's colour: the background alone, and the dimmed
	// colour on it, the mark too.
	r = tokenRow(now)
	r.Branch.Stale, r.Worktree.Git.Stale = true, true
	want = darkDim + yellow + "#52\x1b[0m" + darkDim + yellow + "?\x1b[0m" + black + yellow + " \x1b[0m" + darkDim + yellow + "↑2\x1b[0m\x1b[0m"
	if got := draw(r, "#[fg=#000000,bg=#ffff00]{pr_number} {git_ahead}", Line{}, dark); got != want {
		t.Errorf("stale tokens on a chip:\n%q, want\n%q", got, want)
	}
	// A remote host and a draft on the same chip, as the stale tokens:
	// on one chip every dim token reads one way.
	r = tokenRow(now)
	r.Branch.PR.Draft = true
	want = darkDim + yellow + "vm\x1b[0m" + black + yellow + " \x1b[0m" + darkDim + yellow + "#52\x1b[0m\x1b[0m"
	if got := draw(r, "#[fg=#000000,bg=#ffff00]{host} {pr_number}", Line{}, dark); got != want {
		t.Errorf("a host and a draft on a chip:\n%q, want\n%q", got, want)
	}
	r = tokenRow(now)
	for _, c := range []struct {
		src  string
		th   palette.Theme
		want string
	}{
		{"#[bg=#112233]{host}", dark, lightDim + dark.SGR("#112233", true) + "vm\x1b[0m\x1b[0m"},
		{"#[bg=#112233]{host}", light, lightDim + dark.SGR("#112233", true) + "vm\x1b[0m\x1b[0m"},
		{"#[bg=highlight_row_bg]{host}", light, darkDim + light.SGR(palette.HighlightRowBg, true) + "vm\x1b[0m\x1b[0m"},
		{"#[bg=230]{host}", dark, darkDim + "\x1b[48;5;230mvm\x1b[0m\x1b[0m"},
		// Between, where a half reads better than either dimmed, the
		// background's own half: lighter on the dark highlight row and
		// colour235, on the light theme's accent and darker on
		// colour243.
		{"#[bg=highlight_row_bg]{host}", dark, dark.SGR("#9399ab", false) + dark.SGR(palette.HighlightRowBg, true) + "vm\x1b[0m\x1b[0m"},
		{"#[bg=colour235]{host}", dark, dark.SGR("#929292", false) + "\x1b[48;5;235mvm\x1b[0m\x1b[0m"},
		{"#[bg=accent]{host}", light, light.SGR("#bba3de", false) + light.SGR(palette.Accent, true) + "vm\x1b[0m\x1b[0m"},
		{"#[bg=colour243]{host}", light, light.SGR("#3b3b3b", false) + "\x1b[48;5;243mvm\x1b[0m\x1b[0m"},
		{"#[bg=colour3]{host}", dark, "\x1b[2m\x1b[48;5;3mvm\x1b[0m\x1b[0m"},
		// Bold keeps its bold.
		{"#[bold,dim,bg=#ffff00]vm", dark, "\x1b[1m" + darkDim + yellow + "vm\x1b[0m\x1b[0m"},
		// A colour of the span's own is drawn as it is.
		{"#[fg=accent,dim,bg=#ffff00]x", dark, dark.SGR(palette.Accent, false) + yellow + "x\x1b[0m\x1b[0m"},
	} {
		if got := draw(r, c.src, Line{}, c.th); got != c.want {
			t.Errorf("%s: %q, want %q", c.src, got, c.want)
		}
	}
	// Off where no template background is drawn: the line as it is
	// without the background.
	r.Branch.Stale = true
	src := "#[bg=#ffff00]{host} {pr_number} #[dim]x"
	chip := func(l Line) Line {
		for i := range l.Spans {
			l.Spans[i].band = true
		}
		return l
	}
	for _, c := range []struct {
		name string
		l    Line
		band bool
		th   palette.Theme
	}{
		{"under the band", Line{Reverse: true}, false, dark},
		{"under reverse video", Line{Reverse: true}, false, guessed},
		{"on a dim line", Line{Dim: true}, false, dark},
		{"on a chip's band", Line{}, true, dark},
		{"on a chip's reverse band", Line{}, true, guessed},
		{"without colours", Line{}, false, palette.Mono()},
	} {
		with, without := c.l, c.l
		with.Spans = m.line(mustParse(t, src), r, 40, 0)
		without.Spans = m.line(mustParse(t, "{host} {pr_number} #[dim]x"), r, 40, 0)
		if c.band {
			with, without = chip(with), chip(without)
		}
		if got, want := ANSI(with, c.th), ANSI(without, c.th); got != want {
			t.Errorf("%s: %q, want %q", c.name, got, want)
		}
	}
}

// Overflow: the stats shrink, then the labels are cut to the floor, the
// rightmost first, then the fields on the right go, the widest first,
// and a cut label grows back into the room that leaves; a label cut
// under two cells is dropped, and the line is clipped last.
func TestTemplateOverflow(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	m := &Model{Now: now, LocalHost: "mac"}
	r := tokenRow(now)
	r.Agent.Title = "Permission to run pnpm test in packages/api"
	render := func(src string, w int) string {
		t.Helper()
		tm, err := ParseTemplate(src)
		if err != nil {
			t.Fatal(err)
		}
		return Text([]Line{{Spans: m.line(Compiled{Template: tm}, r, w, 0)}})
	}
	third := "{pane_title}{fill}{pr_number} {pr_checks}"
	for _, c := range []struct {
		w    int
		want string
	}{
		{60, "Permission to run pnpm test in packages/api        #52 × 3/5"},
		{40, "Permission to run pnpm test i… #52 × 3/5"}, // the title cut to fit
		{22, "Permission … #52 × 3/5"},                   // cut by what is over, the floor (7) not reached
		{14, "Per… #52 × 3/5"},                           // to the floor: a third, 4
		{12, "Permi… #52 ×"},                             // the checks shrink; the title grows back by two
		{6, "Per… ×"},                                    // the PR dropped, the widest; the title grown back
	} {
		if got := render(third, c.w); got != c.want+"\n" {
			t.Errorf("%d: %q, want %q", c.w, got, c.want)
		}
	}
	// The stats shrink to the rebase mark before the PR goes.
	if got := render("{primary} ({host}){fill}{git_stats}  {pr_number}", 24); got != "fix-ls (vm)       R  #52\n" {
		t.Errorf("shrunk: %q", got)
	}
	// A cut label grows back into the room a dropped field leaves.
	r.Worktree.Branch = "feature/long-branch-name"
	if got := render("{primary}{fill}{status_label}", 10); got != "feature/l…\n" {
		t.Errorf("regrown: %q", got)
	}
	// A flexible token on the right is cut like one on the left, the
	// rightmost first, and grows back after the left.
	if got := render("{primary}{fill}{session}", 20); got != "feature/long… laatm…\n" {
		t.Errorf("a flexible token on the right: %q", got)
	}
	r.Worktree.Branch = "fix-ls"
	// Two flexible tokens: the rightmost is cut first, one under the
	// floor left alone.
	if got := render("{pane_title} {secondary}{fill}{elapsed}", 30); got != "Permission to ru… laatmux 2:00\n" {
		t.Errorf("rightmost first: %q", got)
	}
	if got := render("{pane_title} {session}{fill}{elapsed}", 30); got != "Permission to… laatmux/f… 2:00\n" {
		t.Errorf("rightmost first: %q", got)
	}
	// A label with under two cells of room is dropped; a line with no
	// flexible token is clipped.
	if got := render("{secondary}", 1); got != "\n" {
		t.Errorf("a label in one cell: %q", got)
	}
	// A shrunk token grows back too, after the labels, into the room a
	// dropped field leaves: the counts return once the stats go.
	r.Worktree.Git.Rebasing = false
	if got := render("{host}{fill}{git_stats} {pr_checks}", 12); got != "vm     × 3/5\n" {
		t.Errorf("a shrunk token regrown: %q", got)
	}
	// The stats never shrink to nothing: their smallest form stays
	// until the field is dropped, and the rebase mark is one.
	r.Worktree.Git.Rebasing = true
	if got := render("{host}{fill}{git_stats} {pr_checks}", 12); got != "vm   R × 3/5\n" {
		t.Errorf("the smallest form: %q", got)
	}
	// An empty left side pays no gap.
	if got := render("{fill}{elapsed}", 4); got != "2:00\n" {
		t.Errorf("no left side: %q", got)
	}
	if got := render("{host}{fill}{elapsed}", 3); got != "vm\n" {
		t.Errorf("clipped: %q", got)
	}
	// Tokens on the left go whole, the last first, before the line is
	// clipped; literal text alone is clipped.
	if got := render("{host} {agent_label} {status_label}", 9); got != "vm claude\n" {
		t.Errorf("the last token dropped: %q", got)
	}
	if got := render("literal text", 7); got != "literal\n" {
		t.Errorf("literal text clipped: %q", got)
	}
	// A bracketed token goes with its brackets, and a dropped token
	// takes only the spaces beside it, not a neighbour's bracket.
	r.Worktree.Branch, r.Host = "feature-branch", "build-server"
	if got := render("{primary} ({host})", 15); got != "feature-branch\n" {
		t.Errorf("the host's parentheses: %q", got)
	}
	r.Worktree.Branch, r.Host = "fix-ls", "vm"
	if got := render("{host}{fill}{elapsed} [{pr_number}]", 9); got != "vm  [#52]\n" {
		t.Errorf("a neighbour's bracket: %q", got)
	}
	if got := render("{host}{fill}{elapsed} [{pr_number}]", 7); got != "vm\n" {
		t.Errorf("the number's brackets: %q", got)
	}
	// The separator is whatever is not a bracket: the @ goes with the
	// host, both spaces with the number, and a bracket around two
	// tokens stays with the one left.
	r.Host = "build-server"
	if got := render("{secondary} @{host}", 9); got != "laatmux\n" {
		t.Errorf("the @ with the host: %q", got)
	}
	if got := render("{session}@{host}", 10); got != "laatmux/f…\n" {
		t.Errorf("a separator without a space: %q", got)
	}
	r.Host = "vm"
	if got := render("{host}{fill}{git_rebase}  {pr_number} {pr_checks}", 8); got != "vm   R ×\n" {
		t.Errorf("both spaces: %q", got)
	}
	if got := render("({host} {session})", 5); got != "(vm)\n" {
		t.Errorf("a label cut away leaves with its spaces: %q", got)
	}
	if got := render("{host}{fill}[{pr_number} {pr_checks}]", 8); got != "vm   [×]\n" {
		t.Errorf("a bracket around two tokens: %q", got)
	}
	// A separator a style split goes whole.
	if got := render("{primary} #[dim]@ {host}", 6); got != "fix-ls\n" {
		t.Errorf("a split separator: %q", got)
	}
	if got := render("{primary} #[dim]({host})", 6); got != "fix-ls\n" {
		t.Errorf("a split separator before a bracket pair: %q", got)
	}
	if got := render("{fill}{elapsed} #[dim]· {pr_number}", 5); got != "  #52\n" {
		t.Errorf("a split separator after: %q", got)
	}
}

// {elapsed} on a tree line is its agent's time, and nothing on a line
// with neither agent nor task; a blank tile line is no line; the
// compact layout draws the third tile line under a row with Titles;
// the tree's templates come from the config; an error in one is drawn
// in its place.
func TestConfiguredTemplates(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	// {elapsed} on a tree line: the agent's time, and nothing on a line
	// with no agent and no task.
	tm := &Model{Now: now, LocalHost: "mac", View: ViewTree, Width: 60, Height: 30}
	tm.SetTree(rows.Tree(treeInput(now)))
	tm.SetTemplates(CompileTemplates(nil, "", nil, "", "{indent}{fold}{branch}|{elapsed}|", "", "", ""))
	if out := Text(tm.Render()); !strings.Contains(out, "fix-sidebar||") || !strings.Contains(out, "auto-layout|1:00|") {
		t.Errorf("{elapsed} on tree lines:\n%s", out)
	}
	m := model(now)
	m.Layout, m.Width, m.Height = Tiles, 40, 12
	m.SetTemplates(CompileTemplates([]string{"{status_icon} {primary}", "", "{secondary}"}, "", nil, "", "", "", "", ""))
	out := Text(m.Render())
	lines := strings.Split(out, "\n")
	if strings.TrimRight(lines[1], " ") != "💬 fix-ls" || strings.TrimRight(lines[2], " ") != "laatmux" || !strings.HasPrefix(lines[3], "───") {
		t.Errorf("two-line tiles:\n%s", out)
	}
	m.Layout, m.Titles = Compact, true
	m.SetTemplates(CompileTemplates([]string{"{primary}", "{secondary}", "{pane_title} {pr_number}"}, "{status_icon} {primary} @{host}", nil, "", "", "", "", ""))
	out = Text(m.Render())
	lines = strings.Split(out, "\n")
	if strings.TrimRight(lines[1], " ") != "💬 fix-ls @vm" || !strings.HasPrefix(lines[2], "Permission to run pnpm test") {
		t.Errorf("compact with the third line:\n%s", out)
	}
	// The tree: a worktree template with the branch and the child
	// count; an agent template that does not parse.
	in := treeInput(now)
	tree := &Model{Now: now, LocalHost: "mac", View: ViewTree, Width: 60, Height: 20}
	tree.SetTemplates(CompileTemplates(nil, "", nil, "{repo} ({repo_count})", "{indent}{fold}{branch} [{child_count}]", "{indent}{status_icon} {nope}", "", ""))
	tree.SetTree(rows.Tree(in))
	tree.SetRows(rows.Agents(in, rows.Tree(in)))
	out = Text(tree.Render())
	if !strings.Contains(out, "laatmux (4)") || !strings.Contains(out, "  ▾ agents-config [4]") {
		t.Errorf("tree templates:\n%s", out)
	}
	if !strings.Contains(out, "template error: unknown token {nope} at column 23 in tree.ag") {
		t.Errorf("the agent template's error:\n%s", out)
	}
	// The config's templates reach the view through the same names.
	cfg, err := config.Parse([]byte("hosts: [{name: mac}]\nrepos: [git@github.com:laat/laatmux.git]\nsidebar:\n  templates:\n    tiles: [\"{primary}\", \"{nope}\"]\n    tree: {run: \"{indent}> {command}\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	ct := CompileTemplates(cfg.Sidebar.Templates.Tiles, cfg.Sidebar.Templates.Compact, cfg.Sidebar.Templates.Top, cfg.Sidebar.Templates.Tree.Repo, cfg.Sidebar.Templates.Tree.Worktree, cfg.Sidebar.Templates.Tree.Agent, cfg.Sidebar.Templates.Tree.Pane, cfg.Sidebar.Templates.Tree.Run)
	if len(ct.Tiles) != 2 || ct.Tiles[1].Err != "template error: unknown token {nope} at column 1 in tiles[1]" || ct.Tree.Run.src != "{indent}> {command}" || ct.Compact.src != DefaultCompact || len(ct.Top) != 3 {
		t.Errorf("compiled from the config: %+v", ct)
	}
	// The painter draws a background, not under the selection's band,
	// and dim with a background stays dim, in the dim colour that
	// reads on it (TestDimOnBackground).
	th, _ := palette.New(true, nil)
	l := Line{Spans: []Span{{Text: "x", Bg: palette.Accent}}}
	if s := ANSI(l, th); !strings.Contains(s, th.SGR(palette.Accent, true)) {
		t.Errorf("no background in %q", s)
	}
	l.Reverse = true
	if s := ANSI(l, th); strings.Contains(s, th.SGR(palette.Accent, true)) {
		t.Errorf("a background under the band in %q", s)
	}
	l = Line{Spans: []Span{{Text: "x", Bg: palette.Accent, Dim: true}}}
	if s := ANSI(l, th); !strings.Contains(s, th.DimmedOn(palette.Accent)+th.SGR(palette.Accent, true)) || strings.Contains(s, "\x1b[2m") {
		t.Errorf("dim with a background in %q", s)
	}
	// A dim line draws no background.
	l = Line{Dim: true, Spans: []Span{{Text: "x", Bg: palette.Accent}}}
	if s := ANSI(l, th); strings.Contains(s, th.SGR(palette.Accent, true)) {
		t.Errorf("a background on a dim line in %q", s)
	}
	// A task standing on a listed worktree with stats: its state, a
	// space, the stats and the PR.
	in.Pendings = []protocol.Pending{{ID: "add-al", Host: "vm", EnvironmentID: "venv", Source: "git@github.com:laat/laatmux.git", Repo: "laatmux", Branch: "auto-layout", Root: "/r/auto-layout", Session: "laatmux/auto-layout", Taken: true, SubmittedAt: now}}
	tree.SetTemplates(DefaultTemplates())
	tree.Width, tree.Height = 70, 20
	tree.SetTree(rows.Tree(in))
	tree.SetRows(rows.Agents(in, rows.Tree(in)))
	out = Text(tree.Render())
	if !strings.Contains(out, "auto-layout (vm)") || !strings.Contains(out, "adding +318 -87  #49 × 3/5") {
		t.Errorf("a standing task's line:\n%s", out)
	}
}

// The folded line's icon is dropped last, after the check mark; a
// host unknown on a tree line is ?; {session} falls back to the
// worktree's, the task's and the local session.
func TestTemplateTreeEdges(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := treeInput(now)
	m := &Model{Now: now, LocalHost: "mac", View: ViewTree, Width: 60, Height: 30}
	m.SetTree(rows.Tree(in))
	m.SetRows(rows.Agents(in, rows.Tree(in)))
	m.Render()
	m.Select("venv/worktree//r/agents-config")
	m.Handle(term.Key{Rune: 'h'})
	r := m.Tree[m.indexOf("venv/worktree//r/agents-config")]
	line := func(src string, r rows.Row, w int) string {
		t.Helper()
		return strings.TrimRight(Text([]Line{{Spans: m.line(mustParse(t, src), r, w, 0)}}), "\n")
	}
	if got := line(DefaultWorktree, r, 20); got != "  ▸ agents-… (vm) ⠋⠙" {
		t.Errorf("the icon at 20: %q", got)
	}
	if got := line(DefaultWorktree, r, 24); got != "  ▸ agents-c… (vm) ✓  ⠋⠙" {
		t.Errorf("the icon at 24: %q", got)
	}
	r.Host = ""
	if got := line("{primary} ({host})", r, 40); got != "agents-config (?)" {
		t.Errorf("a host unknown: %q", got)
	}
	for _, c := range []struct{ id, want string }{
		{"venv/worktree//r/agents-config", "laatmux/agents-config"}, {"menv/worktree//w/fix-sidebar", ""}, {"session/mac/laatmux/gone", "mac/laatmux/gone"},
	} {
		if got := line("{session}", m.Tree[m.indexOf(c.id)], 40); got != c.want {
			t.Errorf("%s {session} = %q, want %q", c.id, got, c.want)
		}
	}
	task := rows.Row{Kind: rows.KindTask, Host: "vm", Pending: &protocol.Pending{ID: "t", Session: "laatmux/new"}}
	if got := line("{session}", task, 40); got != "laatmux/new" {
		t.Errorf("a task's session: %q", got)
	}
	home := rows.Row{Kind: rows.KindWorktree, Host: "vm", Worktree: &protocol.Worktree{Session: "laatmux/x"}}
	if got := line("{session}", home, 40); got != "laatmux/x" {
		t.Errorf("a worktree's session: %q", got)
	}
}

// mustParse is a template for a test.
func mustParse(t *testing.T, src string) Compiled {
	t.Helper()
	tm, err := ParseTemplate(src)
	if err != nil {
		t.Fatal(err)
	}
	return Compiled{Template: tm}
}

// staleStyled draws src, a style with the background #112233 and a
// colour and bold over stale tokens, at every width up to 60: each span
// drawn that is not spaces is dim and plain on the background. At 60
// the line is want, so every token is drawn.
func staleStyled(t *testing.T, m *Model, r rows.Row, src, want string) {
	t.Helper()
	for w := 1; w <= 60; w++ {
		spans := m.line(mustParse(t, src), r, w, 0)
		for _, sp := range spans {
			if strings.TrimSpace(sp.Text) != "" && (!sp.Dim || sp.Bold || sp.Fg != "" || sp.Bg != "#112233") {
				t.Errorf("%q at %d: %+v not dim and plain on the background", src, w, sp)
			}
		}
		if got := strings.TrimSpace(Text([]Line{{Spans: spans}})); w == 60 && got != want {
			t.Errorf("%q at 60: %q, want %q", src, got, want)
		}
	}
}

// The dashboard's columns: {git_sync} is →base off main or master, the
// conflict mark and ↑A ↓B, shrinking base first, then ↓B, then ↑A, and
// never to nothing; {pr_state} is the PR's state as the set's icon;
// {pr_detail} the pending time or the first failing check's name; the
// dashboard's defaults carry them and the sidebar's do not, and a
// configured template applies to both.
func TestColumns(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	m := &Model{Now: now, LocalHost: "mac", Width: 80}
	r := tokenRow(now)
	render := func(src string, w int) string {
		t.Helper()
		return Debug([]Line{{Spans: m.line(mustParse(t, src), r, w, 0)}})
	}
	plain := func(src string, w int) string {
		t.Helper()
		return strings.TrimRight(Text([]Line{{Spans: m.line(mustParse(t, src), r, w, 0)}}), "\n")
	}
	// The base off main, its origin/ taken off; the conflict mark red.
	r.Worktree.Git.Base = "origin/feature"
	if got := render("{git_sync}", 40); got != "...|→feature «⟨danger:!⟩» ↑2 ↓1\n" {
		t.Errorf("git_sync: %q", got)
	}
	for _, c := range []struct {
		w    int
		want string
	}{{40, "→feature ! ↑2 ↓1"}, {15, "→featu… ! ↑2 ↓1"}, {12, "→fe… ! ↑2 ↓1"}, {11, "! ↑2 ↓1"}, {6, "! ↑2"}, {3, "!"}, {1, "!"}} {
		if got := plain("{fill}{git_sync}", c.w); strings.TrimSpace(got) != c.want {
			t.Errorf("git_sync at %d: %q, want %q", c.w, got, c.want)
		}
	}
	// On main, in sync and without a conflict: nothing; behind alone
	// is the smallest form then.
	r.Worktree.Git.Base, r.Worktree.Git.Conflict, r.Worktree.Git.Ahead = "origin/main", nil, 0
	if got := plain("{git_sync}", 40); got != "↓1" {
		t.Errorf("git_sync behind alone: %q", got)
	}
	if got := plain("{fill}{git_sync}", 1); got != "" {
		t.Errorf("git_sync with no room for its smallest form: %q", got)
	}
	r.Worktree.Git.Behind = 0
	if got := plain("a {git_sync} b", 40); got != "a b" {
		t.Errorf("git_sync with nothing to say: %q", got)
	}
	r.Worktree.Git.Base = "master"
	if got := plain("{git_sync}", 40); got != "" {
		t.Errorf("git_sync on master: %q", got)
	}
	// Stale: dim and plain, the conflict mark too.
	r.Worktree.Git.Base, r.Worktree.Git.Stale = "topic", true
	if got := render("{git_sync}", 40); got != "...|‹→topic›\n" {
		t.Errorf("git_sync stale: %q", got)
	}
	yes := true
	r.Worktree.Git.Conflict = &yes
	if got := render("{git_sync}", 40); got != "...|‹→topic›‹ ›‹!›\n" {
		t.Errorf("git_sync stale with a conflict: %q", got)
	}
	r.Worktree.Git.Conflict, r.Worktree.Git.Stale = nil, false
	// A long base is cut to twelve cells with the arrow, one of twelve
	// whole; a base alone is cut to the room; the row's own branch as
	// the base is left out; the smallest form stays until the field
	// goes.
	r.Worktree.Git.Base, r.Worktree.Git.Ahead = "origin/feature/JIRA-1234-add-the-thing", 2
	if got := plain("{git_sync}", 40); got != "→feature/JI… ↑2" {
		t.Errorf("git_sync with a long base: %q", got)
	}
	r.Worktree.Git.Base = "origin/abcdefghijk"
	if got := plain("{git_sync}", 40); got != "→abcdefghijk ↑2" {
		t.Errorf("git_sync with a base of twelve cells: %q", got)
	}
	r.Worktree.Git.Ahead = 0
	if got := plain("{fill}{git_sync}", 8); got != "→abcdef…" {
		t.Errorf("git_sync with a base alone cut to the room: %q", got)
	}
	r.Worktree.Git.Ahead = 2
	r.Worktree.Git.Base = "origin/fix-ls"
	if got := plain("{git_sync}", 40); got != "↑2" {
		t.Errorf("git_sync with the branch as its own base: %q", got)
	}
	r.Worktree.Git.Base, r.Worktree.Git.Behind = "origin/main", 1
	if got := plain("{fill}{git_sync} {elapsed}", 5); got != "↑2 ↓1" {
		t.Errorf("git_sync never to nothing: %q", got)
	}
	r.Worktree.Git.Ahead = 0
	// The PR's state per set, and dim when stale or a draft.
	for _, c := range []struct {
		set, state string
		draft      bool
		want       string
	}{
		{IconsEmoji, "open", false, "...|«⟨success:●⟩»\n"}, {IconsEmoji, "open", true, "...|‹◌›\n"},
		{IconsEmoji, "merged", false, "...|⟨accent:◆⟩\n"}, {IconsEmoji, "closed", false, "...|‹⟨danger:⊘⟩›\n"},
		{IconsASCII, "open", false, "...|«⟨success:o⟩»\n"}, {IconsASCII, "open", true, "...|‹d›\n"},
		{IconsASCII, "merged", false, "...|⟨accent:m⟩\n"}, {IconsASCII, "closed", false, "...|‹⟨danger:c⟩›\n"},
		{IconsNerdFont, "open", false, "...|«⟨success:\uf407⟩»\n"}, {IconsNerdFont, "open", true, "...|‹\uf4dd›\n"},
		{IconsNerdFont, "merged", false, "...|⟨accent:\uf419⟩\n"}, {IconsNerdFont, "closed", false, "...|‹⟨danger:\uf4dc⟩›\n"},
		{"", "open", true, "...|‹◌›\n"},
		// A draft closed as one is closed.
		{IconsEmoji, "closed", true, "...|‹⟨danger:⊘⟩›\n"}, {IconsEmoji, "merged", true, "...|⟨accent:◆⟩\n"},
	} {
		m.Icons.Set = c.set
		r.Branch.PR.State, r.Branch.PR.Draft = c.state, c.draft
		if got := render("{pr_state}", 40); got != c.want {
			t.Errorf("pr_state %s %s draft=%v: %q", c.set, c.state, c.draft, got)
		}
	}
	m.Icons.Set = ""
	r.Branch.PR.State, r.Branch.PR.Draft = "closed", true
	if got := render("{pr_number}", 40); got != "...|‹⟨danger:#52⟩›\n" {
		t.Errorf("the number of a closed draft: %q", got)
	}
	r.Branch.PR.State, r.Branch.PR.Draft = "open", false
	r.Branch.Stale = true
	if got := render("{pr_state}", 40); got != "...|‹●›\n" {
		t.Errorf("pr_state stale: %q", got)
	}
	// The number of a stale open PR is plain as its state is: not bold.
	if got := render("{pr_number} {pr_state}", 40); got != "...|‹#52›‹?› ‹●›\n" {
		t.Errorf("pr_number and pr_state stale: %q", got)
	}
	r.Branch.Stale = false
	r.Branch.PR = nil
	if got := plain("{pr_state}", 40); got != "" {
		t.Errorf("pr_state without a PR: %q", got)
	}
	r.Branch.PR = &protocol.PullRequest{Number: 52, State: "open"}
	// The detail: the failing name red, cut as a label when narrow;
	// pending, the time since the checks were first seen pending,
	// ticking under an hour; stale dim; nothing else.
	if got := render("{pr_detail}", 40); got != "...|⟨danger:test (macos-latest)⟩\n" {
		t.Errorf("pr_detail failing: %q", got)
	}
	if got := plain("{host} {pr_detail}", 12); got != "vm test (ma…" {
		t.Errorf("pr_detail cut: %q", got)
	}
	r.Branch.Checks = &protocol.Checks{State: protocol.ChecksPending, Passed: 1, Total: 5, PendingSince: now.Add(-4*time.Minute - 12*time.Second)}
	sp := m.line(mustParse(t, "{pr_detail}"), r, 40, 2)
	if len(sp) != 1 || sp[0].Text != "4:12" || sp[0].Fg != palette.Accent || !sp[0].tick {
		t.Errorf("pr_detail pending: %+v", sp)
	}
	// The time is shown whole or dropped, not cut.
	if got := plain("{host}{fill}{pr_detail}", 6); got != "vm" {
		t.Errorf("pr_detail's time in six cells: %q", got)
	}
	if got := plain("{host}{fill}{pr_detail}", 7); got != "vm 4:12" {
		t.Errorf("pr_detail's time in seven cells: %q", got)
	}
	r.Branch.Checks.PendingSince = now.Add(-3 * time.Hour)
	sp = m.line(mustParse(t, "{pr_detail}"), r, 40, 2)
	if len(sp) != 1 || sp[0].Text != "3h" || sp[0].tick {
		t.Errorf("pr_detail pending for hours: %+v", sp)
	}
	// Stale: the time as of the last answer, standing still.
	r.Branch.Stale, r.Branch.FetchedAt = true, now.Add(-10*time.Minute)
	r.Branch.Checks.PendingSince = now.Add(-14*time.Minute - 12*time.Second)
	sp = m.line(mustParse(t, "{pr_detail}"), r, 40, 2)
	if len(sp) != 1 || sp[0].Text != "4:12" || sp[0].tick || !sp[0].Dim || sp[0].Fg != "" {
		t.Errorf("pr_detail stale under an hour: %+v", sp)
	}
	r.Branch.Checks.PendingSince = now.Add(-3 * time.Hour)
	if got := render("{pr_detail}", 40); got != "...|‹2h›\n" {
		t.Errorf("pr_detail stale: %q", got)
	}
	// A stale failing name: dim and plain, and cut as a label still.
	pending := r.Branch.Checks
	r.Branch.Checks = &protocol.Checks{State: protocol.ChecksFailure, Passed: 3, Total: 5, Failing: "test (macos-latest)"}
	if got := render("{pr_detail}", 40); got != "...|‹test (macos-latest)›\n" {
		t.Errorf("pr_detail failing stale: %q", got)
	}
	if got := render("{host} {pr_detail}", 12); got != "...|‹vm› ‹test (ma…›\n" {
		t.Errorf("pr_detail failing stale cut: %q", got)
	}
	r.Branch.Checks = pending
	r.Branch.Stale = false
	r.Branch.Checks.PendingSince = time.Time{}
	if got := plain("{pr_detail}", 40); got != "" {
		t.Errorf("pr_detail pending since never: %q", got)
	}
	r.Branch.Checks = &protocol.Checks{State: protocol.ChecksSuccess, Passed: 5, Total: 5}
	if got := plain("{pr_detail}", 40); got != "" {
		t.Errorf("pr_detail on success: %q", got)
	}
	// On main: the failing name, never the pending time.
	r.Worktree.Branch = "main"
	r.Branch.Checks = &protocol.Checks{State: protocol.ChecksPending, PendingSince: now.Add(-time.Minute)}
	if got := plain("{pr_detail}", 40); got != "" {
		t.Errorf("pr_detail pending on main: %q", got)
	}
	r.Branch.Checks = &protocol.Checks{State: protocol.ChecksFailure, Failing: "lint"}
	if got := plain("{pr_state} {pr_detail}", 40); got != "lint" {
		t.Errorf("pr_detail failing on main: %q", got)
	}
	r.Worktree.Branch = "fix-ls"
	// The dashboard's defaults carry the columns, the sidebar's do not;
	// a configured line is the same in both.
	dash := CompileTemplatesOver(DashboardDefaults, nil, "", nil, "", "", "", "", "")
	side := DefaultTemplates()
	for _, c := range []struct {
		name       string
		dash, side Template
	}{{"tiles[1]", dash.Tiles[1].Template, side.Tiles[1].Template}, {"tiles[2]", dash.Tiles[2].Template, side.Tiles[2].Template}, {"compact", dash.Compact.Template, side.Compact.Template}, {"tree.worktree", dash.Tree.Worktree.Template, side.Tree.Worktree.Template}} {
		if c.dash.src == c.side.src || !strings.Contains(c.dash.src, "{git_sync}") && !strings.Contains(c.dash.src, "{pr_state}") || strings.Contains(c.side.src, "_sync") || strings.Contains(c.side.src, "pr_state") {
			t.Errorf("%s: dashboard %q, sidebar %q", c.name, c.dash.src, c.side.src)
		}
	}
	for _, c := range []Compiled{dash.Tiles[0], dash.Tiles[1], dash.Tiles[2], dash.Compact, dash.Tree.Worktree} {
		if c.Err != "" {
			t.Errorf("a dashboard default does not parse: %s", c.Err)
		}
	}
	own := CompileTemplatesOver(DashboardDefaults, []string{"{primary}"}, "{host}", nil, "", "{repo}", "", "", "")
	if own.Tiles[0].src != "{primary}" || len(own.Tiles) != 1 || own.Compact.src != "{host}" || own.Tree.Worktree.src != "{repo}" || own.Tree.Run.src != DefaultRun {
		t.Errorf("configured lines over the dashboard's defaults: %+v", own)
	}
	m.SetTemplates(dash)
	m.Layout, m.Titles, m.Width = Compact, true, 100
	r.Worktree.Git.Base, r.Worktree.Git.Ahead, r.Worktree.Git.Behind = "origin/feature", 2, 1
	r.Worktree.Git.Conflict = &yes
	r.Branch.Checks = &protocol.Checks{State: protocol.ChecksFailure, Passed: 3, Total: 5, Failing: "test (macos-latest)"}
	lines := m.compact(r, 0)
	if got := Text(lines); got != "▌ 💬 fix-ls (2) laatmux @vm                                R +46 -11 ✎ +28 -3  →feature ! ↑2 ↓1 2:00\n▌    Permission to run pnpm test                                     ● #52 × 3/5 test (macos-latest)\n" {
		t.Errorf("the dashboard's compact lines:\n%s", got)
	}
	// The worktree line with a long base, at widths going down: the
	// base is at most twelve cells from the start, so the counts and
	// the name it outlives, by the engine's order, cost little; then
	// the base is cut further and the stats shrink. At these widths
	// the base gives way before the stats; at some others the regrow
	// after a stats shrink hands the base more room than the width
	// above had, which is the engine's greedy order, not the token's.
	r.Worktree.Git.Base, r.Worktree.Git.Rebasing = "origin/feature/JIRA-1234-add-the-thing", false
	r.Kind, r.Depth = rows.KindWorktree, 1
	for _, c := range []struct {
		w    int
		want string
	}{
		{110, "  ▸ fix-ls (vm)                        +46 -11 ✎ +28 -3  →feature/JI… ! ↑2 ↓1  ● #52 × 3/5 test (macos-latest)"},
		{90, "  ▸ fix-ls (vm)    +46 -11 ✎ +28 -3  →feature/JI… ! ↑2 ↓1  ● #52 × 3/5 test (macos-latest)"},
		{80, "  ▸ fix-ls (vm) +46 -11 ✎ +28 -3  →feature/JI… ! ↑2 ↓1  ● #52 × 3/5 test (macos…"},
		{70, "  ▸ fix-ls (vm) +46 -11 ✎ +28 -3  →feat… ! ↑2 ↓1  ● #52 × test (macos…"},
	} {
		if got := strings.TrimRight(Text([]Line{{Spans: m.line(dash.Tree.Worktree, r, c.w, 0)}}), "\n"); got != c.want {
			t.Errorf("the dashboard's worktree line at %d:\n%q\n%q", c.w, got, c.want)
		}
	}
	r.Kind, r.Depth = rows.KindTile, 0
	// The second tile line: the sync after the stats, the first to
	// shrink.
	r.Worktree.Git.Base = "origin/feature"
	if got := strings.TrimRight(Text([]Line{{Spans: m.line(dash.Tiles[1], r, 50, 2)}}), "\n"); got != "▌    laatmux @vm +46 -11 ✎ +28 -3  →featu… ! ↑2 ↓1" {
		t.Errorf("the dashboard's second tile line: %q", got)
	}
}
