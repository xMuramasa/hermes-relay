// Package github is the relay's fixed GitHub client: the Work board (a Projects v2 board), the issues of the
// configured owner's repos, comments, close/reopen and the board's Status and Priority fields. Every query is a
// constant here; requests only fill in checked repo names, issue numbers, option ids and comment text.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Error codes the server answers with.
const (
	CodeNotConfigured = "not_configured"
	CodeRateLimited   = "rate_limited"
	CodeNotFound      = "not_found"
	CodeForbidden     = "forbidden"
	CodeBadRequest    = "bad_request"
	CodeGitHubError   = "github_error"
)

// Error is a failed GitHub call, already mapped to a relay error code. Messages never carry GitHub's own text.
type Error struct {
	Code    string
	Message string
	Status  int       // GitHub's HTTP status (0 if none), for logs
	RetryAt time.Time // rate_limited only
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

const (
	cacheTTL    = 30 * time.Second
	fieldsTTL   = 10 * time.Minute
	maxResponse = 8 << 20
	maxPages    = 10 // shortcut: the board is read up to 1000 items; page further if it ever grows past that
)

// Client talks to GitHub's GraphQL API for one owner and one project.
type Client struct {
	api, token, owner, project string
	http                       *http.Client

	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	at time.Time
	v  any
}

// New makes a client. api is the REST base URL (https://api.github.com); GraphQL is api + "/graphql".
func New(api, token, owner, project string) *Client {
	return &Client{
		api: strings.TrimRight(api, "/"), token: token, owner: owner, project: project,
		http: &http.Client{
			Timeout: 20 * time.Second,
			// A moved repo redirects; never follow it (it may lead outside the owner's repos).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		cache: map[string]entry{},
	}
}

// cached returns the value under key if younger than ttl, else fetches and stores it. Errors are not cached.
func cached[T any](c *Client, key string, ttl time.Duration, fetch func() (T, error)) (T, error) {
	c.mu.Lock()
	e, ok := c.cache[key]
	c.mu.Unlock()
	if ok && time.Since(e.at) < ttl {
		return e.v.(T), nil
	}
	v, err := fetch()
	if err != nil {
		return v, err
	}
	c.mu.Lock()
	for k, e := range c.cache {
		if time.Since(e.at) > fieldsTTL {
			delete(c.cache, k)
		}
	}
	c.cache[key] = entry{time.Now(), v}
	c.mu.Unlock()
	return v, nil
}

// changed drops the cached reads after a write (the field definitions stay).
func (c *Client) changed() {
	c.mu.Lock()
	for k := range c.cache {
		if k != "fields" {
			delete(c.cache, k)
		}
	}
	c.mu.Unlock()
}

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// checkRepo lets through only the configured owner's repos.
func (c *Client) checkRepo(owner, repo string) error {
	if !strings.EqualFold(owner, c.owner) {
		return &Error{Code: CodeForbidden, Message: "only " + c.owner + "'s repos are reachable"}
	}
	if !repoNameRe.MatchString(repo) || repo == "." || repo == ".." {
		return notFound
	}
	return nil
}

var notFound = &Error{Code: CodeNotFound, Message: "not found"}

// graphql runs one fixed query and decodes its data into out.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/graphql", bytes.NewReader(body))
	if err != nil {
		return &Error{Code: CodeGitHubError, Message: "bad GitHub URL"}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "trismegistus-relay")
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Code: CodeGitHubError, Message: "GitHub is unreachable"}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return &Error{Code: CodeGitHubError, Message: "reading GitHub's answer failed"}
	}
	if resp.StatusCode != http.StatusOK {
		return statusError(resp.StatusCode, resp.Header)
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type string `json:"type"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return &Error{Code: CodeGitHubError, Message: "GitHub sent an invalid answer", Status: resp.StatusCode}
	}
	if len(env.Errors) > 0 {
		switch env.Errors[0].Type {
		case "NOT_FOUND":
			return notFound
		case "RATE_LIMITED":
			return rateLimited(resp.StatusCode, resp.Header)
		case "FORBIDDEN", "INSUFFICIENT_SCOPES":
			return &Error{Code: CodeForbidden, Message: "GitHub refused; the token may lack the repo or project scope"}
		case "UNPROCESSABLE":
			return &Error{Code: CodeBadRequest, Message: "GitHub refused the change"}
		}
		return &Error{Code: CodeGitHubError, Message: "GitHub answered with an error", Status: resp.StatusCode}
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return &Error{Code: CodeGitHubError, Message: "GitHub sent an invalid answer", Status: resp.StatusCode}
	}
	return nil
}

func statusError(st int, h http.Header) *Error {
	if st == http.StatusTooManyRequests || (st == http.StatusForbidden && !retryAt(h).IsZero()) {
		return rateLimited(st, h)
	}
	switch st {
	case http.StatusUnauthorized:
		return &Error{Code: CodeNotConfigured, Status: st,
			Message: "GitHub refused the token; check GITHUB_TOKEN in the relay's .env (scopes repo and project)"}
	case http.StatusForbidden:
		return &Error{Code: CodeForbidden, Status: st, Message: "GitHub refused; the token may lack the repo or project scope"}
	case http.StatusNotFound, http.StatusGone:
		return &Error{Code: CodeNotFound, Status: st, Message: "not found"}
	}
	return &Error{Code: CodeGitHubError, Status: st, Message: "GitHub answered HTTP " + strconv.Itoa(st)}
}

func rateLimited(st int, h http.Header) *Error {
	at := retryAt(h)
	if at.IsZero() {
		at = time.Now().Add(time.Minute)
	}
	return &Error{Code: CodeRateLimited, Status: st, RetryAt: at, Message: "GitHub rate limit; try again later"}
}

// retryAt reads Retry-After (secondary limits) or an exhausted X-RateLimit-Reset (primary limit).
func retryAt(h http.Header) time.Time {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s >= 0 {
		return time.Now().Add(time.Duration(s) * time.Second)
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if r, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(r, 0)
		}
	}
	return time.Time{}
}

// GraphQL shapes shared by the queries.
type (
	gqlValue  struct{ Name string }
	gqlLabels struct{ Nodes []struct{ Name string } }
	gqlUser   struct{ Login string }
	gqlField  struct {
		ID, Name string
		Options  []struct{ ID, Name string }
	}
	gqlItem struct {
		ID       string
		Project  struct{ ID string }
		Status   *gqlValue
		Priority *gqlValue
	}
)

func (l gqlLabels) names() []string {
	out := []string{}
	for _, n := range l.Nodes {
		out = append(out, n.Name)
	}
	return out
}

func name(v *gqlValue) string {
	if v == nil {
		return ""
	}
	return v.Name
}

func author(u *gqlUser) string {
	if u == nil {
		return "ghost" // deleted accounts
	}
	return u.Login
}

// itemValues are the board columns read from an item; the names are what the fields are looked up by.
const itemValues = `
	status: fieldValueByName(name: "Status") { ... on ProjectV2ItemFieldSingleSelectValue { name } }
	priority: fieldValueByName(name: "Priority") { ... on ProjectV2ItemFieldSingleSelectValue { name } }`

const fieldsPart = `fields(first: 50) { nodes { ... on ProjectV2SingleSelectField { id name options { id name } } } }`

const boardQuery = `query($project: ID!, $cursor: String) {
  node(id: $project) {
    ... on ProjectV2 {
      ` + fieldsPart + `
      items(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {` + itemValues + `
          content { ... on Issue { number title state updatedAt url labels(first: 20) { nodes { name } } repository { name owner { login } } } }
        }
      }
    }
  }
}`

// BoardItem is one issue on the board.
type BoardItem struct {
	Repo      string    `json:"repo"` // owner/name
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`    // open, closed
	Status    string    `json:"status"`   // "" when unset
	Priority  string    `json:"priority"` // "" when unset
	Labels    []string  `json:"labels"`
	UpdatedAt time.Time `json:"updated_at"`
	URL       string    `json:"url"`
}

