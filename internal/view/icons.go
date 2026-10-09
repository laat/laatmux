package view

import (
	"fmt"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/palette"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/rows"
)

// Status is what a row's icon and stripe say: waiting (blocked, or a
// task that needs the user), working, done, stale (idle for long, or a
// settled workspace's agent that does not want the user), and none.
type Status int

const (
	StatusNone Status = iota
	StatusWorking
	StatusWaiting
	StatusDone
	StatusStale
)

// statusColor is the palette colour of a status's icon and stripe;
// none has the border's.
var statusColor = map[Status]string{
	StatusNone:    palette.Border,
	StatusWorking: palette.Info,
	StatusWaiting: palette.Accent,
	StatusDone:    palette.Success,
	StatusStale:   palette.Dimmed,
}

// Icon sets. A working icon of "" is the spinner.
const (
	IconsEmoji    = "emoji"
	IconsNerdFont = "nerdfont"
	IconsASCII    = "ascii"
)

var iconSets = map[string]map[Status]string{
	IconsEmoji:    {StatusWaiting: "💬", StatusDone: "✅", StatusStale: "💤"},
	IconsNerdFont: {StatusWaiting: "\uf075", StatusDone: "\U000f0134", StatusStale: "\U000f04b2"},
	IconsASCII:    {StatusWorking: "*", StatusWaiting: "!", StatusDone: "-", StatusStale: "-"},
}

// Icons is the icon set the views draw statuses with, and the icons the
// config sets by status over it; "" keeps the set's. Worktree and Main
// are the config's kind icons over the set's in the same way.
type Icons struct {
	Set                           string
	Working, Waiting, Done, Stale string
	Worktree, Main                string
}

// icon is a status's icon, "" for the spinner on working.
func (ic Icons) icon(s Status) string {
	over := map[Status]string{StatusWorking: ic.Working, StatusWaiting: ic.Waiting, StatusDone: ic.Done, StatusStale: ic.Stale}[s]
	if over != "" {
		return over
	}
	set, ok := iconSets[ic.Set]
	if !ok {
		set = iconSets[IconsEmoji]
	}
	return set[s]
}

// kindIcons are the sets' glyphs for what a row's checkout is: a
// worktree, and a repository's main checkout.
var kindIcons = map[string]struct{ worktree, main string }{
	IconsEmoji:    {"⎇", "⌂"},
	IconsNerdFont: {"\uf418", "\uf015"},
	IconsASCII:    {"+", "="},
}

// kind is a row's `{kind_icon}`: the main checkout's glyph on a main
// checkout's line and the rows under it, the worktree's on a
// worktree's, a detached one's too; "" on a row in no worktree, an
// orphaned session's or another session's, and on a task's, which is
// neither while it stands for one.
func (ic Icons) kind(r rows.Row) string {
	if r.Worktree == nil || r.Pending != nil {
		return ""
	}
	set, ok := kindIcons[ic.Set]
	if !ok {
		set = kindIcons[IconsEmoji]
	}
	if r.Worktree.Main {
		if ic.Main != "" {
			return ic.Main
		}
		return set.main
	}
	if ic.Worktree != "" {
		return ic.Worktree
	}
	return set.worktree
}

// iconWidth is the cells an icon takes on a line: the spinner's two, so
// a set of one-cell icons lines the labels up with the spinner's rows.
const iconWidth = 2

// The spinner a working row's icon cycles through: two braille cells, as
// workmux draws it, one frame per spinTick from the clock, so the panes
// in every window spin in step.
var spinnerFrames = []string{"⠋⠙", "⠙⠹", "⠹⠸", "⠸⠼", "⠼⠴", "⠴⠦", "⠦⠧", "⠧⠇", "⠇⠏", "⠏⠋"}

const spinTick = 250 * time.Millisecond

// frame is the spinner frame for t. A zero t, before the first draw, is
// a negative count.
func frame(t time.Time) string {
	n := int64(len(spinnerFrames))
	i := (t.UnixNano()/int64(spinTick))%n + n
	return spinnerFrames[i%n]
}

