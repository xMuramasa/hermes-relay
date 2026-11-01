package server_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/github"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

const (
	ghOwner   = "xMuramasa"
	ghProject = "PVT_test"
)

type ghIssue struct {
	repo, title, body, state string
	number                   int
	board                    bool
	status, priority         string
	comments                 []string
}

func (is *ghIssue) id() string { return "I_" + is.repo + "_" + strconv.Itoa(is.number) }

// fakeGitHub answers the relay's fixed GraphQL queries from a few issues, like GitHub would.
type fakeGitHub struct {
	*httptest.Server
	t      *testing.T
	token  string
	mu     sync.Mutex
	issues []*ghIssue
	calls  map[string]int
	fail   func(w http.ResponseWriter) bool // answers instead of the fake when it returns true
}

var ghOptions = map[string]map[string]string{ // field id → option id → name
	"F_status": {"O_todo": "Todo", "O_doing": "In Progress", "O_done": "Done"},
	"F_prio":   {"O_p0": "P0", "O_p1": "P1", "O_p2": "P2"},
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, token: secret("ghp_fake_github_token_0123456789", "GitHub token"), calls: map[string]int{},
		issues: []*ghIssue{
			{repo: "trismegistus", number: 21, title: "GitHub endpoints", body: secret("issue body: the secret plan", "issue body"),
				state: "OPEN", board: true, status: "In Progress", priority: "P1", comments: []string{"first comment"}},
			{repo: "trismegistus", number: 22, title: "Not on the board", body: "b", state: "OPEN"},
			{repo: "trismegistus", number: 5, title: "Old", body: "b", state: "CLOSED", board: true, status: "Done"},
			{repo: "homelab", number: 3, title: "Caddy", body: "b", state: "OPEN", board: true, status: "Todo"},
		}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHub) find(repo string, number int) *ghIssue {
	for _, is := range f.issues {
		if is.repo == repo && is.number == number {
			return is
		}
	}
	return nil
}

func value(name string) any {
	if name == "" {
		return nil
	}
	return map[string]string{"name": name}
}

func (f *fakeGitHub) content(is *ghIssue) map[string]any {
	return map[string]any{"number": is.number, "title": is.title, "state": is.state, "updatedAt": "2026-10-31T10:00:00Z",
		"url":      "https://github.com/" + ghOwner + "/" + is.repo + "/issues/" + strconv.Itoa(is.number),
		"comments": map[string]int{"totalCount": len(is.comments)}, "labels": map[string]any{"nodes": []any{map[string]string{"name": "relay"}}},
		"repository": map[string]any{"name": is.repo, "owner": map[string]string{"login": ghOwner}}}
}

