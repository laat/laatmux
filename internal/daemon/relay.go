package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/protocol"
)

// The relay: the background add, living in the daemon on the machine
// the user sits at. A client sends an add naming a host; the daemon
// writes a pending file first, answers accepted, and then dials the
// host as the client would have, sends the add under the client's id,
// and follows it to the result, with the reconnect backoff of the
// merged stream for as long as the task is outstanding. The pending
// record is in the merged stream meanwhile, and after the result the
// relay takes the host's listings until one reflects the add, then
// hands the record over to the worktree row it became.
//
// Accepting a task means it cannot be lost: the file is on disk before
// the answer, prompt included, mode 0600, and a daemon that starts
// picks up every file it finds. The prompt leaves the file when the
// delivery is delivered or none, or on dismiss.

const (
	// relayLifetime is how long after its submission the relay sends or
	// resends an add, by this machine's clock: the sender's side of the
	// clock contract with the host's journal, which keeps an entry for
	// thirty days.
	relayLifetime = 7 * 24 * time.Hour
	// handoffRetention is how long a retired record's file, and its
	// handoff in snapshots, are kept.
	handoffRetention = 24 * time.Hour
	relaySweep       = time.Hour
	// relayOutcomeUnknown is the error of an add that cannot be sent
	// again and whose outcome no daemon can say.
	relayOutcomeUnknown = "outcome unknown"
)

// pendingFile is what the relay keeps per task: the record as the
// stream carries it, and what the stream must not: the prompt, the
// listing barrier, whether the add may have reached the host, and the
// handoff once retired.
type pendingFile struct {
	protocol.Pending
	PromptText string            `json:"prompt_text,omitempty"`
	Barrier    *protocol.Listing `json:"barrier,omitempty"`
	Sent       bool              `json:"sent,omitempty"` // the add may have reached the host: follow rather than send
	ReplacedBy string            `json:"replaced_by,omitempty"`
	RetiredAt  time.Time         `json:"retired_at,omitzero"`
}

func (p *pendingFile) retired() bool { return p.ReplacedBy != "" }

// relay is the pending records, on disk under dir and in memory.
type relay struct {
	dir    string
	logger *log.Logger
	mu     sync.Mutex
	recs   map[string]*pendingFile
	// attempts serializes the deliveries per record.
	attempts map[string]*sync.Mutex
}

func openRelay(dir string, logger *log.Logger) (*relay, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := &relay{dir: dir, logger: logger, recs: map[string]*pendingFile{}, attempts: map[string]*sync.Mutex{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			logger.Printf("pending: %s: %v", de.Name(), err)
			continue
		}
		var p pendingFile
		if err := json.Unmarshal(b, &p); err != nil || p.ID == "" {
			logger.Printf("pending: %s: not a record: %v", de.Name(), err)
			continue
		}
		r.recs[p.ID] = &p
	}
	return r, nil
}

func (r *relay) get(id string) (pendingFile, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.recs[id]
	if !ok {
		return pendingFile{}, false
	}
	return *p, true
}

// create writes a new record; an id the relay has is not written again.
func (r *relay) create(p pendingFile) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.recs[p.ID]; ok {
		return false, nil
	}
	cp := p
	if err := r.writeLocked(&cp); err != nil {
		return false, err
	}
	r.recs[p.ID] = &cp
	return true, nil
}

// update applies change to the record and writes it when write is set;
// a change that is not written is the in-memory state only, which the
// progress lines are, since a line of setup output is not worth a file
// write and the next daemon follows the add anyway.
func (r *relay) update(id string, write bool, change func(*pendingFile)) (pendingFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.recs[id]
	if !ok {
		return pendingFile{}, errors.New("pending: no record " + id)
	}
	cp := *p
	change(&cp)
	cp.UpdatedAt = time.Now()
	if write {
		if err := r.writeLocked(&cp); err != nil {
			return *p, err
		}
	}
	*p = cp
	return cp, nil
}

