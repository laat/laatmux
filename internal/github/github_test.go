package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/laat/laatmux/internal/protocol"
)

// fake answers queries from a function of the variables, and records
// every call.
type fake struct {
	calls   []map[string]string
	queries []string
	answer  func(vars map[string]string) (string, error)
}

func (f *fake) run(ctx context.Context, host, query string, vars map[string]string) ([]byte, error) {
	f.calls = append(f.calls, vars)
	f.queries = append(f.queries, query)
	s, err := f.answer(vars)
	return []byte(s), err
}

func rollupJSON(id, state string, runs, statuses map[string]int) string {
	enc := func(m map[string]int) string {
		var parts []string
		for k, v := range m {
			parts = append(parts, fmt.Sprintf(`{"state":%q,"count":%d}`, k, v))
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	return fmt.Sprintf(`{"id":%q,"state":%q,"contexts":{"checkRunCountsByState":%s,"statusContextCountsByState":%s}}`, id, state, enc(runs), enc(statuses))
}

// Names travel as variables only: a branch named like GraphQL is never
// in the query text; branches go 32 to a query.
func TestQueryVariablesAndChunks(t *testing.T) {
	f := &fake{answer: func(vars map[string]string) (string, error) {
		data := map[string]json.RawMessage{}
		for k := range vars {
			if strings.HasPrefix(k, "b") {
				data[k] = json.RawMessage(`{"url":"https://github.com/o/r","ref":{"target":{"oid":"abc"}},"pullRequests":{"nodes":[]}}`)
			}
		}
		b, _ := json.Marshal(map[string]any{"data": data})
		return string(b), nil
	}}
	var bs []Branch
	for i := 0; i < 70; i++ {
		bs = append(bs, Branch{Owner: "o", Repo: "r", Branch: fmt.Sprintf(`x") { evil } #%d`, i)})
	}
	rs, err := Fetch(context.Background(), f.run, "github.com", bs)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 || len(f.calls[0]) != 4*Chunk || len(f.calls[2]) != 4*6 {
		t.Errorf("%d calls, sizes %d/%d", len(f.calls), len(f.calls[0]), len(f.calls[2]))
	}
	for _, q := range f.queries {
		if strings.Contains(q, "evil") {
			t.Fatal("a branch name in the query text")
		}
	}
	if len(rs) != 70 || rs[69].HeadOID != "abc" || rs[69].ChecksURL != "https://github.com/o/r/commit/abc/checks" {
		t.Errorf("results: %d, last %+v", len(rs), rs[69])
	}
}

// The PR: from the source's own repository only, an open one first,
// else the newest; its last commit's checks. A branch gone from GitHub
// with a merged PR keeps the PR; one with none has no ref.
func TestParse(t *testing.T) {
	pr := func(n int, state string, cross bool, oid, rl string) string {
		return fmt.Sprintf(`{"number":%d,"state":%q,"isDraft":false,"url":"https://github.com/o/r/pull/%d","isCrossRepository":%v,"commits":{"nodes":[{"commit":{"oid":%q,"statusCheckRollup":%s}}]}}`, n, state, n, cross, oid, rl)
	}
	ok := rollupJSON("R1", "SUCCESS", map[string]int{"SUCCESS": 2}, nil)
	body := func(ref string, prs ...string) string {
		var open []string
		for _, p := range prs {
			if strings.Contains(p, `"state":"OPEN"`) {
				open = append(open, p)
			}
		}
		return `{"data":{"b0":{"url":"https://github.com/o/r","ref":` + ref + `,"open":{"nodes":[` + strings.Join(open, ",") +
			`]},"pullRequests":{"nodes":[` + strings.Join(prs, ",") + `]}}}}`
	}
	ref := `{"target":{"oid":"own","statusCheckRollup":null}}`
	for _, c := range []struct {
		name, body string
		want       Result
	}{
		{"fork excluded, open first", body(ref, pr(9, "OPEN", true, "f", ok), pr(8, "MERGED", false, "m", ok), pr(7, "OPEN", false, "p", ok)),
			Result{HeadOID: "p", PR: &protocol.PullRequest{Number: 7, State: "open", URL: "https://github.com/o/r/pull/7"}}},
		{"newest closed, where the branch is", body(`{"target":{"oid":"c","statusCheckRollup":null}}`, pr(8, "CLOSED", false, "c", ok), pr(6, "MERGED", false, "m", ok)),
			Result{HeadOID: "c", PR: &protocol.PullRequest{Number: 8, State: "closed", URL: "https://github.com/o/r/pull/8"}}},
		{"a branch moved on from its merged PR", body(ref, pr(8, "MERGED", false, "old", ok)), Result{HeadOID: "own"}},
		{"deleted after merge", body("null", pr(5, "MERGED", false, "m", ok)),
			Result{HeadOID: "m", PR: &protocol.PullRequest{Number: 5, State: "merged", URL: "https://github.com/o/r/pull/5"}}},
		{"gone", body("null"), Result{NoRef: true}},
		{"only a fork's", body(ref, pr(9, "OPEN", true, "f", ok)), Result{HeadOID: "own"}},
	} {
		f := &fake{answer: func(map[string]string) (string, error) { return c.body, nil }}
		rs, err := Fetch(context.Background(), f.run, "github.com", []Branch{{Owner: "o", Repo: "r", Branch: "b"}})
		if err != nil {
			t.Fatal(err)
		}
		got := rs[0]
		if got.NoRef != c.want.NoRef || got.HeadOID != c.want.HeadOID || (got.PR == nil) != (c.want.PR == nil) || got.PR != nil && *got.PR != *c.want.PR {
			t.Errorf("%s: %+v pr %+v", c.name, got, got.PR)
		}
	}
}

// The counts come from the aggregates: failure beats pending; neutral,
// skipped and stale are left out of both; only those is success with 0.
func TestAggregate(t *testing.T) {
	for _, c := range []struct {
		runs, statuses map[string]int
		state          string
		passed, total  int
	}{
		{map[string]int{"SUCCESS": 3, "FAILURE": 1, "IN_PROGRESS": 1}, nil, protocol.ChecksFailure, 3, 5},
		{map[string]int{"SUCCESS": 3, "QUEUED": 2}, map[string]int{"EXPECTED": 1}, protocol.ChecksPending, 3, 6},
		{map[string]int{"SKIPPED": 2, "NEUTRAL": 1}, map[string]int{"SUCCESS": 1}, protocol.ChecksSuccess, 1, 1},
		{map[string]int{"SKIPPED": 2, "STALE": 1}, nil, protocol.ChecksSuccess, 0, 0},
		{nil, map[string]int{"ERROR": 1, "SUCCESS": 1}, protocol.ChecksFailure, 1, 2},
	} {
		var rl rollup
		if err := json.Unmarshal([]byte(rollupJSON("x", "", c.runs, c.statuses)), &rl); err != nil {
			t.Fatal(err)
		}
		got := aggregate(&rl)
		if got.State != c.state || got.Passed != c.passed || got.Total != c.total {
			t.Errorf("%v %v: %+v", c.runs, c.statuses, got)
		}
	}
}

// The failing check's name is paged for, past the first page of
// contexts.
func TestFailingName(t *testing.T) {
	failing := rollupJSON("RID", "FAILURE", map[string]int{"FAILURE": 1, "SUCCESS": 150}, nil)
	f := &fake{answer: func(vars map[string]string) (string, error) {
		if _, ok := vars["id"]; !ok {
			return `{"data":{"b0":{"url":"u","ref":{"target":{"oid":"h","statusCheckRollup":` + failing + `}},"pullRequests":{"nodes":[]}}}}`, nil
		}
		if vars["after"] == "" {
			return `{"data":{"node":{"contexts":{"pageInfo":{"hasNextPage":true,"endCursor":"C1"},"nodes":[{"name":"lint","conclusion":"SUCCESS"}]}}}}`, nil
		}
		return `{"data":{"node":{"contexts":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"name":"test (ubuntu)","conclusion":"FAILURE"}]}}}}`, nil
	}}
	rs, err := Fetch(context.Background(), f.run, "github.com", []Branch{{Owner: "o", Repo: "r", Branch: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	FillFailing(context.Background(), f.run, "github.com", rs, nil)
	if rs[0].Checks == nil || rs[0].Checks.Failing != "test (ubuntu)" || rs[0].Checks.State != protocol.ChecksFailure {
		t.Errorf("%+v", rs[0].Checks)
	}
}

// gh missing or logged out is the error returned; a chunk failing for
// another reason is its branches' error alone.
func TestFetchErrors(t *testing.T) {
	for _, want := range []error{ErrNoGH, fmt.Errorf("%w to github.com", ErrLoggedOut)} {
		f := &fake{answer: func(map[string]string) (string, error) { return "", want }}
		rs, err := Fetch(context.Background(), f.run, "github.com", []Branch{{Owner: "o", Repo: "r", Branch: "b"}})
		if err == nil || rs[0].Err == nil {
			t.Errorf("%v: %v %+v", want, err, rs)
		}
	}
	f := &fake{answer: func(map[string]string) (string, error) { return `{"errors":[{"message":"rate limited"}]}`, nil }}
	rs, err := Fetch(context.Background(), f.run, "github.com", []Branch{{Owner: "o", Repo: "r", Branch: "b"}})
	if err != nil || rs[0].Err == nil || !strings.Contains(rs[0].Err.Error(), "rate limited") {
		t.Errorf("a failed chunk: %v %+v", err, rs)
	}
}

// An error under a branch's alias makes its answer partial: a null ref
// there is not taken for a branch gone, and the other branches stand.
func TestPartialErrors(t *testing.T) {
	f := &fake{answer: func(map[string]string) (string, error) {
		return `{"data":{"b0":{"url":"u","ref":null,"open":{"nodes":[]},"pullRequests":{"nodes":[]}},` +
			`"b1":{"url":"u","ref":{"target":{"oid":"h"}},"open":{"nodes":[]},"pullRequests":{"nodes":[]}}},` +
			`"errors":[{"message":"Something went wrong","path":["b0","ref"]},{"message":"x","path":["b9","pullRequests","nodes",0]}]}`, nil
	}}
	rs, err := Fetch(context.Background(), f.run, "github.com", []Branch{{Owner: "o", Repo: "r", Branch: "a"}, {Owner: "o", Repo: "r", Branch: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Err == nil || rs[0].NoRef {
		t.Errorf("partial answer: %+v", rs[0])
	}
	if rs[1].Err != nil || rs[1].HeadOID != "h" {
		t.Errorf("the other branch: %+v", rs[1])
	}
}

// A failing name known for the rollup is not paged for again.
func TestFailingNameKnown(t *testing.T) {
	failing := rollupJSON("RID", "FAILURE", map[string]int{"FAILURE": 1}, nil)
	f := &fake{answer: func(vars map[string]string) (string, error) {
		if _, ok := vars["id"]; ok {
			t.Error("paged for a known name")
		}
		return `{"data":{"b0":{"url":"u","ref":{"target":{"oid":"h","statusCheckRollup":` + failing + `}},"open":{"nodes":[]},"pullRequests":{"nodes":[]}}}}`, nil
	}}
	rs, err := Fetch(context.Background(), f.run, "github.com", []Branch{{Owner: "o", Repo: "r", Branch: "b"}})
	FillFailing(context.Background(), f.run, "github.com", rs, map[string]string{FailingKey("RID", &protocol.Checks{Passed: 0, Total: 1}): "lint"})
	if err != nil || rs[0].Checks.Failing != "lint" {
		t.Errorf("%+v %v", rs[0].Checks, err)
	}
}

// Forks' open PRs filling the first page do not hide the repository's
// own: the next pages are asked for.
func TestOwnPRPastForks(t *testing.T) {
	fork := `{"number":9,"state":"OPEN","isDraft":false,"url":"u9","isCrossRepository":true,"commits":{"nodes":[]}}`
	own := `{"number":3,"state":"OPEN","isDraft":false,"url":"u3","isCrossRepository":false,"commits":{"nodes":[{"commit":{"oid":"p","statusCheckRollup":null}}]}}`
	f := &fake{answer: func(vars map[string]string) (string, error) {
		if vars["after"] == "C1" {
			return `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[` + own + `]}}}}`, nil
		}
		forks := strings.Repeat(fork+",", 4) + fork
		return `{"data":{"b0":{"url":"u","ref":{"target":{"oid":"p"}},"open":{"pageInfo":{"hasNextPage":true,"endCursor":"C1"},"nodes":[` + forks +
			`]},"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[` + forks + `]}}}}`, nil
	}}
	rs, err := Fetch(context.Background(), f.run, "github.com", []Branch{{Owner: "o", Repo: "r", Branch: "b"}})
	if err != nil || rs[0].PR == nil || rs[0].PR.Number != 3 {
		t.Errorf("%+v %v", rs[0], err)
	}
}
