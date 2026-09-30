package view

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
)

// Every line of every layout is a template: literal text, tokens in
// braces, `{primary}` say, and styles in tmux's syntax, `#[fg=accent,
// bold]`, which hold until the next style. One `{fill}` splits the line
// into a left and a right part, the right against the right edge.
//
// A line wider than the pane gives way in this order: the flexible
// tokens, the labels and the pane title on either side, are cut with …
// down to a floor of a third of the width, at most twelve cells, the
// rightmost first; `{git_stats}` and `{pr_checks}` shrink themselves,
// never to nothing; fields on the right are dropped, the widest first
// and a folded line's `{worst_status}` icon last; the flexible tokens
// are cut further; then tokens on the left are dropped, the last
// first; then the line is clipped. What dropping leaves over goes back
// to the cut tokens, then to the shrunk ones. An empty token
// takes the adjacent run of spaces with it, the one after it, else the
// one before, so separators do not pile up; a line whose tokens are all
// empty is still a line, so tiles keep their height; a blank entry in
// the tiles list is no line at all.

// partKind is what a piece of a template is.
type partKind int

const (
	partText partKind = iota
	partToken
	partFill
)

// style is what a `#[…]` sets: a foreground, a background, bold and
// dim; "" and false leave a token's own.
type style struct {
	fg, bg    string
	bold, dim bool
}

// part is one piece of a parsed template: literal text, a token by name
// or the fill, with the style in force and the column it starts at.
type part struct {
	kind partKind
	text string
	st   style
	col  int
}

// Template is one parsed line.
type Template struct {
	src   string
	parts []part
	fill  bool
}

// Blank reports whether the template draws no line: its source is
// empty, or spaces alone.
func (t Template) Blank() bool { return strings.TrimSpace(t.src) == "" }

// tokenKind says how a token gives way on a line too narrow.
type tokenKind int

const (
	tokenPlain  tokenKind = iota
	tokenFlex             // a label or the title: cut with …
	tokenShrink           // the git stats or the checks: shrink themselves
)

// tokens is the table of token names.
var tokens = map[string]tokenKind{
	"primary": tokenFlex, "secondary": tokenFlex, "branch": tokenFlex, "repo": tokenFlex, "host": tokenPlain,
	"session": tokenFlex, "window": tokenFlex, "window_index": tokenPlain, "pane_title": tokenFlex, "pane_suffix": tokenPlain,
	"status_icon": tokenPlain, "status_label": tokenPlain, "agent_icon": tokenPlain, "agent_label": tokenPlain, "elapsed": tokenPlain,
	"stripe":    tokenPlain,
	"git_stats": tokenShrink, "git_committed": tokenPlain, "git_uncommitted": tokenPlain, "git_ahead": tokenPlain, "git_behind": tokenPlain,
	"git_dirty": tokenPlain, "git_conflict": tokenPlain, "git_rebase": tokenPlain, "git_branch": tokenFlex,
	"pr_number": tokenPlain, "pr_checks": tokenShrink,
	"idx": tokenPlain, "jump_key": tokenPlain,
	"repo_count": tokenPlain, "fold": tokenPlain, "worst_status": tokenPlain, "child_count": tokenPlain,
	"command": tokenFlex, "indent": tokenPlain,
}

// ParseTemplate parses one line. The error names the column, counted
// from 1, and is the message the view shows in the template's place.
func ParseTemplate(src string) (Template, error) {
	t := Template{src: src}
	var st style
	rs := []rune(src)
	var text strings.Builder
	textCol := 1
	flush := func() {
		if text.Len() > 0 {
			t.parts = append(t.parts, part{kind: partText, text: text.String(), st: st, col: textCol})
			text.Reset()
		}
	}
	for i := 0; i < len(rs); i++ {
		col := i + 1
		switch {
		case rs[i] == '#' && i+1 < len(rs) && rs[i+1] == '[':
			end := indexRune(rs, i+2, ']')
			if end < 0 {
				return t, fmt.Errorf("unclosed #[ at column %d", col)
			}
			flush()
			ns, err := parseStyle(st, string(rs[i+2:end]))
			if err != nil {
				return t, fmt.Errorf("%v at column %d", err, col)
			}
			st = ns
			i = end
			textCol = i + 2
		case rs[i] == '{':
			end := indexRune(rs, i+1, '}')
			if end < 0 {
				return t, fmt.Errorf("unclosed { at column %d", col)
			}
			name := string(rs[i+1 : end])
			flush()
			if name == "fill" {
				if t.fill {
					return t, fmt.Errorf("a second {fill} at column %d", col)
				}
				t.fill = true
				t.parts = append(t.parts, part{kind: partFill, st: st, col: col})
			} else if _, ok := tokens[name]; ok {
				t.parts = append(t.parts, part{kind: partToken, text: name, st: st, col: col})
			} else {
				return t, fmt.Errorf("unknown token {%s} at column %d", name, col)
			}
			i = end
			textCol = i + 2
		default:
			if text.Len() == 0 {
				textCol = col
			}
			text.WriteRune(rs[i])
		}
	}
	flush()
	return t, nil
}

