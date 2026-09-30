package view

import (
	"fmt"
	"strconv"
	"strings"
)

// A sidebar subcommand reaches a pane over its socket and is handled as
// a navigation event, not as typed keys: it moves the selection or
// switches the view whether the pane is filtering or not, and is
// ignored while an overlay or a question is open.

// Command is one such event.
type Command struct {
	// Name is next, prev, jump, view or scope.
	Name string
	// N is jump's digit; Arg view's view or scope's scope.
	N   int
	Arg string
	// Client is the tmux client a jump switches, "" for the pane's own.
	Client string
}

// ParseCommand reads a command as the CLI sends it: the name and its
// argument, then `client=<name>` for a jump.
func ParseCommand(line string) (Command, error) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return Command{}, fmt.Errorf("empty command")
	}
	c := Command{Name: f[0]}
	args := f[1:]
	if n := len(args); n > 0 && strings.HasPrefix(args[n-1], "client=") {
		c.Client = strings.TrimPrefix(args[n-1], "client=")
		args = args[:n-1]
	}
	switch c.Name {
	case "next", "prev":
		if len(args) != 0 {
			return c, fmt.Errorf("%s takes no argument", c.Name)
		}
	case "jump":
		if len(args) != 1 {
			return c, fmt.Errorf("jump takes a number")
		}
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 1 || n > 9 {
			return c, fmt.Errorf("jump %q: not 1 to 9", args[0])
		}
		c.N = n
	case "view":
		if len(args) != 1 {
			return c, fmt.Errorf("view takes agents or tree")
		}
		if _, err := ParseView(args[0]); err != nil {
			return c, err
		}
		c.Arg = args[0]
	case "scope":
		if len(args) != 1 {
			return c, fmt.Errorf("scope takes all, session or project")
		}
		if _, err := ParseScope(args[0]); err != nil {
			return c, err
		}
		c.Arg = args[0]
	default:
		return c, fmt.Errorf("unknown command %q", c.Name)
	}
	return c, nil
}

// Command applies a command to the model: the selection moved or the
// view or scope switched, a jump's action returned for the host. With
// an overlay or a question up it does nothing.
func (m *Model) Command(c Command) Action {
	if m.Overlay != nil || m.Confirm != "" {
		return Action{}
	}
	switch c.Name {
	case "next":
		m.move(1)
	case "prev":
		m.move(-1)
	case "jump":
		if i, ok := m.nth(c.N); ok {
			return m.jumpTo(i)
		}
	case "view":
		// The CLI wrote the default: the pane only applies it; a strip
		// shows the agent view alone.
		if v, err := ParseView(c.Arg); err == nil && v != m.View && m.Layout != Strip {
			m.Switch()
		}
		m.settings = false
	case "scope":
		if s, err := ParseScope(c.Arg); err == nil && s != m.scope() {
			m.Scope, m.prevScope = s, ""
			m.Selection()
		}
		m.settings = false
	}
	return Action{}
}
