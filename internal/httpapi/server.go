// Package httpapi serves the synced data over a GitHub-compatible HTTP surface.
// Reads that the store mirrors are answered from SQLite (and, for repo content,
// from a local git clone); everything else — unmirrored reads and all writes —
// falls through to api.github.com via the write proxy. The server binds
// localhost and trusts any local caller: clients present no token, and the
// server's own GITHUB_TOKEN is used only for the proxy fallback.
package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mevdschee/github-export/internal/gitbackend"
	"github.com/mevdschee/github-export/internal/graphqlmirror"
	"github.com/mevdschee/github-export/internal/query"
	"github.com/mevdschee/github-export/internal/shadow"
	"github.com/mevdschee/github-export/internal/writeproxy"
)

// Server holds the read query layer, the proxy fallback, and request routing.
type Server struct {
	q        *query.Query
	proxy    *writeproxy.Proxy
	git      *gitbackend.Backend   // optional; nil disables local content endpoints
	gql      *graphqlmirror.Mirror // optional; nil forwards all GraphQL to proxy
	compare  *shadow.Comparator    // optional; --debug-compare parity harness
	owner    string
	repo     string
	syncedAt string
	mux      *http.ServeMux
}

// Config configures a Server.
type Config struct {
	Query    *query.Query
	Proxy    *writeproxy.Proxy
	Git      *gitbackend.Backend
	GraphQL  *graphqlmirror.Mirror
	Compare  *shadow.Comparator
	Owner    string
	Repo     string
	SyncedAt string
}

// New builds a Server and registers its routes.
func New(cfg Config) *Server {
	s := &Server{
		q: cfg.Query, proxy: cfg.Proxy, git: cfg.Git, gql: cfg.GraphQL, compare: cfg.Compare,
		owner: cfg.Owner, repo: cfg.Repo, syncedAt: cfg.SyncedAt,
		mux: http.NewServeMux(),
	}
	s.routes()
	return s
}

// maybeCompare fires the shadow-compare harness for a mirrored GET response.
func (s *Server) maybeCompare(r *http.Request, body []byte) {
	if s.compare == nil || r.Method != http.MethodGet {
		return
	}
	path := r.URL.RequestURI()
	b := append([]byte(nil), body...)
	go s.compare.Check(path, b)
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	rp := "/repos/" + s.owner + "/" + s.repo
	m := s.mux

	m.HandleFunc("GET "+rp, s.handleRepo)
	m.HandleFunc("GET "+rp+"/issues", s.handleListIssues)
	m.HandleFunc("GET "+rp+"/issues/{number}", s.handleGetIssue)
	m.HandleFunc("GET "+rp+"/issues/{number}/comments", s.handleIssueComments)
	m.HandleFunc("GET "+rp+"/issues/{number}/timeline", s.handleIssueTimeline)
	m.HandleFunc("GET "+rp+"/pulls", s.handleListPulls)
	m.HandleFunc("GET "+rp+"/pulls/{number}", s.handleGetPull)
	m.HandleFunc("GET "+rp+"/pulls/{number}/reviews", s.handlePullReviews)
	m.HandleFunc("GET "+rp+"/pulls/{number}/comments", s.handlePullReviewComments)
	m.HandleFunc("GET "+rp+"/labels", s.handleLabels)
	m.HandleFunc("GET "+rp+"/milestones", s.handleMilestones)
	m.HandleFunc("GET "+rp+"/releases", s.handleReleases)
	m.HandleFunc("GET "+rp+"/releases/tags/{tag}", s.handleReleaseByTag)
	m.HandleFunc("GET "+rp+"/discussions", s.handleDiscussions)
	m.HandleFunc("GET "+rp+"/discussions/{number}", s.handleGetDiscussion)
	m.HandleFunc("GET /search/issues", s.handleSearchIssues)

	// GraphQL: Projects v2 and Discussions are GraphQL-only, and gh/MCP issue
	// many reads as GraphQL. Supported queries are served from SQLite; anything
	// the mirror does not fully cover falls through to api.github.com so coverage
	// stays complete.
	m.HandleFunc("POST /graphql", s.handleGraphQL)

	// Local git content endpoints (only when a backend is configured).
	if s.git != nil {
		m.HandleFunc("GET "+rp+"/branches", s.handleBranches)
		m.HandleFunc("GET "+rp+"/tags", s.handleTags)
		m.HandleFunc("GET "+rp+"/commits", s.handleCommits)
		m.HandleFunc("GET "+rp+"/commits/{sha}", s.handleCommit)
		m.HandleFunc("GET "+rp+"/contents/{path...}", s.handleContents)
	}

	// Meta endpoints.
	m.HandleFunc("GET /status", s.handleStatus)
	m.HandleFunc("GET /openapi.yaml", s.handleOpenAPI)
	m.HandleFunc("GET /docs", s.handleDocs)

	// Everything else (writes, unmirrored reads) falls through to the proxy.
	m.HandleFunc("/", s.handleProxy)
}

