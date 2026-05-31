// Package query is the backend-agnostic read layer over the SQLite store. It
// returns data in GitHub's REST JSON shape (straight from the stored raw_json
// where possible) so the HTTP API, MCP server, and native CLI subcommands can
// all be thin adapters over it.
package query

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strings"

	"github.com/mevdschee/github-export/internal/store"
)

// Query wraps the store's database handle with read-only, GitHub-shaped queries.
type Query struct {
	db *sql.DB
	s  *store.Store
}

// New builds a Query over an open store.
func New(s *store.Store) *Query {
	return &Query{db: s.DB(), s: s}
}

// Store returns the underlying store (for meta reads).
func (q *Query) Store() *store.Store { return q.s }

// raw is a pre-serialized JSON document read from the store.
type raw = json.RawMessage

// ListIssuesOpts filters and paginates an issue/PR listing. Zero values mean
// "unset" and fall back to GitHub's defaults.
type ListIssuesOpts struct {
	State     string // open (default), closed, all
	Labels    []string
	Creator   string // author login
	Assignee  string // assignee login, or "none"/"*"
	Milestone string // milestone title, or "none"/"*"
	Since     string // ISO8601; updated_at >= since
	Sort      string // created (default), updated, comments
	Direction string // asc, desc (default desc)
	PerPage   int
	Page      int
	// OnlyPulls/OnlyIssues narrow the set; both false means GitHub's /issues
	// behaviour (issues and PRs together).
	OnlyPulls  bool
	OnlyIssues bool
}

func (o ListIssuesOpts) perPage() int {
	if o.PerPage <= 0 {
		return 30
	}
	if o.PerPage > 100 {
		return 100
	}
	return o.PerPage
}

func (o ListIssuesOpts) page() int {
	if o.Page <= 0 {
		return 1
	}
	return o.Page
}

