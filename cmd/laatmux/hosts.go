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
	type row struct {
		name, status string
	}
	rows := make([]row, len(cfg.Hosts))
	var wg sync.WaitGroup
	for i, h := range cfg.Hosts {
		wg.Add(1)
		go func(i int, h client.Host) {
			defer wg.Done()
			dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			start := time.Now()
			c, err := client.Dial(dctx, h)
			if err != nil {
				rows[i] = row{h.Name, "unreachable: " + err.Error()}
				return
			}
			defer c.Close()
			rows[i] = row{h.Name, fmt.Sprintf("ok  daemon %s  protocol %d  env %s  caps %s  %dms",
				c.Hello.Version, c.Hello.Protocol, c.Hello.EnvironmentID, strings.Join(c.Hello.Capabilities, ","), time.Since(start).Milliseconds())}
		}(i, h)
	}
	wg.Wait()
	for _, r := range rows {
		fmt.Printf("%-16s %s\n", r.name, r.status)
	}
	return nil
}
