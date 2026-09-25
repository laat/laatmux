package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/protocol"
)

// The journal's clock contract, from the milestone-four note. Retention
// is how long an entry outlives the moment it became terminal, by this
// host's clock; an add whose submission is older than that, or more than
// a day in this host's future, is refused, so a sender's seven-day
// lifetime reaches a tombstone unless the clocks disagree by more than
// three weeks.
const (
	journalRetention = 30 * 24 * time.Hour
	journalFuture    = 24 * time.Hour
	journalSweep     = time.Hour
)

// Launch states of an entry: the agent stage journaled as two
// transitions around new-session.
const (
	launchNone      = ""
	launchLaunching = "launching" // written before new-session; a daemon that dies here leaves unknown
	launchLaunched  = "launched"  // new-session returned; the pane and server instance are recorded
)

// attemptState values: a typed-in delivery journaled around the paste.
const (
	attemptAttempting = "attempting"
)

// attempt is one typed-in delivery of the prompt: written as attempting
// before the paste and rewritten with its outcome, a delivery state,
// after. A daemon that dies between the two leaves it attempting, which
// the next daemon reads as unknown.
type attempt struct {
	N     int       `json:"n"`
	State string    `json:"state"`
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`
}

// entry is what the host remembers of one add: the two decisions a
// retry cannot inspect, the branch allocated and the prompt delivered,
// with the state around them. Never the prompt itself.
type entry struct {
	ID          string    `json:"id"`
	SubmittedAt time.Time `json:"submitted_at,omitzero"`
	FirstSeen   time.Time `json:"first_seen"` // this host's clock; retention counts from the terminal moment
	Source      string    `json:"source"`
	Repo        string    `json:"repo"`
	Agent       string    `json:"agent,omitempty"`
	HasPrompt   bool      `json:"has_prompt,omitempty"`
	// Branch is the proposal until Allocated, then the name the add
	// uses. An explicit branch is allocated as given, so it reserves
	// its name against a generated one the same way.
	Branch    string `json:"branch"`
	Generated bool   `json:"generated,omitempty"`
	Allocated bool   `json:"allocated,omitempty"`
	Root      string `json:"root,omitempty"`
	Stage     string `json:"stage,omitempty"` // the last stage begun, for interrupted

	// The launch: the session name from launching on, the pane and the
	// server instance from launched on, and whether the argv carried
	// the prompt, which makes launched the delivery.
	Launch     string `json:"launch,omitempty"`
	Session    string `json:"session,omitempty"`
	PaneID     string `json:"pane_id,omitempty"`
	ServerPID  int    `json:"server_pid,omitempty"`
	ArgvPrompt bool   `json:"argv_prompt,omitempty"`

	// The delivery: the agent identity bound before the first attempt,
	// which every later one requires; the state and its reason; the
	// attempts in order.
	Identity      *protocol.Identity `json:"identity,omitempty"`
	Delivery      string             `json:"delivery,omitempty"`
	DeliveryError string             `json:"delivery_error,omitempty"`
	Attempts      []attempt          `json:"attempts,omitempty"`
	// Typing is a paste in progress, the add's own or an attempt's:
	// written before the paste and cleared with its outcome. A daemon
	// that dies in between leaves it set, and the next reads unknown.
	Typing bool `json:"typing,omitempty"`

	// Result is the recorded result of the add, which makes the entry
	// terminal; Removed is rm having removed the worktree since, which
	// makes it terminal too. TerminalAt starts the retention.
	Result     *protocol.Message `json:"result,omitempty"`
	Removed    bool              `json:"removed,omitempty"`
	TerminalAt time.Time         `json:"terminal_at,omitzero"`
}

func (e *entry) terminal() bool { return e.Result != nil || e.Removed }

// clone is a copy that shares nothing with e, so a change applied to
// it reaches the live entry only once the file holds it.
func (e *entry) clone() entry {
	cp := *e
	cp.Attempts = slices.Clone(e.Attempts)
	if e.Identity != nil {
		id := *e.Identity
		cp.Identity = &id
	}
	if e.Result != nil {
		res := *e.Result
		cp.Result = &res
	}
	return cp
}

// lastAttempt is the most recent attempt, or nil.
func (e *entry) lastAttempt() *attempt {
	if len(e.Attempts) == 0 {
		return nil
	}
	return &e.Attempts[len(e.Attempts)-1]
}

// journal is the command journal: one file per add under dir, written
// atomically before a decision is acted on and rewritten as it plays
// out, and the same entries in memory. It is the daemon's own; one
// process writes it.
type journal struct {
	dir    string
	logger *log.Logger
	mu     sync.Mutex
	byID   map[string]*entry
}

// openJournal loads every entry under dir, making the directory when it
// is missing. An entry the previous daemon died in is read as what the
// death left: an attempt still attempting is unknown, and a launch still
// launching stays so, which the add reads as unknown. A file that does
// not parse is left alone and logged; it is not laatmux's to delete.
func openJournal(dir string, logger *log.Logger) (*journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	j := &journal{dir: dir, logger: logger, byID: map[string]*entry{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		if strings.HasSuffix(de.Name(), ".tmp") {
			// A write that died before its rename is nobody's entry.
			os.Remove(filepath.Join(dir, de.Name()))
			continue
		}
		if !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			logger.Printf("journal: %s: %v", de.Name(), err)
			continue
		}
		var e entry
		if err := json.Unmarshal(b, &e); err != nil || e.ID == "" {
			logger.Printf("journal: %s: not an entry: %v", de.Name(), err)
			continue
		}
		// A paste the daemon died in is unknown; an attempt it died in
		// before the paste, waiting for the pane, is provably not
		// delivered, since the paste is written before it happens.
		a := e.lastAttempt()
		open := a != nil && a.State == attemptAttempting
		if e.Typing || open {
			state, reason := protocol.DeliveryNotDelivered, "daemon restarted before the paste"
			if e.Typing {
				state, reason = protocol.DeliveryUnknown, "daemon restarted during the paste"
			}
			e.Delivery, e.DeliveryError, e.Typing = state, reason, false
			if open {
				a.State, a.Error = state, reason
			}
			if err := j.writeLocked(&e); err != nil {
				logger.Printf("journal: %s: %v", de.Name(), err)
			}
		}
		j.byID[e.ID] = &e
	}
	return j, nil
}

// FileName is the file a command id is kept under, in the journal and
// in the relay's pending directory: the id itself when it is safe as a
// name, else a hash of it, so a client-chosen id never names a path.
func FileName(id string) string {
	safe := id != "" && id[0] != '.' && strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
	}) < 0
	if safe && len(id) <= 200 {
		return id + ".json"
	}
	sum := sha256.Sum256([]byte(id))
	return "h-" + hex.EncodeToString(sum[:16]) + ".json"
}

// get returns a copy of the entry for id.
func (j *journal) get(id string) (entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.byID[id]
	if !ok {
		return entry{}, false
	}
	return e.clone(), true
}

// create writes a new entry, refusing an id the journal has.
func (j *journal) create(e entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.byID[e.ID]; ok {
		return errors.New("journal: entry exists")
	}
	cp := e
	if err := j.writeLocked(&cp); err != nil {
		return err
	}
	j.byID[e.ID] = &cp
	return nil
}

// update applies change to the entry for id and writes it, atomically
// with respect to every other update, so a removal by rm and a
// transition by the add never lose each other. The changed copy is
// returned.
func (j *journal) update(id string, change func(*entry)) (entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.byID[id]
	if !ok {
		return entry{}, errors.New("journal: no entry " + id)
	}
	cp := e.clone()
	change(&cp)
	if err := j.writeLocked(&cp); err != nil {
		return entry{}, err
	}
	*e = cp
	return cp.clone(), nil
}

// writeLocked writes e's file through a temporary renamed into place.
func (j *journal) writeLocked(e *entry) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(j.dir, FileName(e.ID))
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// reserved lists the names allocated by entries of the source that are
// not terminal, other than id's own: what an allocation must avoid
// between another add's allocation and its branch, across a daemon
// death.
func (j *journal) reserved(source, id string) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var names []string
	for _, e := range j.byID {
		if e.ID != id && e.Source == source && e.Allocated && !e.terminal() {
			names = append(names, e.Branch)
		}
	}
	sort.Strings(names)
	return names
}

// markRemoved makes every entry at root terminal as removed, and
// returns their ids. A tombstone that cannot be written is an error
// for rm: without it a follow would answer the old outcome and an
// interrupted add could resume and remake the worktree.
func (j *journal) markRemoved(root string, now time.Time) ([]string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var ids []string
	for _, e := range j.byID {
		if e.Root != root || e.Removed {
			continue
		}
		cp := e.clone()
		cp.Removed = true
		cp.TerminalAt = now
		if err := j.writeLocked(&cp); err != nil {
			return ids, fmt.Errorf("journal: %s: %w", e.ID, err)
		}
		*e = cp
		ids = append(ids, e.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

// sweep deletes the entries that have been terminal for the retention.
func (j *journal) sweep(now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for id, e := range j.byID {
		if !e.terminal() || now.Sub(e.TerminalAt) < journalRetention {
			continue
		}
		if err := os.Remove(filepath.Join(j.dir, FileName(id))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			j.logger.Printf("journal: sweep %s: %v", id, err)
			continue
		}
		delete(j.byID, id)
	}
}

// expired reports whether a submission is outside what the journal
// would keep: more than a day in this host's future, or older than the
// retention. A zero time, from a client without task, is never expired.
func expired(submitted, now time.Time) bool {
	if submitted.IsZero() {
		return false
	}
	return submitted.After(now.Add(journalFuture)) || submitted.Before(now.Add(-journalRetention))
}

// recorded is the result message a terminal entry answers with: the
// recorded result, or removed for a worktree rm took since.
func (e *entry) recorded() protocol.Message {
	if e.Removed {
		return protocol.Message{Type: protocol.TypeResult, ID: e.ID, Error: protocol.ErrRemoved, Root: e.Root, Branch: e.Branch}
	}
	res := *e.Result
	res.Type, res.ID = protocol.TypeResult, e.ID
	return res
}

// interrupted is the result a follow gets for an entry that is not
// terminal when the daemon has no command for it: the daemon died in
// the add, at the stage recorded.
func (e *entry) interrupted() protocol.Message {
	return protocol.Message{Type: protocol.TypeResult, ID: e.ID, Error: protocol.ErrInterrupted, Stage: e.Stage, Root: e.Root, Branch: e.Branch}
}
