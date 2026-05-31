// Package graphqlmirror answers a subset of GitHub's GraphQL API from the local
// SQLite store. gh and the GitHub MCP issue many reads as GraphQL (and Projects
// v2 / Discussions are GraphQL-only), so a REST-only mirror is not enough.
//
// The design rule is strict: a query is served locally only if EVERY requested
// field is one the mirror knows how to resolve correctly. The moment any field,
// argument, or top-level shape is unsupported, Execute reports servedLocally
// false and the caller forwards the original request to api.github.com. We never
// return a half-correct GraphQL response.
package graphqlmirror

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"github.com/mevdschee/github-export/internal/query"
)

// errUnsupported signals that some part of the query cannot be served locally;
// the caller proxies the whole request.
var errUnsupported = errors.New("graphql: unsupported field for local mirror")

// Mirror resolves GraphQL queries against the store.
type Mirror struct {
	q     *query.Query
	owner string
	repo  string
}

// New builds a Mirror.
func New(q *query.Query, owner, repo string) *Mirror {
	return &Mirror{q: q, owner: owner, repo: repo}
}

// exec carries per-request state (fragments + variables) through resolution.
type exec struct {
	m         *Mirror
	fragments map[string]*ast.FragmentDefinition
	vars      map[string]any
}

// Execute resolves a GraphQL request. servedLocally is false when any part of
// the query is unsupported (the caller must then proxy to GitHub).
func (m *Mirror) Execute(queryStr string, variables map[string]any) (response json.RawMessage, servedLocally bool) {
	doc, err := parser.ParseQuery(&ast.Source{Input: queryStr})
	if err != nil || len(doc.Operations) != 1 {
		return nil, false
	}
	op := doc.Operations[0]
	if op.Operation != ast.Query {
		return nil, false // mutations/subscriptions always proxy
	}
	e := &exec{
		m:         m,
		fragments: map[string]*ast.FragmentDefinition{},
		vars:      variables,
	}
	for _, f := range doc.Fragments {
		e.fragments[f.Name] = f
	}

	data, err := e.resolveObject(op.SelectionSet, e.resolveRoot)
	if err != nil {
		return nil, false
	}
	out, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return nil, false
	}
	return out, true
}

// resolveObject walks a selection set, resolving each field with fn and keying
// the result by alias-or-name. Fragment spreads and inline fragments are
// flattened. Any unsupported field aborts the whole resolution.
func (e *exec) resolveObject(sel ast.SelectionSet, fn func(*ast.Field) (any, error)) (map[string]any, error) {
	out := map[string]any{}
	for _, s := range sel {
		switch f := s.(type) {
		case *ast.Field:
			if f.Name == "__typename" {
				continue // tolerated; value omitted
			}
			v, err := fn(f)
			if err != nil {
				return nil, err
			}
			out[aliasOf(f)] = v
		case *ast.FragmentSpread:
			frag := e.fragments[f.Name]
			if frag == nil {
				return nil, errUnsupported
			}
			sub, err := e.resolveObject(frag.SelectionSet, fn)
			if err != nil {
				return nil, err
			}
			for k, v := range sub {
				out[k] = v
			}
		case *ast.InlineFragment:
			sub, err := e.resolveObject(f.SelectionSet, fn)
			if err != nil {
				return nil, err
			}
			for k, v := range sub {
				out[k] = v
			}
		default:
			return nil, errUnsupported
		}
	}
	return out, nil
}

// --- root: only `repository(owner,name)` (and a tolerated rateLimit) ---

func (e *exec) resolveRoot(f *ast.Field) (any, error) {
	switch f.Name {
	case "repository":
		owner := e.argStr(f, "owner")
		name := e.argStr(f, "name")
		// A mismatch with the synced repo means we have no local data: proxy.
		if owner != e.m.owner || name != e.m.repo {
			return nil, errUnsupported
		}
		return e.resolveObject(f.SelectionSet, e.resolveRepository)
	default:
		return nil, errUnsupported
	}
}

func (e *exec) resolveRepository(f *ast.Field) (any, error) {
	switch f.Name {
	case "name":
		return e.m.repo, nil
	case "nameWithOwner":
		return e.m.owner + "/" + e.m.repo, nil
	case "owner", "login":
		if f.Name == "owner" {
			return e.resolveObject(f.SelectionSet, func(sf *ast.Field) (any, error) {
				if sf.Name == "login" {
					return e.m.owner, nil
				}
				return nil, errUnsupported
			})
		}
		return e.m.owner, nil
	case "issue":
		return e.resolveSingleIssue(f, false)
	case "pullRequest":
		return e.resolveSingleIssue(f, true)
	case "issueOrPullRequest":
		return e.resolveSingleIssue(f, false)
	case "discussion":
		return e.resolveSingleDiscussion(f)
	case "issues":
		return e.resolveIssueConnection(f, false)
	case "pullRequests":
		return e.resolveIssueConnection(f, true)
	case "discussions":
		return e.resolveDiscussionConnection(f)
	default:
		return nil, errUnsupported
	}
}

