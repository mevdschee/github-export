package graphqlmirror

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/mevdschee/github-export/internal/ghmodel"
	"github.com/mevdschee/github-export/internal/query"
	"github.com/mevdschee/github-export/internal/store"
)

func newMirror(t *testing.T) *Mirror {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.UpsertRepo("octocat", "hello", map[string]any{
		"full_name": "octocat/hello", "description": "a test repo",
		"html_url": "https://github.com/octocat/hello", "default_branch": "main",
		"private": false,
	})
	s.UpsertIssue(1, false, map[string]any{
		"number": float64(1), "title": "Bug", "state": "open",
		"user": map[string]any{"login": "alice"}, "body": "broken",
		"labels":     []any{map[string]any{"name": "bug", "color": "red"}},
		"created_at": "2024-01-01T00:00:00Z",
	}, nil, []map[string]any{
		{"event": "commented", "body": "thanks", "user": map[string]any{"login": "bob"}, "created_at": "2024-01-03T00:00:00Z"},
	}, nil)
	s.UpsertIssue(2, true, map[string]any{
		"number": float64(2), "title": "Feature", "state": "closed",
		"user": map[string]any{"login": "carol"}, "created_at": "2024-02-01T00:00:00Z",
	}, map[string]any{"number": float64(2), "title": "Feature", "state": "closed", "merged": true, "user": map[string]any{"login": "carol"}}, nil, nil)

	d := ghmodel.DiscussionNode{Number: 5, Title: "How?", Body: "halp"}
	d.Author.Login = "dave"
	d.Category.Name = "Q&A"
	s.UpsertDiscussion(d)

	s.UpsertProject(3, map[string]any{
		"id": "PVT_x", "number": float64(3), "title": "Roadmap", "closed": false,
		"shortDescription": "the plan",
		"fields": map[string]any{"nodes": []any{
			map[string]any{"name": "Status", "dataType": "SINGLE_SELECT"},
		}},
	}, nil)

	return New(query.New(s), "octocat", "hello")
}

func run(t *testing.T, m *Mirror, q string, vars map[string]any) (map[string]any, bool) {
	t.Helper()
	resp, ok := m.Execute(q, vars)
	if !ok {
		return nil, false
	}
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, resp)
	}
	return out.Data, true
}

func TestSingleIssue(t *testing.T) {
	m := newMirror(t)
	data, ok := run(t, m, `{
		repository(owner:"octocat", name:"hello") {
			issue(number:1) { number title state author { login } }
		}
	}`, nil)
	if !ok {
		t.Fatal("expected local resolution")
	}
	issue := data["repository"].(map[string]any)["issue"].(map[string]any)
	if issue["title"] != "Bug" || issue["state"] != "OPEN" {
		t.Errorf("issue=%v", issue)
	}
	if issue["author"].(map[string]any)["login"] != "alice" {
		t.Errorf("author=%v", issue["author"])
	}
}

func TestIssueWithVariables(t *testing.T) {
	m := newMirror(t)
	data, ok := run(t, m, `query($n:Int!){
		repository(owner:"octocat", name:"hello") { issue(number:$n) { title } }
	}`, map[string]any{"n": 1})
	if !ok {
		t.Fatal("expected local resolution")
	}
	if data["repository"].(map[string]any)["issue"].(map[string]any)["title"] != "Bug" {
		t.Errorf("data=%v", data)
	}
}

func TestIssueLabelsAndComments(t *testing.T) {
	m := newMirror(t)
	data, ok := run(t, m, `{
		repository(owner:"octocat", name:"hello") {
			issue(number:1) {
				labels(first:10){ totalCount nodes { name color } }
				comments(first:10){ totalCount nodes { body author { login } } }
			}
		}
	}`, nil)
	if !ok {
		t.Fatal("expected local resolution")
	}
	issue := data["repository"].(map[string]any)["issue"].(map[string]any)
	labels := issue["labels"].(map[string]any)
	if labels["totalCount"].(float64) != 1 {
		t.Errorf("labels=%v", labels)
	}
	comments := issue["comments"].(map[string]any)
	nodes := comments["nodes"].([]any)
	if len(nodes) != 1 || nodes[0].(map[string]any)["body"] != "thanks" {
		t.Errorf("comments=%v", comments)
	}
}

func TestIssuesConnection(t *testing.T) {
	m := newMirror(t)
	data, ok := run(t, m, `{
		repository(owner:"octocat", name:"hello") {
			issues(first:10, states:[OPEN,CLOSED]) {
				totalCount
				nodes { number }
				pageInfo { hasNextPage endCursor }
			}
		}
	}`, nil)
	if !ok {
		t.Fatal("expected local resolution")
	}
	conn := data["repository"].(map[string]any)["issues"].(map[string]any)
	if conn["totalCount"].(float64) != 1 { // only the non-PR issue
		t.Errorf("issues totalCount=%v, want 1", conn["totalCount"])
	}
}

