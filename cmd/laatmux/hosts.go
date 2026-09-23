package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
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
	fmt.Print(hostsReport(rows, version))
	return nil
}

// hostRow is one host in the listing.
type hostRow struct {
	name, status string
	differs      bool // the daemon's build is not this client's
}

// hostsReport is the listing, with a mark on every daemon whose build is
// not this client's and a line saying what the mark means. Versions are
// git describe strings, so they are equal or not, never ordered.
func hostsReport(rows []hostRow, build string) string {
	var b strings.Builder
	marked := false
	for _, r := range rows {
		mark := " "
		if r.differs {
			mark, marked = "*", true
		}
		fmt.Fprintf(&b, "%s %-16s %s\n", mark, r.name, r.status)
	}
	if marked {
		fmt.Fprintf(&b, "* daemon build differs from this client's (%s); laatmux upgrade <host> installs this one\n", build)
	}
	return b.String()
}
