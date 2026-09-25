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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/client"
	cmdpkg "github.com/laat/laatmux/internal/command"
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
	// Sent, that the add may have reached the host, is the record's own
	// field, so the views see it: same key in the file as before.
	ReplacedBy string    `json:"replaced_by,omitempty"`
	RetiredAt  time.Time `json:"retired_at,omitzero"`
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
	// runners are the goroutines following a record's add or attempt,
	// so a resubmit or a restart never starts a second, and a dismiss
	// of a record the host will never answer for can end them.
	runners map[string][]*runner
	// The gone checks: checking is the records one runs for, so a
	// removal and the listings after it start one, not one each;
	// recheck is a trigger that came while it ran, so it runs again;
	// checked is the listing each record was last checked against, so a
	// poll that repeats it starts nothing.
	checking map[string]bool
	recheck  map[string]bool
	checked  map[string]string
}

// runner is one goroutine on a record: its cancel, and done once it
// has returned.
type runner struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func openRelay(dir string, logger *log.Logger) (*relay, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := &relay{dir: dir, logger: logger, recs: map[string]*pendingFile{}, attempts: map[string]*sync.Mutex{}, runners: map[string][]*runner{},
		checking: map[string]bool{}, recheck: map[string]bool{}, checked: map[string]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		if strings.HasSuffix(de.Name(), ".tmp") {
			// A write that died before its rename: the prompt may be in
			// it, and it is nobody's record.
			os.Remove(filepath.Join(dir, de.Name()))
			continue
		}
		if !strings.HasSuffix(de.Name(), ".json") {
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
	return r.createLocked(p)
}

// createLocked is create with r.mu held.
func (r *relay) createLocked(p pendingFile) (bool, error) {
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

// updateLocked applies change to the record and writes it when write
// is set; a change that is not written is the in-memory state only,
// which the progress lines are, since a line of setup output is not
// worth a file write and the next daemon follows the add anyway.
// Called with r.mu held: the mutation and its publication into the
// stream are one step, so a snapshot or a removal never interleaves
// with a stale upsert.
func (r *relay) updateLocked(id string, write bool, change func(*pendingFile)) (pendingFile, error) {
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
	path := filepath.Join(r.dir, FileName(p.ID))
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

// removeLocked deletes the record and its file. Called with r.mu held.
func (r *relay) removeLocked(id string) error {
	if _, ok := r.recs[id]; !ok {
		return nil
	}
	if err := os.Remove(filepath.Join(r.dir, FileName(id))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(r.recs, id)
	delete(r.checked, id)
	delete(r.recheck, id)
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
		if err := os.Remove(filepath.Join(r.dir, FileName(id))); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.logger.Printf("pending: sweep %s: %v", id, err)
			continue
		}
		delete(r.recs, id)
		delete(r.checked, id)
		delete(r.recheck, id)
	}
}

// attemptLock serializes the deliveries of one record, and dismiss
// with them; settleLock serializes its settling.
func (r *relay) attemptLock(id string) *sync.Mutex { return r.lock("attempt/" + id) }
func (r *relay) settleLock(id string) *sync.Mutex  { return r.lock("settle/" + id) }

func (r *relay) lock(key string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.attempts[key]
	if !ok {
		l = &sync.Mutex{}
		r.attempts[key] = l
	}
	return l
}

// pendings is every record not retired, sorted by submission; handoffs
// every retired one, sorted by id.
func (r *relay) pendings() ([]protocol.Pending, []protocol.Handoff) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pendingsLocked()
}

// pendingsLocked is pendings with r.mu held.
func (r *relay) pendingsLocked() ([]protocol.Pending, []protocol.Handoff) {
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
// there, started by the caller once the answer is written, so the host
// is contacted after the acceptance and not before. A host the config
// does not have, or one whose daemon is known not to have the task
// capability, is refused here, before the file: the form reads the
// same cached capability and says so before submit. An id the relay
// has is accepted again and started if it is not running, which is
// how a client whose answer was lost gets its task run.
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
	h, ok, err := d.relayHost(m.Relay)
	if err != nil {
		res.Error = "config: " + err.Error()
		return res
	}
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
	d.relay.mu.Lock()
	fresh, err := d.relay.createLocked(p)
	if err == nil && fresh {
		d.publishPending(p.Pending)
	}
	d.relay.mu.Unlock()
	if err != nil {
		res.Error = "pending: " + err.Error()
		return res
	}
	res.OK = true
	return res
}

// startPending starts the goroutine that follows the record's add,
// unless one is running or the add has its outcome. Idempotent, so
// the accept path and the resume at start share it.
func (d *Daemon) startPending(ctx context.Context, id string) {
	d.relay.mu.Lock()
	defer d.relay.mu.Unlock()
	p, ok := d.relay.recs[id]
	if !ok || p.Done || len(d.relay.runners[id]) > 0 {
		return
	}
	d.startRunnerLocked(ctx, id, d.runPending)
}

// startRunnerLocked runs fn on the record in a goroutine registered
// under the record's id, with a context a dismiss can cancel. Called
// with the relay's mutex held.
func (d *Daemon) startRunnerLocked(ctx context.Context, id string, fn func(context.Context, string)) {
	ctx, cancel := context.WithCancel(ctx)
	r := &runner{cancel: cancel, done: make(chan struct{})}
	d.relay.runners[id] = append(d.relay.runners[id], r)
	go func() {
		defer func() {
			cancel()
			d.relay.mu.Lock()
			rs := d.relay.runners[id]
			for i, x := range rs {
				if x == r {
					rs = append(rs[:i], rs[i+1:]...)
					break
				}
			}
			if len(rs) == 0 {
				delete(d.relay.runners, id)
			} else {
				d.relay.runners[id] = rs
			}
			d.relay.mu.Unlock()
			close(r.done)
		}()
		fn(ctx, id)
	}()
}

// stopRunners cancels the record's goroutines and waits for them.
func (d *Daemon) stopRunners(id string) {
	d.relay.mu.Lock()
	rs := append([]*runner(nil), d.relay.runners[id]...)
	d.relay.mu.Unlock()
	for _, r := range rs {
		r.cancel()
	}
	for _, r := range rs {
		<-r.done
	}
}

// hostConfigured reports whether the config has the host. A config
// that cannot be read says nothing, and the host counts as configured:
// a broken file must not make a running task dismissable.
func (d *Daemon) hostConfigured(name string) bool {
	_, ok, err := d.relayHost(name)
	return ok || err != nil
}

// relayHost finds the configured host by name: ok when it is there,
// an error when the config could not be read.
func (d *Daemon) relayHost(name string) (client.Host, bool, error) {
	hosts, err := d.cfg.Hosts()
	if err != nil {
		return client.Host{}, false, err
	}
	for _, h := range hosts {
		if h.Name == name {
			return h, true, nil
		}
	}
	return client.Host{}, false, nil
}

// publishPending sends the record into the merged stream. Called with
// the relay's mutex held, before the daemon's: that is the order the
// merged snapshot takes them in too.
func (d *Daemon) publishPending(p protocol.Pending) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.msubs) == 0 {
		return
	}
	d.mbroadcastLocked(protocol.Message{Type: protocol.TypeUpsert, Pending: &p})
}

