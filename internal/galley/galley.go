// Package galley reads the pull requests staged in galley, the user's
// local review server, whose CLI mirrors gh's. A branch with no pull
// request on GitHub may have one staged there, which the daemon shows
// in its place.
package galley

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/laat/laatmux/internal/protocol"
)

// Runner runs the galley CLI with args and returns its standard output.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Errors a runner reports for galley itself, which the daemon takes as
// nothing staged rather than as a failed lookup: galley not installed,
// its server not running, and a repository it does not know, which is
// every one nothing was staged in.
var (
	ErrNoGalley    = errors.New("galley is not installed")
	ErrNotRunning  = errors.New("the galley server is not running")
	ErrUnknownRepo = errors.New("the repository is not known to galley")
)

// Timeout bounds one galley call.
const Timeout = 30 * time.Second

// Binary is the galley CLI: on PATH, else beside this program, where
// a user's installs live together, ~/.local/bin say, and the daemon,
// started with a PATH without it, would not find it; "" when neither
// has it.
func Binary() string {
	if p, err := exec.LookPath("galley"); err == nil {
		return p
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	p := filepath.Join(filepath.Dir(exe), "galley")
	if st, err := os.Stat(p); err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return ""
	}
	return p
}

// loginPath is the PATH the user's login shell sets, read once: the
// daemon, started from a tmux server or left to launchd, runs with a
// PATH without the user's own tool directories, and galley's CLI is a
// node script that needs node from one of them. "" when the shell
// does not say within its time.
var loginPath = sync.OnceValue(readLoginPath)

func readLoginPath() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, shell, "-lc", `printf %s "$PATH"`).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// cliPath is the PATH the CLI runs with: the login shell's, then this
// process's own, so what either has is found.
func cliPath() string {
	var parts []string
	seen := map[string]bool{}
	for _, p := range strings.Split(loginPath()+string(os.PathListSeparator)+os.Getenv("PATH"), string(os.PathListSeparator)) {
		if p != "" && !seen[p] {
			seen[p] = true
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// CLI runs `galley` with args, on the login shell's PATH. The server is
// never started for it: a lookup from the daemon is no reason to have
// one running, so a server that is not answering is ErrNotRunning.
func CLI(ctx context.Context, args ...string) ([]byte, error) {
	bin := Binary()
	if bin == "" {
		return nil, ErrNoGalley
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(cmd.Environ(), "PATH="+cliPath(), "GALLEY_NO_AUTOSTART=1", "NO_COLOR=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), nil
	}
	msg := strings.TrimSpace(errb.String())
	if strings.Contains(msg, "server is not running") || strings.Contains(msg, "nothing answered at") {
		return nil, ErrNotRunning
	}
	if strings.Contains(msg, "is not known to galley") {
		return nil, ErrUnknownRepo
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if msg == "" {
		msg = err.Error()
	}
	return nil, fmt.Errorf("galley: %s", msg)
}

// URL is the web UI's address: GALLEY_URL, else localhost on
// GALLEY_PORT or galley's own port, as the CLI reads them.
func URL() string {
	if u := os.Getenv("GALLEY_URL"); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	port := os.Getenv("GALLEY_PORT")
	if port == "" {
		port = "24823"
	}
	return "http://localhost:" + port
}

// staged is what `galley pr list --json` says of one pull request.
type staged struct {
	Number      int    `json:"number"`
	State       string `json:"state"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	IsDraft     bool   `json:"isDraft"`
}

// Staged is the pull requests staged in galley for the repository, by
// branch, as the records the daemon publishes: open, marked staged,
// numbered as galley numbers them, with the web UI's page as the URL.
// A published one is left out: it is on GitHub, where the daemon finds
// it. host is the repository's GitHub host, which names it to galley
// beside owner and repo as gh's -R does.
func Staged(ctx context.Context, run Runner, host, owner, repo string) (map[string]*protocol.PullRequest, error) {
	name := owner + "/" + repo
	if host != "" && !strings.EqualFold(host, "github.com") {
		name = host + "/" + name
	}
	out, err := run(ctx, "pr", "list", "-R", name, "-s", "staged", "-L", "200", "--json", "number,state,headRefName,baseRefName,isDraft")
	if err != nil {
		return nil, err
	}
	return parse(out, URL(), owner, repo)
}

// parse reads the listing into records, with the pages under base.
func parse(out []byte, base, owner, repo string) (map[string]*protocol.PullRequest, error) {
	var list []staged
	if err := json.Unmarshal(bytes.TrimSpace(out), &list); err != nil {
		return nil, fmt.Errorf("galley pr list: %w", err)
	}
	prs := map[string]*protocol.PullRequest{}
	for _, s := range list {
		// STAGED, as gh writes states; staged in galley's own tables.
		if !strings.EqualFold(s.State, "staged") || s.HeadRefName == "" {
			continue
		}
		if _, dup := prs[s.HeadRefName]; dup {
			// The newest first, as galley lists them: the first stays.
			continue
		}
		prs[s.HeadRefName] = &protocol.PullRequest{
			Number: s.Number, State: "open", Draft: s.IsDraft, Base: s.BaseRefName, Staged: true,
			URL: fmt.Sprintf("%s/%s/%s/pull/%d", base, owner, repo, s.Number),
		}
	}
	return prs, nil
}