// indexRune is the index of r in rs at or after from, -1 for none.
func indexRune(rs []rune, from int, r rune) int {
	for i := from; i < len(rs); i++ {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

// parseStyle applies the items of a `#[…]` to the style in force:
// fg=colour, bg=colour, bold, nobold, dim, nodim and default, as tmux
// spells them. A colour is a palette name or one the config accepts.
func parseStyle(st style, items string) (style, error) {
	for _, item := range strings.Split(items, ",") {
		item = strings.TrimSpace(item)
		switch {
		case item == "":
		case item == "default":
			st = style{}
		case item == "bold":
			st.bold = true
		case item == "nobold":
			st.bold = false
		case item == "dim":
			st.dim = true
		case item == "nodim":
			st.dim = false
		case strings.HasPrefix(item, "fg=") || strings.HasPrefix(item, "bg="):
			c := strings.TrimSpace(item[3:])
			if c == "default" {
				// tmux's own: the colour cleared.
				c = ""
			} else if !paletteName(c) {
				if _, err := palette.Parse(c); err != nil {
					return st, fmt.Errorf("unknown colour %q", c)
				}
			}
			if item[:2] == "fg" {
				st.fg = c
			} else {
				st.bg = c
			}
		default:
			return st, fmt.Errorf("unknown style %q", item)
		}
	}
	return st, nil
}

// paletteName reports whether s names a palette colour.
func paletteName(s string) bool {
	for _, n := range palette.Names {
		if n == s {
			return true
		}
	}
	return false
}

// Templates is the set of lines the views draw with, compiled from the
// config with the defaults for what it leaves out. A template that did
// not parse keeps its error, which the view draws in its place.
type Templates struct {
	Tiles   []Compiled
	Compact Compiled
	Top     []Compiled
	Tree    TreeTemplates
}

// TreeTemplates is the tree's lines by node kind.
type TreeTemplates struct {
	Repo, Worktree, Agent, Pane, Run Compiled
}

// Compiled is a template with its parse error, named for the message.
type Compiled struct {
	Template
	Err string
}

// Compile parses a template named for its error message, "tiles[0]"
// say; an empty source is the default given. Only a tile line can be
// removed, by an empty entry in the list: the compact row and a tree
// node keep a line, so they can be selected, and a template of spaces
// alone draws it empty.
func Compile(name, src, def string) Compiled {
	if src == "" {
		src = def
	}
	t, err := ParseTemplate(src)
	if err != nil {
		return Compiled{Template: t, Err: "template error: " + err.Error() + " in " + name}
	}
	return Compiled{Template: t}
}

// The default templates: the tiles, the compact line, the top layout's
// item, and the tree's lines.
const (
	DefaultTile1    = "{stripe} {status_icon} {primary} {pane_suffix}{fill}{elapsed}"
	DefaultTile2    = "{stripe}    {secondary} @{host}{fill}{git_stats}"
	DefaultTile3    = "{stripe}    {pane_title}{fill}{pr_number} {pr_checks}"
	DefaultCompact  = "{stripe} {status_icon} {primary} {pane_suffix} {secondary} @{host}{fill}{git_stats} {elapsed}"
	DefaultTop1     = "{status_icon} {primary} {pane_suffix}"
	DefaultTop2     = "{secondary} @{host}"
	DefaultTop3     = "{pane_title}"
	DefaultRepo     = "#[fg=header,bold]{fold}{repo}"
	DefaultWorktree = "{indent}{fold}{primary} ({host}){fill}#[fg=warning]{status_label}#[default] {git_stats}  {pr_number} {pr_checks}  {worst_status}"
	DefaultAgent    = "{indent}{status_icon} {agent_label}  #[dim]{pane_title}"
	DefaultPane     = "{indent}$ {command}"
	DefaultRun      = "{indent}▶ {command}{fill}{elapsed}"
)

// DefaultTiles are the tile's three lines; DefaultTops the strip's
// chip's, drawn as far as its height allows.
var (
	DefaultTiles = []string{DefaultTile1, DefaultTile2, DefaultTile3}
	DefaultTops  = []string{DefaultTop1, DefaultTop2, DefaultTop3}
)

// DefaultTemplates is the set with nothing configured.
func DefaultTemplates() Templates {
	return CompileTemplates(nil, "", nil, "", "", "", "", "")
}

// CompileTemplates compiles the config's templates, the defaults where
// it sets none: a nil tiles or top list is the default three, an empty
// line in it a line removed.
func CompileTemplates(tiles []string, compact string, top []string, repo, worktree, agent, pane, run string) Templates {
	var t Templates
	if tiles == nil {
		tiles = DefaultTiles
	}
	for i, src := range tiles {
		t.Tiles = append(t.Tiles, Compile("tiles["+strconv.Itoa(i)+"]", src, ""))
	}
	t.Compact = Compile("compact", compact, DefaultCompact)
	if top == nil {
		top = DefaultTops
	}
	for i, src := range top {
		t.Top = append(t.Top, Compile("top["+strconv.Itoa(i)+"]", src, ""))
	}
	t.Tree.Repo = Compile("tree.repo", repo, DefaultRepo)
	t.Tree.Worktree = Compile("tree.worktree", worktree, DefaultWorktree)
	t.Tree.Agent = Compile("tree.agent", agent, DefaultAgent)
	t.Tree.Pane = Compile("tree.pane", pane, DefaultPane)
	t.Tree.Run = Compile("tree.run", run, DefaultRun)
	return t
}

// templates is the model's set, the defaults until SetTemplates.
func (m *Model) templates() *Templates {
	if m.tmpl == nil {
		t := DefaultTemplates()
		m.tmpl = &t
	}
	return m.tmpl
}

// SetTemplates gives the model the config's templates.
func (m *Model) SetTemplates(t Templates) { m.tmpl = &t }

// item is a template part evaluated for a row: literal text or a
// token's spans, with how it gives way.
type item struct {
	part   part
	spans  []Span
	kind   tokenKind
	shrink func(w int) []Span // a shrinking token's smaller forms
	whole  []Span             // a cut token's spans before the cut
	shrunk bool               // a shrinking token in a smaller form
}

func (it item) width() int { return spansWidth(it.spans) }

// line draws one template for a row in w cells: the evaluated parts
// fitted to the width by the rules above. A template that did not parse
// draws its error.
func (m *Model) line(t Compiled, r rows.Row, w int) []Span {
	if t.Err != "" {
		return clip([]Span{{Text: t.Err, Fg: palette.Danger}}, w)
	}
	var left, right []item
	var gap style // the fill's style, on the padding
	side := &left
	for _, p := range t.parts {
		switch p.kind {
		case partFill:
			side = &right
			gap = p.st
		case partText:
			*side = append(*side, item{part: p, spans: []Span{styled(Span{Text: p.text}, p.st)}})
		case partToken:
			it := m.token(p.text, r)
			it.part = p
			for i := range it.spans {
				it.spans[i] = styled(it.spans[i], p.st)
			}
			if it.shrink != nil {
				inner := it.shrink
				it.shrink = func(w int) []Span {
					out := inner(w)
					for i := range out {
						out[i] = styled(out[i], p.st)
					}
					return out
				}
			}
			*side = append(*side, it)
		}
	}
	left, right = collapse(left), collapse(right)
	stale := r.Branch != nil && r.Branch.Stale
	staleMark(left, right, stale, false)
	if itemsWidth(right) == 0 {
		// Nothing on the right: no gap, and nothing to drop.
		right = nil
	}
	total := func() int {
		n := itemsWidth(left) + itemsWidth(right)
		if len(right) > 0 && itemsWidth(left) > 0 {
			n++ // the gap
		}
		return n
	}
	floor := min(12, w/3)
	for total() > w {
		over := total() - w
		switch {
		case cutFlex(right, over, floor), cutFlex(left, over, floor):
		case shrinkAny(left, right, over):
		case len(right) > 0:
			right = dropWidest(right)
			// The checks gone: the number takes the stale mark, and
			// the fitting goes on with it counted.
			staleMark(left, right, stale, true)
		case cutFlex(left, over, 2):
		case len(left) > 0 && hasToken(left):
			// The last token on the left, whole: a PR number clipped
			// would lose its stale mark.
			left = dropLast(left)
			staleMark(left, right, stale, true)
		default:
			return clip(flatten(left), w)
		}
	}
	regrow(left, right, w-total())
	// A label cut away and not grown back leaves as an empty token
	// does, with its spaces.
	left, right = collapse(left), collapse(right)
	out := flatten(left)
	if len(right) > 0 {
		pad := w - itemsWidth(left) - itemsWidth(right)
		out = append(out, styled(Span{Text: strings.Repeat(" ", pad)}, gap))
		out = append(out, flatten(right)...)
	}
	return clip(out, w)
}

// staleMark puts a stale branch's ? on the PR pair once: the checks
// carry it when they are drawn, else the number does. Before the
// fitting the number's goes when the checks are there; when a drop
// takes the checks, the number takes it back, and the fitting counts
// it.
func staleMark(left, right []item, stale, back bool) {
	if !stale {
		return
	}
	var number *item
	checks := false
	for _, items := range [][]item{left, right} {
		for i := range items {
			it := &items[i]
			if it.part.kind != partToken || it.width() == 0 {
				continue
			}
			switch it.part.text {
			case "pr_number":
				number = it
			case "pr_checks":
				checks = true
			}
		}
	}
	if number == nil {
		return
	}
	last := number.spans[len(number.spans)-1]
	switch {
	case checks && last.Text == "?":
		number.spans = number.spans[:len(number.spans)-1]
	case !checks && last.Text != "?" && back:
		number.spans = append(number.spans, styled(Span{Text: "?", Dim: true}, number.part.st))
	}
}

// styled is a span with a style's settings where the span has none of
// its own.
func styled(sp Span, st style) Span {
	if sp.own {
		return sp
	}
	if sp.Fg == "" {
		sp.Fg = st.fg
	}
	if sp.Bg == "" {
		sp.Bg = st.bg
	}
	sp.Bold = sp.Bold || st.bold
	sp.Dim = sp.Dim || st.dim
	return sp
}

// collapse removes empty tokens, each with the adjacent run of spaces:
// the one after it, else the one before.
func collapse(items []item) []item {
	var out []item
	for i := 0; i < len(items); i++ {
		it := items[i]
		if it.part.kind == partToken && it.width() == 0 {
			// The run may span text parts a style split.
			switch {
			case i+1 < len(items) && items[i+1].part.kind == partText && strings.HasPrefix(items[i+1].spans[0].Text, " "):
				for j := i + 1; j < len(items) && items[j].part.kind == partText; j++ {
					t := items[j].spans[0].Text
					items[j].spans[0].Text = strings.TrimLeft(t, " ")
					if strings.TrimLeft(t, " ") != "" {
						break
					}
				}
			case len(out) > 0 && out[len(out)-1].part.kind == partText && strings.HasSuffix(out[len(out)-1].spans[0].Text, " "):
				for j := len(out) - 1; j >= 0 && out[j].part.kind == partText; j-- {
					t := out[j].spans[0].Text
					out[j].spans[0].Text = strings.TrimRight(t, " ")
					if strings.TrimRight(t, " ") != "" {
						break
					}
				}
			}
			continue
		}
		if it.part.kind == partText && it.spans[0].Text == "" {
			continue
		}
		out = append(out, it)
	}
	// Text parts a trim before emptied.
	kept := out[:0]
	for _, it := range out {
		if it.part.kind == partText && it.spans[0].Text == "" {
			continue
		}
		kept = append(kept, it)
	}
	return kept
}

func itemsWidth(items []item) int {
	n := 0
	for _, it := range items {
		n += it.width()
	}
	return n
}

func flatten(items []item) []Span {
	var out []Span
	for _, it := range items {
		out = append(out, it.spans...)
	}
	return out
}

// shrinkAny asks the shrinking tokens, the rightmost first, for a form
// that takes over fewer cells; true when one did.
func shrinkAny(left, right []item, over int) bool {
	for _, side := range [][]item{right, left} {
		for i := len(side) - 1; i >= 0; i-- {
			it := &side[i]
			if it.shrink == nil {
				continue
			}
			cur := it.width()
			if cur == 0 {
				continue
			}
			if got := it.shrink(cur - over); spansWidth(got) < cur {
				it.spans, it.shrunk = got, true
				return true
			}
		}
	}
	return false
}

// cutFlex cuts the rightmost flexible token wider than floor down to it,
// or by over when that is less; true when one was cut. A token cut
// under two cells is dropped instead.
func cutFlex(items []item, over, floor int) bool {
	for i := len(items) - 1; i >= 0; i-- {
		it := &items[i]
		if it.kind != tokenFlex || it.width() <= floor {
			continue
		}
		to := max(it.width()-over, floor)
		if it.whole == nil {
			it.whole = it.spans
		}
		if to < 2 {
			it.spans = nil
			return true
		}
		it.spans = cutSpans(it.whole, to)
		return true
	}
	return false
}

// regrow gives room cells back, what dropping a field on the right
// left over: to the cut tokens first, the leftmost first, never to
// under two cells, then to the shrunk ones.
func regrow(left, right []item, room int) {
	for _, items := range [][]item{left, right} {
		for i := range items {
			it := &items[i]
			if it.whole == nil || room <= 0 {
				continue
			}
			whole := spansWidth(it.whole)
			switch to := it.width() + room; {
			case to >= whole:
				room -= whole - it.width()
				it.spans = it.whole
			case to < 2:
			default:
				it.spans = cutSpans(it.whole, to)
				room = 0
			}
		}
	}
	for _, items := range [][]item{left, right} {
		for i := range items {
			it := &items[i]
			if !it.shrunk || room <= 0 {
				continue
			}
			if got := it.shrink(it.width() + room); spansWidth(got) > it.width() && spansWidth(got) <= it.width()+room {
				room -= spansWidth(got) - it.width()
				it.spans = got
			}
		}
	}
}

// cutSpans cuts spans to w cells, the last kept ending in ….
func cutSpans(spans []Span, w int) []Span {
	out := clip(spans, w-1)
	if len(out) == 0 {
		return []Span{{Text: "…"}}
	}
	out[len(out)-1].Text += "…"
	return out
}

// hasToken reports whether items hold a token.
func hasToken(items []item) bool {
	for _, it := range items {
		if it.part.kind == partToken {
			return true
		}
	}
	return false
}

// dropLast drops the last token with its separator.
func dropLast(items []item) []item {
	at := -1
	for i, it := range items {
		if it.part.kind == partToken {
			at = i
		}
	}
	if at < 0 {
		return items
	}
	return remove(items, at)
}

// remove takes the token at i out with its separator: what the literal
// before it ends with, else what the literal after it starts with, up
// to a bracket, which is a neighbour's; a bracket pair around the token
// alone, `({host})` say, goes with it. A literal left empty goes too.
func remove(items []item, at int) []item {
	var prev, next *string
	if at > 0 && items[at-1].part.kind == partText {
		prev = &items[at-1].spans[0].Text
	}
	if at+1 < len(items) && items[at+1].part.kind == partText {
		next = &items[at+1].spans[0].Text
	}
	if prev != nil && next != nil {
		if open, ok := lastRune(*prev); ok {
			if close, ok := brackets[open]; ok && strings.HasPrefix(*next, string(close)) {
				*prev = strings.TrimSuffix(*prev, string(open))
				*next = strings.TrimPrefix(*next, string(close))
			}
		}
	}
	// The separator may span text parts a style split, as a run of
	// spaces may for collapse.
	sep := func(r rune) bool { return !strings.ContainsRune("()[]{}<>", r) }
	// The side the separator is on: by the last text before the token,
	// past a part the bracket pair emptied.
	before := false
	for j := at - 1; j >= 0 && items[j].part.kind == partText; j-- {
		if r, ok := lastRune(items[j].spans[0].Text); ok {
			before = sep(r)
			break
		}
	}
	switch {
	case before:
		for j := at - 1; j >= 0 && items[j].part.kind == partText; j-- {
			t := &items[j].spans[0].Text
			*t = strings.TrimRightFunc(*t, sep)
			if *t != "" {
				break
			}
		}
	case next != nil:
		for j := at + 1; j < len(items) && items[j].part.kind == partText; j++ {
			t := &items[j].spans[0].Text
			*t = strings.TrimLeftFunc(*t, sep)
			if *t != "" {
				break
			}
		}
	}
	out := append(items[:at:at], items[at+1:]...)
	kept := out[:0]
	for _, it := range out {
		if it.part.kind == partText && it.spans[0].Text == "" {
			continue
		}
		kept = append(kept, it)
	}
	return kept
}

// brackets pairs an opening bracket with its closing one.
var brackets = map[rune]rune{'(': ')', '[': ']', '{': '}', '<': '>'}

// lastRune is the last rune of s, ok for a non-empty s.
func lastRune(s string) (rune, bool) {
	rs := []rune(s)
	if len(rs) == 0 {
		return 0, false
	}
	return rs[len(rs)-1], true
}

// dropWidest drops the widest token on the right, the last of equals,
// with its separator. A folded line's icon, {worst_status}, goes last:
// a blocked or done agent inside is not to be missed.
func dropWidest(right []item) []item {
	at, widest := -1, -1
	for i, it := range right {
		if it.part.kind != partToken || it.part.text == "worst_status" {
			continue
		}
		// Of equals the last goes, except that the number goes before
		// the checks: on a stale row the number would take the mark
		// and go too.
		if it.width() > widest || it.width() == widest && !(right[at].part.text == "pr_number" && it.part.text == "pr_checks") {
			at, widest = i, it.width()
		}
	}
	if at < 0 {
		for i, it := range right {
			if it.part.kind == partToken {
				at = i
			}
		}
	}
	if at < 0 {
		return nil
	}
	return remove(right, at)
}

// token evaluates a token for a row.
func (m *Model) token(name string, r rows.Row) item {
	it := item{kind: tokens[name]}
	text := func(s string) item {
		if s != "" {
			it.spans = []Span{{Text: s}}
		}
		return it
	}
	p, sec := r.Labels()
	if r.Orphaned {
		p = r.Name
	}
	var g *protocol.GitStatus
	if r.Worktree != nil {
		g = r.Worktree.Git
	}
	switch name {
	case "primary":
		if r.Kind == rows.KindRepo {
			p = r.Name
		}
		if p != "" {
			it.spans = []Span{m.primary(r, p)}
		}
		return it
	case "secondary":
		return text(sec)
	case "branch":
		switch {
		case r.Pending != nil:
			return text(r.Pending.Branch)
		case r.Worktree != nil:
			return text(r.Worktree.Branch)
		case r.Local != nil:
			return text(r.Local.Branch)
		}
		return it
	case "repo":
		switch {
		case r.Kind == rows.KindRepo:
			return text(r.Name)
		case r.Pending != nil:
			return text(r.Pending.Repo)
		case r.Worktree != nil:
			return text(r.Worktree.Repo)
		}
		return it
	case "host":
		switch r.Kind {
		case rows.KindRepo, rows.KindFold, rows.KindGroup:
			return it
		case rows.KindTile, rows.KindAgent:
			// The agent's server after the host when observed off the
			// managed server; ? for a host unknown.
			where := m.where(r)
			where.Text = strings.TrimPrefix(where.Text, "@")
			it.spans = []Span{where}
			return it
		}
		host := r.Host
		if host == "" {
			host = "?" // no host record claims the record
		}
		it.spans = []Span{{Text: host, Dim: host != m.LocalHost}}
		return it
	case "session":
		switch {
		case r.Agent != nil:
			return text(r.Agent.Session)
		case r.Pane != nil:
			return text(r.Pane.Session)
		case r.Worktree != nil && r.Worktree.Session != "":
			return text(r.Worktree.Session)
		case r.Pending != nil:
			return text(r.Pending.Session)
		case r.Local != nil:
			// A local session alone, an orphaned one say.
			return text(r.Local.Name)
		}
		return it
	case "window":
		// tmux's target for the window: the records carry no name.
		if r.Agent != nil {
			return text(r.Agent.Session + ":" + strconv.Itoa(r.Agent.Window))
		}
		if r.Pane != nil {
			return text(r.Pane.Session + ":" + strconv.Itoa(r.Pane.Window))
		}
		return it
	case "window_index":
		if r.Agent != nil {
			return text(strconv.Itoa(r.Agent.Window))
		}
		if r.Pane != nil {
			return text(strconv.Itoa(r.Pane.Window))
		}
		return it
	case "pane_title":
		if r.Kind == rows.KindAgent && r.Agent != nil && r.Agent.Liveness == protocol.Gone {
			return text("gone")
		}
		if r.Kind == rows.KindAgent && r.Agent != nil {
			return text(cleanTitle(r.Agent.Title, p, sec, r.Host, m.Machine))
		}
		if r.Kind == rows.KindTile {
			return text(m.third(r))
		}
		return it
	case "pane_suffix":
		if r.Suffix != "" {
			it.spans = []Span{m.primary(r, r.Suffix)}
		}
		return it
	case "status_icon":
		if r.Kind == rows.KindTile || r.Kind == rows.KindAgent {
			it.spans = []Span{m.iconSpan(r)}
		}
		return it
	case "status_label":
		if r.Orphaned && r.Pending == nil {
			// Dim, as the line is: no colour a style would give it.
			it.spans = []Span{{Text: "worktree gone", Dim: true, own: true}}
			return it
		}
		return text(m.statusLabel(r))
	case "agent_icon":
		if r.Agent == nil {
			return it
		}
		if ic, ok := AgentIconFor(r.Agent.Agent, m.AgentIcons); ok {
			it.spans = []Span{{Text: ic.Icon, Fg: ic.Color}}
		}
		return it
	case "agent_label":
		return text(r.AgentName())
	case "elapsed":
		if r.Kind == rows.KindRun && r.Run != nil {
			d := m.Now.Sub(r.Run.StartedAt)
			it.spans = []Span{{Text: elapsed(d), tick: d < time.Hour}}
			return it
		}
		if t, secs := m.since(r); t != "" {
			it.spans = []Span{{Text: t, tick: secs}}
		}
		return it
	case "stripe":
		if r.Kind == rows.KindTile {
			it.spans = []Span{m.stripe(r)}
		}
		return it
	case "git_stats":
		it.spans = gitSpans(r, 1<<20)
		if len(it.spans) > 0 {
			// Never to nothing: the smallest form stays until the
			// field is dropped.
			least := gitSpans(r, 1)
			for w := 2; least == nil && w <= spansWidth(it.spans); w++ {
				least = gitSpans(r, w)
			}
			it.shrink = func(w int) []Span {
				if s := gitSpans(r, max(w, 1)); s != nil {
					return s
				}
				return least
			}
		}
		return it
	case "git_committed":
		if g != nil {
			it.spans = gitCommitted(g)
			if g.Stale {
				it.spans = gitStale(it.spans)
			}
		}
		return it
	case "git_uncommitted":
		if g != nil {
			it.spans = gitUncommitted(g)
			if g.Stale {
				it.spans = gitStale(it.spans)
			}
		}
		return it
	case "git_ahead", "git_behind", "git_dirty", "git_conflict", "git_rebase":
		if g == nil {
			return it
		}
		switch {
		case name == "git_ahead" && g.Ahead > 0:
			it.spans = []Span{{Text: "↑" + strconv.Itoa(g.Ahead)}}
		case name == "git_behind" && g.Behind > 0:
			it.spans = []Span{{Text: "↓" + strconv.Itoa(g.Behind)}}
		case name == "git_dirty" && (g.Dirty || g.Uncommitted != [2]int{}):
			it.spans = []Span{{Text: "✎"}}
		case name == "git_conflict" && g.Conflict != nil && *g.Conflict:
			it.spans = []Span{{Text: "!", Fg: palette.Danger, Bold: true}}
		case name == "git_rebase":
			it.spans = gitRebase(g)
		}
		if g.Stale {
			// A refresh that timed out: dim and plain, as the stats.
			it.spans = gitStale(it.spans)
		}
		return it
	case "git_branch":
		if g != nil && g.Base != "" {
			return text(g.Base)
		}
		return it
	case "pr_number":
		it.spans = m.prNumber(r)
		return it
	case "pr_checks":
		it.spans = m.prChecks(r, 1<<20)
		if len(it.spans) > 0 {
			it.shrink = func(w int) []Span { return m.prChecks(r, max(w, 1)) }
		}
		return it
	case "idx":
		if r.Numbered() && m.rowIdx > 0 {
			return text(strconv.Itoa(m.rowIdx))
		}
		return it
	case "jump_key":
		if r.Numbered() && m.JumpKeys && m.rowIdx > 0 && m.rowIdx <= 9 {
			return text("M-" + strconv.Itoa(m.rowIdx))
		}
		return it
	case "repo_count":
		if r.Kind == rows.KindRepo {
			return text(strconv.Itoa(r.Children))
		}
		return it
	case "fold":
		switch {
		case r.Kind == rows.KindRepo:
			if m.closed(&r) {
				return text("▸ ")
			}
			return it
		case r.Kind == rows.KindWorktree || r.Kind == rows.KindTask:
			if !r.Foldable() {
				return text("  ")
			}
			if m.closed(&r) {
				return text("▸ ")
			}
			return text("▾ ")
		}
		return it
	case "worst_status":
		if (r.Kind == rows.KindWorktree || r.Kind == rows.KindTask) && r.Foldable() && m.closed(&r) && r.Worst != nil {
			// The most pressing agent inside, so a blocked or done one
			// is not missed.
			if icon := m.iconSpan(*r.Worst); strings.TrimSpace(icon.Text) != "" {
				it.spans = []Span{icon}
			}
		}
		return it
	case "child_count":
		if r.Kind == rows.KindWorktree || r.Kind == rows.KindTask {
			return text(strconv.Itoa(r.Children))
		}
		return it
	case "command":
		if r.Kind == rows.KindPane || r.Kind == rows.KindRun {
			return text(r.Name)
		}
		return it
	case "indent":
		return text(strings.Repeat("  ", r.Depth))
	}
	return it
}

// statusLabel is a row's status as a word: a task's state, `worktree
// gone` for an orphaned line, the agent's status on a tile or an agent
// line; "" on a worktree line, whose folded icon says it.
func (m *Model) statusLabel(r rows.Row) string {
	switch {
	case r.Pending != nil:
		return r.State()
	case r.Orphaned:
		return "worktree gone"
	case r.Kind != rows.KindTile && r.Kind != rows.KindAgent:
		return ""
	case r.Agent == nil:
		return r.State()
	case r.Agent.Liveness == protocol.Gone:
		return "gone"
	}
	switch status(r) {
	case StatusWaiting:
		return "waiting"
	case StatusDone:
		return "done"
	case StatusStale:
		if r.Settled && !r.Stale {
			return "settled"
		}
		return "stale"
	case StatusWorking:
		return "working"
	}
	return "idle"
}

// prNumber is the branch's PR: #N, green when open, purple when
// merged, red when closed, dim when a draft, and dim with ? after when
// the answer is stale; nothing on main or master.
func (m *Model) prNumber(r rows.Row) []Span {
	b := r.Branch
	if b == nil || b.PR == nil || mainline(r) {
		return nil
	}
	// Without colours the states still differ: open bold, merged
	// plain, closed and draft faint.
	sp := Span{Text: fmt.Sprintf("#%d", b.PR.Number)}
	switch {
	case b.PR.Draft:
		sp.Dim = true
	case b.PR.State == "open":
		sp.Fg, sp.Bold = palette.Success, true
	case b.PR.State == "merged":
		sp.Fg = palette.Accent
	default:
		sp.Fg, sp.Dim = palette.Danger, true
	}
	out := []Span{sp}
	if b.Stale {
		// Dim, with ? after; staleMark takes it off when the checks
		// are drawn with theirs.
		for i := range out {
			out[i].Dim, out[i].Fg = true, ""
		}
		out = append(out, Span{Text: "?", Dim: true})
	}
	return out
}

// mainline is a worktree on main or master, whose PR is left out and
// whose checks show only when they fail.
func mainline(r rows.Row) bool {
	return r.Worktree != nil && (r.Worktree.Branch == "main" || r.Worktree.Branch == "master")
}

// prChecks is the branch's checks in at most w cells: ✓ in green, × 3/5
// in red, or a spinner and 3/5 in purple; when narrow the counts go
// first; a stale answer dim with ? after.
func (m *Model) prChecks(r rows.Row, w int) []Span {
	b := r.Branch
	if b == nil || b.Checks == nil || mainline(r) && b.Checks.State != protocol.ChecksFailure {
		return nil
	}
	ch := b.Checks
	ratio := fmt.Sprintf("%d/%d", ch.Passed, ch.Total)
	ascii := m.Icons.Set == IconsASCII
	var mark, counts []Span
	switch ch.State {
	case protocol.ChecksSuccess:
		mark = []Span{{Text: map[bool]string{false: "✓", true: "ok"}[ascii], Fg: palette.Success}}
	case protocol.ChecksFailure:
		mark = []Span{{Text: map[bool]string{false: "×", true: "x"}[ascii], Fg: palette.Danger}}
		counts = []Span{{Text: " " + ratio, Fg: palette.Danger}}
	case protocol.ChecksPending:
		// The spinner spins on a live row with a fresh answer; a
		// stale or dim one stands still.
		spinning := !b.Stale && !r.Dim && !ascii
		text := string([]rune(spinnerFrames[0])[0])
		switch {
		case ascii:
			text = "*"
		case spinning:
			text = string([]rune(frame(m.Now))[0])
		}
		mark = []Span{{Text: text, Fg: palette.Accent, spin: spinning}}
		counts = []Span{{Text: " " + ratio, Fg: palette.Accent}}
	default:
		return nil
	}
	finish := func(spans []Span) []Span {
		out := append([]Span{}, spans...)
		if b.Stale {
			out = append(out, Span{Text: "?"})
			for i := range out {
				out[i].Dim, out[i].Fg = true, ""
			}
		}
		return out
	}
	full := finish(append(append([]Span{}, mark...), counts...))
	if spansWidth(full) <= w || len(counts) == 0 {
		return full
	}
	return finish(mark)
}