// Board is the Work board: its Status and Priority options (board order) and its issues.
type Board struct {
	StatusOptions   []string    `json:"status_options"`
	PriorityOptions []string    `json:"priority_options"`
	Items           []BoardItem `json:"items"`
}

type boardFields struct {
	project string
	status  *gqlField
	prio    *gqlField
}

func findFields(nodes []gqlField) (status, prio *gqlField) {
	for i := range nodes {
		switch {
		case strings.EqualFold(nodes[i].Name, "Status") && status == nil:
			status = &nodes[i]
		case strings.EqualFold(nodes[i].Name, "Priority") && prio == nil:
			prio = &nodes[i]
		}
	}
	return
}

func optionNames(f *gqlField) []string {
	out := []string{}
	if f != nil {
		for _, o := range f.Options {
			out = append(out, o.Name)
		}
	}
	return out
}

// Board reads the board's issues from the owner's repos (other owners' issues, pull requests and drafts are left out).
func (c *Client) Board(ctx context.Context) (*Board, error) {
	return cached(c, "board", cacheTTL, func() (*Board, error) {
		b := &Board{Items: []BoardItem{}}
		var cursor any
		for page := 0; page < maxPages; page++ {
			var out struct {
				Node *struct {
					Fields struct{ Nodes []gqlField }
					Items  *struct {
						PageInfo struct {
							HasNextPage bool
							EndCursor   string
						}
						Nodes []struct {
							gqlItem
							Content struct {
								Number     int
								Title      string
								State      string
								UpdatedAt  time.Time
								URL        string
								Labels     gqlLabels
								Repository struct {
									Name  string
									Owner gqlUser
								}
							}
						}
					}
				}
			}
			if err := c.graphql(ctx, boardQuery, map[string]any{"project": c.project, "cursor": cursor}, &out); err != nil {
				return nil, err
			}
			if out.Node == nil || out.Node.Items == nil {
				return nil, &Error{Code: CodeNotFound, Message: "GITHUB_PROJECT is not a board this token can read"}
			}
			if page == 0 {
				st, pr := findFields(out.Node.Fields.Nodes)
				b.StatusOptions, b.PriorityOptions = optionNames(st), optionNames(pr)
			}
			for _, n := range out.Node.Items.Nodes {
				ct := n.Content
				if ct.Number == 0 || !strings.EqualFold(ct.Repository.Owner.Login, c.owner) {
					continue
				}
				b.Items = append(b.Items, BoardItem{
					Repo: ct.Repository.Owner.Login + "/" + ct.Repository.Name, Number: ct.Number, Title: ct.Title,
					State: strings.ToLower(ct.State), Status: name(n.Status), Priority: name(n.Priority),
					Labels: ct.Labels.names(), UpdatedAt: ct.UpdatedAt, URL: ct.URL,
				})
			}
			if !out.Node.Items.PageInfo.HasNextPage {
				break
			}
			cursor = out.Node.Items.PageInfo.EndCursor
		}
		return b, nil
	})
}

