// Package mcp exposes the synced store and local git clone through a Model
// Context Protocol server whose tool names and argument shapes mirror GitHub's
// official MCP server, so existing agent configs work unchanged. Read tools
// resolve against internal/query (and the git backend); write tools proxy to
// api.github.com via internal/writeproxy and re-sync the touched entity.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mevdschee/github-export/internal/gitbackend"
	"github.com/mevdschee/github-export/internal/query"
	"github.com/mevdschee/github-export/internal/writeproxy"
)

// Deps bundles what the tools need.
type Deps struct {
	Query    *query.Query
	Git      *gitbackend.Backend
	Proxy    *writeproxy.Proxy
	Owner    string
	Repo     string
	SyncedAt string
	ReadOnly bool
}

// NewServer builds an MCP server with GitHub-MCP-compatible tools registered.
func NewServer(d Deps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "github-export",
		Title:   "github-export (local GitHub mirror)",
		Version: "1.0.0",
	}, nil)

	t := &tools{d: d}
	t.registerReads(s)
	if !d.ReadOnly {
		t.registerWrites(s)
	}
	return s
}

type tools struct{ d Deps }

// --- helpers ---

func textResult(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errResult(err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

// rawResult wraps already-serialized JSON plus a freshness note.
func (t *tools) rawResult(doc json.RawMessage) *mcp.CallToolResult {
	text := string(doc)
	if t.d.SyncedAt != "" {
		text = fmt.Sprintf("// synced_at: %s\n%s", t.d.SyncedAt, text)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func (t *tools) rawArrayResult(items []json.RawMessage) *mcp.CallToolResult {
	if items == nil {
		items = []json.RawMessage{}
	}
	b, _ := json.Marshal(items)
	return t.rawResult(b)
}

func errResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}

func notFound(what string) *mcp.CallToolResult {
	return errResult(fmt.Errorf("%s not found in local store (try `github-export sync`)", what))
}

func argInt(args json.RawMessage, key string) int64 {
	var m map[string]any
	json.Unmarshal(args, &m)
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case string:
		var n int64
		fmt.Sscanf(v, "%d", &n)
		return n
	}
	return 0
}

func argStr(args json.RawMessage, key string) string {
	var m map[string]any
	json.Unmarshal(args, &m)
	s, _ := m[key].(string)
	return s
}

// schema builds a minimal JSON schema object for a tool's inputs.
func schema(props map[string]string, required ...string) json.RawMessage {
	p := map[string]any{}
	for name, typ := range props {
		p[name] = map[string]any{"type": typ}
	}
	obj := map[string]any{"type": "object", "properties": p}
	if len(required) > 0 {
		obj["required"] = required
	}
	b, _ := json.Marshal(obj)
	return b
}

func (t *tools) registerReads(s *mcp.Server) {
	s.AddTool(&mcp.Tool{
		Name:        "get_issue",
		Description: "Get a single issue by number from the local mirror.",
		InputSchema: schema(map[string]string{"owner": "string", "repo": "string", "issue_number": "integer"}, "issue_number"),
	}, t.getIssue)

	s.AddTool(&mcp.Tool{
		Name:        "list_issues",
		Description: "List issues (state: open|closed|all). Served from the local mirror.",
		InputSchema: schema(map[string]string{"owner": "string", "repo": "string", "state": "string", "labels": "string", "perPage": "integer", "page": "integer"}),
	}, t.listIssues)

	s.AddTool(&mcp.Tool{
		Name:        "get_pull_request",
		Description: "Get a single pull request by number from the local mirror.",
		InputSchema: schema(map[string]string{"owner": "string", "repo": "string", "pullNumber": "integer"}, "pullNumber"),
	}, t.getPull)

	s.AddTool(&mcp.Tool{
		Name:        "list_pull_requests",
		Description: "List pull requests (state: open|closed|all). Served from the local mirror.",
		InputSchema: schema(map[string]string{"owner": "string", "repo": "string", "state": "string", "perPage": "integer", "page": "integer"}),
	}, t.listPulls)

	s.AddTool(&mcp.Tool{
		Name:        "get_pull_request_reviews",
		Description: "List reviews on a pull request from the local mirror.",
		InputSchema: schema(map[string]string{"owner": "string", "repo": "string", "pullNumber": "integer"}, "pullNumber"),
	}, t.pullReviews)

	s.AddTool(&mcp.Tool{
		Name:        "search_issues",
		Description: "Search issues/PRs with GitHub qualifiers (is:, label:, author:, state:) plus free text, served from the local mirror.",
		InputSchema: schema(map[string]string{"query": "string", "perPage": "integer", "page": "integer"}, "query"),
	}, t.searchIssues)

	s.AddTool(&mcp.Tool{
		Name:        "list_releases",
		Description: "List releases from the local mirror.",
		InputSchema: schema(map[string]string{"owner": "string", "repo": "string"}),
	}, t.listReleases)

	s.AddTool(&mcp.Tool{
		Name:        "status",
		Description: "Report local store freshness (synced_at) and per-entity counts.",
		InputSchema: schema(map[string]string{}),
	}, t.status)

	if t.d.Git != nil {
		s.AddTool(&mcp.Tool{
			Name:        "get_file_contents",
			Description: "Get a file or directory from the local git clone.",
			InputSchema: schema(map[string]string{"owner": "string", "repo": "string", "path": "string", "ref": "string"}, "path"),
		}, t.getFileContents)

		s.AddTool(&mcp.Tool{
			Name:        "list_commits",
			Description: "List commits from the local git clone.",
			InputSchema: schema(map[string]string{"owner": "string", "repo": "string", "sha": "string", "perPage": "integer", "page": "integer"}),
		}, t.listCommits)

		s.AddTool(&mcp.Tool{
			Name:        "search_code",
			Description: "Search code in the local git clone (git grep).",
			InputSchema: schema(map[string]string{"query": "string", "ref": "string"}, "query"),
		}, t.searchCode)
	}
}

func (t *tools) getIssue(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	doc, ok, err := t.d.Query.GetIssue(argInt(req.Params.Arguments, "issue_number"))
	if err != nil {
		return errResult(err), nil
	}
	if !ok {
		return notFound("issue"), nil
	}
	return t.rawResult(doc), nil
}

func (t *tools) listIssues(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	o := query.ListIssuesOpts{
		State:   argStr(req.Params.Arguments, "state"),
		PerPage: int(argInt(req.Params.Arguments, "perPage")),
		Page:    int(argInt(req.Params.Arguments, "page")),
	}
	if lbls := argStr(req.Params.Arguments, "labels"); lbls != "" {
		o.Labels = splitCSV(lbls)
	}
	items, _, err := t.d.Query.ListIssues(o)
	if err != nil {
		return errResult(err), nil
	}
	return t.rawArrayResult(items), nil
}

func (t *tools) getPull(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	doc, ok, err := t.d.Query.GetPull(argInt(req.Params.Arguments, "pullNumber"))
	if err != nil {
		return errResult(err), nil
	}
	if !ok {
		return notFound("pull request"), nil
	}
	return t.rawResult(doc), nil
}

func (t *tools) listPulls(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	o := query.ListIssuesOpts{
		State: argStr(req.Params.Arguments, "state"), OnlyPulls: true,
		PerPage: int(argInt(req.Params.Arguments, "perPage")), Page: int(argInt(req.Params.Arguments, "page")),
	}
	items, _, err := t.d.Query.ListIssues(o)
	if err != nil {
		return errResult(err), nil
	}
	return t.rawArrayResult(items), nil
}

func (t *tools) pullReviews(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	items, err := t.d.Query.PullReviews(argInt(req.Params.Arguments, "pullNumber"))
	if err != nil {
		return errResult(err), nil
	}
	return t.rawArrayResult(items), nil
}

func (t *tools) searchIssues(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	items, total, err := t.d.Query.SearchIssues(argStr(req.Params.Arguments, "query"),
		int(argInt(req.Params.Arguments, "perPage")), int(argInt(req.Params.Arguments, "page")))
	if err != nil {
		return errResult(err), nil
	}
	raws := make([]any, len(items))
	for i, it := range items {
		raws[i] = json.RawMessage(it)
	}
	return textResult(map[string]any{"total_count": total, "items": raws}), nil
}

func (t *tools) listReleases(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	items, err := t.d.Query.ListReleases()
	if err != nil {
		return errResult(err), nil
	}
	return t.rawArrayResult(items), nil
}

func (t *tools) status(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	counts, err := t.d.Query.Counts()
	if err != nil {
		return errResult(err), nil
	}
	return textResult(map[string]any{
		"owner": t.d.Owner, "repo": t.d.Repo, "synced_at": t.d.SyncedAt, "counts": counts,
	}), nil
}

func (t *tools) getFileContents(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	doc, ok, err := t.d.Git.Contents(argStr(req.Params.Arguments, "path"), argStr(req.Params.Arguments, "ref"))
	if err != nil || !ok {
		return notFound("path"), nil
	}
	return textResult(doc), nil
}

func (t *tools) listCommits(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	commits, err := t.d.Git.Commits(argStr(req.Params.Arguments, "sha"),
		int(argInt(req.Params.Arguments, "perPage")), int(argInt(req.Params.Arguments, "page")))
	if err != nil {
		return errResult(err), nil
	}
	return textResult(commits), nil
}

func (t *tools) searchCode(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	results, err := t.d.Git.Grep(argStr(req.Params.Arguments, "query"), argStr(req.Params.Arguments, "ref"))
	if err != nil {
		return errResult(err), nil
	}
	return textResult(results), nil
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