func fieldNodes() []any {
	nodes := []any{map[string]any{}} // a text field: not single-select, so GitHub sends {}
	for _, fid := range []string{"F_status", "F_prio"} {
		var opts []any
		for _, oid := range slices.Sorted(maps.Keys(ghOptions[fid])) {
			opts = append(opts, map[string]string{"id": oid, "name": ghOptions[fid][oid]})
		}
		name := map[string]string{"F_status": "Status", "F_prio": "Priority"}[fid]
		nodes = append(nodes, map[string]any{"id": fid, "name": name, "options": opts})
	}
	return nodes
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.fail != nil && f.fail(w) {
		return
	}
	var req struct {
		Query     string
		Variables map[string]any
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("bad request body: %v", err)
	}
	q, v := req.Query, req.Variables
	if o, ok := v["owner"]; ok && o != ghOwner {
		f.t.Errorf("query for owner %v", o)
	}
	if p, ok := v["project"]; ok && p != ghProject {
		f.t.Errorf("query for project %v", p)
	}
	byID := func() *ghIssue {
		for _, is := range f.issues {
			if is.id() == v["id"] {
				return is
			}
		}
		f.t.Errorf("unknown issue id %v", v["id"])
		return &ghIssue{}
	}
	notFound := map[string]any{"data": nil, "errors": []any{map[string]string{"type": "NOT_FOUND", "message": "Could not resolve"}}}
	var out any
	switch {
	case strings.Contains(q, "addComment("):
		f.calls["addComment"]++
		is := byID()
		is.comments = append(is.comments, v["body"].(string))
		out = map[string]any{"addComment": map[string]any{"commentEdge": map[string]any{"node": map[string]any{
			"author": map[string]string{"login": ghOwner}, "body": v["body"], "createdAt": "2026-11-01T12:00:00Z", "url": "https://github.com/c"}}}}
	case strings.Contains(q, "closeIssue("), strings.Contains(q, "reopenIssue("):
		f.calls["state"]++
		is := byID()
		is.state = map[bool]string{true: "CLOSED", false: "OPEN"}[strings.Contains(q, "closeIssue(")]
		out = map[string]any{"x": map[string]any{"issue": map[string]string{"state": is.state}}}
	case strings.Contains(q, "addProjectV2ItemById("):
		f.calls["addItem"]++
		is := byID()
		is.board = true
		out = map[string]any{"addProjectV2ItemById": map[string]any{"item": map[string]string{"id": "PVTI_" + is.id()}}}
	case strings.Contains(q, "updateProjectV2ItemFieldValue("):
		f.calls["setField"]++
		item, _ := v["item"].(string)
		var is *ghIssue
		for _, c := range f.issues {
			if "PVTI_"+c.id() == item && c.board {
				is = c
			}
		}
		opt, ok := ghOptions[v["field"].(string)][v["option"].(string)]
		if is == nil || !ok {
			_ = json.NewEncoder(w).Encode(notFound)
			return
		}
		if v["field"] == "F_status" {
			is.status = opt
		} else {
			is.priority = opt
		}
		out = map[string]any{"updateProjectV2ItemFieldValue": map[string]any{"projectV2Item": map[string]string{"id": item}}}
	case strings.Contains(q, "items(first"):
		f.calls["board"]++
		var nodes []any
		for _, is := range f.issues {
			if is.board {
				nodes = append(nodes, map[string]any{"status": value(is.status), "priority": value(is.priority), "content": f.content(is)})
			}
		}
		// Another owner's issue and a draft: both left out by the relay.
		foreign := f.content(&ghIssue{repo: "elsewhere", number: 1, title: "foreign", state: "OPEN"})
		foreign["repository"] = map[string]any{"name": "elsewhere", "owner": map[string]string{"login": "someone-else"}}
		nodes = append(nodes, map[string]any{"status": value("Todo"), "content": foreign}, map[string]any{"content": map[string]any{}})
		page, next := nodes[:2], true // two pages: the relay follows the cursor
		if v["cursor"] == "c1" {
			page, next = nodes[2:], false
		}
		out = map[string]any{"node": map[string]any{"fields": map[string]any{"nodes": fieldNodes()}, "items": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": next, "endCursor": "c1"}, "nodes": page}}}
	case strings.Contains(q, "fields(first"):
		f.calls["fields"]++
		out = map[string]any{"node": map[string]any{"id": ghProject, "fields": map[string]any{"nodes": fieldNodes()}}}
	case strings.Contains(q, "issues(first"):
		f.calls["issues"]++
		var nodes []any
		for _, is := range f.issues {
			if is.repo == v["repo"] && is.state == v["states"].([]any)[0] {
				nodes = append(nodes, f.content(is))
			}
		}
		if nodes == nil {
			_ = json.NewEncoder(w).Encode(notFound)
			return
		}
		out = map[string]any{"repository": map[string]any{"issues": map[string]any{"totalCount": len(nodes), "nodes": nodes}}}
	case strings.Contains(q, "issue(number"):
		f.calls["issue"]++
		is := f.find(v["repo"].(string), int(v["number"].(float64)))
		if is == nil {
			_ = json.NewEncoder(w).Encode(notFound)
			return
		}
		c := f.content(is)
		c["id"], c["body"], c["createdAt"], c["author"] = is.id(), is.body, "2026-10-30T09:00:00Z", nil
		var comments []any
		for _, b := range is.comments {
			comments = append(comments, map[string]any{"author": map[string]string{"login": "hermes"}, "body": b,
				"createdAt": "2026-10-31T11:00:00Z", "url": "https://github.com/c"})
		}
		c["comments"] = map[string]any{"totalCount": len(comments), "nodes": comments}
		items := []any{map[string]any{"id": "PVTI_other", "project": map[string]string{"id": "PVT_other"}, "status": value("Done")}}
		if is.board {
			items = append(items, map[string]any{"id": "PVTI_" + is.id(), "project": map[string]string{"id": ghProject},
				"status": value(is.status), "priority": value(is.priority)})
		}
		c["projectItems"] = map[string]any{"nodes": items}
		out = map[string]any{"repository": map[string]any{"issue": c}}
	default:
		f.t.Errorf("unexpected query %q", q)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
}

func (f *fakeGitHub) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

func ghRelay(t *testing.T) (*relay, *fakeGitHub) {
	r := newRelay(t, newFake(t, "plain"))
	gh := newFakeGitHub(t)
	r.srv.SetGitHub(github.New(gh.URL, gh.token, ghOwner, ghProject))
	return r, gh
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

func TestGitHubNotConfigured(t *testing.T) {
	r := newRelay(t, newFake(t, "plain"))
	mac := r.pairAs("mac", store.RoleManager)
	for _, c := range [][2]string{{"GET", "/api/github/board"}, {"GET", "/api/github/repos/xMuramasa/x/issues/1"},
		{"PATCH", "/api/github/repos/xMuramasa/x/issues/1"}} {
		if st, b := r.do(c[0], c[1], mac, map[string]string{"state": "closed"}); st != 503 || errCode(b) != "not_configured" {
			t.Fatalf("%v: %d %s", c, st, b)
		}
	}
	if st, _ := r.do("GET", "/api/github/board", "", nil); st != 401 {
		t.Fatalf("no token: %d", st)
	}
}

func TestGitHubReads(t *testing.T) {
	r, gh := ghRelay(t)
	phone := r.pair("pixel-8")

	st, b := r.do("GET", "/api/github/board", phone, nil)
	if st != 200 {
		t.Fatalf("board: %d %s", st, b)
	}
	board := decode[github.Board](t, b)
	if strings.Join(board.StatusOptions, ",") != "In Progress,Done,Todo" || strings.Join(board.PriorityOptions, ",") != "P0,P1,P2" {
		t.Fatalf("options: %+v", board)
	}
	var got []string
	for _, it := range board.Items {
		got = append(got, it.Repo+"#"+strconv.Itoa(it.Number)+" "+it.State+" "+it.Status+"/"+it.Priority+" "+strings.Join(it.Labels, ","))
	}
	want := "xMuramasa/trismegistus#21 open In Progress/P1 relay|xMuramasa/trismegistus#5 closed Done/ relay|xMuramasa/homelab#3 open Todo/ relay"
	if strings.Join(got, "|") != want {
		t.Fatalf("items:\n%s\nwant\n%s", strings.Join(got, "|"), want)
	}
	// Cached: a second read within 30 s doesn't reach GitHub; repos come from the same board.
	r.do("GET", "/api/github/board", phone, nil)
	if st, b := r.do("GET", "/api/github/repos", phone, nil); st != 200 || string(b) != `{"repos":["xMuramasa/homelab","xMuramasa/trismegistus"]}` {
		t.Fatalf("repos: %d %s", st, b)
	}
	if n := gh.count("board"); n != 2 { // two pages, once
		t.Fatalf("board queries: %d", n)
	}

	st, b = r.do("GET", "/api/github/repos/xmuramasa/trismegistus/issues", phone, nil)
	if l := decode[github.IssueList](t, b); st != 200 || l.State != "open" || len(l.Issues) != 2 || l.Issues[0].Comments != 1 || l.Repo != "xMuramasa/trismegistus" {
		t.Fatalf("open issues: %d %s", st, b)
	}
	st, b = r.do("GET", "/api/github/repos/xMuramasa/trismegistus/issues?state=closed", phone, nil)
	if l := decode[github.IssueList](t, b); st != 200 || len(l.Issues) != 1 || l.Issues[0].Number != 5 || l.Issues[0].State != "closed" {
		t.Fatalf("closed issues: %d %s", st, b)
	}

	st, b = r.do("GET", "/api/github/repos/xMuramasa/trismegistus/issues/21", phone, nil)
	is := decode[github.Issue](t, b)
	if st != 200 || !strings.Contains(is.Body, "secret plan") || !is.OnBoard || is.Status != "In Progress" || is.Priority != "P1" ||
		is.Author != "ghost" || len(is.Comments) != 1 || is.Comments[0].Author != "hermes" || is.CommentsTotal != 1 {
		t.Fatalf("issue: %d %s", st, b)
	}
	st, b = r.do("GET", "/api/github/repos/xMuramasa/trismegistus/issues/22", phone, nil)
	if is := decode[github.Issue](t, b); st != 200 || is.OnBoard || is.Status != "" {
		t.Fatalf("issue off the board: %d %s", st, b)
	}

	for _, c := range []struct {
		path string
		st   int
		code string
	}{
		{"/api/github/repos/someone-else/trismegistus/issues", 403, "forbidden"},
		{"/api/github/repos/someone-else/trismegistus/issues/21", 403, "forbidden"},
		{"/api/github/repos/xMuramasa/trismegistus/issues/99", 404, "not_found"},
		{"/api/github/repos/xMuramasa/nope/issues", 404, "not_found"},
		{"/api/github/repos/xMuramasa/trismegistus/issues/abc", 404, "not_found"},
		{"/api/github/repos/xMuramasa/trismegistus/issues/0", 404, "not_found"},
		{"/api/github/repos/xMuramasa/..%2F..%2Fx/issues", 404, "not_found"},
		{"/api/github/repos/xMuramasa/trismegistus/issues?state=all", 400, "bad_request"},
	} {
		if st, b := r.do("GET", c.path, phone, nil); st != c.st || errCode(b) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.path, st, b, c.st, c.code)
		}
	}
}

func TestGitHubWrites(t *testing.T) {
	r, gh := ghRelay(t)
	mac, phone := r.pairAs("mac", store.RoleManager), r.pair("pixel-8")
	issue := "/api/github/repos/xMuramasa/trismegistus/issues/"
	comment := secret("a comment nobody may log", "comment text")

	writes := []struct {
		method, path string
		body         any
	}{
		{"POST", issue + "21/comments", map[string]string{"body": comment}},
		{"PATCH", issue + "21", map[string]string{"state": "closed"}},
		{"PUT", issue + "21/board", map[string]string{"status": "Done"}},
	}
	for _, w := range writes {
		if st, b := r.do(w.method, w.path, phone, w.body); st != 403 || errCode(b) != "forbidden" {
			t.Fatalf("viewer %s %s: %d %s", w.method, w.path, st, b)
		}
		other := strings.Replace(w.path, "xMuramasa", "someone-else", 1)
		if st, b := r.do(w.method, other, mac, w.body); st != 403 || errCode(b) != "forbidden" {
			t.Fatalf("other owner %s: %d %s", other, st, b)
		}
	}
	if n := gh.count("addComment") + gh.count("state") + gh.count("setField"); n != 0 {
		t.Fatalf("refused writes reached GitHub: %d", n)
	}

	r.do("GET", issue+"21", mac, nil) // cache it; the writes must drop it
	st, b := r.do("POST", issue+"21/comments", mac, map[string]string{"body": comment})
	if c := decode[github.Comment](t, b); st != 201 || c.Body != comment || c.Author != ghOwner {
		t.Fatalf("comment: %d %s", st, b)
	}
	if st, b := r.do("PATCH", issue+"21", mac, map[string]string{"state": "closed"}); st != 200 || string(b) != `{"state":"closed"}` {
		t.Fatalf("close: %d %s", st, b)
	}
	st, b = r.do("PUT", issue+"21/board", mac, map[string]string{"status": "Done", "priority": "P0"})
	if st != 200 || string(b) != `{"status":"Done","priority":"P0"}` {
		t.Fatalf("board fields: %d %s", st, b)
	}
	st, b = r.do("GET", issue+"21", mac, nil)
	if is := decode[github.Issue](t, b); st != 200 || is.State != "closed" || len(is.Comments) != 2 || is.Status != "Done" || is.Priority != "P0" {
		t.Fatalf("after writes: %d %s", st, b)
	}
	if st, b := r.do("PATCH", issue+"21", mac, map[string]string{"state": "open"}); st != 200 || string(b) != `{"state":"open"}` {
		t.Fatalf("reopen: %d %s", st, b)
	}

	// Not on the board: added first, then set.
	st, b = r.do("PUT", issue+"22/board", mac, map[string]string{"priority": "P2"})
	if st != 200 || string(b) != `{"status":"","priority":"P2"}` || gh.count("addItem") != 1 || !gh.find("trismegistus", 22).board {
		t.Fatalf("add to board: %d %s (adds %d)", st, b, gh.count("addItem"))
	}
	if n := gh.count("fields"); n != 1 {
		t.Fatalf("field lookups: %d, want 1 (cached)", n)
	}

	for _, c := range []struct {
		method, path string
		body         any
		st           int
		code         string
	}{
		{"PUT", issue + "21/board", map[string]string{"status": "Blocked"}, 400, "bad_request"},
		{"PUT", issue + "21/board", map[string]string{}, 400, "bad_request"},
		{"PUT", issue + "21/board", map[string]string{"column": "x"}, 400, "bad_request"},
		{"PATCH", issue + "21", map[string]string{"state": "merged"}, 400, "bad_request"},
		{"POST", issue + "21/comments", map[string]string{"body": "  "}, 400, "bad_request"},
		{"POST", issue + "99/comments", map[string]string{"body": "x"}, 404, "not_found"},
		{"POST", issue + "21/comments", map[string]string{"body": strings.Repeat("x", 20000)}, 413, "too_large"},
	} {
		if st, b := r.do(c.method, c.path, mac, c.body); st != c.st || errCode(b) != c.code {
			t.Errorf("%s %s %v: %d %s, want %d %s", c.method, c.path, c.body, st, b, c.st, c.code)
		}
	}
}

func TestGitHubErrors(t *testing.T) {
	r, gh := ghRelay(t)
	phone := r.pair("pixel-8")
	reset := time.Now().Add(10 * time.Minute).Unix()
	for _, c := range []struct {
		name string
		fail func(w http.ResponseWriter)
		st   int
		code string
	}{
		{"bad token", func(w http.ResponseWriter) { w.WriteHeader(401) }, 503, "not_configured"},
		{"primary limit", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
			w.WriteHeader(403)
		}, 429, "rate_limited"},
		{"secondary limit", func(w http.ResponseWriter) { w.Header().Set("Retry-After", "60"); w.WriteHeader(429) }, 429, "rate_limited"},
		{"graphql limit", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
		}, 429, "rate_limited"},
		{"no scope", func(w http.ResponseWriter) { w.WriteHeader(403) }, 403, "forbidden"},
		{"outage", func(w http.ResponseWriter) { w.WriteHeader(502) }, 502, "github_error"},
		{"garbage", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`<html>`)) }, 502, "github_error"},
		{"redirect", func(w http.ResponseWriter) { w.Header().Set("Location", "/elsewhere"); w.WriteHeader(301) }, 502, "github_error"},
	} {
		gh.mu.Lock()
		gh.fail = func(w http.ResponseWriter) bool { c.fail(w); return true }
		gh.mu.Unlock()
		// Errors are never cached, so every case reaches the fake.
		st, b := r.do("GET", "/api/github/repos/xMuramasa/trismegistus/issues/21", phone, nil)
		if st != c.st || errCode(b) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, st, b, c.st, c.code)
			continue
		}
		if c.code == "rate_limited" {
			var out struct {
				Error struct {
					RetryAt time.Time `json:"retry_at"`
				}
			}
			_ = json.Unmarshal(b, &out)
			if until := time.Until(out.Error.RetryAt); until < 30*time.Second || until > 11*time.Minute {
				t.Errorf("%s: retry_at %v", c.name, out.Error.RetryAt)
			}
		}
	}
	gh.mu.Lock()
	gh.fail = nil
	gh.mu.Unlock()
	if st, b := r.do("GET", "/api/github/repos/xMuramasa/trismegistus/issues/21", phone, nil); st != 200 {
		t.Fatalf("after recovery: %d %s", st, b)
	}
}