// Repos lists the owner's repos that have issues on the board, as owner/name, sorted.
func (c *Client) Repos(ctx context.Context) ([]string, error) {
	b, err := c.Board(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, it := range b.Items {
		if !slices.Contains(out, it.Repo) {
			out = append(out, it.Repo)
		}
	}
	slices.Sort(out)
	return out, nil
}

const fieldsQuery = `query($project: ID!) { node(id: $project) { ... on ProjectV2 { id ` + fieldsPart + ` } } }`

// fields looks up the board's Status and Priority fields by name (cached; ids are never hard-coded).
func (c *Client) fields(ctx context.Context) (*boardFields, error) {
	return cached(c, "fields", fieldsTTL, func() (*boardFields, error) {
		var out struct {
			Node *struct {
				ID     string
				Fields struct{ Nodes []gqlField }
			}
		}
		if err := c.graphql(ctx, fieldsQuery, map[string]any{"project": c.project}, &out); err != nil {
			return nil, err
		}
		if out.Node == nil || out.Node.ID == "" {
			return nil, &Error{Code: CodeNotFound, Message: "GITHUB_PROJECT is not a board this token can read"}
		}
		f := &boardFields{project: out.Node.ID}
		f.status, f.prio = findFields(out.Node.Fields.Nodes)
		return f, nil
	})
}

const issuesQuery = `query($owner: String!, $repo: String!, $states: [IssueState!]) {
  repository(owner: $owner, name: $repo) {
    issues(first: 100, states: $states, orderBy: {field: UPDATED_AT, direction: DESC}) {
      totalCount
      nodes { number title state updatedAt url comments { totalCount } labels(first: 20) { nodes { name } } }
    }
  }
}`

// IssueSummary is one row of a repo's issue list.
type IssueSummary struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Labels    []string  `json:"labels"`
	Comments  int       `json:"comments"`
	UpdatedAt time.Time `json:"updated_at"`
	URL       string    `json:"url"`
}

