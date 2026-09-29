// Package github reads the PR and check state of pushed branches from
// GitHub through the gh CLI on this machine: batched GraphQL queries with
// every name passed as a variable, never put into the query text. See
// milestone five's note, PR and checks, on the laptop.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/protocol"
)

// Runner runs one GraphQL query against a GitHub host with string
// variables and returns the response body. GH is the real one; tests
// stand in for it.
type Runner func(ctx context.Context, host, query string, vars map[string]string) ([]byte, error)

// Errors a runner reports for gh itself, which the daemon shows as the
// reason it has no PR state rather than as a failed query.
var (
	ErrNoGH      = errors.New("gh is not installed")
	ErrLoggedOut = errors.New("gh is not logged in")
)

// Timeout bounds one gh call.
const Timeout = 30 * time.Second

// GH runs `gh api graphql` against host. A body with data is returned
// even when gh exits non-zero, since GitHub answers what it can beside
// the errors of one branch.
func GH(ctx context.Context, host, query string, vars map[string]string) ([]byte, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, ErrNoGH
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	args := []string{"api", "graphql", "--hostname", host, "-f", "query=" + query}
	for k, v := range vars {
		args = append(args, "-f", k+"="+v)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1", "NO_COLOR=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), nil
	}
	msg := strings.TrimSpace(errb.String())
	if strings.Contains(msg, "gh auth login") || strings.Contains(msg, "not logged") || strings.Contains(msg, "authentication") {
		return nil, fmt.Errorf("%w to %s", ErrLoggedOut, host)
	}
	if bytes.Contains(out.Bytes(), []byte(`"data"`)) {
		return out.Bytes(), nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if msg == "" {
		msg = err.Error()
	}
	return nil, fmt.Errorf("gh api graphql: %s", msg)
}

// Branch is one pushed branch to ask about.
type Branch struct {
	Owner, Repo, Branch string
}

// Result is what GitHub says of one branch. Err is set when the answer
// had nothing for it, a repository not found say; NoRef when the branch
// does not exist there.
type Result struct {
	Err       error
	NoRef     bool
	HeadOID   string
	ChecksURL string
	PR        *protocol.PullRequest
	Checks    *protocol.Checks
	rollupID  string
}

// Chunk is how many branches one query asks about.
const Chunk = 32

const rollupFragment = `fragment R on StatusCheckRollup { id state contexts(first: 1) { checkRunCountsByState { state count } statusContextCountsByState { state count } } }`

// query is the GraphQL text for n branches: aliases and variable names
// only, b0..bN-1.
func query(n int) string {
	var decl, body strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			decl.WriteString(", ")
		}
		fmt.Fprintf(&decl, "$o%d: String!, $r%d: String!, $q%d: String!, $b%d: String!", i, i, i, i)
		fmt.Fprintf(&body, ` b%d: repository(owner: $o%d, name: $r%d) { url `+
			`ref(qualifiedName: $q%d) { target { oid ... on Commit { statusCheckRollup { ...R } } } } `+
			`pullRequests(headRefName: $b%d, first: 20, orderBy: {field: CREATED_AT, direction: DESC}) { nodes { `+
			`number state isDraft url headRepository { nameWithOwner } `+
			`commits(last: 1) { nodes { commit { oid statusCheckRollup { ...R } } } } } } }`, i, i, i, i, i)
	}
	return "query(" + decl.String() + ") {" + body.String() + " } " + rollupFragment
}

type counts []struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

type rollup struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Contexts struct {
		CheckRuns counts `json:"checkRunCountsByState"`
		Statuses  counts `json:"statusContextCountsByState"`
	} `json:"contexts"`
}