func (r *relay) writeLocked(p *pendingFile) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(r.dir, fileName(p.ID))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// remove deletes the record and its file.
func (r *relay) remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.recs[id]; !ok {
		return nil
	}
	if err := os.Remove(filepath.Join(r.dir, fileName(id))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(r.recs, id)
	return nil
}

// sweep deletes the records retired longer than the handoff retention.
func (r *relay) sweep(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.recs {
		if !p.retired() || now.Sub(p.RetiredAt) < handoffRetention {
			continue
		}
		if err := os.Remove(filepath.Join(r.dir, fileName(id))); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.logger.Printf("pending: sweep %s: %v", id, err)
			continue
		}
		delete(r.recs, id)
	}
}

// attemptLock serializes the deliveries of one record.
func (r *relay) attemptLock(id string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.attempts[id]
	if !ok {
		l = &sync.Mutex{}
		r.attempts[id] = l
	}
	return l
}

// pendings is every record not retired, sorted by submission; handoffs
// every retired one, sorted by id.
func (r *relay) pendings() ([]protocol.Pending, []protocol.Handoff) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ps []protocol.Pending
	var hs []protocol.Handoff
	for _, p := range r.recs {
		if p.retired() {
			hs = append(hs, protocol.Handoff{ID: p.ID, ReplacedBy: p.ReplacedBy})
			continue
		}
		ps = append(ps, p.Pending)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].SubmittedAt.Before(ps[j].SubmittedAt) })
	sort.Slice(hs, func(i, j int) bool { return hs[i].ID < hs[j].ID })
	return ps, hs
}

// acceptRelay takes a relayed add: the pending file is written first,
// atomically, then the answer says accepted, and the relay runs from
// there. A host the config does not have, or one whose daemon is known
// not to have the task capability, is refused here, before the file:
// the form reads the same cached capability and says so before submit.
func (d *Daemon) acceptRelay(ctx context.Context, m protocol.Message) protocol.Message {
	res := protocol.Message{Type: protocol.TypeResult, ID: m.ID}
	if d.relay == nil {
		res.Error = "this daemon has no relay capability"
		return res
	}
	switch {
	case m.ID == "":
		res.Error = "command id required"
	case m.Repo == "":
		res.Error = "repository required"
	case m.Branch == "":
		res.Error = "branch required"
	}
	if res.Error != "" {
		return res
	}
	h, ok := d.relayHost(m.Relay)
	if !ok {
		res.Error = fmt.Sprintf("host %q is not in the config", m.Relay)
		return res
	}
	d.mu.Lock()
	var env string
	if mh, ok := d.mhosts[h.Name]; ok && mh.status.EnvironmentID != "" {
		env = mh.status.EnvironmentID
		if !protocol.Has(mh.status.Capabilities, protocol.CapTask) {
			res.Error = fmt.Sprintf("tasks not supported by %s's daemon %s", h.Name, mh.status.Version)
		}
	}
	d.mu.Unlock()
	if res.Error != "" {
		return res
	}
	submitted := m.SubmittedAt
	if submitted.IsZero() {
		submitted = time.Now()
	}
	name := m.Name
	if name == "" {
		name = m.Repo
	}
	p := pendingFile{Pending: protocol.Pending{
		ID: m.ID, Host: h.Name, EnvironmentID: env, Source: m.Repo, Repo: name, Branch: m.Branch, Generated: m.Generated,
		Agent: m.AgentName, Cmd: m.Cmd, SubmittedAt: submitted, UpdatedAt: time.Now(),
	}, PromptText: m.Prompt}
	fresh, err := d.relay.create(p)
	if err != nil {
		res.Error = "pending: " + err.Error()
		return res
	}
	res.OK = true
	if fresh {
		d.publishPending(p.Pending)
		go d.runPending(ctx, m.ID)
	}
	return res
}

