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
	"sort"
	"strconv"
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
	if bytes.Contains(out.Bytes(), []byte(`"data"`)) {
		return out.Bytes(), nil
	}
	msg := strings.TrimSpace(errb.String())
	var ee *exec.ExitError
	// gh exits 4 when it has no credentials; a bad or expired token is
	// GitHub's 401.
	if errors.As(err, &ee) && ee.ExitCode() == 4 || strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "Bad credentials") ||
		strings.Contains(msg, "gh auth login") || strings.Contains(msg, "not logged") {
		return nil, fmt.Errorf("%w to %s", ErrLoggedOut, host)
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
	// NoPaging is that a recent round paged past the forks' PRs and
	// found none of the repository's own: the pages are not read again
	// for a while, since on a crowded name, patch-1 say, they cost every
	// round and never change.
	NoPaging bool
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
	// RollupID identifies the head's check rollup, which with counts,
	// every count by state, a failing check's name is kept by.
	RollupID string
	counts   string
	// PagedNone is that the forks' PRs were paged past and none of the
	// repository's own was found.
	PagedNone bool
}

// Chunk is how many branches one query asks about.
const Chunk = 32

const prFragment = `fragment P on PullRequestConnection { pageInfo { hasNextPage endCursor } nodes { ...Q } } ` + prNodeFragment

const prNodeFragment = `fragment Q on PullRequest { number state isDraft url isCrossRepository commits(last: 1) { nodes { commit { oid statusCheckRollup { ...R } } } } }`

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
		fmt.Fprintf(&body, ` b%d: repository(owner: $o%d, name: $r%d) { url defaultBranchRef { name } `+
			`ref(qualifiedName: $q%d) { target { oid ... on Commit { statusCheckRollup { ...R } } } } `+
			`open: pullRequests(headRefName: $b%d, states: [OPEN], first: 5, orderBy: {field: CREATED_AT, direction: DESC}) { ...P } `+
			`pullRequests(headRefName: $b%d, first: 5, orderBy: {field: CREATED_AT, direction: DESC}) { ...P } }`, i, i, i, i, i, i)
	}
	return "query(" + decl.String() + ") {" + body.String() + " } " + rollupFragment + " " + prFragment
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

