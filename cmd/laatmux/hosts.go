package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/github"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/peer"
	"github.com/laat/laatmux/internal/protocol"
)

// cmdHosts lists the hosts, each dialled as a client dials it but a
// paused one, which its line says; with pause or resume and a host's
// name it sets paused on the host's entry in the config, or takes it
// off, which every daemon and view that follows the file acts on at its
// next look.
func cmdHosts(ctx context.Context, args []string) error {
	switch {
	case len(args) == 2 && (args[0] == "pause" || args[0] == "resume"):
		changed, err := config.SetPaused(config.Path(), args[1], args[0] == "pause")
		if err != nil {
			return err
		}
		fmt.Println(pausedLine(args[1], args[0] == "pause", changed))
		if args[0] == "pause" {
			if note := pauseUnknown(localHello(ctx)); note != "" {
				fmt.Println(note)
			}
		}
		return nil
	case len(args) > 0:
		return errors.New("usage: laatmux hosts [pause|resume <host>]")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Print(hostsReport(probeHosts(ctx, cfg.Hosts, client.Dial), version, githubLine(ctx)))
	return nil
}

// probeHosts dials the hosts, all at once, but a paused one, whose row
// says so: nothing that runs on its own dials it.
func probeHosts(ctx context.Context, hosts []config.Host, dial func(context.Context, peer.Host) (*client.Conn, error)) []hostRow {
	rows := make([]hostRow, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		if h.Paused {
			rows[i] = hostRow{name: h.Name, status: "paused; laatmux hosts resume " + h.Name + " connects it"}
			continue
		}
		wg.Add(1)
		go func(i int, h config.Host) {
			defer wg.Done()
			dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			start := time.Now()
			c, err := dial(dctx, h.Host)
			if err != nil {
				rows[i] = hostRow{name: h.Name, status: "unreachable: " + err.Error()}
				return
			}
			defer c.Close()
			rows[i] = hostRow{name: h.Name, status: fmt.Sprintf("ok  daemon %s  protocol %d  env %s  caps %s  %dms",
				c.Hello.Version, c.Hello.Protocol, c.Hello.EnvironmentID, strings.Join(c.Hello.Capabilities, ","), time.Since(start).Milliseconds()),
				differs: c.Hello.Version != version}
		}(i, h)
	}
	wg.Wait()
	return rows
}

// localHello is the hello of this machine's daemon, the zero message
// when none is running; none is started.
func localHello(ctx context.Context) protocol.Message {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	nc, err := client.DialLocal(ctx, false)
	if err != nil {
		return protocol.Message{}
	}
	c, err := client.Connect(ctx, peer.Host{Name: "local"}, nc, nc, func() { nc.Close() })
	if err != nil {
		return protocol.Message{}
	}
	defer c.Close()
	return c.Hello
}

// pauseUnknown is the note for a local daemon whose hello lacks the
// pause capability, an older build still running, which dials a paused
// host all the same; "" for one with it, and for none running, whose
// successor reads the file as it starts.
func pauseUnknown(hello protocol.Message) string {
	if hello.Type == "" || protocol.Has(hello.Capabilities, protocol.CapPause) {
		return ""
	}
	return "the local daemon " + hello.Version + " is older than pause and dials a paused host all the same; laatmux stop ends it, and the next command starts this build"
}

// pausedLine says what pausing or resuming the host did, changed being
// that the config had it the other way.
func pausedLine(name string, paused, changed bool) string {
	switch {
	case !changed && paused:
		return name + " is paused already"
	case !changed:
		return name + " is not paused"
	case paused:
		return name + " paused; this machine dials it for upgrade alone until it is resumed"
	}
	return name + " resumed; it is dialled again"
}

// githubLine is the local daemon's GitHub state, from its merged
// snapshot: why it cannot read PRs and checks, or that it can. "" when
// the daemon is not running or has no branches capability.
func githubLine(ctx context.Context) string {
	c, ok := merged.Dial(ctx)
	if !ok {
		return ""
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapBranches) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	defer c.CloseOnDone(ctx)()
	if err := c.Write(protocol.Message{Type: protocol.TypeSubscribe, Merged: true}); err != nil {
		return ""
	}
	msg, err := c.Read()
	if err != nil || msg.Type != protocol.TypeSnapshot {
		return ""
	}
	status := msg.GitHubError
	if status == "" {
		// The daemon may not have asked yet, with no view open: gh is
		// asked here, on the same machine, before saying ok.
		status = ghStatus(ctx)
	}
	return fmt.Sprintf("  %-16s %s", "github:", status)
}

// ghStatus is whether gh on this machine can read github.com: "ok", or
// why not, from the smallest query the daemon's own reading would make,
// so the two tell the same failures apart.
var ghStatus = func(ctx context.Context) string {
	body, err := github.GH(ctx, "github.com", "query { viewer { login } }", nil)
	if err != nil {
		return err.Error()
	}
	return viewerStatus(body)
}

// viewerStatus reads the viewer query's answer: ok with a login, else
// the answer's first error.
func viewerStatus(body []byte) string {
	var resp struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "gh api graphql: " + err.Error()
	}
	if len(resp.Errors) > 0 {
		return "gh api graphql: " + resp.Errors[0].Message
	}
	if resp.Data.Viewer.Login == "" {
		return "gh api graphql: no viewer"
	}
	return "ok"
}

// hostRow is one host in the listing.
type hostRow struct {
	name, status string
	differs      bool // the daemon's build is not this client's
}

// hostsReport is the listing, with a mark on every daemon whose build is
// not this client's and a line saying what the mark means. Versions are
// git describe strings, so they are equal or not, never ordered.
func hostsReport(rows []hostRow, build, github string) string {
	var b strings.Builder
	marked := false
	for _, r := range rows {
		mark := " "
		if r.differs {
			mark, marked = "*", true
		}
		fmt.Fprintf(&b, "%s %-16s %s\n", mark, r.name, r.status)
	}
	if github != "" {
		b.WriteString(github + "\n")
	}
	if marked {
		fmt.Fprintf(&b, "* daemon build differs from this client's (%s); laatmux upgrade <host> installs this one\n", build)
	}
	return b.String()
}