type repoAnswer struct {
	URL string `json:"url"`
	Ref *struct {
		Target struct {
			OID    string  `json:"oid"`
			Rollup *rollup `json:"statusCheckRollup"`
		} `json:"target"`
	} `json:"ref"`
	PullRequests struct {
		Nodes []struct {
			Number   int    `json:"number"`
			State    string `json:"state"`
			IsDraft  bool   `json:"isDraft"`
			URL      string `json:"url"`
			HeadRepo *struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"headRepository"`
			Commits struct {
				Nodes []struct {
					Commit struct {
						OID    string  `json:"oid"`
						Rollup *rollup `json:"statusCheckRollup"`
					} `json:"commit"`
				} `json:"nodes"`
			} `json:"commits"`
		} `json:"nodes"`
	} `json:"pullRequests"`
}

// Fetch asks host about branches, Chunk at a time, then for the name of
// the first failing check of each rollup that fails. A chunk that fails
// sets Err on its branches; the error returned is the runner's own when
// every chunk failed with it, ErrNoGH or ErrLoggedOut say.
func Fetch(ctx context.Context, run Runner, host string, branches []Branch) ([]Result, error) {
	results := make([]Result, len(branches))
	var last error
	failed := 0
	chunks := 0
	for start := 0; start < len(branches); start += Chunk {
		end := min(start+Chunk, len(branches))
		chunks++
		if err := fetchChunk(ctx, run, host, branches[start:end], results[start:end]); err != nil {
			for i := start; i < end; i++ {
				results[i] = Result{Err: err}
			}
			last = err
			failed++
		}
	}
	for i := range results {
		r := &results[i]
		if r.Err == nil && r.Checks != nil && r.Checks.State == protocol.ChecksFailure && r.rollupID != "" {
			r.Checks.Failing = failingName(ctx, run, host, r.rollupID)
		}
	}
	if chunks > 0 && failed == chunks && (errors.Is(last, ErrNoGH) || errors.Is(last, ErrLoggedOut)) {
		return results, last
	}
	return results, nil
}

func fetchChunk(ctx context.Context, run Runner, host string, branches []Branch, out []Result) error {
	vars := map[string]string{}
	for i, b := range branches {
		vars[fmt.Sprintf("o%d", i)] = b.Owner
		vars[fmt.Sprintf("r%d", i)] = b.Repo
		vars[fmt.Sprintf("q%d", i)] = "refs/heads/" + b.Branch
		vars[fmt.Sprintf("b%d", i)] = b.Branch
	}
	body, err := run(ctx, host, query(len(branches)), vars)
	if err != nil {
		return err
	}
	var resp struct {
		Data   map[string]*repoAnswer `json:"data"`
		Errors []struct {
			Message string   `json:"message"`
			Path    []string `json:"path"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("gh api graphql: %w", err)
	}
	if resp.Data == nil {
		msg := "no data"
		if len(resp.Errors) > 0 {
			msg = resp.Errors[0].Message
		}
		return fmt.Errorf("gh api graphql: %s", msg)
	}
	for i, b := range branches {
		out[i] = parse(resp.Data[fmt.Sprintf("b%d", i)], b)
	}
	return nil
}

// parse is one branch's answer: the PR from the source's own repository,
// an open one first, else the newest; its last commit's checks, or the
// branch's own when it has no PR.
func parse(a *repoAnswer, b Branch) Result {
	if a == nil {
		return Result{Err: fmt.Errorf("%s/%s: no answer", b.Owner, b.Repo)}
	}
	var r Result
	var rl *rollup
	if a.Ref != nil {
		r.HeadOID, rl = a.Ref.Target.OID, a.Ref.Target.Rollup
	}
	own := strings.ToLower(b.Owner + "/" + b.Repo)
	pick := -1
	for i, pr := range a.PullRequests.Nodes {
		if pr.HeadRepo == nil || strings.ToLower(pr.HeadRepo.NameWithOwner) != own {
			continue
		}
		if pr.State == "OPEN" {
			pick = i
			break
		}
		if pick < 0 {
			pick = i
		}
	}
	if pick >= 0 {
		pr := a.PullRequests.Nodes[pick]
		r.PR = &protocol.PullRequest{Number: pr.Number, State: strings.ToLower(pr.State), Draft: pr.IsDraft, URL: pr.URL}
		r.ChecksURL = pr.URL + "/checks"
		if n := pr.Commits.Nodes; len(n) > 0 {
			r.HeadOID, rl = n[0].Commit.OID, n[0].Commit.Rollup
		}
	} else if a.Ref == nil {
		// No branch and no PR of it: gone from GitHub. A branch deleted
		// after its PR merged keeps the PR.
		return Result{NoRef: true}
	} else if a.URL != "" && r.HeadOID != "" {
		r.ChecksURL = a.URL + "/commit/" + r.HeadOID + "/checks"
	}
	if rl != nil {
		r.Checks = aggregate(rl)
		r.rollupID = rl.ID
	}
	return r
}

// The rollup's context states, by what they count as.
var (
	failing = map[string]bool{"FAILURE": true, "CANCELLED": true, "TIMED_OUT": true, "STARTUP_FAILURE": true, "ACTION_REQUIRED": true, "ERROR": true}
	pending = map[string]bool{"IN_PROGRESS": true, "QUEUED": true, "PENDING": true, "REQUESTED": true, "WAITING": true, "EXPECTED": true}
	passing = map[string]bool{"SUCCESS": true, "COMPLETED": true}
)

// aggregate counts a rollup from its aggregates, which count every
// context whatever the page. Failure beats pending; neutral, skipped and
// stale contexts count for neither passed nor total, so a rollup of only
// those is success with a total of 0.
func aggregate(rl *rollup) *protocol.Checks {
	c := &protocol.Checks{State: protocol.ChecksSuccess}
	var failed, waiting int
	for _, list := range []counts{rl.Contexts.CheckRuns, rl.Contexts.Statuses} {
		for _, s := range list {
			switch {
			case failing[s.State]:
				failed += s.Count
			case pending[s.State]:
				waiting += s.Count
			case passing[s.State]:
				c.Passed += s.Count
			default:
				continue
			}
			c.Total += s.Count
		}
	}
	switch {
	case failed > 0:
		c.State = protocol.ChecksFailure
	case waiting > 0:
		c.State = protocol.ChecksPending
	}
	return c
}

const failingPages = 10

// failingName pages the rollup's contexts until a failing one is found,
// and returns its name; "" when none is found or a page fails.
func failingName(ctx context.Context, run Runner, host, id string) string {
	after := ""
	for page := 0; page < failingPages; page++ {
		q := `query($id: ID!) { node(id: $id) { ... on StatusCheckRollup { contexts(first: 100) { ` + contextsSelection + ` } } } }`
		vars := map[string]string{"id": id}
		if after != "" {
			q = `query($id: ID!, $after: String!) { node(id: $id) { ... on StatusCheckRollup { contexts(first: 100, after: $after) { ` + contextsSelection + ` } } } }`
			vars["after"] = after
		}
		body, err := run(ctx, host, q, vars)
		if err != nil {
			return ""
		}
		var resp struct {
			Data struct {
				Node struct {
					Contexts struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							Name       string `json:"name"`
							Conclusion string `json:"conclusion"`
							Context    string `json:"context"`
							State      string `json:"state"`
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"node"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &resp) != nil {
			return ""
		}
		cs := resp.Data.Node.Contexts
		for _, n := range cs.Nodes {
			if n.Name != "" && failing[n.Conclusion] {
				return n.Name
			}
			if n.Context != "" && failing[n.State] {
				return n.Context
			}
		}
		if !cs.PageInfo.HasNextPage || cs.PageInfo.EndCursor == "" {
			return ""
		}
		after = cs.PageInfo.EndCursor
	}
	return ""
}

const contextsSelection = `pageInfo { hasNextPage endCursor } nodes { ... on CheckRun { name conclusion } ... on StatusContext { context state } }`