// relayHost finds the configured host by name.
func (d *Daemon) relayHost(name string) (client.Host, bool) {
	hosts, err := d.cfg.Hosts()
	if err != nil {
		return client.Host{}, false
	}
	for _, h := range hosts {
		if h.Name == name {
			return h, true
		}
	}
	return client.Host{}, false
}

// publishPending sends the record into the merged stream.
func (d *Daemon) publishPending(p protocol.Pending) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.msubs) == 0 {
		return
	}
	d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Pending: &p})
}

// publishRemoved sends the record's removal, with the worktree id it
// retired into when it did.
func (d *Daemon) publishRemoved(id, replacedBy string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.msubs) == 0 {
		return
	}
	d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, PendingID: id, ReplacedBy: replacedBy})
}

// setPending applies a change and publishes the record.
func (d *Daemon) setPending(id string, write bool, change func(*pendingFile)) (pendingFile, bool) {
	p, err := d.relay.update(id, write, change)
	if err != nil {
		d.cfg.Logger.Printf("pending: %s: %v", id, err)
		return p, false
	}
	d.publishPending(p.Pending)
	return p, true
}

// startRelays resumes every record a daemon finds at start: an add
// without an outcome is followed, an attempt left open is followed
// before anything else, a success whose listing has not been seen is
// waited on, and a retired record is kept for its handoff.
func (d *Daemon) startRelays(ctx context.Context) {
	ps, _ := d.relay.pendings()
	for _, p := range ps {
		switch {
		case !p.Done:
			go d.runPending(ctx, p.ID)
		case p.AttemptOpen:
			go d.runAttempt(ctx, p.ID)
		case p.OK && !p.Listed:
			go d.retire(ctx, p.ID)
		}
	}
}