// status is a row's status, in the note's precedence: blocked, done,
// then stale or settled, then working. A dim row keeps its status for the
// icon, but does not spin: its agent is gone, its host down, or it is
// settled.
func status(r rows.Row) Status {
	switch {
	case r.Pending != nil && r.NeedsUser():
		return StatusWaiting
	case r.Pending != nil:
		return StatusWorking
	case r.Agent == nil || r.Agent.Liveness == protocol.Gone:
		return StatusNone
	case r.Agent.Activity == protocol.Blocked:
		return StatusWaiting
	case r.Done:
		return StatusDone
	case r.Stale || r.Settled:
		return StatusStale
	case r.Agent.Activity == protocol.Working:
		return StatusWorking
	}
	return StatusNone
}

// spins reports whether the row's icon is the spinner: a working status
// on a row that is not dim, with no working icon set.
func (m *Model) spins(r rows.Row) bool {
	return status(r) == StatusWorking && !r.Dim && m.Icons.icon(StatusWorking) == ""
}

// iconSpan is the row's icon, padded to iconWidth, in its status colour.
func (m *Model) iconSpan(r rows.Row) Span {
	st := status(r)
	var text string
	switch {
	case m.spins(r):
		return Span{Text: frame(m.Now), Fg: statusColor[st], spin: true}
	case st == StatusWorking && m.Icons.icon(st) == "":
		// A dim working row, on a host that is down say: the spinner
		// stands still.
		text = spinnerFrames[0]
	default:
		text = m.Icons.icon(st)
	}
	return Span{Text: pad(fit(text, iconWidth), iconWidth), Fg: statusColor[st]}
}

// stripe is the bar down a row's left edge, in its status colour.
func (m *Model) stripe(r rows.Row) Span {
	return Span{Text: "▌", Fg: statusColor[status(r)]}
}

// AgentIcon is an agent's icon and its colour, `#rrggbb` or 0 to 255,
// for the `{agent_icon}` template token.
type AgentIcon struct {
	Icon, Color string
}

// DefaultAgentIcons are workmux's for the agents laatmux detects.
var DefaultAgentIcons = map[string]AgentIcon{
	"claude": {Icon: "CC", Color: "#d97757"},
	"codex":  {Icon: "CX", Color: "#10a37f"},
}

// AgentIconFor is an agent's icon, the config's over the default; ok is
// false for an agent neither knows.
func AgentIconFor(agent string, over map[string]AgentIcon) (AgentIcon, bool) {
	def, known := DefaultAgentIcons[agent]
	o, set := over[agent]
	if o.Icon == "" {
		o.Icon = def.Icon
	}
	if o.Color == "" {
		o.Color = def.Color
	}
	return o, (known || set) && o.Icon != ""
}

// shells are the names a pane title is dropped for: a shell's own name
// says nothing the row does not.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "tcsh": true, "nu": true, "pwsh": true, "-zsh": true, "-bash": true}

// cleanTitle is the pane title as the views show it: the leading spinner
// and status characters and an `OC |` prefix stripped; a title that
// starts with `Claude Code`, is a shell's name, repeats a label, or is
// the host's name or one of the machine names given, dropped. tmux
// titles a pane with the machine's name until the program sets one.
// A machine's name matches whole or as its first label; a title that
// merely begins with a name, a file `dev.yaml` say, stays.
func cleanTitle(title, primary, secondary, host string, machines ...string) string {
	t := strings.TrimSpace(title)
	for {
		trimmed := strings.TrimLeftFunc(t, func(c rune) bool {
			return c >= 0x2800 && c <= 0x28ff || c >= 0x25d0 && c <= 0x25d3 || strings.ContainsRune("✳●○◌✓✗ ", c)
		})
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "OC |"))
		if trimmed == t {
			break
		}
		t = trimmed
	}
	switch {
	case t == "", strings.HasPrefix(t, "Claude Code"), shells[t],
		t == primary, secondary != "" && t == secondary, host != "" && t == host:
		return ""
	}
	// A machine's name matches whole, or its first label alone: tmux
	// titles a pane with the name the system gives, which is one of
	// the two; a title that merely begins with the name stays.
	for _, name := range machines {
		if name != "" && (t == name || t == strings.SplitN(name, ".", 2)[0]) {
			return ""
		}
	}
	return t
}

// elapsed is the time since a status changed as the views show it:
// `m:ss` under an hour, then `Nh`, then `Nd`.
func elapsed(d time.Duration) string {
	switch {
	case d < 0:
		return "0:00"
	case d < time.Hour:
		s := int(d.Seconds())
		return fmt.Sprintf("%d:%02d", s/60, s%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