// --- single issue / PR ---

func (e *exec) resolveSingleIssue(f *ast.Field, wantPR bool) (any, error) {
	number := e.argInt(f, "number")
	if number == 0 {
		return nil, errUnsupported
	}
	var doc json.RawMessage
	var ok bool
	var err error
	if wantPR {
		doc, ok, err = e.m.q.GetPull(number)
	} else {
		doc, ok, err = e.m.q.GetIssue(number)
	}
	if err != nil {
		return nil, errUnsupported
	}
	if !ok {
		return nil, nil // GraphQL null for a missing node is valid
	}
	var issue map[string]any
	if err := json.Unmarshal(doc, &issue); err != nil {
		return nil, errUnsupported
	}
	return e.resolveObject(f.SelectionSet, e.issueFieldResolver(number, issue))
}

// issueFieldResolver maps GraphQL issue/PR fields onto the stored REST payload.
func (e *exec) issueFieldResolver(number int64, issue map[string]any) func(*ast.Field) (any, error) {
	return func(f *ast.Field) (any, error) {
		switch f.Name {
		case "number":
			return jsonNumber(issue["number"]), nil
		case "title":
			return issue["title"], nil
		case "body":
			return issue["body"], nil
		case "url", "permalink", "resourcePath":
			return issue["html_url"], nil
		case "state":
			return upperState(str(issue, "state")), nil
		case "closed":
			return str(issue, "state") == "closed", nil
		case "createdAt":
			return issue["created_at"], nil
		case "updatedAt":
			return issue["updated_at"], nil
		case "closedAt":
			return nullable(issue["closed_at"]), nil
		case "locked":
			return boolOf(issue["locked"]), nil
		case "isDraft":
			return boolOf(issue["draft"]), nil
		case "merged":
			return boolOf(issue["merged"]), nil
		case "mergedAt":
			return nullable(issue["merged_at"]), nil
		case "databaseId", "id":
			return jsonNumber(issue["id"]), nil
		case "author", "user":
			return e.resolveObject(f.SelectionSet, loginResolver(userLogin(issue, "user")))
		case "labels":
			return e.resolveLabelConnection(f, labelList(issue))
		case "assignees":
			return e.resolveLoginConnection(f, logins(issue, "assignees"))
		case "comments":
			return e.resolveCommentConnection(f, number)
		default:
			return nil, errUnsupported
		}
	}
}

// --- discussions (stored already in GraphQL shape) ---

func (e *exec) resolveSingleDiscussion(f *ast.Field) (any, error) {
	number := e.argInt(f, "number")
	if number == 0 {
		return nil, errUnsupported
	}
	doc, ok, err := e.m.q.GetDiscussion(number)
	if err != nil {
		return nil, errUnsupported
	}
	if !ok {
		return nil, nil
	}
	var node map[string]any
	if err := json.Unmarshal(doc, &node); err != nil {
		return nil, errUnsupported
	}
	return e.resolveObject(f.SelectionSet, e.genericResolver(node))
}

// genericResolver resolves fields by matching the GraphQL field name to the same
// key in an already-GraphQL-shaped source map (discussions, projects). Nested
// objects and connection-like {nodes} sub-structures are recursed into.
func (e *exec) genericResolver(src map[string]any) func(*ast.Field) (any, error) {
	return func(f *ast.Field) (any, error) {
		val, present := src[f.Name]
		if !present {
			// Common computed scalars we can derive.
			switch f.Name {
			case "id", "databaseId":
				return nullable(src["databaseId"]), nil
			}
			return nil, errUnsupported
		}
		if len(f.SelectionSet) == 0 {
			return val, nil
		}
		switch child := val.(type) {
		case map[string]any:
			return e.resolveObject(f.SelectionSet, e.genericResolver(child))
		case []any:
			var nodes []any
			for _, it := range child {
				m, _ := it.(map[string]any)
				if m == nil {
					return nil, errUnsupported
				}
				r, err := e.resolveObject(f.SelectionSet, e.genericResolver(m))
				if err != nil {
					return nil, err
				}
				nodes = append(nodes, r)
			}
			return nodes, nil
		case nil:
			return nil, nil
		default:
			return nil, errUnsupported
		}
	}
}

// --- connections ---