// IssueList is a repo's open or closed issues, most recently updated first.
type IssueList struct {
	Repo   string         `json:"repo"`
	State  string         `json:"state"`
	Total  int            `json:"total"` // shortcut: only the first 100 are listed; add paging if a repo outgrows that
	Issues []IssueSummary `json:"issues"`
}

// Issues lists a repo's issues; state is open or closed.
func (c *Client) Issues(ctx context.Context, owner, repo, state string) (*IssueList, error) {
	if err := c.checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if state != "open" && state != "closed" {
		return nil, &Error{Code: CodeBadRequest, Message: "state must be open or closed"}
	}
	return cached(c, "issues:"+strings.ToLower(repo)+":"+state, cacheTTL, func() (*IssueList, error) {
		var out struct {
			Repository *struct {
				Issues struct {
					TotalCount int
					Nodes      []struct {
						Number    int
						Title     string
						State     string
						UpdatedAt time.Time
						URL       string
						Comments  struct{ TotalCount int }
						Labels    gqlLabels
					}
				}
			}
		}
		vars := map[string]any{"owner": c.owner, "repo": repo, "states": []string{strings.ToUpper(state)}}
		if err := c.graphql(ctx, issuesQuery, vars, &out); err != nil {
			return nil, err
		}
		if out.Repository == nil {
			return nil, notFound
		}
		l := &IssueList{Repo: c.owner + "/" + repo, State: state, Total: out.Repository.Issues.TotalCount, Issues: []IssueSummary{}}
		for _, n := range out.Repository.Issues.Nodes {
			l.Issues = append(l.Issues, IssueSummary{Number: n.Number, Title: n.Title, State: strings.ToLower(n.State),
				Labels: n.Labels.names(), Comments: n.Comments.TotalCount, UpdatedAt: n.UpdatedAt, URL: n.URL})
		}
		return l, nil
	})
}

const issueQuery = `query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    issue(number: $number) {
      id number title body state url createdAt updatedAt
      author { login }
      labels(first: 20) { nodes { name } }
      comments(last: 100) { totalCount nodes { author { login } body createdAt url } }
      projectItems(first: 20) { nodes { id project { id }` + itemValues + ` } }
    }
  }
}`

// Comment is one issue comment.
type Comment struct {
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	URL       string    `json:"url"`
}