// --- mirror handlers ---

func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	doc, ok, err := s.q.Repo()
	s.one(w, r, doc, ok, err)
}

func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	o := issueOptsFromQuery(r)
	items, total, err := s.q.ListIssues(o)
	s.list(w, r, items, total, o.Page, o.PerPage, err)
}

func (s *Server) handleListPulls(w http.ResponseWriter, r *http.Request) {
	o := issueOptsFromQuery(r)
	o.OnlyPulls = true
	items, total, err := s.q.ListIssues(o)
	s.list(w, r, items, total, o.Page, o.PerPage, err)
}

func (s *Server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	doc, ok, err := s.q.GetIssue(numFromPath(r, "number"))
	s.one(w, r, doc, ok, err)
}

func (s *Server) handleGetPull(w http.ResponseWriter, r *http.Request) {
	doc, ok, err := s.q.GetPull(numFromPath(r, "number"))
	s.one(w, r, doc, ok, err)
}

func (s *Server) handleIssueComments(w http.ResponseWriter, r *http.Request) {
	items, err := s.q.IssueComments(numFromPath(r, "number"))
	s.array(w, r, items, err)
}

func (s *Server) handleIssueTimeline(w http.ResponseWriter, r *http.Request) {
	items, err := s.q.IssueTimeline(numFromPath(r, "number"))
	s.array(w, r, items, err)
}

func (s *Server) handlePullReviews(w http.ResponseWriter, r *http.Request) {
	items, err := s.q.PullReviews(numFromPath(r, "number"))
	s.array(w, r, items, err)
}

func (s *Server) handlePullReviewComments(w http.ResponseWriter, r *http.Request) {
	items, err := s.q.PullReviewComments(numFromPath(r, "number"))
	s.array(w, r, items, err)
}

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request) {
	items, err := s.q.ListLabels()
	s.array(w, r, items, err)
}

func (s *Server) handleMilestones(w http.ResponseWriter, r *http.Request) {
	items, err := s.q.ListMilestones()
	s.array(w, r, items, err)
}

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	items, err := s.q.ListReleases()
	s.array(w, r, items, err)
}

func (s *Server) handleReleaseByTag(w http.ResponseWriter, r *http.Request) {
	doc, ok, err := s.q.GetRelease(r.PathValue("tag"))
	s.one(w, r, doc, ok, err)
}

func (s *Server) handleDiscussions(w http.ResponseWriter, r *http.Request) {
	items, err := s.q.ListDiscussions()
	s.array(w, r, items, err)
}

func (s *Server) handleGetDiscussion(w http.ResponseWriter, r *http.Request) {
	doc, ok, err := s.q.GetDiscussion(numFromPath(r, "number"))
	s.one(w, r, doc, ok, err)
}

func (s *Server) handleSearchIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	page := atoiDefault(r.URL.Query().Get("page"), 1)
	perPage := atoiDefault(r.URL.Query().Get("per_page"), 30)
	items, total, err := s.q.SearchIssues(q, perPage, page)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.freshness(w)
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"total_count":        total,
		"incomplete_results": false,
		"items":              items,
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	counts, err := s.q.Counts()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.freshness(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"owner":     s.owner,
		"repo":      s.repo,
		"synced_at": s.syncedAt,
		"counts":    counts,
		"proxy":     !s.proxy.Disabled,
		"git":       s.git != nil,
	})
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	s.proxy.Forward(w, r)
}

