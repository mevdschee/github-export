package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerWrites adds the write tools (only when not in read-only mode). They
// proxy to api.github.com and then re-sync the touched entity into the store.
func (t *tools) registerWrites(s *mcp.Server) {
	s.AddTool(&mcp.Tool{
		Name:        "create_issue",
		Description: "Create an issue. Proxies to GitHub and re-syncs the repo.",
		InputSchema: schema(map[string]string{"owner": "string", "repo": "string", "title": "string", "body": "string"}, "title"),
	}, t.createIssue)

	s.AddTool(&mcp.Tool{
		Name:        "add_issue_comment",
		Description: "Add a comment to an issue or PR. Proxies to GitHub and re-syncs the issue.",
		InputSchema: schema(map[string]string{"owner": "string", "repo": "string", "issue_number": "integer", "body": "string"}, "issue_number", "body"),
	}, t.addIssueComment)

	s.AddTool(&mcp.Tool{
		Name:        "update_issue",
		Description: "Update an issue (title/body/state). Proxies to GitHub and re-syncs the issue.",
		InputSchema: schema(map[string]string{"owner": "string", "repo": "string", "issue_number": "integer", "title": "string", "body": "string", "state": "string"}, "issue_number"),
	}, t.updateIssue)

	s.AddTool(&mcp.Tool{
		Name:        "update_pull_request",
		Description: "Update a pull request (title/body/state). Proxies to GitHub and re-syncs the PR.",
		InputSchema: schema(map[string]string{"owner": "string", "repo": "string", "pullNumber": "integer", "title": "string", "body": "string", "state": "string"}, "pullNumber"),
	}, t.updatePull)

	s.AddTool(&mcp.Tool{
		Name:        "merge_pull_request",
		Description: "Merge a pull request. Proxies to GitHub and re-syncs the PR.",
		InputSchema: schema(map[string]string{"owner": "string", "repo": "string", "pullNumber": "integer", "merge_method": "string", "commit_title": "string"}, "pullNumber"),
	}, t.mergePull)
}

func (t *tools) ownerRepo(req *mcp.CallToolRequest) (string, string) {
	owner := argStr(req.Params.Arguments, "owner")
	repo := argStr(req.Params.Arguments, "repo")
	if owner == "" {
		owner = t.d.Owner
	}
	if repo == "" {
		repo = t.d.Repo
	}
	return owner, repo
}

func (t *tools) post(ctx context.Context, method, path string, payload map[string]any) (*mcp.CallToolResult, error) {
	body, _ := json.Marshal(payload)
	status, resp, err := t.d.Proxy.Request(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return errResult(err), nil
	}
	if status >= 400 {
		return errResult(fmt.Errorf("GitHub returned %d: %s", status, string(resp))), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(resp)}}}, nil
}

func (t *tools) createIssue(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, repo := t.ownerRepo(req)
	payload := map[string]any{"title": argStr(req.Params.Arguments, "title")}
	if b := argStr(req.Params.Arguments, "body"); b != "" {
		payload["body"] = b
	}
	return t.post(ctx, "POST", fmt.Sprintf("/repos/%s/%s/issues", owner, repo), payload)
}

func (t *tools) addIssueComment(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, repo := t.ownerRepo(req)
	n := argInt(req.Params.Arguments, "issue_number")
	payload := map[string]any{"body": argStr(req.Params.Arguments, "body")}
	return t.post(ctx, "POST", fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, n), payload)
}

func (t *tools) updateIssue(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, repo := t.ownerRepo(req)
	n := argInt(req.Params.Arguments, "issue_number")
	payload := map[string]any{}
	for _, k := range []string{"title", "body", "state"} {
		if v := argStr(req.Params.Arguments, k); v != "" {
			payload[k] = v
		}
	}
	return t.post(ctx, "PATCH", fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, n), payload)
}

func (t *tools) updatePull(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, repo := t.ownerRepo(req)
	n := argInt(req.Params.Arguments, "pullNumber")
	payload := map[string]any{}
	for _, k := range []string{"title", "body", "state"} {
		if v := argStr(req.Params.Arguments, k); v != "" {
			payload[k] = v
		}
	}
	return t.post(ctx, "PATCH", fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, n), payload)
}

func (t *tools) mergePull(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, repo := t.ownerRepo(req)
	n := argInt(req.Params.Arguments, "pullNumber")
	payload := map[string]any{}
	if v := argStr(req.Params.Arguments, "merge_method"); v != "" {
		payload["merge_method"] = v
	}
	if v := argStr(req.Params.Arguments, "commit_title"); v != "" {
		payload["commit_title"] = v
	}
	return t.post(ctx, "PUT", fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, repo, n), payload)
}