// Issue is one issue with its body, its last 100 comments and its board columns.
type Issue struct {
	Repo          string    `json:"repo"`
	Number        int       `json:"number"`
	Title         string    `json:"title"`
	Body          string    `json:"body"`
	State         string    `json:"state"`
	Author        string    `json:"author"`
	Labels        []string  `json:"labels"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	URL           string    `json:"url"`
	OnBoard       bool      `json:"on_board"`
	Status        string    `json:"status"`
	Priority      string    `json:"priority"`
	CommentsTotal int       `json:"comments_total"`
	Comments      []Comment `json:"comments"` // oldest first; shortcut: the last 100 only, page if threads get longer

	id, itemID string // node ids for writes
}

// Issue reads one issue (cached).
func (c *Client) Issue(ctx context.Context, owner, repo string, number int) (*Issue, error) {
	if err := c.checkRepo(owner, repo); err != nil {
		return nil, err
	}
	return cached(c, "issue:"+strings.ToLower(repo)+":"+strconv.Itoa(number), cacheTTL, func() (*Issue, error) {
		return c.fetchIssue(ctx, repo, number)
	})
}

func (c *Client) fetchIssue(ctx context.Context, repo string, number int) (*Issue, error) {
	var out struct {
		Repository *struct {
			Issue *struct {
				ID, Title, Body, State, URL string
				Number                      int
				CreatedAt, UpdatedAt        time.Time
				Author                      *gqlUser
				Labels                      gqlLabels
				Comments                    struct {
					TotalCount int
					Nodes      []struct {
						Author    *gqlUser
						Body, URL string
						CreatedAt time.Time
					}
				}
				ProjectItems struct{ Nodes []gqlItem }
			}
		}
	}
	vars := map[string]any{"owner": c.owner, "repo": repo, "number": number}
	if err := c.graphql(ctx, issueQuery, vars, &out); err != nil {
		return nil, err
	}
	if out.Repository == nil || out.Repository.Issue == nil {
		return nil, notFound
	}
	is := out.Repository.Issue
	res := &Issue{Repo: c.owner + "/" + repo, Number: is.Number, Title: is.Title, Body: is.Body,
		State: strings.ToLower(is.State), Author: author(is.Author), Labels: is.Labels.names(), CreatedAt: is.CreatedAt,
		UpdatedAt: is.UpdatedAt, URL: is.URL, CommentsTotal: is.Comments.TotalCount, Comments: []Comment{}, id: is.ID}
	for _, n := range is.Comments.Nodes {
		res.Comments = append(res.Comments, Comment{Author: author(n.Author), Body: n.Body, CreatedAt: n.CreatedAt, URL: n.URL})
	}
	for _, it := range is.ProjectItems.Nodes {
		if it.Project.ID == c.project {
			res.OnBoard, res.itemID, res.Status, res.Priority = true, it.ID, name(it.Status), name(it.Priority)
		}
	}
	return res, nil
}

const addCommentMutation = `mutation($id: ID!, $body: String!) {
  addComment(input: {subjectId: $id, body: $body}) { commentEdge { node { author { login } body createdAt url } } }
}`

// AddComment comments on an issue (pull requests are not issues here, so they can't be reached).
func (c *Client) AddComment(ctx context.Context, owner, repo string, number int, body string) (*Comment, error) {
	is, err := c.writable(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	var out struct {
		AddComment struct {
			CommentEdge struct {
				Node struct {
					Author    *gqlUser
					Body, URL string
					CreatedAt time.Time
				}
			}
		}
	}
	if err := c.graphql(ctx, addCommentMutation, map[string]any{"id": is.id, "body": body}, &out); err != nil {
		return nil, err
	}
	c.changed()
	n := out.AddComment.CommentEdge.Node
	return &Comment{Author: author(n.Author), Body: n.Body, CreatedAt: n.CreatedAt, URL: n.URL}, nil
}

const (
	closeMutation  = `mutation($id: ID!) { closeIssue(input: {issueId: $id}) { issue { state } } }`
	reopenMutation = `mutation($id: ID!) { reopenIssue(input: {issueId: $id}) { issue { state } } }`
)

// SetState closes or reopens an issue; state is open or closed.
func (c *Client) SetState(ctx context.Context, owner, repo string, number int, state string) error {
	q := map[string]string{"open": reopenMutation, "closed": closeMutation}[state]
	if q == "" {
		return &Error{Code: CodeBadRequest, Message: "state must be open or closed"}
	}
	is, err := c.writable(ctx, owner, repo, number)
	if err != nil {
		return err
	}
	if err := c.graphql(ctx, q, map[string]any{"id": is.id}, &struct{}{}); err != nil {
		return err
	}
	c.changed()
	return nil
}

const (
	addItemMutation = `mutation($project: ID!, $id: ID!) {
  addProjectV2ItemById(input: {projectId: $project, contentId: $id}) { item { id } }
}`
	setFieldMutation = `mutation($project: ID!, $item: ID!, $field: ID!, $option: String!) {
  updateProjectV2ItemFieldValue(input: {projectId: $project, itemId: $item, fieldId: $field, value: {singleSelectOptionId: $option}}) { projectV2Item { id } }
}`
)

// BoardValues are an issue's Status and Priority on the board.
type BoardValues struct {
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

func optionID(f *gqlField, field, option string) (fieldID, optID string, err error) {
	if f == nil {
		return "", "", &Error{Code: CodeBadRequest, Message: "the board has no " + field + " field"}
	}
	for _, o := range f.Options {
		if o.Name == option {
			return f.ID, o.ID, nil
		}
	}
	return "", "", &Error{Code: CodeBadRequest, Message: "not a " + field + " option of the board"}
}

// SetBoard sets an issue's Status and/or Priority (nil leaves it), by option name. An issue not on the board is
// added first.
func (c *Client) SetBoard(ctx context.Context, owner, repo string, number int, status, priority *string) (*BoardValues, error) {
	if err := c.checkRepo(owner, repo); err != nil {
		return nil, err
	}
	f, err := c.fields(ctx)
	if err != nil {
		return nil, err
	}
	type set struct{ field, option string }
	var sets []set
	for _, v := range []struct {
		f     *gqlField
		label string
		val   *string
	}{{f.status, "Status", status}, {f.prio, "Priority", priority}} {
		if v.val == nil {
			continue
		}
		fid, oid, err := optionID(v.f, v.label, *v.val)
		if err != nil {
			return nil, err
		}
		sets = append(sets, set{fid, oid})
	}
	is, err := c.writable(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	item := is.itemID
	if item == "" {
		var out struct {
			AddProjectV2ItemById struct{ Item struct{ ID string } }
		}
		if err := c.graphql(ctx, addItemMutation, map[string]any{"project": f.project, "id": is.id}, &out); err != nil {
			return nil, err
		}
		c.changed()
		if item = out.AddProjectV2ItemById.Item.ID; item == "" {
			return nil, &Error{Code: CodeGitHubError, Message: "GitHub did not add the issue to the board"}
		}
	}
	for _, s := range sets {
		vars := map[string]any{"project": f.project, "item": item, "field": s.field, "option": s.option}
		if err := c.graphql(ctx, setFieldMutation, vars, &struct{}{}); err != nil {
			c.changed()
			return nil, err
		}
	}
	c.changed()
	res := &BoardValues{Status: is.Status, Priority: is.Priority}
	if status != nil {
		res.Status = *status
	}
	if priority != nil {
		res.Priority = *priority
	}
	return res, nil
}

// writable checks the repo and reads the issue fresh (its node id and board item) for a write.
func (c *Client) writable(ctx context.Context, owner, repo string, number int) (*Issue, error) {
	if err := c.checkRepo(owner, repo); err != nil {
		return nil, err
	}
	is, err := c.fetchIssue(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	if is.id == "" {
		return nil, &Error{Code: CodeGitHubError, Message: "GitHub sent no issue id"}
	}
	return is, nil
}

// AsError returns err as a *Error, mapping anything else to github_error.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeGitHubError, Message: "GitHub request failed"}
}