// runRelaySweep sweeps retired records hourly.
func (d *Daemon) runRelaySweep(ctx context.Context) {
	t := time.NewTicker(relaySweep)
	defer t.Stop()
	for {
		d.relay.sweep(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// relayConn is one connection to the task's host, held to the
// environment the record is pinned to and checked for the capabilities
// a relayed add needs. A host never reached has no id yet; the first
// hello binds it, written before anything is sent.
func (d *Daemon) relayConn(ctx context.Context, id string) (*client.Conn, pendingFile, error) {
	p, ok := d.relay.get(id)
	if !ok {
		return nil, p, errors.New("record gone")
	}
	h, ok := d.relayHost(p.Host)
	if !ok {
		return nil, p, errHostRemoved
	}
	c, err := d.cfg.Dial(ctx, h)
	if err != nil {
		return nil, p, err
	}
	for _, cap := range []string{protocol.CapAdd, protocol.CapFollow, protocol.CapTask} {
		if !protocol.Has(c.Hello.Capabilities, cap) {
			c.Close()
			return nil, p, &refusal{fmt.Sprintf("tasks not supported by %s's daemon %s: no %s capability", h.Name, c.Hello.Version, cap)}
		}
	}
	switch {
	case p.EnvironmentID == "":
		env := c.Hello.EnvironmentID
		p, ok = d.setPending(id, true, func(p *pendingFile) { p.EnvironmentID = env })
		if !ok {
			c.Close()
			return nil, p, errors.New("pending: the environment could not be written")
		}
	case c.Hello.EnvironmentID != p.EnvironmentID:
		c.Close()
		return nil, p, fmt.Errorf("%s answers as environment %s, not %s the task was accepted for", h.Name, c.Hello.EnvironmentID, p.EnvironmentID)
	}
	return c, p, nil
}

// errHostRemoved is a record whose host is gone from the config: it
// stays, saying so, until the host is back or the user dismisses it.
var errHostRemoved = errors.New("host removed from the config")

// refusal is an answer that ends a relay: the host cannot take the task.
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

// unreachable marks the record as waiting on the host, with the reason.
func (d *Daemon) unreachable(id string, err error) {
	msg := err.Error()
	d.setPending(id, false, func(p *pendingFile) {
		p.Reachable = false
		p.Unreachable = msg
	})
}

// backoff waits the reconnect backoff, doubled to the max, or returns
// false when ctx ends.
func (d *Daemon) relayBackoff(ctx context.Context, wait *time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(*wait):
	}
	*wait = min(*wait*2, reconnectMax)
	return true
}

// runPending runs one task's add against its host until it has an
// outcome, then waits on the listing after it. Connectivity is not
// outcome: a lost connection is followed again with backoff, for as
// long as the task is outstanding, and the record says the host is
// unreachable meanwhile, never failed. A follow answered interrupted,
// or unknown command, resends the add under the same id, within the
// lifetime; one answered from the journal is the outcome.
func (d *Daemon) runPending(ctx context.Context, id string) {
	wait := d.cfg.ReconnectMin
	var after uint64
	for ctx.Err() == nil {
		p, ok := d.relay.get(id)
		if !ok || p.Done {
			break
		}
		c, p, err := d.relayConn(ctx, id)
		if err != nil {
			var ref *refusal
			if errors.As(err, &ref) {
				d.setPending(id, true, func(p *pendingFile) {
					p.Done, p.OK, p.Error, p.Reachable = true, false, ref.msg, true
				})
				return
			}
			d.unreachable(id, err)
			if !d.relayBackoff(ctx, &wait) {
				return
			}
			continue
		}
		wait = d.cfg.ReconnectMin
		req := protocol.Message{
			Type: protocol.TypeAdd, ID: id, Repo: p.Source, Branch: p.Branch, Generated: p.Generated,
			AgentName: p.Agent, Cmd: p.Cmd, Prompt: p.PromptText, SubmittedAt: p.SubmittedAt,
		}
		if p.Sent {
			req = protocol.Message{Type: protocol.TypeFollow, ID: id, After: after}
		} else if time.Since(p.SubmittedAt) > relayLifetime {
			c.Close()
			d.setPending(id, true, func(p *pendingFile) {
				p.Done, p.OK, p.Error, p.Reachable = true, false, relayOutcomeUnknown+": the submission is older than seven days and is not sent again", true
			})
			return
		} else {
			// The send is on disk before it happens: a daemon that dies
			// between the write and the answer follows rather than
			// sends again.
			if _, ok := d.setPending(id, true, func(p *pendingFile) { p.Sent = true }); !ok {
				c.Close()
				if !d.relayBackoff(ctx, &wait) {
					return
				}
				continue
			}
		}
		d.setPending(id, false, func(p *pendingFile) { p.Reachable, p.Unreachable = true, "" })
		res, err := d.relayExchange(ctx, c, req, func(m protocol.Message) {
			if m.N <= after && m.N != 0 {
				return
			}
			if m.N > after {
				after = m.N
			}
			write := m.Stage == protocol.StageAllocate && m.Root != ""
			d.setPending(id, write, func(p *pendingFile) {
				p.Taken = true
				p.Stage, p.State, p.Detail = m.Stage, m.State, m.Detail
				if m.Stage == protocol.StageAllocate && m.Root != "" {
					p.Branch, p.Root = m.Branch, m.Root
				}
			})
		})
		c.Close()
		if err != nil {
			// A transport failure: no message came. Follow next time.
			d.unreachable(id, fmt.Errorf("connection lost: %v", err))
			if !d.relayBackoff(ctx, &wait) {
				return
			}
			continue
		}
		if !res.OK && (res.Error == protocol.ErrUnknownCommand || res.Error == protocol.ErrInterrupted) {
			// The host never took it, or died in it: sent again, and
			// the numbering starts over.
			after = 0
			d.setPending(id, true, func(p *pendingFile) { p.Sent = false })
			continue
		}
		d.setPending(id, true, func(p *pendingFile) {
			p.Taken, p.Done, p.Reachable = true, true, true
			p.OK = res.OK
			if res.Branch != "" {
				p.Branch = res.Branch
			}
			if res.Root != "" {
				p.Root = res.Root
			}
			p.Session = res.Session
			if res.OK {
				p.Prompt, p.Error = res.Prompt, res.Error
				if p.Prompt == "" {
					p.Prompt = protocol.DeliveryNone
				}
				p.Barrier = res.Listing
				if p.Complete() {
					p.PromptText = ""
				}
			} else {
				p.Stage = res.Stage
				p.Error = res.Error
				if res.Error == protocol.ErrSubmissionExpired {
					p.Error = relayOutcomeUnknown + ": " + res.Error
				} else if res.Stage != "" {
					p.Error = "failed at " + res.Stage + ": " + res.Error
				}
			}
		})
		break
	}
	if p, ok := d.relay.get(id); ok && p.Done && p.OK && !p.Listed {
		d.retire(ctx, p.ID)
	}
}

// relayExchange sends one request on the connection and reads until its
// result, passing progress to onProgress.
func (d *Daemon) relayExchange(ctx context.Context, c *client.Conn, req protocol.Message, onProgress func(protocol.Message)) (protocol.Message, error) {
	defer c.CloseOnDone(ctx)()
	if err := c.Write(req); err != nil {
		return protocol.Message{}, err
	}
	for {
		m, err := c.Read()
		if err != nil {
			if ctx.Err() != nil {
				return protocol.Message{}, ctx.Err()
			}
			return protocol.Message{}, err
		}
		if m.ID != req.ID {
			continue
		}
		switch m.Type {
		case protocol.TypeProgress:
			onProgress(m)
		case protocol.TypeError:
			return m, nil
		case protocol.TypeResult:
			return m, nil
		}
	}
}

// retire waits for the host's listing to reflect a successful add, on
// the relay's own connection: plain snapshots and the listing stamps
// after them until one satisfies the result's barrier. A worktree at
// the root in that listing hands the record over to the worktree row,
// once the merged stream shows it too when there is a merged
// subscriber to show it to; no worktree at the root is done, worktree
// gone, a record that needs the user only to be dismissed. A record
// that is not complete stays for the user either way, with its listing
// noted.
func (d *Daemon) retire(ctx context.Context, id string) {
	wait := d.cfg.ReconnectMin
	for ctx.Err() == nil {
		p, ok := d.relay.get(id)
		if !ok || !p.Done || !p.OK || p.Listed {
			return
		}
		if p.Barrier == nil || p.Root == "" {
			// A host without a barrier cannot prove its listing; the
			// record stays with what the result said.
			d.setPending(id, true, func(p *pendingFile) { p.Listed = true })
			return
		}
		c, p, err := d.relayConn(ctx, id)
		if err != nil {
			var ref *refusal
			if errors.As(err, &ref) {
				d.setPending(id, true, func(p *pendingFile) { p.Listed, p.Gone = true, false; p.Error = ref.msg })
				return
			}
			d.unreachable(id, err)
			if !d.relayBackoff(ctx, &wait) {
				return
			}
			continue
		}
		wait = d.cfg.ReconnectMin
		d.setPending(id, false, func(p *pendingFile) { p.Reachable, p.Unreachable = true, "" })
		present, err := d.awaitListing(ctx, c, *p.Barrier, p.Root)
		c.Close()
		if err != nil {
			d.unreachable(id, fmt.Errorf("connection lost: %v", err))
			if !d.relayBackoff(ctx, &wait) {
				return
			}
			continue
		}
		if !present {
			d.setPending(id, true, func(p *pendingFile) { p.Listed, p.Gone = true, true })
			return
		}
		if !p.Complete() {
			d.setPending(id, true, func(p *pendingFile) { p.Listed = true })
			return
		}
		d.handoff(ctx, id, p.WorktreeID())
		return
	}
}

// awaitListing subscribes to the host and reads until a listing at or
// past the barrier, then reports whether a worktree is at root in it.
// The worktrees are tracked from the snapshot on, since the stamp of a
// listing arrives after the records it changed.
func (d *Daemon) awaitListing(ctx context.Context, c *client.Conn, barrier protocol.Listing, root string) (bool, error) {
	defer c.CloseOnDone(ctx)()
	if err := c.Write(protocol.Message{Type: protocol.TypeSubscribe}); err != nil {
		return false, err
	}
	roots := map[string]string{} // worktree id -> root
	for {
		m, err := c.Read()
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, err
		}
		switch m.Type {
		case protocol.TypeSnapshot:
			roots = map[string]string{}
			for _, w := range m.Worktrees {
				roots[w.ID] = w.Root
			}
		case protocol.TypeUpsert:
			if m.Worktree != nil {
				roots[m.Worktree.ID] = m.Worktree.Root
			}
		case protocol.TypeRemove:
			if m.WorktreeID != "" {
				delete(roots, m.WorktreeID)
			}
		case protocol.TypeError:
			return false, errors.New(m.Error)
		}
		if m.Listing != nil && m.Listing.Satisfies(barrier) {
			for _, r := range roots {
				if r == root {
					return true, nil
				}
			}
			return false, nil
		}
	}
}

// handoff retires the record into its worktree row: the handoff is
// written to the file, which is kept for a day, before the removal is
// published, so a daemon restarted in between still carries it. When
// a merged subscriber is watching, the removal waits for the merged
// stream to show the worktree, so the row is never gone before the
// one it became is there.
func (d *Daemon) handoff(ctx context.Context, id, worktreeID string) {
	p, ok := d.relay.get(id)
	if !ok {
		return
	}
	deadline := time.Now().Add(30 * time.Second)
	for ctx.Err() == nil {
		d.mu.Lock()
		watching := d.mctx != nil
		shown := false
		if mh, ok := d.mhosts[p.Host]; ok {
			if mh.host.Local() {
				_, shown = d.worktrees[p.Root]
			} else {
				_, shown = mh.worktrees[worktreeID]
			}
		}
		d.mu.Unlock()
		if !watching || shown || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	now := time.Now()
	if _, ok := d.setPending(id, true, func(p *pendingFile) {
		p.Listed, p.ReplacedBy, p.RetiredAt, p.PromptText = true, worktreeID, now, ""
	}); !ok {
		return
	}
	d.publishRemoved(id, worktreeID)
}

// dismiss drops a record that needs the user: its file goes and the
// stream says it is gone. A record whose add is still running is not
// dismissed; the add finishes first.
func (d *Daemon) dismiss(id string) protocol.Message {
	res := protocol.Message{Type: protocol.TypeResult, ID: id}
	if d.relay == nil {
		res.Error = "this daemon has no relay capability"
		return res
	}
	p, ok := d.relay.get(id)
	switch {
	case !ok:
		res.Error = "no pending record " + id
	case !p.Done:
		res.Error = "the add is still running; it cannot be dismissed until it has an outcome"
	case p.AttemptOpen:
		res.Error = "a delivery attempt is unresolved; it cannot be dismissed until it has an outcome"
	}
	if res.Error != "" {
		return res
	}
	if err := d.relay.remove(id); err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	d.publishRemoved(id, "")
	return res
}

// relayPrompt is p on a pending row: one delivery attempt of the
// record's prompt, numbered one past the last, written to the file
// before it is sent, one unresolved at a time. The result is the
// delivery state with its reason; recovery expired is the host having
// swept the add's journal, after which the prompt is the user's to
// paste by hand.
func (d *Daemon) relayPrompt(ctx context.Context, id string) protocol.Message {
	res := protocol.Message{Type: protocol.TypeResult, ID: id}
	if d.relay == nil {
		res.Error = "this daemon has no relay capability"
		return res
	}
	l := d.relay.attemptLock(id)
	l.Lock()
	defer l.Unlock()
	p, ok := d.relay.get(id)
	switch {
	case !ok:
		res.Error = "no pending record " + id
	case !p.Done:
		res.Error = "the add is still running"
	case p.PromptText == "":
		res.Error = "no prompt retained for " + id
	case p.Complete():
		res.Error = "the prompt is delivered"
	case p.AttemptOpen:
		res.Error = fmt.Sprintf("attempt %d is unresolved", p.Attempt)
	case p.Error == protocol.ErrRecoveryExpired:
		res.Error = protocol.ErrRecoveryExpired
	}
	if res.Error != "" {
		return res
	}
	if _, ok := d.setPending(id, true, func(p *pendingFile) {
		p.Attempt++
		p.AttemptOpen = true
	}); !ok {
		res.Error = "pending: the attempt could not be written"
		return res
	}
	p = d.runAttemptLocked(ctx, id, false)
	res.OK = true
	res.Attempt = p.Attempt
	res.Prompt, res.Error = p.Prompt, p.Error
	go d.afterAttempt(ctx, p)
	return res
}

// runAttempt follows the record's open attempt, one the last daemon
// sent, under the record's attempt lock.
func (d *Daemon) runAttempt(ctx context.Context, id string) {
	l := d.relay.attemptLock(id)
	l.Lock()
	p := d.runAttemptLocked(ctx, id, true)
	l.Unlock()
	d.afterAttempt(ctx, p)
}

// afterAttempt retires a record an attempt has completed: into its
// worktree row when the listing after the result showed one, or
// through the listing when that is still owed.
func (d *Daemon) afterAttempt(ctx context.Context, p pendingFile) {
	if !p.Complete() || p.retired() {
		return
	}
	switch {
	case !p.Listed:
		d.retire(ctx, p.ID)
	case !p.Gone:
		d.handoff(ctx, p.ID, p.WorktreeID())
	}
}

// runAttemptLocked sends the open attempt as a prompt message and
// follows it until it has an outcome, with backoff on a host that
// cannot be reached. With sent, the attempt was sent before, by the
// daemon before this one, and is followed by number first; one the
// host never saw is sent. Called with the attempt lock held.
func (d *Daemon) runAttemptLocked(ctx context.Context, id string, sent bool) pendingFile {
	wait := d.cfg.ReconnectMin
	for ctx.Err() == nil {
		p, ok := d.relay.get(id)
		if !ok || !p.AttemptOpen {
			return p
		}
		c, p, err := d.relayConn(ctx, id)
		if err != nil {
			var ref *refusal
			if errors.As(err, &ref) {
				p, _ = d.setPending(id, true, func(p *pendingFile) {
					p.AttemptOpen, p.Prompt, p.Error = false, protocol.DeliveryNotDelivered, ref.msg
				})
				return p
			}
			d.unreachable(id, err)
			if !d.relayBackoff(ctx, &wait) {
				return p
			}
			continue
		}
		wait = d.cfg.ReconnectMin
		d.setPending(id, false, func(p *pendingFile) { p.Reachable, p.Unreachable = true, "" })
		req := protocol.Message{Type: protocol.TypePrompt, ID: id, Attempt: p.Attempt, Prompt: p.PromptText}
		if sent {
			req = protocol.Message{Type: protocol.TypeFollow, ID: id, Attempt: p.Attempt}
		}
		sent = true
		res, err := d.relayExchange(ctx, c, req, func(protocol.Message) {})
		c.Close()
		if err != nil {
			d.unreachable(id, fmt.Errorf("connection lost: %v", err))
			if !d.relayBackoff(ctx, &wait) {
				return p
			}
			continue
		}
		if !res.OK && res.Error == protocol.ErrUnknownAttempt {
			sent = false
			continue
		}
		p, _ = d.setPending(id, true, func(p *pendingFile) {
			p.AttemptOpen = false
			if res.OK {
				p.Prompt, p.Error = res.Prompt, res.Error
				if p.Complete() {
					p.PromptText = ""
				}
			} else {
				p.Error = res.Error
			}
		})
		return p
	}
	p, _ := d.relay.get(id)
	return p
}
