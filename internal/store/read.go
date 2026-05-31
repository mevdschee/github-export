package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/mevdschee/github-export/internal/ghmodel"
	"github.com/mevdschee/github-export/internal/hooks"
)

// --- prior-state reads for change detection (called before upsert) ---

// IssueState returns the stored state for an issue/PR. exists is false when the
// issue has never been synced.
func (s *Store) IssueState(number int64) (exists bool, state string, isPR, merged bool, err error) {
	var st string
	var pr, mg int
	row := s.db.QueryRow("SELECT state, is_pull_request, merged FROM issues WHERE number = ?", number)
	switch err = row.Scan(&st, &pr, &mg); err {
	case sql.ErrNoRows:
		return false, "", false, false, nil
	case nil:
		return true, st, pr != 0, mg != 0, nil
	default:
		return false, "", false, false, err
	}
}

// ReleaseState reports whether a release with the given tag exists and, if so,
// whether it was previously a prerelease.
func (s *Store) ReleaseState(tag string) (exists, prerelease bool, err error) {
	var pr int
	switch err = s.db.QueryRow("SELECT prerelease FROM releases WHERE tag = ?", tag).Scan(&pr); err {
	case sql.ErrNoRows:
		return false, false, nil
	case nil:
		return true, pr != 0, nil
	default:
		return false, false, err
	}
}

// DiscussionNode returns the previously stored discussion, or exists=false.
func (s *Store) DiscussionNode(number int64) (d ghmodel.DiscussionNode, exists bool, err error) {
	var raw string
	switch err = s.db.QueryRow("SELECT raw_json FROM discussions WHERE number = ?", number).Scan(&raw); err {
	case sql.ErrNoRows:
		return d, false, nil
	case nil:
		if err = json.Unmarshal([]byte(raw), &d); err != nil {
			return d, false, fmt.Errorf("decoding stored discussion #%d: %w", number, err)
		}
		return d, true, nil
	default:
		return d, false, err
	}
}

// ProjectItems returns the previously stored item maps for a project, or
// exists=false if the project has never been synced.
func (s *Store) ProjectItems(number int64) (items []map[string]any, exists bool, err error) {
	var raw sql.NullString
	switch err = s.db.QueryRow("SELECT items_json FROM projects WHERE number = ?", number).Scan(&raw); err {
	case sql.ErrNoRows:
		return nil, false, nil
	case nil:
		if raw.Valid && raw.String != "" {
			if err = json.Unmarshal([]byte(raw.String), &items); err != nil {
				return nil, true, fmt.Errorf("decoding stored items for project #%d: %w", number, err)
			}
		}
		return items, true, nil
	default:
		return nil, false, err
	}
}

// SearchFTS returns the issue/PR numbers whose title or body match the FTS5
// query, ordered by relevance (best first). An empty or malformed query returns
// (nil, false) so the caller can fall back to a non-FTS path.
func (s *Store) SearchFTS(ftsQuery string) (numbers []int64, ok bool, err error) {
	if ftsQuery == "" {
		return nil, false, nil
	}
	rows, err := s.db.Query(
		"SELECT rowid FROM fts_issues WHERE fts_issues MATCH ? ORDER BY rank", ftsQuery)
	if err != nil {
		// A malformed MATCH expression is a user-input issue, not a store error:
		// signal "no FTS result" so the caller can degrade gracefully.
		return nil, false, nil
	}
	defer rows.Close()
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, false, err
		}
		numbers = append(numbers, n)
	}
	return numbers, true, rows.Err()
}

