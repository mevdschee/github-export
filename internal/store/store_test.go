package store

import (
	"path/filepath"
	"testing"

	"github.com/mevdschee/github-export/internal/ghmodel"
	"github.com/mevdschee/github-export/internal/hooks"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateSetsVersion(t *testing.T) {
	s := openTemp(t)
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version=%d, want %d", v, schemaVersion)
	}
	// Re-opening an existing DB must be a no-op, not an error.
	if err := s.migrate(); err != nil {
		t.Errorf("second migrate: %v", err)
	}
}

func TestMeta(t *testing.T) {
	s := openTemp(t)
	if v, err := s.GetMeta("missing"); err != nil || v != "" {
		t.Errorf("GetMeta(missing)=%q,%v want \"\",nil", v, err)
	}
	if err := s.SetMeta("owner", "octocat"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta("owner", "octocat2"); err != nil { // upsert
		t.Fatal(err)
	}
	if v, _ := s.GetMeta("owner"); v != "octocat2" {
		t.Errorf("owner=%q, want octocat2", v)
	}
}

func TestUpsertIssueRoundTripAndState(t *testing.T) {
	s := openTemp(t)
	issue := map[string]any{
		"number":     float64(42),
		"title":      "Crash on empty input",
		"state":      "open",
		"user":       map[string]any{"login": "octocat"},
		"labels":     []any{map[string]any{"name": "bug"}, map[string]any{"name": "p1"}},
		"assignees":  []any{map[string]any{"login": "hubot"}},
		"milestone":  map[string]any{"title": "v2.1"},
		"body":       "boom",
		"created_at": "2024-01-01T00:00:00Z",
	}
	if err := s.UpsertIssue(42, false, issue, nil, nil, []string{"Roadmap"}); err != nil {
		t.Fatalf("UpsertIssue: %v", err)
	}

	exists, state, isPR, merged, err := s.IssueState(42)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || state != "open" || isPR || merged {
		t.Errorf("IssueState = exists=%v state=%q isPR=%v merged=%v", exists, state, isPR, merged)
	}

	rows, err := s.AllIssues()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("AllIssues len=%d, want 1", len(rows))
	}
	r := rows[0]
	if r.Number != 42 || r.IsPR {
		t.Errorf("row = number=%d isPR=%v", r.Number, r.IsPR)
	}
	if got := r.Issue["title"]; got != "Crash on empty input" {
		t.Errorf("title round-trip = %v", got)
	}
	if len(r.Projects) != 1 || r.Projects[0] != "Roadmap" {
		t.Errorf("projects = %v, want [Roadmap]", r.Projects)
	}

	// Idempotency + state transition: re-upsert as closed/merged PR.
	issue["state"] = "closed"
	pr := map[string]any{"merged": true, "draft": false}
	if err := s.UpsertIssue(42, true, issue, pr, nil, nil); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if rows, _ := s.AllIssues(); len(rows) != 1 {
		t.Errorf("after re-upsert AllIssues len=%d, want 1 (idempotent)", len(rows))
	}
	exists, state, isPR, merged, _ = s.IssueState(42)
	if !exists || state != "closed" || !isPR || !merged {
		t.Errorf("after transition: state=%q isPR=%v merged=%v", state, isPR, merged)
	}
	// projects join should have been cleared (passed nil this time).
	if rows, _ := s.AllIssues(); len(rows[0].Projects) != 0 {
		t.Errorf("projects not cleared on re-upsert: %v", rows[0].Projects)
	}

	if max, _ := s.MaxIssueNumber(); max != 42 {
		t.Errorf("MaxIssueNumber=%d, want 42", max)
	}
}

func TestLabelsAndMilestonesReplace(t *testing.T) {
	s := openTemp(t)
	if err := s.ReplaceLabels([]map[string]any{
		{"name": "bug", "color": "red", "description": "a bug"},
		{"name": "docs", "color": "blue"},
	}); err != nil {
		t.Fatal(err)
	}
	// Replace again with fewer — must not accumulate.
	if err := s.ReplaceLabels([]map[string]any{{"name": "bug", "color": "red"}}); err != nil {
		t.Fatal(err)
	}
	labels, err := s.AllLabels()
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels[0]["name"] != "bug" {
		t.Errorf("labels=%v, want single bug", labels)
	}

	if err := s.ReplaceMilestones([]map[string]any{
		{"number": float64(1), "title": "v1", "state": "closed"},
	}); err != nil {
		t.Fatal(err)
	}
	ms, _ := s.AllMilestones()
	if len(ms) != 1 || ms[0]["title"] != "v1" {
		t.Errorf("milestones=%v", ms)
	}
}

