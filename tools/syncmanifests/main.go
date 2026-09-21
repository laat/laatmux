// Command syncmanifests refreshes the vendored agent-detection manifests in
// internal/detect/manifests from herdr's published catalog.
//
// herdr serves the same manifests it ships in its binary at
// https://herdr.dev/agent-detection/, and updates them there between
// releases, so the catalog is the freshest copy of each file. This tool
// downloads the catalog, fetches every manifest that is already vendored
// (plus any ids named on the command line), and rewrites the files that
// changed together with the NOTICE that records their provenance.
//
// An upstream manifest is only written when the ported engine can evaluate
// all of it (detect.Validate). If herdr bumps min_engine_version or adds a
// region selector, the sync stops with an error and nothing is written, so
// the tree never carries rules that would silently never match.
//
// Usage:
//
//	go run ./tools/syncmanifests            # update vendored manifests
//	go run ./tools/syncmanifests -check     # exit 1 if any is out of date
//	go run ./tools/syncmanifests gemini     # also vendor a new agent
//	go generate ./internal/detect/...       # same as the first form
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/laat/laatmux/internal/detect"
)

const (
	defaultCatalog = "https://herdr.dev/agent-detection/index.toml"
	defaultDir     = "internal/detect/manifests"
	upstreamRepo   = "https://github.com/herdrdev/herdr"
	maxFetchBytes  = 256 * 1024
)

type options struct {
	catalog string
	dir     string
	check   bool
	agents  []string // extra ids to vendor
}

func main() {
	var opts options
	flag.StringVar(&opts.catalog, "catalog", defaultCatalog, "URL of herdr's agent-detection catalog")
	flag.StringVar(&opts.dir, "dir", defaultDir, "directory holding the vendored manifests")
	flag.BoolVar(&opts.check, "check", false, "report outdated manifests and exit 1 instead of writing")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: syncmanifests [flags] [agent-id ...]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	opts.agents = flag.Args()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := run(ctx, opts, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "syncmanifests:", err)
		os.Exit(1)
	}
}

type catalog struct {
	SchemaVersion int            `toml:"schema_version"`
	Agents        []catalogEntry `toml:"agents"`
}

type catalogEntry struct {
	ID   string `toml:"id"`
	Path string `toml:"path"`
}

// manifestHeader is the part of a manifest NOTICE reports on.
type manifestHeader struct {
	ID        string `toml:"id"`
	Version   string `toml:"version"`
	UpdatedAt string `toml:"updated_at"`
}

// vendored is one manifest as it will exist on disk after the sync.
type vendored struct {
	file    string // basename in opts.dir
	header  manifestHeader
	raw     []byte
	changed bool
}

var errOutdated = errors.New("vendored manifests are out of date")