func TestPullRequestsConnection(t *testing.T) {
	m := newMirror(t)
	data, ok := run(t, m, `{
		repository(owner:"octocat", name:"hello") {
			pullRequests(first:10, states:[MERGED,CLOSED]) { totalCount nodes { number merged } }
		}
	}`, nil)
	if !ok {
		t.Fatal("expected local resolution")
	}
	conn := data["repository"].(map[string]any)["pullRequests"].(map[string]any)
	if conn["totalCount"].(float64) != 1 {
		t.Errorf("pulls totalCount=%v", conn["totalCount"])
	}
}

func TestDiscussion(t *testing.T) {
	m := newMirror(t)
	data, ok := run(t, m, `{
		repository(owner:"octocat", name:"hello") {
			discussion(number:5) { number title author { login } category { name } }
		}
	}`, nil)
	if !ok {
		t.Fatal("expected local resolution")
	}
	d := data["repository"].(map[string]any)["discussion"].(map[string]any)
	if d["title"] != "How?" || d["author"].(map[string]any)["login"] != "dave" {
		t.Errorf("discussion=%v", d)
	}
	if d["category"].(map[string]any)["name"] != "Q&A" {
		t.Errorf("category=%v", d["category"])
	}
}

func TestProjectV2(t *testing.T) {
	m := newMirror(t)
	data, ok := run(t, m, `{
		repository(owner:"octocat", name:"hello") {
			projectV2(number:3) { number title closed fields(first:10){ nodes { name dataType } } }
		}
	}`, nil)
	if !ok {
		t.Fatal("expected local resolution")
	}
	p := data["repository"].(map[string]any)["projectV2"].(map[string]any)
	if p["title"] != "Roadmap" || p["closed"] != false {
		t.Errorf("project=%v", p)
	}
	fields := p["fields"].(map[string]any)["nodes"].([]any)
	if len(fields) != 1 || fields[0].(map[string]any)["name"] != "Status" {
		t.Errorf("fields=%v", fields)
	}
}

func TestProjectsV2Connection(t *testing.T) {
	m := newMirror(t)
	data, ok := run(t, m, `{
		repository(owner:"octocat", name:"hello") {
			projectsV2(first:10){ totalCount nodes { number title } }
		}
	}`, nil)
	if !ok {
		t.Fatal("expected local resolution")
	}
	conn := data["repository"].(map[string]any)["projectsV2"].(map[string]any)
	if conn["totalCount"].(float64) != 1 {
		t.Errorf("projectsV2 totalCount=%v", conn["totalCount"])
	}
}

func TestRepositoryScalars(t *testing.T) {
	m := newMirror(t)
	data, ok := run(t, m, `{
		repository(owner:"octocat", name:"hello") {
			name nameWithOwner description isPrivate
			owner { login }
			defaultBranchRef { name }
		}
	}`, nil)
	if !ok {
		t.Fatal("expected local resolution")
	}
	repo := data["repository"].(map[string]any)
	if repo["name"] != "hello" || repo["description"] != "a test repo" || repo["isPrivate"] != false {
		t.Errorf("repo scalars=%v", repo)
	}
	if repo["owner"].(map[string]any)["login"] != "octocat" {
		t.Errorf("owner=%v", repo["owner"])
	}
	if repo["defaultBranchRef"].(map[string]any)["name"] != "main" {
		t.Errorf("defaultBranchRef=%v", repo["defaultBranchRef"])
	}
}

func TestUnsupportedFieldFallsThrough(t *testing.T) {
	m := newMirror(t)
	// `mergeStateStatus` is not mapped → whole query must proxy.
	_, ok := m.Execute(`{
		repository(owner:"octocat", name:"hello") {
			issue(number:1) { title mergeStateStatus }
		}
	}`, nil)
	if ok {
		t.Error("expected fallthrough (servedLocally=false) for unsupported field")
	}
}

func TestUnknownRepoFallsThrough(t *testing.T) {
	m := newMirror(t)
	_, ok := m.Execute(`{ repository(owner:"other", name:"repo") { name } }`, nil)
	if ok {
		t.Error("expected fallthrough for a repo we have not synced")
	}
}

func TestMutationFallsThrough(t *testing.T) {
	m := newMirror(t)
	_, ok := m.Execute(`mutation { addComment(input:{}) { clientMutationId } }`, nil)
	if ok {
		t.Error("expected mutations to proxy")
	}
}

func TestFragmentSupport(t *testing.T) {
	m := newMirror(t)
	data, ok := run(t, m, `
		fragment IssueFields on Issue { number title }
		{ repository(owner:"octocat", name:"hello") { issue(number:1) { ...IssueFields } } }
	`, nil)
	if !ok {
		t.Fatal("expected fragments to resolve locally")
	}
	issue := data["repository"].(map[string]any)["issue"].(map[string]any)
	if issue["title"] != "Bug" {
		t.Errorf("fragment issue=%v", issue)
	}
}