// publishRemoved sends the record's removal, with the worktree id it
// retired into when it did. Called with the relay's mutex held.
func (d *Daemon) publishRemoved(id, replacedBy string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.msubs) == 0 {
		return
	}
	d.mbroadcastLocked(protocol.Message{Type: protocol.TypeRemove, PendingID: id, ReplacedBy: replacedBy})
}

// setPending applies a change and publishes the record, as one step
// under the relay's mutex.
func (d *Daemon) setPending(id string, write bool, change func(*pendingFile)) (pendingFile, bool) {
	d.relay.mu.Lock()
	defer d.relay.mu.Unlock()
	if rec, ok := d.relay.recs[id]; ok && rec.retired() {
		// Kept only for its handoff: a runner still finishing, a gone
		// check say, must neither change it nor publish it again after
		// its removal was published.
		return *rec, false
	}
	p, err := d.relay.updateLocked(id, write, change)
	if err != nil {
		d.cfg.Logger.Printf("pending: %s: %v", id, err)
		return p, false
	}
	d.publishPending(p.Pending)
	return p, true
}

// persist is setPending for a change that must reach the disk: the
// write is retried with backoff until it does or ctx ends, since a
// record left without its outcome would be followed by no one.
func (d *Daemon) persist(ctx context.Context, id string, change func(*pendingFile)) (pendingFile, bool) {
	wait := d.cfg.ReconnectMin
	for {
		p, ok := d.setPending(id, true, change)
		if ok {
			return p, true
		}
		if cur, exists := d.relay.get(id); !exists || cur.retired() {
			return p, false
		}
		if !d.relayBackoff(ctx, &wait) {
			return p, false
		}
	}
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
			d.startPending(ctx, p.ID)
		case p.AttemptOpen:
			d.relay.mu.Lock()
			d.startRunnerLocked(ctx, p.ID, d.runAttempt)
			d.relay.mu.Unlock()
		case p.OK:
			// A listing owed, or a handoff the last daemon did not get
			// to write: settle decides which.
			d.relay.mu.Lock()
			d.startRunnerLocked(ctx, p.ID, d.settle)
			d.relay.mu.Unlock()
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
	h, ok, err := d.relayHost(p.Host)
	if err != nil {
		return nil, p, fmt.Errorf("config: %w", err)
	}
	if !ok {
		return nil, p, errHostRemoved
	}
	c, err := d.cfg.Dial(ctx, h)
	if err != nil {
		return nil, p, err
	}
	// The pin first: a machine that is not the one the task was
	// accepted for gets nothing, whatever it can do, and is waited on
	// as an unreachable host is. Then the capabilities, and only then
	// is an empty pin bound.
	if p.EnvironmentID != "" && c.Hello.EnvironmentID != p.EnvironmentID {
		c.Close()
		msg := fmt.Sprintf("%s answers as environment %s, not %s the task was accepted for", h.Name, c.Hello.EnvironmentID, p.EnvironmentID)
		d.setPending(id, false, func(p *pendingFile) { p.Mismatch = msg })
		return nil, p, errors.New(msg)
	}
	d.setPending(id, false, func(p *pendingFile) { p.Mismatch = "" })
	for _, cap := range []string{protocol.CapAdd, protocol.CapFollow, protocol.CapTask} {
		if !protocol.Has(c.Hello.Capabilities, cap) {
			c.Close()
			return nil, p, &refusal{fmt.Sprintf("tasks not supported by %s's daemon %s: no %s capability", h.Name, c.Hello.Version, cap)}
		}
	}
	if p.EnvironmentID == "" {
		env := c.Hello.EnvironmentID
		p, ok = d.setPending(id, true, func(p *pendingFile) { p.EnvironmentID = env })
		if !ok {
			c.Close()
			return nil, p, errors.New("pending: the environment could not be written")
		}
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
		if !p.Sent && time.Since(p.SubmittedAt) > cmdpkg.SenderLifetime {
			// Past the lifetime nothing is sent, to a host that answers
			// or to one that never will: the outcome is unknown.
			d.persist(ctx, id, func(p *pendingFile) {
				p.Done, p.OK, p.Error, p.Reachable, p.Mismatch = true, false, relayOutcomeUnknown+": the submission is older than seven days and is not sent again", true, ""
			})
			return
		}
		c, p, err := d.relayConn(ctx, id)
		if err != nil {
			// A host that cannot take the task refuses it for good only
			// while the add cannot have reached it; once sent, a
			// handshake says nothing about the add's outcome, and the
			// host is waited on as an unreachable one is.
			var ref *refusal
			if errors.As(err, &ref) && !p.Sent && !p.Taken {
				d.persist(ctx, id, func(p *pendingFile) {
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
			// the numbering starts over. Taken stays what it was, and
			// is set by interrupted: the host has known the add, so no
			// later refusal can take it for one that never arrived.
			after = 0
			taken := res.Error == protocol.ErrInterrupted
			if _, ok := d.persist(ctx, id, func(p *pendingFile) {
				p.Sent = false
				if taken {
					p.Taken = true
				}
			}); !ok {
				return
			}
			continue
		}
		// The outcome must reach the disk: a record without it would be
		// followed by no one.
		d.persist(ctx, id, func(p *pendingFile) {
			p.Taken, p.Done, p.Reachable, p.Mismatch = true, true, true, ""
			p.OK = res.OK
			if res.Branch != "" {
				p.Branch = res.Branch
			}
			if res.Root != "" {
				p.Root = res.Root
			}
			p.Session = res.Session
			// The delivery state is the host's whatever the add did:
			// a launch that failed after the agent may have started
			// says unknown, and the prompt leaves the file once it is
			// delivered or there was none.
			p.Prompt = res.Prompt
			if res.OK {
				p.Error = res.Error
				if p.Prompt == "" {
					p.Prompt = protocol.DeliveryNone
				}
				p.Barrier = res.Listing
			} else {
				p.Stage = res.Stage
				p.Error = res.Error
				if res.Error == protocol.ErrSubmissionExpired {
					p.Error = relayOutcomeUnknown + ": " + res.Error
				} else if res.Stage != "" {
					p.Error = "failed at " + res.Stage + ": " + res.Error
				}
			}
			if p.Delivered() {
				p.PromptText = ""
			}
		})
		break
	}
	if ctx.Err() == nil {
		d.settle(ctx, id)
	}
}

// settle brings a record with a successful outcome to rest: the
// listing after the result is waited for once, and a record that is
// complete, prompt delivered or none, whose worktree that listing
// showed is handed over. It is called after the result, after every
// attempt and at start, serialized per record, and decides on the
// record as it is when each step ends, so a listing that completes
// while an attempt delivers, or the other way round, strands nothing.
func (d *Daemon) settle(ctx context.Context, id string) {
	l := d.relay.settleLock(id)
	l.Lock()
	defer l.Unlock()
	p, ok := d.relay.get(id)
	if !ok || !p.Done || !p.OK || p.retired() || ctx.Err() != nil {
		return
	}
	if !p.Listed {
		if !d.retire(ctx, id) {
			return
		}
		if p, ok = d.relay.get(id); !ok {
			return
		}
	}
	if p.Complete() && p.Listed && !p.Gone && !p.retired() {
		d.handoff(ctx, id, p.WorktreeID())
	}
}

// handoffRecheck is how often a handoff waiting for the merged stream
// to show the worktree looks at the host's listing again, and
// handoffPatience how long it waits for the stream in all before it
// hands off on the listing alone.
var (
	handoffRecheck  = 3 * time.Second
	handoffPatience = time.Minute
)

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
// after them until one satisfies the result's barrier, with the host's
// listing error on the record meanwhile. It records what the listing
// said, a worktree at the root or gone, and reports whether it was
// seen; the handoff is settle's, on the record as it then is.
func (d *Daemon) retire(ctx context.Context, id string) bool {
	wait := d.cfg.ReconnectMin
	for ctx.Err() == nil {
		p, ok := d.relay.get(id)
		if !ok || !p.Done || !p.OK {
			return false
		}
		if p.Listed {
			return true
		}
		if p.Barrier == nil || p.Root == "" {
			// A result without a barrier cannot be listed for; the
			// record stays awaiting a listing that says so.
			d.setPending(id, false, func(p *pendingFile) { p.ListingError = "the result carries no listing barrier" })
			return false
		}
		c, p, err := d.relayConn(ctx, id)
		if err != nil {
			// A host that cannot list for the task now, refused or
			// unreachable, is waited on: the record says why, and the
			// listing is still owed.
			var ref *refusal
			if errors.As(err, &ref) {
				d.setPending(id, false, func(p *pendingFile) { p.Reachable, p.ListingError = true, ref.msg })
			} else {
				d.unreachable(id, err)
			}
			if !d.relayBackoff(ctx, &wait) {
				return false
			}
			continue
		}
		wait = d.cfg.ReconnectMin
		d.setPending(id, false, func(p *pendingFile) { p.Reachable, p.Unreachable = true, "" })
		present, err := d.awaitListing(ctx, c, *p.Barrier, p.Root, func(msg string) {
			d.setPending(id, false, func(p *pendingFile) { p.ListingError = msg })
		})
		c.Close()
		if err != nil {
			d.unreachable(id, fmt.Errorf("connection lost: %v", err))
			if !d.relayBackoff(ctx, &wait) {
				return false
			}
			continue
		}
		_, ok = d.persist(ctx, id, func(p *pendingFile) { p.Listed, p.Gone, p.ListingError = true, !present, "" })
		return ok
	}
	return false
}

// awaitListing subscribes to the host and reads until a listing at or
// past the barrier, then reports whether a worktree is at root in it.
// The worktrees are tracked from the snapshot on, since the stamp of a
// listing arrives after the records it changed.
func (d *Daemon) awaitListing(ctx context.Context, c *client.Conn, barrier protocol.Listing, root string, listingErr func(string)) (bool, error) {
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
		if m.Type == protocol.TypeSnapshot || m.Listing != nil {
			// The host's listing error, or its clearing, rides on the
			// snapshot and on every stamp.
			listingErr(m.ListingError)
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
	wait := d.cfg.ReconnectMin
	began := time.Now()
	recheck := time.Now().Add(handoffRecheck)
	for ctx.Err() == nil {
		// The check, the write and the publication are one step under
		// the relay's mutex, which a merged subscription takes for its
		// snapshot: a viewer that arrives sees the record and, once it
		// is gone, the worktree row it became.
		d.relay.mu.Lock()
		rec, ok := d.relay.recs[id]
		if !ok || rec.retired() || rec.Gone {
			d.relay.mu.Unlock()
			return
		}
		p := *rec // copied under the mutex; read after it is released
		d.mu.Lock()
		watching := d.mctx != nil
		mh, configured := d.mhosts[p.Host]
		shown := false
		if configured {
			if mh.host.Local() {
				_, shown = d.worktrees[p.Root]
			} else {
				_, shown = mh.worktrees[worktreeID]
			}
		}
		d.mu.Unlock()
		if watching && configured && !shown && time.Since(began) < handoffPatience {
			// The merged stream's own connection to the host may be
			// down while the relay's is up, or the worktree may be
			// gone again already: the row stays while the stream is
			// expected to show the replacement, the host's listing
			// looked at again meanwhile, and a worktree that is not
			// there any more makes the record gone instead.
			d.relay.mu.Unlock()
			if time.Now().After(recheck) {
				recheck = time.Now().Add(handoffRecheck)
				if present, ok := d.listingHas(ctx, id, p); ok && !present {
					d.persist(ctx, id, func(p *pendingFile) { p.Gone = true })
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		now := time.Now()
		_, err := d.relay.updateLocked(id, true, func(p *pendingFile) {
			p.Listed, p.ReplacedBy, p.RetiredAt, p.PromptText = true, worktreeID, now, ""
		})
		if err == nil {
			d.publishRemoved(id, worktreeID)
		}
		d.relay.mu.Unlock()
		if err == nil {
			return
		}
		// The handoff must reach the disk before the removal is
		// published; a write that fails is retried, everything checked
		// again.
		d.cfg.Logger.Printf("pending: %s: %v", id, err)
		if !d.relayBackoff(ctx, &wait) {
			return
		}
	}
}

// listingHas asks the host, on a connection of the relay's own, whether
// a listing past the record's barrier still has its worktree; not ok
// when the host could not be asked.
func (d *Daemon) listingHas(ctx context.Context, id string, p pendingFile) (present, ok bool) {
	if p.Barrier == nil {
		return false, false
	}
	// Bounded: a host whose listings fail after a restart would hold
	// the handoff past its patience otherwise.
	ctx, cancel := context.WithTimeout(ctx, handoffRecheck)
	defer cancel()
	c, _, err := d.relayConn(ctx, id)
	if err != nil {
		return false, false
	}
	defer c.Close()
	present, err = d.awaitListing(ctx, c, *p.Barrier, p.Root, func(msg string) {
		d.setPending(id, false, func(p *pendingFile) { p.ListingError = msg })
	})
	return present, err == nil
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
	if !ok {
		res.Error = "no pending record " + id
		return res
	}
	switch {
	case p.retired():
		// Kept for its handoff, which a view may still need; the sweep
		// takes it after the day.
		res.Error = "the task " + id + " has handed over to its worktree row; nothing to dismiss"
		return res
	case !p.Sent && !p.Taken:
		// The host has no trace of it: removed first, under the relay's
		// mutex, and its goroutines ended after; a runner about to
		// send has to write Sent under the same mutex, which fails once
		// the record is gone, so nothing is sent for a file that is
		// not there.
		d.relay.mu.Lock()
		p, ok := d.relay.recs[id]
		if !ok || p.Sent || p.Taken {
			d.relay.mu.Unlock()
			return d.dismissEnded(id)
		}
		if err := d.relay.removeLocked(id); err != nil {
			d.relay.mu.Unlock()
			res.Error = err.Error()
			return res
		}
		d.publishRemoved(id, "")
		d.relay.mu.Unlock()
		d.stopRunners(id)
		res.OK = true
		return res
	case !d.hostConfigured(p.Host) || p.Mismatch != "":
		// A host gone from the config, or a machine under its name that
		// is not the one the task was accepted for, will never answer:
		// the goroutines are ended, then the record is removed if that
		// still holds; a host back meanwhile means the goroutines are
		// started again and the record stays.
		d.stopRunners(id)
		p, _ = d.relay.get(id)
		if !d.hostConfigured(p.Host) || p.Mismatch != "" {
			d.relay.mu.Lock()
			p, ok := d.relay.recs[id]
			if ok && !p.retired() {
				if err := d.relay.removeLocked(id); err != nil {
					d.relay.mu.Unlock()
					res.Error = err.Error()
					return res
				}
				d.publishRemoved(id, "")
				d.relay.mu.Unlock()
				res.OK = true
				return res
			}
			d.relay.mu.Unlock()
		}
		d.restartRunners(id)
		return d.dismissEnded(id)
	}
	return d.dismissEnded(id)
}

// dismissEnded is dismissSettled with the record's goroutines ended
// once it is gone: a settle waiting on the host's listing would keep
// its channel open otherwise. After dismissSettled has let go of its
// locks, so a runner queued on the attempt lock can see its cancel.
func (d *Daemon) dismissEnded(id string) protocol.Message {
	res := d.dismissSettled(id)
	if res.OK {
		d.stopRunners(id)
	}
	return res
}

// dismissSettled dismisses a record that needs the user: one with an
// outcome and no attempt open. Under the record's attempt lock, so a
// prompt request cannot open an attempt between the check and the
// removal; an attempt in flight holds it, and is the refusal, not a
// wait on the network. The check, the removal and its publication are
// one step under the relay's mutex.
func (d *Daemon) dismissSettled(id string) protocol.Message {
	res := protocol.Message{Type: protocol.TypeResult, ID: id}
	l := d.relay.attemptLock(id)
	if !l.TryLock() {
		res.Error = "a delivery attempt is unresolved; it cannot be dismissed until it has an outcome"
		return res
	}
	defer l.Unlock()
	d.relay.mu.Lock()
	defer d.relay.mu.Unlock()
	p, ok := d.relay.recs[id]
	switch {
	case !ok:
		res.Error = "no pending record " + id
	case p.retired():
		res.Error = "the task " + id + " has handed over to its worktree row; nothing to dismiss"
	case !p.Done:
		res.Error = "the add is still running; it cannot be dismissed until it has an outcome"
	case p.AttemptOpen:
		res.Error = "a delivery attempt is unresolved; it cannot be dismissed until it has an outcome"
	}
	if res.Error != "" {
		return res
	}
	if err := d.relay.removeLocked(id); err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	d.publishRemoved(id, "")
	return res
}

// restartRunners starts again what a dismiss stopped and then did not
// remove: the add's follower when the add has no outcome, the
// attempt's when one is open, the settling otherwise.
func (d *Daemon) restartRunners(id string) {
	p, ok := d.relay.get(id)
	if !ok || p.retired() {
		return
	}
	ctx := d.runCtx()
	switch {
	case !p.Done:
		d.startPending(ctx, id)
	case p.AttemptOpen:
		d.relay.mu.Lock()
		d.startRunnerLocked(ctx, id, d.runAttempt)
		d.relay.mu.Unlock()
	case p.OK:
		d.relay.mu.Lock()
		d.startRunnerLocked(ctx, id, d.settle)
		d.relay.mu.Unlock()
	}
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
	// An attempt in flight holds the lock, through every reconnect: a
	// second request is answered that it is open, not queued behind it
	// to open the next.
	l := d.relay.attemptLock(id)
	if !l.TryLock() {
		if p, ok := d.relay.get(id); ok {
			res.Attempt = p.Attempt
			res.Error = fmt.Sprintf("attempt %d is open; %s", p.Attempt, p.Unreachable)
		} else {
			res.Error = "no pending record " + id
		}
		return res
	}
	defer l.Unlock()
	p, ok := d.relay.get(id)
	switch {
	case !ok:
		res.Error = "no pending record " + id
	case !p.Done:
		res.Error = "the add is still running"
	case p.PromptText == "":
		res.Error = "no prompt retained for " + id
	case p.Delivered():
		res.Error = "the prompt is delivered"
	case !p.OK:
		// An add that failed has no agent of its own to deliver to; the
		// prompt is the user's, tasks show prints it.
		res.Error = "the add failed; laatmux tasks show " + id + " prints the prompt"
	case p.Gone:
		res.Error = "the worktree is gone; laatmux tasks show " + id + " prints the prompt"
	case p.Prompt != protocol.DeliveryNotDelivered && p.Prompt != protocol.DeliveryUnknown:
		res.Error = "nothing to deliver: the prompt is " + p.Prompt
	case p.AttemptOpen:
		res.Error = fmt.Sprintf("attempt %d is unresolved", p.Attempt)
	case p.AttemptError == protocol.ErrRecoveryExpired:
		res.Error = protocol.ErrRecoveryExpired + "; laatmux tasks show " + id + " prints the prompt"
	}
	if res.Error != "" {
		return res
	}
	if _, ok := d.setPending(id, true, func(p *pendingFile) {
		p.Attempt++
		p.AttemptOpen, p.AttemptError = true, ""
	}); !ok {
		res.Error = "pending: the attempt could not be written"
		return res
	}
	p, resolved := d.runAttemptLocked(ctx, id, false, false)
	res.Attempt = p.Attempt
	if _, ok := d.relay.get(id); !ok {
		// Dismissed while the attempt ran: the record is gone as the
		// user asked, and nothing follows it.
		res.Error = "no pending record " + id
		return res
	}
	if !resolved {
		// The host cannot be reached now: the attempt stays open on
		// disk and is followed in the background, and the answer says
		// so rather than an outcome the host never gave.
		res.Error = "attempt " + strconv.Itoa(p.Attempt) + " is open; " + p.Unreachable
		d.relay.mu.Lock()
		d.startRunnerLocked(d.runCtx(), id, d.runAttempt)
		d.relay.mu.Unlock()
		return res
	}
	if p.AttemptError != "" {
		res.Error = p.AttemptError
		return res
	}
	res.OK = true
	res.Prompt, res.Error = p.Prompt, p.Error
	d.relay.mu.Lock()
	d.startRunnerLocked(d.runCtx(), id, d.settle)
	d.relay.mu.Unlock()
	return res
}

// runAttempt follows the record's open attempt, one sent before, under
// the record's attempt lock for as long as it takes, then settles the
// record.
func (d *Daemon) runAttempt(ctx context.Context, id string) {
	l := d.relay.attemptLock(id)
	l.Lock()
	d.runAttemptLocked(ctx, id, true, true)
	l.Unlock()
	if ctx.Err() == nil {
		d.settle(ctx, id)
	}
}

// runAttemptLocked sends the open attempt as a prompt message and
// follows it until the host answers. With sent, the attempt was sent
// before, by the daemon before this one, and is followed by number
// first; one the host never saw is sent. An attempt is closed by the
// host's answer alone: a host that cannot be reached, or that refuses
// the connection, leaves it open, waited on with backoff when wait is
// set, else reported as unresolved to the caller, who follows it in
// the background. Called with the attempt lock held.
func (d *Daemon) runAttemptLocked(ctx context.Context, id string, sent, wait bool) (pendingFile, bool) {
	backoff := d.cfg.ReconnectMin
	for ctx.Err() == nil {
		p, ok := d.relay.get(id)
		if !ok || !p.AttemptOpen {
			return p, true
		}
		c, p, err := d.relayConn(ctx, id)
		if err != nil {
			d.unreachable(id, err)
			if !wait {
				p, _ = d.relay.get(id)
				return p, false
			}
			if !d.relayBackoff(ctx, &backoff) {
				return p, false
			}
			continue
		}
		backoff = d.cfg.ReconnectMin
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
			if !wait {
				p, _ = d.relay.get(id)
				return p, false
			}
			if !d.relayBackoff(ctx, &backoff) {
				return p, false
			}
			continue
		}
		if !res.OK && res.Error == protocol.ErrUnknownAttempt {
			sent = false
			continue
		}
		if !res.OK && strings.HasPrefix(res.Error, protocol.ErrAttemptNotRecorded) {
			// The host did nothing and the number is not taken: the
			// attempt stays open and is sent again as it is.
			sent = false
			d.setPending(id, false, func(p *pendingFile) { p.Unreachable = res.Error })
			if !wait {
				p, _ = d.relay.get(id)
				return p, false
			}
			if !d.relayBackoff(ctx, &backoff) {
				return p, false
			}
			continue
		}
		// An outcome that could not be written leaves the attempt open
		// on disk: not resolved, so the caller follows it again.
		var written bool
		p, written = d.persist(ctx, id, func(p *pendingFile) {
			p.AttemptOpen = false
			if res.OK {
				p.Prompt, p.Error, p.AttemptError = res.Prompt, res.Error, ""
				if p.Delivered() {
					p.PromptText = ""
				}
				return
			}
			// The host refused the attempt and recorded nothing: the
			// refusal is kept apart from the add's own outcome, and the
			// number goes back to what the host has, which its answer
			// says when the numbers had drifted.
			p.AttemptError = res.Error
			if n, ok := journalHas(res.Error); ok {
				p.Attempt = n
			} else if p.Attempt > 0 {
				p.Attempt--
			}
		})
		return p, written
	}
	p, _ := d.relay.get(id)
	return p, false
}

// journalHas reads the attempt count from the host's answer to a number
// out of order, "attempt N is not the next; the journal has M".
func journalHas(msg string) (int, bool) {
	i := strings.LastIndex(msg, "the journal has ")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(msg[i+len("the journal has "):]))
	if err != nil {
		return 0, false
	}
	return n, true
}