func TestReleaseState(t *testing.T) {
	s := openTemp(t)
	if exists, _, _ := mustReleaseState(t, s, "v1"); exists {
		t.Error("release should not exist yet")
	}
	if err := s.UpsertRelease(map[string]any{"tag_name": "v1", "prerelease": true}); err != nil {
		t.Fatal(err)
	}
	exists, pre, _ := mustReleaseState(t, s, "v1")
	if !exists || !pre {
		t.Errorf("state exists=%v prerelease=%v, want true,true", exists, pre)
	}
	// Promote to full release.
	if err := s.UpsertRelease(map[string]any{"tag_name": "v1", "prerelease": false}); err != nil {
		t.Fatal(err)
	}
	_, pre, _ = mustReleaseState(t, s, "v1")
	if pre {
		t.Error("prerelease should be false after promotion")
	}
	if rels, _ := s.AllReleases(); len(rels) != 1 {
		t.Errorf("AllReleases len=%d, want 1", len(rels))
	}
}

func mustReleaseState(t *testing.T, s *Store, tag string) (bool, bool, bool) {
	t.Helper()
	exists, pre, err := s.ReleaseState(tag)
	if err != nil {
		t.Fatal(err)
	}
	return exists, pre, true
}

func TestProjectUpsertDeleteAndItems(t *testing.T) {
	s := openTemp(t)
	if _, exists, _ := s.ProjectItems(1); exists {
		t.Error("project 1 should not exist")
	}
	items := []map[string]any{
		{"type": "ISSUE", "content": map[string]any{"__typename": "Issue", "number": float64(5), "title": "T"}},
	}
	p := map[string]any{"number": float64(1), "title": "Board", "closed": false}
	if err := s.UpsertProject(1, p, items); err != nil {
		t.Fatal(err)
	}
	got, exists, err := s.ProjectItems(1)
	if err != nil || !exists {
		t.Fatalf("ProjectItems exists=%v err=%v", exists, err)
	}
	if len(got) != 1 {
		t.Errorf("items len=%d, want 1", len(got))
	}
	if rows, _ := s.AllProjects(); len(rows) != 1 || rows[0].Number != 1 {
		t.Errorf("AllProjects=%v", rows)
	}
	if err := s.DeleteProject(1); err != nil {
		t.Fatal(err)
	}
	if _, exists, _ := s.ProjectItems(1); exists {
		t.Error("project 1 should be gone after delete")
	}
}

func TestDiscussionRoundTrip(t *testing.T) {
	s := openTemp(t)
	d := ghmodel.DiscussionNode{Number: 7, Title: "Q", Closed: false}
	d.Author.Login = "octocat"
	d.Category.Name = "Q&A"
	d.Comments.Nodes = []ghmodel.DiscussionComment{{DatabaseID: 100}}
	if err := s.UpsertDiscussion(d); err != nil {
		t.Fatal(err)
	}
	got, exists, err := s.DiscussionNode(7)
	if err != nil || !exists {
		t.Fatalf("DiscussionNode exists=%v err=%v", exists, err)
	}
	if got.Title != "Q" || got.Category.Name != "Q&A" || len(got.Comments.Nodes) != 1 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if all, _ := s.AllDiscussions(); len(all) != 1 {
		t.Errorf("AllDiscussions len=%d, want 1", len(all))
	}
}

func TestEventsLifecycle(t *testing.T) {
	s := openTemp(t)
	evs := []hooks.Event{
		{Type: "issue_created", Number: 1, Title: "A", Labels: []string{"bug"}, File: "issues/0001.md", Repo: "o/r"},
		{Type: "comment_created", Number: 1, Extra: map[string]string{"k": "v"}},
	}
	if err := s.InsertEvents("2024-01-01T00:00:00Z", evs); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending=%d, want 2", len(pending))
	}
	if pending[0].Event.Type != "issue_created" || len(pending[0].Event.Labels) != 1 {
		t.Errorf("event0=%+v", pending[0].Event)
	}
	if pending[1].Event.Extra["k"] != "v" {
		t.Errorf("event1 extra=%v", pending[1].Event.Extra)
	}

	ids := []int64{pending[0].ID, pending[1].ID}
	if err := s.MarkEventsExported(ids, "2024-01-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.PendingEvents(); len(again) != 0 {
		t.Errorf("pending after mark=%d, want 0", len(again))
	}
}
