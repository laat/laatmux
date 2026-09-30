package view

import (
	"strings"
	"testing"
	"time"

	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
)

// The parser: tokens, styles and the fill; an unknown token, an unclosed
// brace or style, an unknown style item or colour, and a second fill
// are errors naming the column.
func TestParseTemplate(t *testing.T) {
	for _, src := range []string{
		"", "plain text", "{primary}", "{stripe} {status_icon} {primary} {pane_suffix}{fill}{elapsed}",
		"#[fg=accent,bold]{repo}#[default] x #[bg=#112233,dim,nobold,nodim]{host}", "#[fg=colour42]{idx}",
		DefaultTile1, DefaultTile2, DefaultTile3, DefaultCompact, DefaultTop, DefaultRepo, DefaultWorktree, DefaultAgent, DefaultPane, DefaultRun,
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
	if len(d.Tiles) != 3 {
		t.Fatalf("default tiles: %d", len(d.Tiles))
	}
	for _, c := range append([]Compiled{d.Compact, d.Top, d.Tree.Repo, d.Tree.Worktree, d.Tree.Agent, d.Tree.Pane, d.Tree.Run}, d.Tiles...) {
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
	a := &protocol.Agent{ID: "venv/laatmux/%1", EnvironmentID: "venv", Session: "laatmux/fix-ls", Window: 2, PaneID: "%1", Agent: "claude", Activity: protocol.Blocked, ActivityAt: now.Add(-2 * time.Minute), Liveness: protocol.Alive, Managed: true, Title: "✳ Permission to run pnpm test"}
	return rows.Row{Kind: rows.KindTile, Host: "vm", Name: "laatmux/fix-ls", Node: a.ID, Worktree: w, Agent: a, Suffix: "(2)", Children: 1,
		Branch: &protocol.BranchStatus{PR: &protocol.PullRequest{Number: 52, State: "open"}, Checks: &protocol.Checks{State: protocol.ChecksFailure, Passed: 3, Total: 5}}}
}

// Every token on the tile row, and what the tree-only ones give there.
func TestTokens(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	m := &Model{Now: now, LocalHost: "mac", Width: 80, AgentIcons: map[string]AgentIcon{"claude": {Icon: "CL"}}, JumpKeys: true}
	m.rowIdx = 2
	r := tokenRow(now)
	for _, c := range []struct{ token, want string }{
		{"primary", "fix-ls"}, {"secondary", "laatmux"}, {"branch", "fix-ls"}, {"repo", "laatmux"}, {"host", "vm"},
		{"session", "laatmux/fix-ls"}, {"window", "laatmux/fix-ls:2"}, {"window_index", "2"}, {"pane_title", "Permission to run pnpm test"}, {"pane_suffix", "(2)"},
		{"status_icon", "💬"}, {"status_label", "waiting"}, {"agent_icon", "CL"}, {"agent_label", "claude"}, {"elapsed", "2:00"}, {"stripe", "▌"},
		{"git_stats", "R +46 -11 ✎ +28 -3"}, {"git_committed", "+46 -11"}, {"git_uncommitted", "✎ +28 -3"}, {"git_ahead", "↑2"}, {"git_behind", "↓1"},
		{"git_dirty", "✎"}, {"git_conflict", "!"}, {"git_rebase", "R"}, {"git_branch", "origin/main"},
		{"pr_number", "#52"}, {"pr_checks", "× 3/5"}, {"idx", "2"}, {"jump_key", "M-2"},
		{"repo_count", ""}, {"fold", ""}, {"worst_status", ""}, {"child_count", ""}, {"command", ""}, {"indent", ""},
	} {
		c := c
		tm, err := ParseTemplate("{" + c.token + "}")
		if err != nil {
			t.Fatal(err)
		}
		got := strings.TrimSpace(Text([]Line{{Spans: m.line(Compiled{Template: tm}, r, 80)}}))
		if got != c.want {
			t.Errorf("{%s} = %q, want %q", c.token, got, c.want)
		}
	}
	// The agent's own icon colour, and the default icon for an agent
	// the config leaves alone.
	m.AgentIcons = nil
	tm, _ := ParseTemplate("{agent_icon}")
	if sp := m.line(Compiled{Template: tm}, r, 80); len(sp) != 1 || sp[0].Text != "CC" || sp[0].Fg != "#d97757" {
		t.Errorf("the default agent icon: %+v", sp)
	}
	// Tree tokens on tree nodes.
	in := treeInput(now)
	tree := &Model{Now: now, LocalHost: "mac", View: ViewTree, Width: 80, Height: 30}
	tree.SetTree(rows.Tree(in))
	tree.SetRows(rows.Agents(in))
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
		got := strings.TrimSpace(Text([]Line{{Spans: tree.line(Compiled{Template: tm}, at(c.id), 80)}}))
		if got != c.want {
			t.Errorf("%s {%s} = %q, want %q", c.id, c.token, got, c.want)
		}
	}
	// The pane's indent is its depth; a folded line shows the worst
	// agent's icon.
	tm, _ = ParseTemplate("{indent}|")
	if got := Text([]Line{{Spans: tree.line(Compiled{Template: tm}, at("venv/pane/laatmux/%7"), 80)}}); !strings.HasPrefix(got, "    |") {
		t.Errorf("a pane's indent: %q", got)
	}
	// A worktree line's host is the worktree's, not its agent's server;
	// a tile's carries the server.
	in.Worktrees[1].Session = ""
	in.Agents[3].Server, in.Agents[3].Managed = "default", false
	tree.SetTree(rows.Tree(in))
	tree.SetRows(rows.Agents(in))
	tm, _ = ParseTemplate("({host})")
	if got := Text([]Line{{Spans: tree.line(Compiled{Template: tm}, at("venv/worktree//r/auto-layout"), 80)}}); got != "(vm)\n" {
		t.Errorf("a worktree line's host: %q", got)
	}
	for _, r := range tree.Rows.Main {
		if r.ID() == "venv/laatmux/%8" {
			if got := Text([]Line{{Spans: tree.line(Compiled{Template: tm}, r, 80)}}); got != "(vm/default)\n" {
				t.Errorf("a tile's host: %q", got)
			}
		}
	}
	in = treeInput(now)
	tree.SetTree(rows.Tree(in))
	tree.SetRows(rows.Agents(in))
	// {idx} through Render counts as the digits do.
	tree.SetTemplates(CompileTemplates(nil, "", "", "", "{indent}{fold}{idx}:{primary}", "{indent}{idx}:{agent_label}", "", ""))
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
	tree.Handle(Key{Rune: 'h'})
	tm, _ = ParseTemplate("{worst_status}")
	if got := strings.TrimSpace(Text([]Line{{Spans: tree.line(Compiled{Template: tm}, at("venv/worktree//r/agents-config"), 80)}})); got == "" {
		t.Error("a folded line has no worst status")
	}
}

// Styles hold until the next one and leave a token's own colours; a
// background is drawn; the fill puts the right part against the edge;
// an empty token takes its separator with it; a template that did not
// parse draws its error.
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
		return Debug([]Line{{Spans: m.line(Compiled{Template: tm}, r, w)}})
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
	if got := Text([]Line{{Spans: m.line(mustParse(t, "{git_uncommitted} | {git_stats}"), r, 60)}}); got != "✎ +28+ -3 | R +46 -11 ✎ +28+ -3\n" {
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
	// alone carries it.
	r.Branch.Stale = true
	if got := render("{pr_number} {pr_checks}", 40); got != "...|‹«#52»› ‹×›‹ 3/5›‹?›\n" {
		t.Errorf("a stale pair: %q", got)
	}
	if got := render("{pr_number}", 40); got != "...|‹«#52»›‹?›\n" {
		t.Errorf("a stale number without the checks drawn: %q", got)
	}
	if got := render("{pr_number}{fill}{pr_checks} {host}", 5); got != "...|‹«#52»›‹?›\n" {
		t.Errorf("a stale number with the checks dropped: %q", got)
	}
	r.Branch.Checks = nil
	if got := render("{pr_number} {pr_checks}", 40); got != "...|‹«#52»›‹?›\n" {
		t.Errorf("a stale number alone: %q", got)
	}
	r = tokenRow(now)
	// A run of spaces a style splits still goes with an empty token.
	r.Suffix = ""
	if got := render("a {pane_suffix} #[fg=accent] b", 20); got != "...|a ⟨accent:b⟩\n" {
		t.Errorf("a split run: %q", got)
	}
	r = tokenRow(now)
	// The single git tokens are stale as the stats are.
	r.Worktree.Git.Stale = true
	if got := render("{git_rebase} {git_conflict} {git_ahead}", 20); got != "...|‹R› ‹!› ‹↑2›\n" {
		t.Errorf("stale git tokens: %q", got)
	}
	r = tokenRow(now)
	// fg=default clears the colour, as tmux spells it.
	if got := render("#[fg=accent]a#[fg=default]b", 10); got != "...|⟨accent:a⟩b\n" {
		t.Errorf("fg=default: %q", got)
	}
	c := Compile("tiles[1]", "{nope}", "")
	if got := Debug([]Line{{Spans: m.line(c, r, 60)}}); got != "...|⟨danger:template error: unknown token {nope} at column 1 in tiles[1]⟩\n" {
		t.Errorf("an error drawn: %q", got)
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
		return Text([]Line{{Spans: m.line(Compiled{Template: tm}, r, w)}})
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
}

// A blank tile line is no line; the compact layout draws the third tile
// line under a row with Titles; the tree's templates come from the
// config; an error in one is drawn in its place.
func TestConfiguredTemplates(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	m := model(now)
	m.Layout, m.Width, m.Height = Tiles, 40, 12
	m.SetTemplates(CompileTemplates([]string{"{status_icon} {primary}", "", "{secondary}"}, "", "", "", "", "", "", ""))
	out := Text(m.Render())
	lines := strings.Split(out, "\n")
	if strings.TrimRight(lines[1], " ") != "💬 fix-ls" || strings.TrimRight(lines[2], " ") != "laatmux" || !strings.HasPrefix(lines[3], "───") {
		t.Errorf("two-line tiles:\n%s", out)
	}
	m.Layout, m.Titles = Compact, true
	m.SetTemplates(CompileTemplates([]string{"{primary}", "{secondary}", "{pane_title} {pr_number}"}, "{status_icon} {primary} @{host}", "", "", "", "", "", ""))
	out = Text(m.Render())
	lines = strings.Split(out, "\n")
	if strings.TrimRight(lines[1], " ") != "💬 fix-ls @vm" || !strings.HasPrefix(lines[2], "Permission to run pnpm test") {
		t.Errorf("compact with the third line:\n%s", out)
	}
	// The tree: a worktree template with the branch and the child
	// count; an agent template that does not parse.
	in := treeInput(now)
	tree := &Model{Now: now, LocalHost: "mac", View: ViewTree, Width: 60, Height: 20}
	tree.SetTemplates(CompileTemplates(nil, "", "", "{repo} ({repo_count})", "{indent}{fold}{branch} [{child_count}]", "{indent}{status_icon} {nope}", "", ""))
	tree.SetTree(rows.Tree(in))
	tree.SetRows(rows.Agents(in))
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
	if len(ct.Tiles) != 2 || ct.Tiles[1].Err != "template error: unknown token {nope} at column 1 in tiles[1]" || ct.Tree.Run.src != "{indent}> {command}" || ct.Compact.src != DefaultCompact {
		t.Errorf("compiled from the config: %+v", ct)
	}
	// The painter draws a background, not under the selection's band,
	// and dim with a background stays dim.
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
	if s := ANSI(l, th); !strings.Contains(s, "\x1b[2m") || !strings.Contains(s, th.SGR(palette.Accent, true)) {
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
	tree.SetRows(rows.Agents(in))
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
	m.SetRows(rows.Agents(in))
	m.Render()
	m.Select("venv/worktree//r/agents-config")
	m.Handle(Key{Rune: 'h'})
	r := m.Tree[m.indexOf("venv/worktree//r/agents-config")]
	line := func(src string, r rows.Row, w int) string {
		t.Helper()
		return strings.TrimRight(Text([]Line{{Spans: m.line(mustParse(t, src), r, w)}}), "\n")
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