func (e *exec) resolveIssueConnection(f *ast.Field, wantPR bool) (any, error) {
	state, err := e.statesArg(f)
	if err != nil {
		return nil, err
	}
	first := e.argInt(f, "first")
	if first <= 0 {
		first = 30
	}
	offset := decodeCursor(e.argStr(f, "after"))
	page := offset/int(first) + 1

	opts := query.ListIssuesOpts{
		State: state, OnlyPulls: wantPR, OnlyIssues: !wantPR,
		PerPage: int(first), Page: page, Sort: "created", Direction: "desc",
	}
	items, total, err := e.m.q.ListIssues(opts)
	if err != nil {
		return nil, errUnsupported
	}
	return e.resolveConnection(f, total, offset, len(items), func(nodeField *ast.Field) (any, error) {
		var nodes []any
		for i, it := range items {
			var issue map[string]any
			if err := json.Unmarshal(it, &issue); err != nil {
				return nil, errUnsupported
			}
			r, err := e.resolveObject(nodeField.SelectionSet,
				e.issueFieldResolver(int64(jsonInt(issue["number"])), issue))
			if err != nil {
				return nil, err
			}
			_ = i
			nodes = append(nodes, r)
		}
		return nodes, nil
	})
}

func (e *exec) resolveDiscussionConnection(f *ast.Field) (any, error) {
	docs, err := e.m.q.ListDiscussions()
	if err != nil {
		return nil, errUnsupported
	}
	first := e.argInt(f, "first")
	if first <= 0 {
		first = 30
	}
	offset := decodeCursor(e.argStr(f, "after"))
	total := len(docs)
	end := offset + int(first)
	if end > total {
		end = total
	}
	start := offset
	if start > total {
		start = total
	}
	pageDocs := docs[start:end]
	return e.resolveConnection(f, total, offset, len(pageDocs), func(nodeField *ast.Field) (any, error) {
		var nodes []any
		for _, d := range pageDocs {
			var node map[string]any
			if err := json.Unmarshal(d, &node); err != nil {
				return nil, errUnsupported
			}
			r, err := e.resolveObject(nodeField.SelectionSet, e.genericResolver(node))
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, r)
		}
		return nodes, nil
	})
}

// resolveConnection assembles the standard {totalCount, nodes, edges, pageInfo}
// connection envelope. nodeBuilder produces the nodes array for a given nodes/
// edges field.
func (e *exec) resolveConnection(f *ast.Field, total, offset, pageLen int, nodeBuilder func(*ast.Field) (any, error)) (any, error) {
	hasNext := offset+pageLen < total
	endCursor := encodeCursor(offset + pageLen)
	return e.resolveObject(f.SelectionSet, func(cf *ast.Field) (any, error) {
		switch cf.Name {
		case "totalCount":
			return total, nil
		case "nodes":
			return nodeBuilder(cf)
		case "edges":
			nodesAny, err := nodeBuilder(cf)
			if err != nil {
				return nil, err
			}
			nodes, _ := nodesAny.([]any)
			var edges []any
			for i, n := range nodes {
				edge, err := e.resolveObject(cf.SelectionSet, func(ef *ast.Field) (any, error) {
					switch ef.Name {
					case "node":
						return n, nil
					case "cursor":
						return encodeCursor(offset + i + 1), nil
					}
					return nil, errUnsupported
				})
				if err != nil {
					return nil, err
				}
				edges = append(edges, edge)
			}
			return edges, nil
		case "pageInfo":
			return e.resolveObject(cf.SelectionSet, func(pf *ast.Field) (any, error) {
				switch pf.Name {
				case "hasNextPage":
					return hasNext, nil
				case "hasPreviousPage":
					return offset > 0, nil
				case "endCursor":
					return endCursor, nil
				case "startCursor":
					return encodeCursor(offset), nil
				}
				return nil, errUnsupported
			})
		default:
			return nil, errUnsupported
		}
	})
}

func (e *exec) resolveLabelConnection(f *ast.Field, labels []map[string]any) (any, error) {
	return e.resolveObject(f.SelectionSet, func(cf *ast.Field) (any, error) {
		switch cf.Name {
		case "totalCount":
			return len(labels), nil
		case "nodes":
			var nodes []any
			for _, l := range labels {
				r, err := e.resolveObject(cf.SelectionSet, func(lf *ast.Field) (any, error) {
					switch lf.Name {
					case "name":
						return l["name"], nil
					case "color":
						return l["color"], nil
					case "description":
						return l["description"], nil
					}
					return nil, errUnsupported
				})
				if err != nil {
					return nil, err
				}
				nodes = append(nodes, r)
			}
			return nodes, nil
		case "pageInfo":
			return e.resolveObject(cf.SelectionSet, falsePageInfo)
		}
		return nil, errUnsupported
	})
}