type prNode struct {
	Number          int    `json:"number"`
	State           string `json:"state"`
	IsDraft         bool   `json:"isDraft"`
	URL             string `json:"url"`
	CrossRepository bool   `json:"isCrossRepository"`
	Commits         struct {
		Nodes []struct {
			Commit struct {
				OID    string  `json:"oid"`
				Rollup *rollup `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

type prConnection struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []prNode `json:"nodes"`
}

// own reports whether a connection holds a PR of the repository's own.
func (c prConnection) own() bool {
	for _, n := range c.Nodes {
		if !n.CrossRepository {
			return true
		}
	}
	return false
}

type repoAnswer struct {
	URL              string `json:"url"`
	DefaultBranchRef *struct {
		Name string `json:"name"`
	} `json:"defaultBranchRef"`
	Ref *struct {
		Target struct {
			OID    string  `json:"oid"`
			Rollup *rollup `json:"statusCheckRollup"`
		} `json:"target"`
	} `json:"ref"`
	Open         prConnection `json:"open"`
	PullRequests prConnection `json:"pullRequests"`
}

// Fetch asks host about branches, Chunk at a time, then for the name of
// the first failing check of each rollup that fails. A chunk that fails
// sets Err on its branches; the error returned is the runner's own when
// every chunk failed with it, ErrNoGH or ErrLoggedOut say.
//
// The failing checks' names are FillFailing's, asked for after every
// host's status, so a slow host's names hold no other host's status.
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
	if chunks > 0 && failed == chunks && (errors.Is(last, ErrNoGH) || errors.Is(last, ErrLoggedOut)) {
		return results, last
	}
	return results, nil
}

// FailingKey is what a failing check's name is kept by: the rollup and
// every count by state, so a rerun, pending or done, changes the key.
func (r Result) FailingKey() string { return r.RollupID + " " + r.counts }

// FillFailing gives each failing result the name of its first failing
// check: the one in known for its rollup id, found recently, else paged
// for.
func FillFailing(ctx context.Context, run Runner, host string, results []Result, known map[string]string) {
	for i := range results {
		r := &results[i]
		if r.Err != nil || r.Checks == nil || r.Checks.State != protocol.ChecksFailure || r.RollupID == "" {
			continue
		}
		// A known name is filled in whatever the time; only a lookup
		// waits for it.
		if name, ok := known[r.FailingKey()]; ok {
			r.Checks.Failing = name
		} else if ctx.Err() == nil {
			r.Checks.Failing = failingName(ctx, run, host, r.RollupID)
		}
	}
}

// prPages bounds the paging for a branch's own PR past the first page.
const prPages = 5

// morePRs pages a branch's PRs past the first answer's, open ones or
// all, with the number and the fork mark alone, until one of the
// repository's own is found; then asks for that one in full. On a
// repository whose main branch has hundreds of forks' PRs of the same
// name this is a few light pages, not full ones.
func morePRs(ctx context.Context, run Runner, host string, b Branch, open bool, after string) ([]prNode, error) {
	states := ""
	if open {
		states = "states: [OPEN], "
	}
	q := `query($o: String!, $r: String!, $b: String!, $after: String!) { repository(owner: $o, name: $r) { ` +
		`pullRequests(headRefName: $b, ` + states + `first: 100, after: $after, orderBy: {field: CREATED_AT, direction: DESC}) { ` +
		`pageInfo { hasNextPage endCursor } nodes { number isCrossRepository } } } }`
	vars := map[string]string{"o": b.Owner, "r": b.Repo, "b": b.Branch}
	for page := 0; page < prPages && after != ""; page++ {
		vars["after"] = after
		var resp struct {
			Data struct {
				Repository *struct {
					PullRequests struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							Number          int  `json:"number"`
							CrossRepository bool `json:"isCrossRepository"`
						} `json:"nodes"`
					} `json:"pullRequests"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := graphql(ctx, run, host, q, vars, &resp, b); err != nil {
			return nil, err
		}
		c := resp.Data.Repository.PullRequests
		for _, n := range c.Nodes {
			if !n.CrossRepository {
				return onePR(ctx, run, host, b, n.Number)
			}
		}
		if !c.PageInfo.HasNextPage {
			return nil, nil
		}
		after = c.PageInfo.EndCursor
	}
	return nil, nil
}

// onePR asks for one PR in full by its number, which is GitHub's own
// answer, not a name of the user's, and so is put in the query text.
func onePR(ctx context.Context, run Runner, host string, b Branch, number int) ([]prNode, error) {
	q := `query($o: String!, $r: String!) { repository(owner: $o, name: $r) { pullRequest(number: ` + strconv.Itoa(number) + `) { ...Q } } } ` +
		rollupFragment + " " + prNodeFragment
	var resp struct {
		Data struct {
			Repository *struct {
				PullRequest *prNode `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := graphql(ctx, run, host, q, map[string]string{"o": b.Owner, "r": b.Repo}, &resp, b); err != nil {
		return nil, err
	}
	if resp.Data.Repository.PullRequest == nil {
		return nil, fmt.Errorf("%s/%s: pull request %d not found", b.Owner, b.Repo, number)
	}
	return []prNode{*resp.Data.Repository.PullRequest}, nil
}

// graphql runs one follow-up query and decodes its data into v; an
// error in the answer, or no repository, is an error.
func graphql(ctx context.Context, run Runner, host, q string, vars map[string]string, v any, b Branch) error {
	body, err := run(ctx, host, q, vars)
	if err != nil {
		return err
	}
	var check struct {
		Data struct {
			Repository json.RawMessage `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &check); err != nil {
		return fmt.Errorf("gh api graphql: %w", err)
	}
	if len(check.Errors) > 0 {
		return fmt.Errorf("%s/%s: %s", b.Owner, b.Repo, check.Errors[0].Message)
	}
	if len(check.Data.Repository) == 0 || string(check.Data.Repository) == "null" {
		return fmt.Errorf("%s/%s: no repository", b.Owner, b.Repo)
	}
	return json.Unmarshal(body, v)
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
			Message string `json:"message"`
			Path    []any  `json:"path"`
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
	// An error names the alias it is under: that branch's answer is
	// partial, a resolver that failed reads as null, and is not taken
	// for what it seems, no ref say.
	partial := map[string]string{}
	for _, e := range resp.Errors {
		if len(e.Path) > 0 {
			if alias, ok := e.Path[0].(string); ok {
				partial[alias] = e.Message
			}
		}
	}
	for i, b := range branches {
		alias := fmt.Sprintf("b%d", i)
		if msg, ok := partial[alias]; ok {
			out[i] = Result{Err: fmt.Errorf("%s/%s: %s", b.Owner, b.Repo, msg)}
			continue
		}
		a := resp.Data[alias]
		if a != nil {
			// Forks' PRs of the same name may fill a page: the next
			// pages are asked for until one of the repository's own. A
			// page that fails leaves the answer partial, not absent.
			// Only what can change the answer is paged: an open PR while
			// the branch is there; any PR when no own one is in sight,
			// a closed one counting once the branch is gone or while it
			// is at that PR's last commit. Not on the repository's
			// default branch, or main or master, whose PR the views
			// never show and whose forks' PRs are many; and not again
			// for a while once the pages held none of the repository's
			// own.
			var err error
			mainline := b.Branch == "main" || b.Branch == "master" || a.DefaultBranchRef != nil && a.DefaultBranchRef.Name == b.Branch
			paged := false
			if !b.NoPaging && a.Ref != nil && !a.Open.own() && a.Open.PageInfo.HasNextPage {
				var more []prNode
				more, err = morePRs(ctx, run, host, b, true, a.Open.PageInfo.EndCursor)
				a.Open.Nodes = append(a.Open.Nodes, more...)
				paged = true
			}
			if err == nil && !b.NoPaging && !mainline && !a.Open.own() && !a.PullRequests.own() && a.PullRequests.PageInfo.HasNextPage {
				var more []prNode
				more, err = morePRs(ctx, run, host, b, false, a.PullRequests.PageInfo.EndCursor)
				a.PullRequests.Nodes = append(a.PullRequests.Nodes, more...)
				paged = true
			}
			if err != nil {
				out[i] = Result{Err: err}
				continue
			}
			out[i] = parse(a, b)
			out[i].PagedNone = paged && !a.Open.own() && !a.PullRequests.own()
			continue
		}
		out[i] = parse(a, b)
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
	// The open ones are asked for apart, so newer closed ones or forks'
	// do not hide an open one; else the newest of the repository's own,
	// a fork's being cross-repository, whatever the names after a
	// rename.
	pick := -1
	nodes := append(append(a.Open.Nodes[:0:0], a.Open.Nodes...), a.PullRequests.Nodes...)
	for i, pr := range nodes {
		if !pr.CrossRepository {
			pick = i
			break
		}
	}
	if pick >= 0 {
		pr := nodes[pick]
		var last string
		var lastRollup *rollup
		if n := pr.Commits.Nodes; len(n) > 0 {
			last, lastRollup = n[0].Commit.OID, n[0].Commit.Rollup
		}
		// A merged or closed PR is the branch's only while the branch
		// is where the PR left it, or gone; a branch that moved on,
		// main after an old main-to-release PR say, is its own.
		if pr.State == "OPEN" || a.Ref == nil || last == r.HeadOID {
			r.PR = &protocol.PullRequest{Number: pr.Number, State: strings.ToLower(pr.State), Draft: pr.IsDraft, URL: pr.URL}
			r.ChecksURL = pr.URL + "/checks"
			if last != "" {
				r.HeadOID, rl = last, lastRollup
			}
		}
	}
	switch {
	case r.PR != nil:
	case a.Ref == nil:
		// No branch and no PR of it: gone from GitHub. A branch deleted
		// after its PR merged keeps the PR.
		return Result{NoRef: true}
	case a.URL != "" && r.HeadOID != "":
		r.ChecksURL = a.URL + "/commit/" + r.HeadOID + "/checks"
	}
	if rl != nil {
		r.Checks = aggregate(rl)
		r.RollupID = rl.ID
		var sig []string
		for _, list := range []counts{rl.Contexts.CheckRuns, rl.Contexts.Statuses} {
			for _, c := range list {
				sig = append(sig, fmt.Sprintf("%s=%d", c.State, c.Count))
			}
		}
		sort.Strings(sig)
		r.counts = strings.Join(sig, ",")
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
