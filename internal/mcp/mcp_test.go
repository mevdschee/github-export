package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mevdschee/github-export/internal/query"
	"github.com/mevdschee/github-export/internal/store"
	"github.com/mevdschee/github-export/internal/writeproxy"
)

func setup(t *testing.T, readOnly bool) *sdk.ClientSession {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.UpsertRepo("octocat", "hello", map[string]any{"full_name": "octocat/hello"})
	s.SetMeta("synced_at", "2024-05-01T00:00:00Z")
	s.UpsertIssue(1, false, map[string]any{
		"number": float64(1), "title": "Bug", "state": "open",
		"user": map[string]any{"login": "alice"}, "body": "broken",
	}, nil, []map[string]any{
		{"event": "commented", "body": "me too", "user": map[string]any{"login": "bob"}, "created_at": "2024-01-03T00:00:00Z"},
	}, nil)

	proxy := writeproxy.New("", nil)
	proxy.Disabled = true
	srv := NewServer(Deps{
		Query: query.New(s), Proxy: proxy, Owner: "octocat", Repo: "hello",
		SyncedAt: "2024-05-01T00:00:00Z", ReadOnly: readOnly,
	})

	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callText(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("%s returned no content", name)
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("%s content not text: %T", name, res.Content[0])
	}
	return tc.Text
}

func TestToolNamesMatchGitHubMCP(t *testing.T) {
	cs := setup(t, false)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{
		"get_issue", "get_issue_comments", "list_issues", "get_pull_request",
		"list_pull_requests", "get_pull_request_reviews", "get_pull_request_comments",
		"list_discussions", "get_discussion", "get_discussion_comments",
		"search_issues", "create_issue", "add_issue_comment", "update_issue",
		"update_pull_request", "merge_pull_request", "status",
	} {
		if !got[want] {
			t.Errorf("missing tool %q (have %v)", want, keys(got))
		}
	}
}

func TestGetIssueTool(t *testing.T) {
	cs := setup(t, false)
	text := callText(t, cs, "get_issue", map[string]any{"issue_number": 1})
	if !strings.Contains(text, "synced_at: 2024-05-01") {
		t.Errorf("missing freshness note: %s", text)
	}
	// Strip the synced_at comment line before parsing.
	_, jsonPart, _ := strings.Cut(text, "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(jsonPart), &m); err != nil {
		t.Fatalf("parse: %v\n%s", err, jsonPart)
	}
	if m["title"] != "Bug" {
		t.Errorf("issue title=%v", m["title"])
	}
}

func TestListIssuesTool(t *testing.T) {
	cs := setup(t, false)
	text := callText(t, cs, "list_issues", map[string]any{"state": "all"})
	if !strings.Contains(text, "Bug") {
		t.Errorf("list_issues missing issue: %s", text)
	}
}

func TestIssueCommentsTool(t *testing.T) {
	cs := setup(t, false)
	text := callText(t, cs, "get_issue_comments", map[string]any{"issue_number": 1})
	if !strings.Contains(text, "me too") {
		t.Errorf("get_issue_comments missing comment: %s", text)
	}
}

func TestReadOnlyHidesWrites(t *testing.T) {
	cs := setup(t, true)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "create_issue" || tool.Name == "update_issue" {
			t.Errorf("read-only server exposed write tool %q", tool.Name)
		}
	}
}

func TestWriteToolProxyDisabled(t *testing.T) {
	cs := setup(t, false)
	// proxy is disabled in the test setup → create_issue surfaces an error result.
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "create_issue", Arguments: map[string]any{"title": "x"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError for disabled proxy write")
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
