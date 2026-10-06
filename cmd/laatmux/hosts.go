package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/github"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/protocol"
)

func cmdHosts(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	rows := make([]hostRow, len(cfg.Hosts))
	var wg sync.WaitGroup
	for i, h := range cfg.Hosts {
		wg.Add(1)
		go func(i int, h config.Host) {
			defer wg.Done()
			dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			start := time.Now()
			c, err := client.Dial(dctx, h.Host)
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
	fmt.Print(hostsReport(rows, version, githubLine(ctx)))
	return nil
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