func (e *exec) resolveLoginConnection(f *ast.Field, logins []string) (any, error) {
	return e.resolveObject(f.SelectionSet, func(cf *ast.Field) (any, error) {
		switch cf.Name {
		case "totalCount":
			return len(logins), nil
		case "nodes":
			var nodes []any
			for _, login := range logins {
				r, err := e.resolveObject(cf.SelectionSet, loginFieldFn(login))
				if err != nil {
					return nil, err
				}
				nodes = append(nodes, r)
			}
			return nodes, nil
		case "pageInfo":
			return e.resolveObject(cf.SelectionSet, falsePageInfo)
		}
		return nil, errUnsupported
	})
}

func (e *exec) resolveCommentConnection(f *ast.Field, number int64) (any, error) {
	comments, err := e.m.q.IssueComments(number)
	if err != nil {
		return nil, errUnsupported
	}
	return e.resolveObject(f.SelectionSet, func(cf *ast.Field) (any, error) {
		switch cf.Name {
		case "totalCount":
			return len(comments), nil
		case "nodes":
			var nodes []any
			for _, c := range comments {
				var cm map[string]any
				if err := json.Unmarshal(c, &cm); err != nil {
					return nil, errUnsupported
				}
				r, err := e.resolveObject(cf.SelectionSet, func(commentField *ast.Field) (any, error) {
					switch commentField.Name {
					case "body":
						return cm["body"], nil
					case "createdAt":
						return cm["created_at"], nil
					case "author", "user":
						return e.resolveObject(commentField.SelectionSet, loginResolver(userLogin(cm, "user")))
					}
					return nil, errUnsupported
				})
				if err != nil {
					return nil, err
				}
				nodes = append(nodes, r)
			}
			return nodes, nil
		case "pageInfo":
			return e.resolveObject(cf.SelectionSet, falsePageInfo)
		}
		return nil, errUnsupported
	})
}

// --- small resolvers / helpers ---

func loginResolver(login string) func(*ast.Field) (any, error) {
	return loginFieldFn(login)
}

func loginFieldFn(login string) func(*ast.Field) (any, error) {
	return func(f *ast.Field) (any, error) {
		switch f.Name {
		case "login":
			return login, nil
		case "name":
			return login, nil
		}
		return nil, errUnsupported
	}
}

func falsePageInfo(f *ast.Field) (any, error) {
	switch f.Name {
	case "hasNextPage", "hasPreviousPage":
		return false, nil
	case "endCursor", "startCursor":
		return nil, nil
	}
	return nil, errUnsupported
}

func (e *exec) statesArg(f *ast.Field) (string, error) {
	v := e.arg(f, "states")
	if v == nil {
		return "all", nil
	}
	list, ok := v.([]any)
	if !ok {
		return "", errUnsupported
	}
	hasOpen, hasClosed, hasMerged := false, false, false
	for _, s := range list {
		switch strings.ToUpper(fmt.Sprint(s)) {
		case "OPEN":
			hasOpen = true
		case "CLOSED":
			hasClosed = true
		case "MERGED":
			hasMerged = true
		default:
			return "", errUnsupported
		}
	}
	switch {
	case hasOpen && (hasClosed || hasMerged):
		return "all", nil
	case hasOpen:
		return "open", nil
	case hasClosed || hasMerged:
		return "closed", nil
	}
	return "all", nil
}

func aliasOf(f *ast.Field) string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
}

func (e *exec) arg(f *ast.Field, name string) any {
	a := f.Arguments.ForName(name)
	if a == nil {
		return nil
	}
	v, err := a.Value.Value(e.vars)
	if err != nil {
		return nil
	}
	return v
}

func (e *exec) argStr(f *ast.Field, name string) string {
	v := e.arg(f, name)
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func (e *exec) argInt(f *ast.Field, name string) int64 {
	v := e.arg(f, name)
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

func encodeCursor(offset int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(offset)))
}

func decodeCursor(s string) int {
	if s == "" {
		return 0
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0
	}
	_, num, ok := strings.Cut(string(b), ":")
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(num)
	return n
}

func upperState(s string) string { return strings.ToUpper(s) }

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func nullable(v any) any { return v }

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

func jsonNumber(v any) any {
	switch n := v.(type) {
	case float64:
		return int64(n)
	default:
		return v
	}
}

func jsonInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

func userLogin(m map[string]any, key string) string {
	u, _ := m[key].(map[string]any)
	if u == nil {
		return ""
	}
	return str(u, "login")
}

func labelList(issue map[string]any) []map[string]any {
	raw, _ := issue["labels"].([]any)
	var out []map[string]any
	for _, l := range raw {
		if m, ok := l.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func logins(issue map[string]any, key string) []string {
	raw, _ := issue[key].([]any)
	var out []string
	for _, u := range raw {
		if m, ok := u.(map[string]any); ok {
			if l := str(m, "login"); l != "" {
				out = append(out, l)
			}
		}
	}
	return out
}