// ListIssues returns issues/PRs matching opts, plus the total count of matches
// (before pagination) for Link-header construction.
func (q *Query) ListIssues(o ListIssuesOpts) ([]raw, int, error) {
	var where []string
	var args []any

	switch o.State {
	case "", "open":
		where = append(where, "state = 'open'")
	case "closed":
		where = append(where, "state = 'closed'")
	case "all":
		// no filter
	default:
		where = append(where, "state = ?")
		args = append(args, o.State)
	}
	if o.OnlyPulls {
		where = append(where, "is_pull_request = 1")
	}
	if o.OnlyIssues {
		where = append(where, "is_pull_request = 0")
	}
	if o.Creator != "" {
		where = append(where, "author = ?")
		args = append(args, o.Creator)
	}
	if o.Milestone != "" && o.Milestone != "*" {
		if o.Milestone == "none" {
			where = append(where, "(milestone IS NULL OR milestone = '')")
		} else {
			where = append(where, "milestone = ?")
			args = append(args, o.Milestone)
		}
	}
	if o.Since != "" {
		where = append(where, "updated_at >= ?")
		args = append(args, o.Since)
	}
	for _, lbl := range o.Labels {
		where = append(where, "number IN (SELECT issue_number FROM issue_labels WHERE label = ?)")
		args = append(args, lbl)
	}
	if o.Assignee != "" && o.Assignee != "*" {
		if o.Assignee == "none" {
			where = append(where, "number NOT IN (SELECT issue_number FROM issue_assignees)")
		} else {
			where = append(where, "number IN (SELECT issue_number FROM issue_assignees WHERE login = ?)")
			args = append(args, o.Assignee)
		}
	}

	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := q.db.QueryRow("SELECT COUNT(*) FROM issues"+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order := orderBy(o.Sort, o.Direction)
	limit := o.perPage()
	offset := (o.page() - 1) * limit
	// For pulls-only listings prefer the richer pr_json (has merged/draft/etc).
	col := "raw_json"
	if o.OnlyPulls {
		col = "COALESCE(pr_json, raw_json)"
	}
	rows, err := q.db.Query(
		"SELECT "+col+" FROM issues"+clause+order+" LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out, err := scanRaw(rows)
	return out, total, err
}

func orderBy(sortField, dir string) string {
	col := "created_at"
	switch sortField {
	case "updated":
		col = "updated_at"
	case "created", "":
		col = "created_at"
	case "number":
		col = "number"
	}
	d := "DESC"
	if strings.EqualFold(dir, "asc") {
		d = "ASC"
	}
	return " ORDER BY " + col + " " + d
}

// GetIssue returns the stored issue/PR by number in the /issues/{n} shape.
func (q *Query) GetIssue(number int64) (raw, bool, error) {
	return q.scalarRaw("SELECT raw_json FROM issues WHERE number = ?", number)
}

// GetPull returns the PR detail (/pulls/{n} shape) for a number, falling back to
// the issue raw_json if PR details were not synced.
func (q *Query) GetPull(number int64) (raw, bool, error) {
	return q.scalarRaw("SELECT COALESCE(pr_json, raw_json) FROM issues WHERE number = ? AND is_pull_request = 1", number)
}

// timelineEntries returns the merged timeline for an issue/PR.
func (q *Query) timelineEntries(number int64) ([]map[string]any, error) {
	var tj sql.NullString
	err := q.db.QueryRow("SELECT timeline_json FROM issues WHERE number = ?", number).Scan(&tj)
	if err == sql.ErrNoRows || !tj.Valid || tj.String == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(tj.String), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// IssueComments returns the comment objects on an issue/PR in the GitHub
// /issues/{n}/comments shape (the synthetic "event" marker is stripped).
func (q *Query) IssueComments(number int64) ([]raw, error) {
	tl, err := q.timelineEntries(number)
	if err != nil {
		return nil, err
	}
	var out []raw
	for _, e := range tl {
		if e["event"] != "commented" {
			continue
		}
		m := cloneWithout(e, "event")
		out = append(out, mustMarshal(m))
	}
	return out, nil
}

// IssueTimeline returns the full merged timeline for an issue/PR.
func (q *Query) IssueTimeline(number int64) ([]raw, error) {
	tl, err := q.timelineEntries(number)
	if err != nil {
		return nil, err
	}
	out := make([]raw, 0, len(tl))
	for _, e := range tl {
		out = append(out, mustMarshal(e))
	}
	return out, nil
}

// PullReviews returns PR reviews (the /pulls/{n}/reviews shape) extracted from
// the stored timeline.
func (q *Query) PullReviews(number int64) ([]raw, error) {
	tl, err := q.timelineEntries(number)
	if err != nil {
		return nil, err
	}
	var out []raw
	for _, e := range tl {
		if e["event"] != "reviewed" {
			continue
		}
		out = append(out, mustMarshal(cloneWithout(e, "event")))
	}
	return out, nil
}

// PullReviewComments returns the inline review comments (/pulls/{n}/comments
// shape) extracted from the stored timeline's line-commented groups.
func (q *Query) PullReviewComments(number int64) ([]raw, error) {
	tl, err := q.timelineEntries(number)
	if err != nil {
		return nil, err
	}
	var out []raw
	for _, e := range tl {
		if e["event"] != "line-commented" {
			continue
		}
		for _, c := range toList(e["comments"]) {
			if cm, ok := c.(map[string]any); ok {
				out = append(out, mustMarshal(cm))
			}
		}
	}
	return out, nil
}

// --- simple full-collection reads ---

func (q *Query) ListLabels() ([]raw, error) {
	return q.queryRaw("SELECT raw_json FROM labels ORDER BY ord")
}

func (q *Query) ListMilestones() ([]raw, error) {
	return q.queryRaw("SELECT raw_json FROM milestones ORDER BY ord")
}

func (q *Query) ListReleases() ([]raw, error) {
	return q.queryRaw("SELECT raw_json FROM releases ORDER BY ord")
}

func (q *Query) GetRelease(tag string) (raw, bool, error) {
	return q.scalarRaw("SELECT raw_json FROM releases WHERE tag = ?", tag)
}

func (q *Query) ListDiscussions() ([]raw, error) {
	return q.queryRaw("SELECT raw_json FROM discussions ORDER BY number")
}

func (q *Query) GetDiscussion(number int64) (raw, bool, error) {
	return q.scalarRaw("SELECT raw_json FROM discussions WHERE number = ?", number)
}

func (q *Query) ListProjects() ([]raw, error) {
	return q.queryRaw("SELECT raw_json FROM projects ORDER BY number")
}

func (q *Query) GetProject(number int64) (raw, bool, error) {
	return q.scalarRaw("SELECT raw_json FROM projects WHERE number = ?", number)
}

// Repo returns stored repository metadata.
func (q *Query) Repo() (raw, bool, error) {
	return q.scalarRaw("SELECT raw_json FROM repository WHERE id = 1")
}

// --- search ---

// SearchIssues runs a GitHub-style issue search. The query string may contain
// qualifiers (is:, state:, label:, author:, assignee:, milestone:, in:) plus
// free text matched against title/body. Returns matches and total count.
func (q *Query) SearchIssues(queryStr string, perPage, page int) ([]raw, int, error) {
	o := ListIssuesOpts{State: "all", PerPage: perPage, Page: page}
	var terms []string
	for _, tok := range strings.Fields(queryStr) {
		if k, v, ok := splitQualifier(tok); ok {
			switch k {
			case "is", "type":
				switch v {
				case "pr", "pull-request":
					o.OnlyPulls = true
				case "issue":
					o.OnlyIssues = true
				case "open", "closed":
					o.State = v
				case "merged":
					o.OnlyPulls = true
					o.State = "all"
				}
			case "state":
				o.State = v
			case "label":
				o.Labels = append(o.Labels, strings.Trim(v, `"`))
			case "author":
				o.Creator = v
			case "assignee":
				o.Assignee = v
			case "milestone":
				o.Milestone = strings.Trim(v, `"`)
			case "sort":
				o.Sort = v
			case "in":
				// in:title / in:body — narrowing handled implicitly by free text
			default:
				// unknown qualifier: treat as free text
				terms = append(terms, tok)
			}
			continue
		}
		terms = append(terms, tok)
	}

	// Build the base filtered set, then apply free-text matching in SQL.
	matches, _, err := q.ListIssues(ListIssuesOpts{
		State: o.State, Labels: o.Labels, Creator: o.Creator, Assignee: o.Assignee,
		Milestone: o.Milestone, OnlyPulls: o.OnlyPulls, OnlyIssues: o.OnlyIssues,
		Sort: o.Sort, PerPage: 100000, Page: 1,
	})
	if err != nil {
		return nil, 0, err
	}

	text := strings.ToLower(strings.Join(terms, " "))
	var filtered []raw
	if text == "" {
		filtered = matches
	} else {
		for _, m := range matches {
			var doc map[string]any
			if err := json.Unmarshal(m, &doc); err != nil {
				continue
			}
			hay := strings.ToLower(str(doc, "title") + "\n" + str(doc, "body"))
			if strings.Contains(hay, text) {
				filtered = append(filtered, m)
			}
		}
	}

	total := len(filtered)
	pp := perPage
	if pp <= 0 {
		pp = 30
	}
	pg := page
	if pg <= 0 {
		pg = 1
	}
	start := (pg - 1) * pp
	if start > total {
		start = total
	}
	end := start + pp
	if end > total {
		end = total
	}
	return filtered[start:end], total, nil
}

// --- status ---

// Counts returns row counts per major entity for the /status endpoint.
func (q *Query) Counts() (map[string]int, error) {
	out := map[string]int{}
	for label, table := range map[string]string{
		"issues":      "issues WHERE is_pull_request = 0",
		"pulls":       "issues WHERE is_pull_request = 1",
		"labels":      "labels",
		"milestones":  "milestones",
		"releases":    "releases",
		"projects":    "projects",
		"discussions": "discussions",
		"events":      "events",
	} {
		var n int
		if err := q.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			return nil, err
		}
		out[label] = n
	}
	return out, nil
}

// --- helpers ---

func (q *Query) queryRaw(sqlStr string, args ...any) ([]raw, error) {
	rows, err := q.db.Query(sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRaw(rows)
}

func (q *Query) scalarRaw(sqlStr string, args ...any) (raw, bool, error) {
	var s string
	switch err := q.db.QueryRow(sqlStr, args...).Scan(&s); err {
	case sql.ErrNoRows:
		return nil, false, nil
	case nil:
		return raw(s), true, nil
	default:
		return nil, false, err
	}
}

func scanRaw(rows *sql.Rows) ([]raw, error) {
	var out []raw
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, raw(s))
	}
	return out, rows.Err()
}

func splitQualifier(tok string) (key, val string, ok bool) {
	i := strings.IndexByte(tok, ':')
	if i <= 0 || i == len(tok)-1 {
		return "", "", false
	}
	return strings.ToLower(tok[:i]), tok[i+1:], true
}

func cloneWithout(m map[string]any, drop ...string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	for _, d := range drop {
		delete(out, d)
	}
	return out
}

func mustMarshal(v any) raw {
	b, err := json.Marshal(v)
	if err != nil {
		return raw("null")
	}
	return raw(b)
}

func toList(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// SortedTags returns release tags sorted for stable output (used by tests).
func (q *Query) SortedTags() ([]string, error) {
	rows, err := q.db.Query("SELECT tag FROM releases")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	sort.Strings(tags)
	return tags, rows.Err()
}
