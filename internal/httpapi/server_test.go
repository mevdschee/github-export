package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mevdschee/github-export/internal/query"
	"github.com/mevdschee/github-export/internal/store"
	"github.com/mevdschee/github-export/internal/writeproxy"
)

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.UpsertRepo("octocat", "hello", map[string]any{"full_name": "octocat/hello", "default_branch": "main"}); err != nil {
		t.Fatal(err)
	}
	s.SetMeta("synced_at", "2024-05-01T00:00:00Z")

	// One open issue with a comment in its timeline, one closed PR.
	issue := map[string]any{
		"number": float64(1), "title": "Bug report", "state": "open",
		"user": map[string]any{"login": "alice"}, "body": "something is broken",
		"labels": []any{map[string]any{"name": "bug"}}, "created_at": "2024-01-01T00:00:00Z",
		"updated_at": "2024-01-02T00:00:00Z",
	}
	timeline := []map[string]any{
		{"event": "commented", "id": float64(99), "body": "thanks", "user": map[string]any{"login": "bob"}, "created_at": "2024-01-03T00:00:00Z"},
	}
	if err := s.UpsertIssue(1, false, issue, nil, timeline, []string{"Roadmap"}); err != nil {
		t.Fatal(err)
	}
	pr := map[string]any{
		"number": float64(2), "title": "Add feature", "state": "closed",
		"user": map[string]any{"login": "carol"}, "merged": true, "created_at": "2024-02-01T00:00:00Z",
		"updated_at": "2024-02-02T00:00:00Z",
	}
	prDetail := map[string]any{"number": float64(2), "merged": true, "draft": false, "title": "Add feature", "state": "closed"}
	prTimeline := []map[string]any{
		{"event": "reviewed", "id": float64(7), "state": "APPROVED", "body": "lgtm", "user": map[string]any{"login": "dave"}, "submitted_at": "2024-02-01T12:00:00Z"},
	}
	if err := s.UpsertIssue(2, true, pr, prDetail, prTimeline, nil); err != nil {
		t.Fatal(err)
	}
	s.ReplaceLabels([]map[string]any{{"name": "bug", "color": "red"}})

	proxy := writeproxy.New("", nil)
	proxy.Disabled = true
	srv := New(Config{Query: query.New(s), Proxy: proxy, Owner: "octocat", Repo: "hello", SyncedAt: "2024-05-01T00:00:00Z"})
	return srv, s
}

func get(t *testing.T, srv *Server, path string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

func TestRepoEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	res := get(t, srv, "/repos/octocat/hello")
	if res.StatusCode != 200 {
		t.Fatalf("status=%d", res.StatusCode)
	}
	if res.Header.Get("X-GitHub-Export-Synced-At") != "2024-05-01T00:00:00Z" {
		t.Errorf("missing freshness header: %v", res.Header)
	}
	var m map[string]any
	json.NewDecoder(res.Body).Decode(&m)
	if m["full_name"] != "octocat/hello" {
		t.Errorf("repo=%v", m)
	}
}

func TestListIssuesDefaultOpenOnly(t *testing.T) {
	srv, _ := newTestServer(t)
	res := get(t, srv, "/repos/octocat/hello/issues")
	var items []map[string]any
	json.NewDecoder(res.Body).Decode(&items)
	if len(items) != 1 || items[0]["number"].(float64) != 1 {
		t.Errorf("default issues listing = %v, want only open #1", items)
	}
}

func TestListIssuesStateAll(t *testing.T) {
	srv, _ := newTestServer(t)
	res := get(t, srv, "/repos/octocat/hello/issues?state=all")
	var items []map[string]any
	json.NewDecoder(res.Body).Decode(&items)
	if len(items) != 2 {
		t.Errorf("state=all len=%d, want 2", len(items))
	}
}

func TestListIssuesLabelFilter(t *testing.T) {
	srv, _ := newTestServer(t)
	res := get(t, srv, "/repos/octocat/hello/issues?state=all&labels=bug")
	var items []map[string]any
	json.NewDecoder(res.Body).Decode(&items)
	if len(items) != 1 || items[0]["number"].(float64) != 1 {
		t.Errorf("label filter = %v", items)
	}
}

func TestPullsOnly(t *testing.T) {
	srv, _ := newTestServer(t)
	res := get(t, srv, "/repos/octocat/hello/pulls?state=all")
	var items []map[string]any
	json.NewDecoder(res.Body).Decode(&items)
	if len(items) != 1 || items[0]["number"].(float64) != 2 {
		t.Errorf("pulls = %v, want only #2", items)
	}
}

func TestIssueComments(t *testing.T) {
	srv, _ := newTestServer(t)
	res := get(t, srv, "/repos/octocat/hello/issues/1/comments")
	var items []map[string]any
	json.NewDecoder(res.Body).Decode(&items)
	if len(items) != 1 || items[0]["body"] != "thanks" {
		t.Errorf("comments = %v", items)
	}
	if _, hasEvent := items[0]["event"]; hasEvent {
		t.Errorf("synthetic event marker leaked into comment: %v", items[0])
	}
}

func TestPullReviews(t *testing.T) {
	srv, _ := newTestServer(t)
	res := get(t, srv, "/repos/octocat/hello/pulls/2/reviews")
	var items []map[string]any
	json.NewDecoder(res.Body).Decode(&items)
	if len(items) != 1 || items[0]["state"] != "APPROVED" {
		t.Errorf("reviews = %v", items)
	}
}

func TestSearchIssues(t *testing.T) {
	srv, _ := newTestServer(t)
	res := get(t, srv, "/search/issues?q=broken")
	var out struct {
		TotalCount int              `json:"total_count"`
		Items      []map[string]any `json:"items"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	if out.TotalCount != 1 || len(out.Items) != 1 || out.Items[0]["number"].(float64) != 1 {
		t.Errorf("search = %+v", out)
	}
}

func TestSearchQualifier(t *testing.T) {
	srv, _ := newTestServer(t)
	res := get(t, srv, "/search/issues?q=is:pr")
	var out struct {
		TotalCount int `json:"total_count"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	if out.TotalCount != 1 {
		t.Errorf("is:pr search total=%d, want 1", out.TotalCount)
	}
}

func TestStatus(t *testing.T) {
	srv, _ := newTestServer(t)
	res := get(t, srv, "/status")
	var st struct {
		Owner  string         `json:"owner"`
		Counts map[string]int `json:"counts"`
	}
	json.NewDecoder(res.Body).Decode(&st)
	if st.Owner != "octocat" || st.Counts["issues"] != 1 || st.Counts["pulls"] != 1 {
		t.Errorf("status=%+v", st)
	}
}

func TestOpenAPIServed(t *testing.T) {
	srv, _ := newTestServer(t)
	res := get(t, srv, "/openapi.yaml")
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/yaml" {
		t.Errorf("openapi status=%d ct=%s", res.StatusCode, res.Header.Get("Content-Type"))
	}
}

func TestPaginationLinkHeader(t *testing.T) {
	srv, _ := newTestServer(t)
	res := get(t, srv, "/repos/octocat/hello/issues?state=all&per_page=1&page=1")
	link := res.Header.Get("Link")
	if link == "" {
		t.Errorf("expected Link header for paginated result, got none")
	}
}

func TestProxyDisabledFallthrough(t *testing.T) {
	srv, _ := newTestServer(t)
	// Unmirrored path → proxy, which is disabled → 501.
	res := get(t, srv, "/repos/octocat/hello/actions/runs")
	if res.StatusCode != http.StatusNotImplemented {
		t.Errorf("status=%d, want 501 (proxy disabled)", res.StatusCode)
	}
}
