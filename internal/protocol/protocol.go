// Package protocol defines the JSON-lines wire format between a laatmux
// client and a laatmux daemon. One JSON object per line, in both directions.
//
// The contract is the boundary between independently released clients and
// daemons. A client branches on the capability set in the daemon's hello
// reply, never on the version string.
package protocol

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Version is the protocol version this build speaks.
const Version = 1

// Message types.
const (
	TypeHello     = "hello"     // both directions; first message on a connection
	TypeSubscribe = "subscribe" // client -> daemon
	TypeSnapshot  = "snapshot"  // daemon -> client, full state after subscribe
	TypeUpsert    = "upsert"    // daemon -> client, one agent changed or appeared
	TypeRemove    = "remove"    // daemon -> client, one agent disappeared
	TypeNew       = "new"       // client -> daemon, create a managed session
	TypeAdd       = "add"       // client -> daemon, create a worktree and start an agent in it
	TypeRm        = "rm"        // client -> daemon, remove a worktree and its managed session
	TypeProgress  = "progress"  // daemon -> client, one step of a running add
	TypeResult    = "result"    // daemon -> client, reply to a command
	TypePing      = "ping"
	TypePong      = "pong"
	TypeError     = "error"
)

// Capabilities a daemon may advertise.
const (
	CapStatus    = "status"    // subscribe / snapshot / upsert / remove
	CapNew       = "new"       // the new command
	CapWorktrees = "worktrees" // worktree records in the subscription stream
	CapAdd       = "add"       // the add command
	CapRm        = "rm"        // the rm command
)

// Progress states, in Message.State of a progress message. A stage may
// report several steps; each step ends in done or skip, and a step that
// runs a command may stream its output first.
const (
	StateStart  = "start"  // a mutating step began; Detail names it
	StateDone   = "done"   // the step completed
	StateSkip   = "skip"   // the step was already done; Detail says how that was seen
	StateOutput = "output" // one line of a setup command's output, in Detail
)

// Stages of add, in order. A result carries the stage that failed.
const (
	StageResolve  = "resolve"
	StageClone    = "clone"
	StageFetch    = "fetch"
	StageWorktree = "worktree"
	StageCopy     = "copy"
	StageSetup    = "setup"
	StageAgent    = "agent"
)

// Activity is what the agent on screen appears to be doing.
type Activity string

const (
	Working Activity = "working"
	Blocked Activity = "blocked"
	Idle    Activity = "idle"
	Unknown Activity = "unknown"
)

// Liveness is whether the identified agent process is still there.
type Liveness string

const (
	Alive Liveness = "alive"
	Gone  Liveness = "gone" // pane exists, identified process does not
	None  Liveness = "none" // no agent process was ever identified in this pane
)

// Identity pins an agent instance: the process, not the pane.
type Identity struct {
	PID       int    `json:"pid"`
	StartUnix int64  `json:"start_unix"`
	Comm      string `json:"comm"`
	LeaderPID int    `json:"leader_pid,omitempty"` // foreground process group leader of the tty
}

// Agent is one pane on one host as the sidebar sees it. Only panes with an
// identified agent instance, alive or gone, are published.
type Agent struct {
	ID            string    `json:"id"` // "<environment_id>/<server>/<pane_id>"; opaque to clients
	EnvironmentID string    `json:"environment_id"`
	Server        string    `json:"server,omitempty"` // tmux server label: "laatmux", "default", or a socket path; "" from older daemons means "laatmux"
	Session       string    `json:"session"`
	Window        int       `json:"window"`
	PaneID        string    `json:"pane_id"`
	TTY           string    `json:"tty"`
	Cwd           string    `json:"cwd"`
	Title         string    `json:"title"`
	Agent         string    `json:"agent"` // "claude", "codex", "" when none identified
	Activity      Activity  `json:"activity"`
	Liveness      Liveness  `json:"liveness"`
	Identity      *Identity `json:"identity,omitempty"`
	Rule          string    `json:"rule,omitempty"`   // detection rule that produced Activity
	Reason        string    `json:"reason,omitempty"` // detection explanation
	Managed       bool      `json:"managed"`          // created by laatmux new
	ActivityAt    time.Time `json:"activity_at"`      // when Activity last changed
	UpdatedAt     time.Time `json:"updated_at"`
}