func run(ctx context.Context, opts options, out io.Writer) error {
	local, err := readLocal(opts.dir)
	if err != nil {
		return err
	}
	want := map[string]string{} // id -> local filename ("" when new)
	for id, v := range local {
		want[id] = v.file
	}
	for _, id := range opts.agents {
		if _, ok := want[id]; !ok {
			want[id] = ""
		}
	}
	if len(want) == 0 {
		return fmt.Errorf("nothing to sync: no manifests in %s and no agent ids given", opts.dir)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	catalogRaw, err := fetch(ctx, client, opts.catalog)
	if err != nil {
		return err
	}
	var cat catalog
	if _, err := toml.Decode(string(catalogRaw), &cat); err != nil {
		return fmt.Errorf("parse catalog %s: %w", opts.catalog, err)
	}
	if cat.SchemaVersion != 1 {
		return fmt.Errorf("catalog %s has schema_version %d, this tool understands 1", opts.catalog, cat.SchemaVersion)
	}
	entries := map[string]catalogEntry{}
	for _, e := range cat.Agents {
		entries[e.ID] = e
	}

	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var result []vendored
	var problems []string
	for _, id := range ids {
		entry, ok := entries[id]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: not in catalog", id))
			continue
		}
		u, err := resolve(opts.catalog, entry.Path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		raw, err := fetch(ctx, client, u)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		if err := detect.Validate(raw); err != nil {
			problems = append(problems, fmt.Sprintf("%s: upstream manifest is not evaluable by this engine, port first: %v", id, err))
			continue
		}
		hdr, err := header(raw)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		if hdr.ID != id {
			problems = append(problems, fmt.Sprintf("%s: catalog path %s holds manifest id %q", id, entry.Path, hdr.ID))
			continue
		}
		file := want[id]
		if file == "" {
			file = path.Base(entry.Path)
		}
		v := vendored{file: file, header: hdr, raw: raw}
		if old, ok := local[id]; ok {
			v.changed = !bytes.Equal(old.raw, raw)
		} else {
			v.changed = true
		}
		result = append(result, v)
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}

	notice := renderNotice(opts.catalog, result)
	oldNotice, _ := os.ReadFile(filepath.Join(opts.dir, "NOTICE"))
	noticeChanged := !bytes.Equal(oldNotice, notice)

	var changed []string
	for _, v := range result {
		status := "up to date"
		if v.changed {
			status = "updated"
			changed = append(changed, v.file)
		}
		fmt.Fprintf(out, "%-20s %s (version %s, updated_at %s)\n", v.file, status, v.header.Version, v.header.UpdatedAt)
	}
	if len(changed) == 0 && !noticeChanged {
		return nil
	}
	if opts.check {
		if len(changed) == 0 {
			return fmt.Errorf("%w: NOTICE needs regenerating", errOutdated)
		}
		return fmt.Errorf("%w: %s", errOutdated, strings.Join(changed, ", "))
	}
	for _, v := range result {
		if !v.changed {
			continue
		}
		if err := os.WriteFile(filepath.Join(opts.dir, v.file), v.raw, 0o644); err != nil {
			return err
		}
	}
	if noticeChanged {
		if err := os.WriteFile(filepath.Join(opts.dir, "NOTICE"), notice, 0o644); err != nil {
			return err
		}
		fmt.Fprintln(out, "NOTICE               regenerated")
	}
	return nil
}

// readLocal maps manifest id to the vendored file currently holding it.
func readLocal(dir string) (map[string]vendored, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		return nil, err
	}
	local := map[string]vendored{}
	for _, name := range names {
		raw, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		hdr, err := header(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if prev, dup := local[hdr.ID]; dup {
			return nil, fmt.Errorf("%s and %s both hold manifest id %q", prev.file, filepath.Base(name), hdr.ID)
		}
		local[hdr.ID] = vendored{file: filepath.Base(name), header: hdr, raw: raw}
	}
	return local, nil
}

func header(raw []byte) (manifestHeader, error) {
	var h manifestHeader
	if _, err := toml.Decode(string(raw), &h); err != nil {
		return h, fmt.Errorf("parse manifest: %w", err)
	}
	if h.ID == "" {
		return h, errors.New("manifest has no id")
	}
	return h, nil
}

func resolve(catalogURL, p string) (string, error) {
	base, err := url.Parse(catalogURL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(p)
	if err != nil {
		return "", fmt.Errorf("catalog path %q: %w", p, err)
	}
	if ref.IsAbs() || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
		return "", fmt.Errorf("catalog path %q must be relative to the catalog", p)
	}
	return base.ResolveReference(ref).String(), nil
}

func fetch(ctx context.Context, client *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "laatmux-syncmanifests")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", u, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", u, err)
	}
	if len(raw) > maxFetchBytes {
		return nil, fmt.Errorf("fetch %s: response exceeds %d bytes", u, maxFetchBytes)
	}
	return raw, nil
}

func renderNotice(catalogURL string, files []vendored) []byte {
	var b strings.Builder
	b.WriteString("The manifests in this directory are copied verbatim from herdr\n")
	fmt.Fprintf(&b, "(%s), which publishes them at\n%s\n", upstreamRepo, catalogURL)
	b.WriteString("and also ships them in its source tree under src/detect/manifests/.\n")
	b.WriteString("Refresh with `go run ./tools/syncmanifests`; see that command for details.\n\n")
	b.WriteString("Vendored files:\n")
	for _, v := range files {
		fmt.Fprintf(&b, "  %-20s version %-14s updated_at %s\n", v.file, v.header.Version, v.header.UpdatedAt)
	}
	b.WriteString("\nherdr is licensed under the Apache License, Version 2.0. You may obtain a copy\n")
	b.WriteString("of the License at http://www.apache.org/licenses/LICENSE-2.0. The manifests are\n")
	b.WriteString("redistributed here under the same license.\n")
	return []byte(b.String())
}
