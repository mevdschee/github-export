package store

import (
	"database/sql"

	"github.com/mevdschee/github-export/internal/ghmodel"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/jsonutil"
)

// UpsertRepo records repository metadata (single row) and the owner/repo in meta.
func (s *Store) UpsertRepo(owner, repo string, m map[string]any) error {
	if err := s.SetMeta("owner", owner); err != nil {
		return err
	}
	if err := s.SetMeta("repo", repo); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		INSERT INTO repository(id, owner, name, raw_json) VALUES(1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET owner=excluded.owner, name=excluded.name, raw_json=excluded.raw_json`,
		owner, repo, mustJSON(m))
	return err
}

// ReplaceLabels replaces the full label set (labels are always full-synced).
func (s *Store) ReplaceLabels(items []map[string]any) error {
	return s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM labels"); err != nil {
			return err
		}
		for i, m := range items {
			if _, err := tx.Exec(
				"INSERT INTO labels(name, color, description, raw_json, ord) VALUES(?, ?, ?, ?, ?)",
				jsonutil.Str(m, "name"), jsonutil.Str(m, "color"), jsonutil.Str(m, "description"),
				mustJSON(m), i); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReplaceMilestones replaces the full milestone set.
func (s *Store) ReplaceMilestones(items []map[string]any) error {
	return s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM milestones"); err != nil {
			return err
		}
		for i, m := range items {
			if _, err := tx.Exec(
				`INSERT INTO milestones(number, title, state, description, due_on, closed_at, raw_json, ord)
				 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
				jsonutil.Int(m, "number"), jsonutil.Str(m, "title"), jsonutil.Str(m, "state"),
				jsonutil.Str(m, "description"), jsonutil.Str(m, "due_on"), jsonutil.Str(m, "closed_at"),
				mustJSON(m), i); err != nil {
				return err
			}
		}
		return nil
	})
}

// UpsertIssue stores an issue or PR with its composed timeline and project
// cross-links. issue is the issues-API map, pr is the pulls-API detail map (nil
// for plain issues), timeline is the merged chronological timeline.
func (s *Store) UpsertIssue(number int64, isPR bool, issue, pr map[string]any, timeline []map[string]any, projects []string) error {
	var prJSON, timelineJSON any
	if pr != nil {
		prJSON = mustJSON(pr)
	}
	if timeline != nil {
		timelineJSON = mustJSON(timeline)
	}
	milestone := ""
	if ms := jsonutil.Map(issue, "milestone"); ms != nil {
		milestone = jsonutil.Str(ms, "title")
	}
	return s.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO issues(number, is_pull_request, title, state, state_reason, draft, locked, merged,
			                   created_at, updated_at, closed_at, author, milestone, body, raw_json, pr_json, timeline_json)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(number) DO UPDATE SET
				is_pull_request=excluded.is_pull_request, title=excluded.title, state=excluded.state,
				state_reason=excluded.state_reason, draft=excluded.draft, locked=excluded.locked,
				merged=excluded.merged, created_at=excluded.created_at, updated_at=excluded.updated_at,
				closed_at=excluded.closed_at, author=excluded.author, milestone=excluded.milestone,
				body=excluded.body, raw_json=excluded.raw_json, pr_json=excluded.pr_json,
				timeline_json=excluded.timeline_json`,
			number, boolInt(isPR), jsonutil.Str(issue, "title"), jsonutil.Str(issue, "state"),
			jsonutil.Str(issue, "state_reason"), boolInt(pr != nil && jsonutil.Bool(pr, "draft")),
			boolInt(jsonutil.Bool(issue, "locked")), boolInt(pr != nil && jsonutil.Bool(pr, "merged")),
			jsonutil.Str(issue, "created_at"), jsonutil.Str(issue, "updated_at"), jsonutil.Str(issue, "closed_at"),
			jsonutil.UserLogin(issue, "user"), milestone, jsonutil.Str(issue, "body"),
			mustJSON(issue), prJSON, timelineJSON)
		if err != nil {
			return err
		}
		if err := replaceJoin(tx, "issue_labels", "label", number, jsonutil.LabelNames(issue, "labels")); err != nil {
			return err
		}
		if err := replaceJoin(tx, "issue_assignees", "login", number, jsonutil.Logins(issue, "assignees")); err != nil {
			return err
		}
		return replaceJoin(tx, "issue_projects", "project", number, projects)
	})
}

// UpsertRelease stores a release row.
func (s *Store) UpsertRelease(m map[string]any) error {
	tag := jsonutil.Str(m, "tag_name")
	if tag == "" {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO releases(tag, name, draft, prerelease, author, created_at, published_at, target_commitish, body, raw_json, ord)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE((SELECT ord FROM releases WHERE tag=?), (SELECT COUNT(*) FROM releases)))
		ON CONFLICT(tag) DO UPDATE SET
			name=excluded.name, draft=excluded.draft, prerelease=excluded.prerelease, author=excluded.author,
			created_at=excluded.created_at, published_at=excluded.published_at,
			target_commitish=excluded.target_commitish, body=excluded.body, raw_json=excluded.raw_json`,
		tag, jsonutil.Str(m, "name"), boolInt(jsonutil.Bool(m, "draft")), boolInt(jsonutil.Bool(m, "prerelease")),
		jsonutil.UserLogin(m, "author"), jsonutil.Str(m, "created_at"), jsonutil.Str(m, "published_at"),
		jsonutil.Str(m, "target_commitish"), jsonutil.Str(m, "body"), mustJSON(m), tag)
	return err
}

