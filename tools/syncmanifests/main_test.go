package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const catalogTOML = `schema_version = 1

[[agents]]
id = "claude"
path = "claude.toml"

[[agents]]
id = "codex"
path = "codex.toml"

[[agents]]
id = "future"
path = "future.toml"
`

func manifest(id, version, extra string) string {
	return `id = "` + id + `"
version = "` + version + `"
min_engine_version = 2
updated_at = "2026-09-11T00:00:00Z"
` + extra + `
[[rules]]
id = "prompt"
state = "idle"
region = "bottom_non_empty_lines(3)"
contains = ["❯"]
`
}

func serve(t *testing.T, files map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/agent-detection/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/agent-detection/index.toml"
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSyncUpdatesChangedManifestsAndNotice(t *testing.T) {
	newClaude := manifest("claude", "2026.09.20.1", "")
	codex := manifest("codex", "2026.09.15.1", "")
	cat := serve(t, map[string]string{
		"index.toml":  catalogTOML,
		"claude.toml": newClaude,
		"codex.toml":  codex,
	})
	dir := t.TempDir()
	write(t, dir, "claude.toml", manifest("claude", "2026.09.11.1", ""))
	write(t, dir, "codex.toml", codex)
	write(t, dir, "NOTICE", "stale\n")

	var out bytes.Buffer
	if err := run(context.Background(), options{catalog: cat, dir: dir}, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if got := read(t, dir, "claude.toml"); got != newClaude {
		t.Errorf("claude.toml not updated:\n%s", got)
	}
	if got := read(t, dir, "codex.toml"); got != codex {
		t.Errorf("codex.toml changed unexpectedly:\n%s", got)
	}
	notice := read(t, dir, "NOTICE")
	for _, want := range []string{cat, "claude.toml", "2026.09.20.1", "codex.toml", "2026.09.15.1", "Apache License, Version 2.0"} {
		if !strings.Contains(notice, want) {
			t.Errorf("NOTICE lacks %q:\n%s", want, notice)
		}
	}
	if s := out.String(); !strings.Contains(s, "claude.toml") || !strings.Contains(s, "updated") || !strings.Contains(s, "up to date") {
		t.Errorf("unexpected report:\n%s", s)
	}
	// future.toml is in the catalog but was neither vendored nor asked for.
	if _, err := os.Stat(filepath.Join(dir, "future.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("future.toml vendored without being asked: %v", err)
	}

	// A second run is a no-op.
	out.Reset()
	if err := run(context.Background(), options{catalog: cat, dir: dir, check: true}, &out); err != nil {
		t.Fatalf("check after sync: %v\n%s", err, out.String())
	}
}

func TestCheckReportsOutdatedWithoutWriting(t *testing.T) {
	cat := serve(t, map[string]string{
		"index.toml":  catalogTOML,
		"claude.toml": manifest("claude", "2026.09.20.1", ""),
	})
	dir := t.TempDir()
	old := manifest("claude", "2026.09.11.1", "")
	write(t, dir, "claude.toml", old)

	var out bytes.Buffer
	err := run(context.Background(), options{catalog: cat, dir: dir, check: true}, &out)
	if !errors.Is(err, errOutdated) || !strings.Contains(err.Error(), "claude.toml") {
		t.Fatalf("check: err = %v", err)
	}
	if got := read(t, dir, "claude.toml"); got != old {
		t.Errorf("check mode wrote claude.toml")
	}
	if _, err := os.Stat(filepath.Join(dir, "NOTICE")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("check mode wrote NOTICE: %v", err)
	}
}

func TestVendorsNewAgentOnRequest(t *testing.T) {
	future := manifest("future", "2026.09.21.1", "")
	cat := serve(t, map[string]string{
		"index.toml":  catalogTOML,
		"claude.toml": manifest("claude", "2026.09.11.1", ""),
		"future.toml": future,
	})
	dir := t.TempDir()
	write(t, dir, "claude.toml", manifest("claude", "2026.09.11.1", ""))

	var out bytes.Buffer
	if err := run(context.Background(), options{catalog: cat, dir: dir, agents: []string{"future"}}, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if got := read(t, dir, "future.toml"); got != future {
		t.Errorf("future.toml not vendored:\n%s", got)
	}
}

func TestRefusesManifestTheEngineCannotEvaluate(t *testing.T) {
	// Upstream bumped the engine and started using a selector we have not
	// ported. Nothing may be written, and the error must say why.
	cat := serve(t, map[string]string{
		"index.toml": catalogTOML,
		"claude.toml": manifest("claude", "2026.09.20.1", "") + `
[[rules]]
id = "new"
state = "working"
region = "after_current_prompt_block_marker"
contains = ["x"]
`,
		"codex.toml": strings.Replace(manifest("codex", "2026.09.20.1", ""), "min_engine_version = 2", "min_engine_version = 4", 1),
	})
	dir := t.TempDir()
	oldClaude := manifest("claude", "2026.09.11.1", "")
	oldCodex := manifest("codex", "2026.09.11.1", "")
	write(t, dir, "claude.toml", oldClaude)
	write(t, dir, "codex.toml", oldCodex)

	var out bytes.Buffer
	err := run(context.Background(), options{catalog: cat, dir: dir}, &out)
	if err == nil {
		t.Fatal("run succeeded")
	}
	for _, want := range []string{"claude: upstream manifest is not evaluable", "after_current_prompt_block_marker", "codex: upstream manifest is not evaluable", "engine version 4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if read(t, dir, "claude.toml") != oldClaude || read(t, dir, "codex.toml") != oldCodex {
		t.Error("files were written despite validation failure")
	}
}

func TestUnknownAgentIsAnError(t *testing.T) {
	cat := serve(t, map[string]string{"index.toml": catalogTOML})
	dir := t.TempDir()
	write(t, dir, "claude.toml", manifest("claude", "2026.09.11.1", ""))
	err := run(context.Background(), options{catalog: cat, dir: dir, agents: []string{"nope"}}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "nope: not in catalog") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveRejectsEscapingPaths(t *testing.T) {
	for _, p := range []string{"../secret.toml", "/etc/passwd", "https://evil.example/x.toml"} {
		if _, err := resolve("https://herdr.dev/agent-detection/index.toml", p); err == nil {
			t.Errorf("resolve(%q) accepted", p)
		}
	}
	got, err := resolve("https://herdr.dev/agent-detection/index.toml", "claude.toml")
	if err != nil || got != "https://herdr.dev/agent-detection/claude.toml" {
		t.Errorf("resolve = %q, %v", got, err)
	}
}