// Worktree is one git worktree on one host, under the host's configured
// worktree directory, in a checkout of a known repository. Git is the
// source of truth: a worktree made by hand is listed, one removed by hand
// disappears, and one whose directory is gone (prunable) is not published.
type Worktree struct {
	ID            string    `json:"id"` // "<environment_id>/worktree/<root>"; opaque to clients
	EnvironmentID string    `json:"environment_id"`
	Repo          string    `json:"repo"`              // repository label from the host's config
	Source        string    `json:"source,omitempty"`  // repository source, the identity; "" from older daemons
	Branch        string    `json:"branch"`            // "" for a detached worktree
	Root          string    `json:"root"`              // absolute path as git registered it
	Session       string    `json:"session,omitempty"` // managed session whose pane records Root, else ""
	UpdatedAt     time.Time `json:"updated_at"`
}

// Message is the single envelope. Fields are used per Type; unused ones are
// omitted on the wire.
type Message struct {
	Type string `json:"type"`

	// hello
	Protocol      int      `json:"protocol,omitempty"`
	Client        string   `json:"client,omitempty"`
	EnvironmentID string   `json:"environment_id,omitempty"`
	Version       string   `json:"version,omitempty"`
	Host          string   `json:"host,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`

	// snapshot / upsert / remove
	Seq        uint64     `json:"seq,omitempty"`
	Agents     []Agent    `json:"agents,omitempty"`
	Agent      *Agent     `json:"agent,omitempty"`
	AgentID    string     `json:"agent_id,omitempty"`
	Worktrees  []Worktree `json:"worktrees,omitempty"`
	Worktree   *Worktree  `json:"worktree,omitempty"`
	WorktreeID string     `json:"worktree_id,omitempty"`

	// commands and results
	ID      string   `json:"id,omitempty"` // client-chosen command id
	Name    string   `json:"name,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Cmd     []string `json:"cmd,omitempty"`
	OK      bool     `json:"ok,omitempty"`
	Error   string   `json:"error,omitempty"`
	Session string   `json:"session,omitempty"`
	PaneID  string   `json:"pane_id,omitempty"`

	// add and rm
	Repo   string `json:"repo,omitempty"`   // repository source or label, as the daemon's config knows it
	Branch string `json:"branch,omitempty"` // branch and worktree name
	// AgentName is the configured agent to start; Cmd, when set, is the
	// command instead. The key is agent_name because agent is the upsert's
	// record in this envelope.
	AgentName string `json:"agent_name,omitempty"`
	// Root on rm is the worktree root from the record. It is what reaches
	// a managed session whose worktree is already gone, since a branch
	// alone cannot be mapped to a root then; send it whenever it is known.
	// Alone, it removes a detached worktree. On a result: the worktree root.
	Root  string `json:"root,omitempty"`
	Force bool   `json:"force,omitempty"` // rm: remove a dirty or locked worktree

	// progress, and the failed stage in a result
	Stage  string `json:"stage,omitempty"`
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Conn is a line-oriented JSON connection. Writes are serialized.
type Conn struct {
	r  *bufio.Reader
	w  io.Writer
	mu sync.Mutex
}

func NewConn(rw io.ReadWriter) *Conn {
	return &Conn{r: bufio.NewReaderSize(rw, 1<<20), w: rw}
}

func NewConnRW(r io.Reader, w io.Writer) *Conn {
	return &Conn{r: bufio.NewReaderSize(r, 1<<20), w: w}
}

// Read blocks for the next message. io.EOF when the peer closed.
func (c *Conn) Read() (Message, error) {
	var m Message
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		if err == io.EOF && len(line) > 0 {
			// trailing message without newline
			if uerr := json.Unmarshal(line, &m); uerr == nil {
				return m, nil
			}
		}
		return m, err
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return m, err
	}
	return m, nil
}

func (c *Conn) Write(m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(b)
	return err
}

// Has reports whether the capability set includes cap.
func Has(caps []string, cap string) bool {
	for _, c := range caps {
		if c == cap {
			return true
		}
	}
	return false
}