// ProjectsForIssue returns the stored project cross-links for an issue/PR in
// sync order (used to preserve them on a targeted re-sync).
func (s *Store) ProjectsForIssue(number int64) ([]string, error) {
	rows, err := s.db.Query("SELECT project FROM issue_projects WHERE issue_number = ? ORDER BY ord", number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MaxIssueNumber returns the largest stored issue/PR number (0 if none).
func (s *Store) MaxIssueNumber() (int64, error) {
	var n sql.NullInt64
	if err := s.db.QueryRow("SELECT MAX(number) FROM issues").Scan(&n); err != nil {
		return 0, err
	}
	return n.Int64, nil
}

// --- export reads ---

// IssueRow is a fully-rehydrated issue/PR ready for rendering.
type IssueRow struct {
	Number   int64
	IsPR     bool
	Issue    map[string]any
	PR       map[string]any
	Timeline []map[string]any
	Projects []string
}

// ProjectRow is a rehydrated project plus its items.
type ProjectRow struct {
	Number  int64
	Project map[string]any
	Items   []map[string]any
}

// PendingEvent pairs an event with its row ID so it can be marked exported.
type PendingEvent struct {
	ID    int64
	Event hooks.Event
}

// ExportRepo returns the stored repo metadata and the last sync timestamp.
func (s *Store) ExportRepo() (owner, repo string, m map[string]any, syncedAt string, err error) {
	var raw string
	err = s.db.QueryRow("SELECT owner, name, raw_json FROM repository WHERE id = 1").Scan(&owner, &repo, &raw)
	if err == sql.ErrNoRows {
		return "", "", nil, "", nil
	}
	if err != nil {
		return "", "", nil, "", err
	}
	if err = json.Unmarshal([]byte(raw), &m); err != nil {
		return "", "", nil, "", err
	}
	syncedAt, err = s.SyncedAt()
	return owner, repo, m, syncedAt, err
}

func (s *Store) rawMaps(query string) ([]map[string]any, error) {
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AllLabels returns label maps in sync order.
func (s *Store) AllLabels() ([]map[string]any, error) {
	return s.rawMaps("SELECT raw_json FROM labels ORDER BY ord")
}

// AllMilestones returns milestone maps in sync order.
func (s *Store) AllMilestones() ([]map[string]any, error) {
	return s.rawMaps("SELECT raw_json FROM milestones ORDER BY ord")
}

// AllReleases returns release maps in sync order.
func (s *Store) AllReleases() ([]map[string]any, error) {
	return s.rawMaps("SELECT raw_json FROM releases ORDER BY ord")
}

// AllIssues returns every issue/PR with its timeline and project cross-links,
// ordered by number.
func (s *Store) AllIssues() ([]IssueRow, error) {
	projects, err := s.issueProjects()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT number, is_pull_request, raw_json, pr_json, timeline_json FROM issues ORDER BY number")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IssueRow
	for rows.Next() {
		var (
			number   int64
			isPR     int
			rawJSON  string
			prJSON   sql.NullString
			timeJSON sql.NullString
		)
		if err := rows.Scan(&number, &isPR, &rawJSON, &prJSON, &timeJSON); err != nil {
			return nil, err
		}
		ir := IssueRow{Number: number, IsPR: isPR != 0, Projects: projects[number]}
		if err := json.Unmarshal([]byte(rawJSON), &ir.Issue); err != nil {
			return nil, err
		}
		if prJSON.Valid && prJSON.String != "" {
			if err := json.Unmarshal([]byte(prJSON.String), &ir.PR); err != nil {
				return nil, err
			}
		}
		if timeJSON.Valid && timeJSON.String != "" {
			if err := json.Unmarshal([]byte(timeJSON.String), &ir.Timeline); err != nil {
				return nil, err
			}
		}
		out = append(out, ir)
	}
	return out, rows.Err()
}

func (s *Store) issueProjects() (map[int64][]string, error) {
	rows, err := s.db.Query("SELECT issue_number, project FROM issue_projects ORDER BY issue_number, ord")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var n int64
		var p string
		if err := rows.Scan(&n, &p); err != nil {
			return nil, err
		}
		out[n] = append(out[n], p)
	}
	return out, rows.Err()
}

// AllProjects returns every stored (open) project with its items, ordered by number.
func (s *Store) AllProjects() ([]ProjectRow, error) {
	rows, err := s.db.Query("SELECT number, raw_json, items_json FROM projects ORDER BY number")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectRow
	for rows.Next() {
		var (
			number    int64
			rawJSON   string
			itemsJSON sql.NullString
		)
		if err := rows.Scan(&number, &rawJSON, &itemsJSON); err != nil {
			return nil, err
		}
		pr := ProjectRow{Number: number}
		if err := json.Unmarshal([]byte(rawJSON), &pr.Project); err != nil {
			return nil, err
		}
		if itemsJSON.Valid && itemsJSON.String != "" {
			if err := json.Unmarshal([]byte(itemsJSON.String), &pr.Items); err != nil {
				return nil, err
			}
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

// AllDiscussions returns every stored discussion node, ordered by number.
func (s *Store) AllDiscussions() ([]ghmodel.DiscussionNode, error) {
	rows, err := s.db.Query("SELECT raw_json FROM discussions ORDER BY number")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ghmodel.DiscussionNode
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var d ghmodel.DiscussionNode
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PendingEvents returns events that have not yet been written to an events/ file.
func (s *Store) PendingEvents() ([]PendingEvent, error) {
	rows, err := s.db.Query(`
		SELECT id, type, number, title, author, state, labels_json, file, repo, body, url, extra_json
		FROM events WHERE exported_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingEvent
	for rows.Next() {
		var (
			pe                                          PendingEvent
			number                                      sql.NullInt64
			labelsJSON, extraJSON                       sql.NullString
			typ, title, author, state, file, repo, body string
			url                                         string
		)
		if err := rows.Scan(&pe.ID, &typ, &number, &title, &author, &state,
			&labelsJSON, &file, &repo, &body, &url, &extraJSON); err != nil {
			return nil, err
		}
		ev := hooks.Event{
			Type: typ, Number: number.Int64, Title: title, Author: author,
			State: state, File: file, Repo: repo, Body: body, URL: url,
		}
		if labelsJSON.Valid && labelsJSON.String != "" {
			if err := json.Unmarshal([]byte(labelsJSON.String), &ev.Labels); err != nil {
				return nil, err
			}
		}
		if extraJSON.Valid && extraJSON.String != "" {
			if err := json.Unmarshal([]byte(extraJSON.String), &ev.Extra); err != nil {
				return nil, err
			}
		}
		pe.Event = ev
		out = append(out, pe)
	}
	return out, rows.Err()
}