// handleGraphQL tries the local mirror first and forwards to GitHub when the
// query is not fully supported. The request body is buffered so it can be
// replayed to the proxy on fallthrough.
func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		s.fail(w, err)
		return
	}
	replayBody := func() {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	if s.gql == nil {
		replayBody()
		s.handleProxy(w, r)
		return
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Query == "" {
		replayBody()
		s.handleProxy(w, r)
		return
	}
	if resp, ok := s.gql.Execute(req.Query, req.Variables); ok {
		s.freshness(w)
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
		return
	}
	replayBody()
	s.handleProxy(w, r)
}

// --- response helpers ---

func (s *Server) freshness(w http.ResponseWriter) {
	if s.syncedAt != "" {
		w.Header().Set("X-GitHub-Export-Synced-At", s.syncedAt)
	}
	w.Header().Set("X-GitHub-Export-Source", "local")
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
}

// one writes a single object, or falls through to the proxy on a miss so the
// caller still gets the upstream answer (and we stay "100% compatible").
func (s *Server) one(w http.ResponseWriter, r *http.Request, doc json.RawMessage, ok bool, err error) {
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		s.handleProxy(w, r)
		return
	}
	s.freshness(w)
	w.Header().Set("Content-Type", "application/json")
	w.Write(doc)
	s.maybeCompare(r, doc)
}

// array writes a JSON array of pre-serialized documents.
func (s *Server) array(w http.ResponseWriter, r *http.Request, items []json.RawMessage, err error) {
	if err != nil {
		s.fail(w, err)
		return
	}
	s.freshness(w)
	w.Header().Set("Content-Type", "application/json")
	body := writeJSONArray(w, items)
	s.maybeCompare(r, body)
}

// list writes a paginated JSON array with a GitHub-style Link header.
func (s *Server) list(w http.ResponseWriter, r *http.Request, items []json.RawMessage, total, page, perPage int, err error) {
	if err != nil {
		s.fail(w, err)
		return
	}
	if page <= 0 {
		page = 1
	}
	if perPage <= 0 {
		perPage = 30
	}
	if link := linkHeader(r, page, perPage, total); link != "" {
		w.Header().Set("Link", link)
	}
	s.freshness(w)
	w.Header().Set("Content-Type", "application/json")
	body := writeJSONArray(w, items)
	s.maybeCompare(r, body)
}

func writeJSONArray(w http.ResponseWriter, items []json.RawMessage) []byte {
	if items == nil {
		items = []json.RawMessage{}
	}
	b, err := json.Marshal(items)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	w.Write(b)
	return b
}

// linkHeader builds a GitHub-compatible pagination Link header.
func linkHeader(r *http.Request, page, perPage, total int) string {
	last := (total + perPage - 1) / perPage
	if last <= 1 {
		return ""
	}
	mk := func(p int, rel string) string {
		u := *r.URL
		qv := u.Query()
		qv.Set("page", strconv.Itoa(p))
		qv.Set("per_page", strconv.Itoa(perPage))
		u.RawQuery = qv.Encode()
		return fmt.Sprintf(`<%s>; rel="%s"`, u.RequestURI(), rel)
	}
	var parts []string
	if page < last {
		parts = append(parts, mk(page+1, "next"), mk(last, "last"))
	}
	if page > 1 {
		parts = append(parts, mk(page-1, "prev"), mk(1, "first"))
	}
	return strings.Join(parts, ", ")
}

func issueOptsFromQuery(r *http.Request) query.ListIssuesOpts {
	v := r.URL.Query()
	o := query.ListIssuesOpts{
		State:     v.Get("state"),
		Creator:   v.Get("creator"),
		Assignee:  v.Get("assignee"),
		Milestone: v.Get("milestone"),
		Since:     v.Get("since"),
		Sort:      v.Get("sort"),
		Direction: v.Get("direction"),
		PerPage:   atoiDefault(v.Get("per_page"), 30),
		Page:      atoiDefault(v.Get("page"), 1),
	}
	if lbls := v.Get("labels"); lbls != "" {
		o.Labels = strings.Split(lbls, ",")
	}
	return o
}

func numFromPath(r *http.Request, key string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(key), 10, 64)
	return n
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
