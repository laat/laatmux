package merged

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
)

// The merged stream, read from the local daemon. The daemon dials the
// hosts in the config and republishes one stream over the local socket
// with every host's records, a host record per host, and the local
// workspace sessions, so a client per window is one local connection
// rather than an ssh channel per host.

// Dial connects to the local daemon when it has the merged capability.
// False is a daemon that could not be reached or started, or an older
// build without the capability; the caller says what that means for
// its command.
func Dial(ctx context.Context) (*client.Conn, bool) {
	c, err := client.Dial(ctx, peer.Host{Name: "local"})
	if err != nil {
		return nil, false
	}
	if !protocol.Has(c.Hello.Capabilities, protocol.CapMerged) {
		c.Close()
		return nil, false
	}
	return c, true
}

// Read subscribes to the merged stream and applies it until ready
// holds of the hosts still pending or the wait is over, then returns
// the hosts that are still neither listed nor failed. The snapshot
// comes at once with what the daemon knows, which on a cold daemon is
// the host rows alone, and the hosts fill in as their connections come
// up.
func (m *State) Read(ctx context.Context, c *client.Conn, wait time.Duration, ready func(pending []string) bool) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	defer c.CloseOnDone(ctx)()
	m.via(c)
	if err := c.Write(protocol.Message{Type: protocol.TypeSubscribe, Merged: true}); err != nil {
		return nil, err
	}
	snapshot := false
	for {
		msg, err := c.Read()
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				if !snapshot {
					// Nothing arrived, not even the host rows: the
					// daemon is stuck before its first poll, or the
					// sessions listing hangs. An empty listing would
					// look complete.
					return nil, fmt.Errorf("local daemon: no snapshot after %s", wait)
				}
				break
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("local daemon: %w", err)
		}
		if msg.Type == protocol.TypeError {
			return nil, fmt.Errorf("local daemon: %s", msg.Error)
		}
		if msg.Type == protocol.TypeSnapshot {
			snapshot = true
		}
		m.Apply(msg)
		if ready(m.Pending()) {
			break
		}
	}
	return m.Pending(), nil
}

// Follow keeps the merged stream applied, reconnecting to the local
// daemon with backoff when it goes away, which a restart for an upgrade
// does. The last state stays on screen with the daemon row saying so. c
// is the connection to start on, nil to dial.
func (m *State) Follow(ctx context.Context, c *client.Conn) {
	backoff := followBackoffMin
	for ctx.Err() == nil {
		var ok bool
		if c == nil {
			c, ok = Dial(ctx)
		} else {
			ok = true
		}
		if ok {
			// The backoff resets on a snapshot, not on a connection: a
			// daemon that answers the hello and then drops the
			// subscription every time is retried as slowly as one that
			// does not answer at all.
			stop := c.CloseOnDone(ctx)
			m.via(c)
			if err := c.Write(protocol.Message{Type: protocol.TypeSubscribe, Merged: true}); err == nil {
				for {
					msg, err := c.Read()
					if err != nil {
						break
					}
					if msg.Type == protocol.TypeError {
						m.setDaemonErr(msg.Error)
						break
					}
					if msg.Type == protocol.TypeSnapshot {
						backoff = followBackoffMin
					}
					m.setDaemonErr("")
					m.Apply(msg)
				}
			}
			stop()
			c.Close()
			c = nil
			if ctx.Err() == nil && m.daemonErrIs("") {
				m.setDaemonErr("disconnected; reconnecting")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, followBackoffMax)
	}
}

// followBackoff is the wait between attempts to reach the local daemon's
// merged stream, doubling from the min to the max. Variables so a test
// can shorten them.
var (
	followBackoffMin = time.Second
	followBackoffMax = 30 * time.Second
)

// via records what the merging daemon at the other end of c forwards.
func (m *State) via(c *client.Conn) {
	m.mu.Lock()
	m.stripped = !protocol.Has(c.Hello.Capabilities, protocol.CapAttribution)
	m.mu.Unlock()
}

func (m *State) setDaemonErr(s string) {
	m.mu.Lock()
	changed := m.daemonErr != s
	m.daemonErr = s
	m.mu.Unlock()
	if changed {
		m.Notify()
	}
}

func (m *State) daemonErrIs(s string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.daemonErr == s
}