// UpsertProject stores a project (open) with its items.
func (s *Store) UpsertProject(number int64, p map[string]any, items []map[string]any) error {
	var itemsJSON any
	if items != nil {
		itemsJSON = mustJSON(items)
	}
	state := "open"
	if jsonutil.Bool(p, "closed") {
		state = "closed"
	}
	_, err := s.db.Exec(`
		INSERT INTO projects(number, title, state, public, owner, url, description, created_at, updated_at, raw_json, items_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(number) DO UPDATE SET
			title=excluded.title, state=excluded.state, public=excluded.public, owner=excluded.owner,
			url=excluded.url, description=excluded.description, created_at=excluded.created_at,
			updated_at=excluded.updated_at, raw_json=excluded.raw_json, items_json=excluded.items_json`,
		number, jsonutil.Str(p, "title"), state, boolInt(jsonutil.Bool(p, "public")), ghmodel.OwnerLogin(p),
		jsonutil.Str(p, "url"), jsonutil.Str(p, "shortDescription"), jsonutil.Str(p, "createdAt"),
		jsonutil.Str(p, "updatedAt"), mustJSON(p), itemsJSON)
	return err
}

// DeleteProject removes a project row (used when a project is closed).
func (s *Store) DeleteProject(number int64) error {
	_, err := s.db.Exec("DELETE FROM projects WHERE number = ?", number)
	return err
}

// UpsertDiscussion stores a discussion node (re-marshaled to raw_json for
// faithful rendering).
func (s *Store) UpsertDiscussion(d ghmodel.DiscussionNode) error {
	stateReason := ""
	if d.StateReason != "" {
		stateReason = d.StateReason
	}
	_, err := s.db.Exec(`
		INSERT INTO discussions(number, title, category, state, state_reason, author, created_at, updated_at, closed_at, answer_id, body, raw_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(number) DO UPDATE SET
			title=excluded.title, category=excluded.category, state=excluded.state,
			state_reason=excluded.state_reason, author=excluded.author, created_at=excluded.created_at,
			updated_at=excluded.updated_at, closed_at=excluded.closed_at, answer_id=excluded.answer_id,
			body=excluded.body, raw_json=excluded.raw_json`,
		d.Number, d.Title, d.Category.Name, ghmodel.DiscussionState(d), stateReason, d.Author.Login,
		d.CreatedAt, d.UpdatedAt, d.ClosedAt, d.AnswerID(), d.Body, mustJSON(d))
	return err
}

// InsertEvents appends detected change events, stamped with detectedAt.
func (s *Store) InsertEvents(detectedAt string, events []hooks.Event) error {
	if len(events) == 0 {
		return nil
	}
	return s.tx(func(tx *sql.Tx) error {
		for _, ev := range events {
			var labelsJSON, extraJSON any
			if len(ev.Labels) > 0 {
				labelsJSON = mustJSON(ev.Labels)
			}
			if len(ev.Extra) > 0 {
				extraJSON = mustJSON(ev.Extra)
			}
			if _, err := tx.Exec(`
				INSERT INTO events(type, number, title, author, state, labels_json, file, repo, body, url, extra_json, detected_at)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ev.Type, ev.Number, ev.Title, ev.Author, ev.State, labelsJSON, ev.File, ev.Repo,
				ev.Body, ev.URL, extraJSON, detectedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// MarkEventsExported stamps the given event IDs as exported at the given time.
func (s *Store) MarkEventsExported(ids []int64, at string) error {
	return s.tx(func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec("UPDATE events SET exported_at = ? WHERE id = ?", at, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- helpers ---

func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func replaceJoin(tx *sql.Tx, table, col string, issueNumber int64, values []string) error {
	if _, err := tx.Exec("DELETE FROM "+table+" WHERE issue_number = ?", issueNumber); err != nil {
		return err
	}
	seen := map[string]bool{}
	ord := 0
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		if _, err := tx.Exec(
			"INSERT INTO "+table+"(issue_number, "+col+", ord) VALUES(?, ?, ?)",
			issueNumber, v, ord); err != nil {
			return err
		}
		ord++
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
