// End-to-end tests for the exporter. Runs a real sync against the TEST
// fixtures on github.com/mevdschee/github-export and asserts that the exported
// files contain the expected data.
//
// Skipped when GITHUB_TOKEN is unset or when -short is passed.
//
// All fixtures used by these tests are TEST-prefixed in the live repo so
// nobody mistakes them for real issues/PRs. See docs/test-fixtures.md for the
// fixture catalogue.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mevdschee/github-export/internal/exporter"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/store"
	"github.com/mevdschee/github-export/internal/sync"
	"gopkg.in/yaml.v3"
)

const (
	testOwner = "mevdschee"
	testRepo  = "github-export"
)

// projectsSynced records whether Projects v2 data made it into the store. It is
// false when the token lacks the read:project scope, in which case the
// project-coupled assertions are skipped rather than reported as failures.
var projectsSynced bool

// TestE2E runs a real full sync of the TEST fixtures into a SQLite store, then
// asserts on the store contents (the source of truth) and on a one-way markdown
// export of that store. The export assertions parse the files structurally, so
// they tolerate format evolution.
func TestE2E(t *testing.T) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Skip("GITHUB_TOKEN not set; skipping E2E test")
	}
	if testing.Short() {
		t.Skip("-short passed; skipping E2E test (hits the live GitHub API)")
	}

	out := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "e2e.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	c := github.NewClient(token)
	syncStart := time.Now().UTC().Format(time.RFC3339)
	if _, err := sync.Run(c, s, testOwner, testRepo, "", syncStart); err != nil {
		t.Fatalf("sync.Run: %v", err)
	}
	if err := exporter.Export(s, out); err != nil {
		t.Fatalf("exporter.Export: %v", err)
	}

	if projs, _ := s.AllProjects(); len(projs) > 0 {
		projectsSynced = true
	} else {
		t.Log("no projects synced (token likely lacks read:project scope); skipping project assertions")
	}

	t.Run("Store", func(t *testing.T) { checkStore(t, s) })
	t.Run("Labels", func(t *testing.T) { checkLabels(t, out) })
	t.Run("Milestones", func(t *testing.T) { checkMilestones(t, out) })
	t.Run("Issue1_OpenWithFullMetadata", func(t *testing.T) { checkIssue1(t, out) })
	t.Run("Issue2_ClosedCompleted_SubIssueChild", func(t *testing.T) { checkIssue2(t, out) })
	t.Run("Issue3_ClosedNotPlanned", func(t *testing.T) { checkIssue3(t, out) })
	t.Run("PR4_MergedWithComment", func(t *testing.T) { checkPR4(t, out) })
	t.Run("PR5_OpenWithRequestedReviewer", func(t *testing.T) { checkPR5(t, out) })
	t.Run("PR6_ClosedUnmerged", func(t *testing.T) { checkPR6(t, out) })
	t.Run("Project1_ItemsAndStatusField", func(t *testing.T) { checkProject1(t, out) })
	t.Run("ReleaseTESTv001_WithBody", func(t *testing.T) { checkReleaseTEST(t, out) })
	t.Run("RepoYml", func(t *testing.T) { checkRepoYml(t, out) })
	t.Run("Discussion7_QAWithAnswer", func(t *testing.T) { checkDiscussion7(t, out) })
	t.Run("Discussion8_GeneralPlain", func(t *testing.T) { checkDiscussion8(t, out) })
}

func checkDiscussion7(t *testing.T, out string) {
	docs := parseDocs(t, filepath.Join(out, "discussions", "0007.md"))
	if len(docs) == 0 {
		t.Fatal("no docs in discussions/0007.md")
	}
	fr := docs[0].front
	if toInt(fr["number"]) != 7 {
		t.Errorf("number=%v, want 7", fr["number"])
	}
	if fr["type"] != "discussion" {
		t.Errorf("type=%v, want discussion", fr["type"])
	}
	if fr["state"] != "open" {
		t.Errorf("state=%v, want open", fr["state"])
	}
	if fr["category"] != "Q&A" {
		t.Errorf("category=%v, want Q&A", fr["category"])
	}
	if fr["author"] != "mevdschee" {
		t.Errorf("author=%v, want mevdschee", fr["author"])
	}
	if toInt(fr["answer_id"]) == 0 {
		t.Errorf("answer_id missing or zero: %v", fr["answer_id"])
	}
	if s, _ := fr["answer_chosen_at"].(string); !strings.Contains(s, "T") {
		t.Errorf("answer_chosen_at malformed: %q", s)
	}
	if fr["answer_chosen_by"] != "mevdschee" {
		t.Errorf("answer_chosen_by=%v, want mevdschee", fr["answer_chosen_by"])
	}
	// Single discussion-body assertion for the suite.
	if !strings.Contains(docs[0].body, "TEST Q&A discussion") {
		t.Errorf("body missing seeded marker; got %q", docs[0].body)
	}

	// One comment, marked as answer.
	var foundAnswerComment, foundReply bool
	var commentID int
	for _, d := range docs[1:] {
		switch d.front["document"] {
		case "comment":
			if d.front["is_answer"] == true {
				foundAnswerComment = true
				commentID = toInt(d.front["id"])
			}
		case "reply":
			foundReply = true
			if pid := toInt(d.front["parent_id"]); commentID != 0 && pid != commentID {
				t.Errorf("reply.parent_id=%d, want %d", pid, commentID)
			}
		}
	}
	if !foundAnswerComment {
		t.Error("no comment with is_answer: true found")
	}
	if !foundReply {
		t.Error("no nested reply found")
	}
}

func checkDiscussion8(t *testing.T, out string) {
	docs := parseDocs(t, filepath.Join(out, "discussions", "0008.md"))
	fr := docs[0].front
	if fr["type"] != "discussion" {
		t.Errorf("type=%v, want discussion", fr["type"])
	}
	if fr["category"] != "General" {
		t.Errorf("category=%v, want General", fr["category"])
	}
	if _, hasAnswer := fr["answer_id"]; hasAnswer {
		t.Errorf("answer_id set on non-Q&A discussion: %v", fr["answer_id"])
	}
	if _, hasChosenAt := fr["answer_chosen_at"]; hasChosenAt {
		t.Errorf("answer_chosen_at set on non-Q&A discussion: %v", fr["answer_chosen_at"])
	}
	// At least one top-level comment, no replies, no is_answer.
	var commentCount int
	for _, d := range docs[1:] {
		if d.front["document"] == "comment" {
			commentCount++
			if d.front["is_answer"] == true {
				t.Error("non-Q&A comment marked as is_answer")
			}
		}
	}
	if commentCount == 0 {
		t.Error("no comments found on discussion #8")
	}
}

func checkRepoYml(t *testing.T, out string) {
	raw, err := os.ReadFile(filepath.Join(out, "repo.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Owner          string   `yaml:"owner"`
		Repo           string   `yaml:"repo"`
		DefaultBranch  string   `yaml:"default_branch"`
		Description    string   `yaml:"description"`
		Visibility     string   `yaml:"visibility"`
		Language       string   `yaml:"language"`
		License        string   `yaml:"license"`
		Topics         []string `yaml:"topics"`
		Archived       bool     `yaml:"archived"`
		HasIssues      bool     `yaml:"has_issues"`
		HasProjects    bool     `yaml:"has_projects"`
		HasWiki        bool     `yaml:"has_wiki"`
		HasPages       bool     `yaml:"has_pages"`
		HasDiscussions bool     `yaml:"has_discussions"`
		CreatedAt      string   `yaml:"created_at"`
		UpdatedAt      string   `yaml:"updated_at"`
		PushedAt       string   `yaml:"pushed_at"`
		SyncedAt       string   `yaml:"synced_at"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.Owner != testOwner || cfg.Repo != testRepo {
		t.Errorf("owner/repo = %s/%s, want %s/%s", cfg.Owner, cfg.Repo, testOwner, testRepo)
	}
	if cfg.DefaultBranch != "main" {
		t.Errorf("default_branch=%q, want main", cfg.DefaultBranch)
	}
	if cfg.Visibility != "public" {
		t.Errorf("visibility=%q, want public", cfg.Visibility)
	}
	if cfg.Language != "Go" {
		t.Errorf("language=%q, want Go", cfg.Language)
	}
	if cfg.License == "" {
		t.Error("license empty")
	}
	if cfg.Description == "" {
		t.Error("description empty")
	}
	if len(cfg.Topics) == 0 {
		t.Error("topics empty")
	}
	// Feature flags: at minimum issues + projects + wiki are on, pages off.
	if !cfg.HasIssues {
		t.Error("has_issues=false, want true")
	}
	if !cfg.HasProjects {
		t.Error("has_projects=false, want true")
	}
	if !cfg.HasWiki {
		t.Error("has_wiki=false, want true")
	}
	if cfg.HasPages {
		t.Error("has_pages=true, want false")
	}
	if cfg.Archived {
		t.Error("archived=true, want false")
	}
	for name, ts := range map[string]string{
		"created_at": cfg.CreatedAt,
		"updated_at": cfg.UpdatedAt,
		"pushed_at":  cfg.PushedAt,
		"synced_at":  cfg.SyncedAt,
	} {
		if !strings.Contains(ts, "T") {
			t.Errorf("%s malformed or empty: %q", name, ts)
		}
	}
}

func checkReleaseTEST(t *testing.T, out string) {
	docs := parseDocs(t, filepath.Join(out, "releases", "TEST-v0.0.1.md"))
	if len(docs) == 0 {
		t.Fatal("no docs in releases/TEST-v0.0.1.md")
	}
	fr := docs[0].front
	if fr["tag"] != "TEST-v0.0.1" {
		t.Errorf("tag=%v, want TEST-v0.0.1", fr["tag"])
	}
	if s, _ := fr["name"].(string); !strings.HasPrefix(s, "TEST:") {
		t.Errorf("name=%q, want TEST-prefixed", s)
	}
	if fr["draft"] != false {
		t.Errorf("draft=%v, want false", fr["draft"])
	}
	if fr["prerelease"] != false {
		t.Errorf("prerelease=%v, want false", fr["prerelease"])
	}
	if fr["author"] != "mevdschee" {
		t.Errorf("author=%v, want mevdschee", fr["author"])
	}
	if s, _ := fr["published_at"].(string); !strings.Contains(s, "T") {
		t.Errorf("published_at malformed or empty: %q", s)
	}
	// Single release-body assertion (one body match per content type).
	if !strings.Contains(docs[0].body, "Multi-paragraph body so the exporter test can assert") {
		t.Errorf("release body missing seeded marker; got %q", docs[0].body)
	}
}

// --- subtest bodies ---

func checkLabels(t *testing.T, out string) {
	raw, err := os.ReadFile(filepath.Join(out, "labels.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var labels []struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(raw, &labels); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"TEST": false, "TEST-bug": false}
	for _, l := range labels {
		if _, ok := want[l.Name]; ok {
			want[l.Name] = true
			if l.Description == "" {
				t.Errorf("label %q: description empty", l.Name)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("label %q not found in labels.yml", name)
		}
	}
}

func checkMilestones(t *testing.T, out string) {
	raw, err := os.ReadFile(filepath.Join(out, "milestones.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var ms []struct {
		Title    string `yaml:"title"`
		State    string `yaml:"state"`
		DueOn    string `yaml:"due_on"`
		ClosedAt string `yaml:"closed_at"`
	}
	if err := yaml.Unmarshal(raw, &ms); err != nil {
		t.Fatal(err)
	}
	by := map[string]struct{ state, due, closed string }{}
	for _, m := range ms {
		by[m.Title] = struct{ state, due, closed string }{m.State, m.DueOn, m.ClosedAt}
	}
	if m, ok := by["TEST milestone"]; !ok {
		t.Error(`"TEST milestone" not found`)
	} else if m.state != "open" {
		t.Errorf("TEST milestone: state=%q, want open", m.state)
	}
	if m, ok := by["TEST milestone (closed)"]; !ok {
		t.Error(`"TEST milestone (closed)" not found`)
	} else {
		if m.state != "closed" {
			t.Errorf("TEST milestone (closed): state=%q, want closed", m.state)
		}
		if m.due == "" {
			t.Error("TEST milestone (closed): due_on empty")
		}
	}
}

func checkIssue1(t *testing.T, out string) {
	docs := parseDocs(t, filepath.Join(out, "issues", "0001.md"))
	if len(docs) == 0 {
		t.Fatal("no docs in 0001.md")
	}
	fr := docs[0].front
	if toInt(fr["number"]) != 1 {
		t.Errorf("number=%v, want 1", fr["number"])
	}
	if s, _ := fr["title"].(string); !strings.HasPrefix(s, "TEST:") {
		t.Errorf("title=%v, want TEST-prefixed", fr["title"])
	}
	if fr["state"] != "open" {
		t.Errorf("state=%v, want open", fr["state"])
	}
	if _, isPR := fr["type"]; isPR {
		t.Errorf("type set on plain issue: %v", fr["type"])
	}
	checkCommonScalars(t, fr)
	for _, want := range []string{"TEST", "TEST-bug"} {
		if !containsString(toStringList(fr["labels"]), want) {
			t.Errorf("labels missing %q: %v", want, fr["labels"])
		}
	}
	if fr["milestone"] != "TEST milestone" {
		t.Errorf("milestone=%v, want %q", fr["milestone"], "TEST milestone")
	}
	if !containsString(toStringList(fr["assignees"]), "mevdschee") {
		t.Errorf("assignees missing mevdschee: %v", fr["assignees"])
	}
	// Single issue-body assertion for the whole suite (one body match per
	// content type is enough — see checkPR5 for the PR-body match and the
	// comment loop below for the comment-body match).
	if !strings.Contains(docs[0].body, "exporter end-to-end test suite") {
		t.Errorf("body missing seeded text; got %q", docs[0].body)
	}
	cs := bodiesByDocType(docs, "comment")
	if len(cs) < 2 {
		t.Errorf("want >=2 comments, got %d", len(cs))
	}
	if !strings.Contains(strings.Join(cs, "\n"), "TEST comment 2") {
		t.Errorf("comments missing 'TEST comment 2': %v", cs)
	}
	if !hasEvent(docs, "sub_issue_added") {
		t.Errorf("expected sub_issue_added on parent issue #1, got events: %v", eventTypes(docs))
	}
}

func checkIssue2(t *testing.T, out string) {
	docs := parseDocs(t, filepath.Join(out, "issues", "0002.md"))
	fr := docs[0].front
	if fr["state"] != "closed" {
		t.Errorf("state=%v, want closed", fr["state"])
	}
	if fr["state_reason"] != "completed" {
		t.Errorf("state_reason=%v, want completed", fr["state_reason"])
	}
	if s, _ := fr["closed_at"].(string); s == "" {
		t.Error("closed_at empty")
	}
	checkCommonScalars(t, fr)
	if !containsString(toStringList(fr["assignees"]), "mevdschee") {
		t.Errorf("assignees missing mevdschee: %v", fr["assignees"])
	}
	if !hasEvent(docs, "parent_issue_added") {
		t.Errorf("expected parent_issue_added on child issue #2, got events: %v", eventTypes(docs))
	}
}

func checkIssue3(t *testing.T, out string) {
	docs := parseDocs(t, filepath.Join(out, "issues", "0003.md"))
	fr := docs[0].front
	if fr["state"] != "closed" {
		t.Errorf("state=%v, want closed", fr["state"])
	}
	if fr["state_reason"] != "not_planned" {
		t.Errorf("state_reason=%v, want not_planned", fr["state_reason"])
	}
	if fr["milestone"] != "TEST milestone (closed)" {
		t.Errorf("milestone=%v, want %q", fr["milestone"], "TEST milestone (closed)")
	}
	checkCommonScalars(t, fr)
}

func checkPR4(t *testing.T, out string) {
	docs := parseDocs(t, filepath.Join(out, "issues", "0004.md"))
	fr := docs[0].front
	if fr["type"] != "pull_request" {
		t.Errorf("type=%v, want pull_request", fr["type"])
	}
	if fr["state"] != "closed" {
		t.Errorf("state=%v, want closed", fr["state"])
	}
	if fr["source_branch"] != "test/pr-merged" {
		t.Errorf("source_branch=%v, want test/pr-merged", fr["source_branch"])
	}
	if fr["target_branch"] != "main" {
		t.Errorf("target_branch=%v, want main", fr["target_branch"])
	}
	merge, _ := fr["merge"].(map[string]any)
	if merge == nil {
		t.Fatal("merge block missing")
	}
	if merge["merged"] != true {
		t.Errorf("merge.merged=%v, want true", merge["merged"])
	}
	if s, _ := merge["commit_sha"].(string); len(s) < 40 {
		t.Errorf("merge.commit_sha looks wrong: %q", s)
	}
	if s, _ := merge["merged_at"].(string); s == "" {
		t.Error("merge.merged_at empty")
	}
	if merge["merged_by"] != "mevdschee" {
		t.Errorf("merge.merged_by=%v, want mevdschee", merge["merged_by"])
	}
	checkCommonScalars(t, fr)
	if len(bodiesByDocType(docs, "comment")) == 0 {
		t.Error("PR #4 has no comment; want the seeded TEST PR comment")
	}
}

func checkPR5(t *testing.T, out string) {
	docs := parseDocs(t, filepath.Join(out, "issues", "0005.md"))
	fr := docs[0].front
	if fr["type"] != "pull_request" {
		t.Errorf("type=%v, want pull_request", fr["type"])
	}
	if fr["state"] != "open" {
		t.Errorf("state=%v, want open", fr["state"])
	}
	if m, _ := fr["merge"].(map[string]any); m == nil || m["merged"] != false {
		t.Errorf("merge.merged=%v, want false", fr["merge"])
	}
	if !containsString(toStringList(fr["assignees"]), "mevdschee") {
		t.Errorf("assignees missing mevdschee: %v", fr["assignees"])
	}
	if !containsString(toStringList(fr["requested_reviewers"]), "mevdschee-xebia") {
		t.Errorf("requested_reviewers missing mevdschee-xebia: %v", fr["requested_reviewers"])
	}
	if fr["milestone"] != "TEST milestone" {
		t.Errorf("milestone=%v, want %q", fr["milestone"], "TEST milestone")
	}
	if fr["source_branch"] != "test/pr-open" {
		t.Errorf("source_branch=%v, want test/pr-open", fr["source_branch"])
	}
	if fr["target_branch"] != "main" {
		t.Errorf("target_branch=%v, want main", fr["target_branch"])
	}
	checkCommonScalars(t, fr)
	// Single PR-body assertion for the whole suite.
	if !strings.Contains(docs[0].body, "Closes #1") {
		t.Errorf("body missing 'Closes #1': %q", docs[0].body)
	}
}

func checkPR6(t *testing.T, out string) {
	docs := parseDocs(t, filepath.Join(out, "issues", "0006.md"))
	fr := docs[0].front
	if fr["type"] != "pull_request" {
		t.Errorf("type=%v, want pull_request", fr["type"])
	}
	if fr["state"] != "closed" {
		t.Errorf("state=%v, want closed", fr["state"])
	}
	if m, _ := fr["merge"].(map[string]any); m == nil || m["merged"] != false {
		t.Errorf("merge.merged=%v, want false (closed without merging)", fr["merge"])
	}
	if fr["source_branch"] != "test/pr-closed" {
		t.Errorf("source_branch=%v, want test/pr-closed", fr["source_branch"])
	}
	if fr["target_branch"] != "main" {
		t.Errorf("target_branch=%v, want main", fr["target_branch"])
	}
	if s, _ := fr["closed_at"].(string); s == "" {
		t.Error("closed_at empty")
	}
	checkCommonScalars(t, fr)
}

func checkProject1(t *testing.T, out string) {
	if !projectsSynced {
		t.Skip("projects not synced (token lacks read:project scope)")
	}
	docs := parseDocs(t, filepath.Join(out, "projects", "0001.md"))
	if len(docs) == 0 {
		t.Fatal("no docs in projects/0001.md")
	}
	fr := docs[0].front
	if fr["title"] != "TEST exporter fixtures" {
		t.Errorf("project title=%v, want %q", fr["title"], "TEST exporter fixtures")
	}
	if fr["state"] != "open" {
		t.Errorf("project state=%v, want open", fr["state"])
	}
	if fr["owner"] != "mevdschee" {
		t.Errorf("project owner=%v, want mevdschee", fr["owner"])
	}
	if s, _ := fr["url"].(string); !strings.HasPrefix(s, "https://github.com/") {
		t.Errorf("project url=%v, want https://github.com/... prefix", fr["url"])
	}
	if s, _ := fr["description"].(string); !strings.Contains(s, "TEST") {
		t.Errorf("project description missing 'TEST': %q", s)
	}

	fields, _ := fr["fields"].([]any)
	var statusOptions []string
	for _, f := range fields {
		m, _ := f.(map[string]any)
		if m == nil || m["name"] != "Status" {
			continue
		}
		if m["type"] != "SINGLE_SELECT" {
			t.Errorf("Status field type=%v, want SINGLE_SELECT", m["type"])
		}
		statusOptions = toStringList(m["options"])
	}
	for _, want := range []string{"Todo", "In Progress", "Done"} {
		if !containsString(statusOptions, want) {
			t.Errorf("Status options missing %q; got %v", want, statusOptions)
		}
	}

	wantStatus := map[int]string{
		1: "In Progress",
		2: "Done",
		3: "Todo",
		4: "Done",
		5: "In Progress",
		6: "Done",
	}
	wantType := map[int]string{
		1: "issue", 2: "issue", 3: "issue",
		4: "pull_request", 5: "pull_request", 6: "pull_request",
	}
	got := map[int]string{}
	seen := map[int]bool{}
	for _, d := range docs[1:] {
		if d.front["document"] != "item" {
			continue
		}
		n := toInt(d.front["number"])
		seen[n] = true
		if d.front["repo"] != "mevdschee/github-export" {
			t.Errorf("item #%d repo=%v, want mevdschee/github-export", n, d.front["repo"])
		}
		if s, _ := d.front["title"].(string); !strings.HasPrefix(s, "TEST") {
			t.Errorf("item #%d title=%q, want TEST-prefixed", n, s)
		}
		if d.front["type"] != wantType[n] {
			t.Errorf("item #%d type=%v, want %q", n, d.front["type"], wantType[n])
		}
		f, _ := d.front["fields"].(map[string]any)
		if s, ok := f["Status"].(string); ok {
			got[n] = s
		}
	}
	for n, want := range wantStatus {
		if !seen[n] {
			t.Errorf("project item #%d missing", n)
			continue
		}
		if got[n] != want {
			t.Errorf("project item #%d Status=%q, want %q", n, got[n], want)
		}
	}
}

// checkCommonScalars verifies the scalar fields that every TEST issue/PR
// fixture shares: author, created_at/updated_at present, TEST label, and
// project membership.
func checkCommonScalars(t *testing.T, fr map[string]any) {
	t.Helper()
	if fr["author"] != "mevdschee" {
		t.Errorf("author=%v, want mevdschee", fr["author"])
	}
	if s, _ := fr["created_at"].(string); !strings.Contains(s, "T") {
		t.Errorf("created_at malformed or empty: %q", s)
	}
	if s, _ := fr["updated_at"].(string); !strings.Contains(s, "T") {
		t.Errorf("updated_at malformed or empty: %q", s)
	}
	if !containsString(toStringList(fr["labels"]), "TEST") {
		t.Errorf("labels missing TEST: %v", fr["labels"])
	}
	if projectsSynced && !containsString(toStringList(fr["projects"]), "TEST exporter fixtures") {
		t.Errorf("projects missing %q: %v", "TEST exporter fixtures", fr["projects"])
	}
}

// checkStore asserts the store holds the expected fixtures. The store is the
// source of truth, so these checks are independent of the markdown format.
func checkStore(t *testing.T, s *store.Store) {
	owner, repo, err := s.OwnerRepo()
	if err != nil || owner != testOwner || repo != testRepo {
		t.Errorf("OwnerRepo = %s/%s (err %v), want %s/%s", owner, repo, err, testOwner, testRepo)
	}
	if at, _ := s.SyncedAt(); !strings.Contains(at, "T") {
		t.Errorf("synced_at malformed or empty: %q", at)
	}

	issues, err := s.AllIssues()
	if err != nil {
		t.Fatalf("AllIssues: %v", err)
	}
	byNum := map[int64]store.IssueRow{}
	for _, r := range issues {
		byNum[r.Number] = r
	}
	for n := int64(1); n <= 6; n++ {
		if _, ok := byNum[n]; !ok {
			t.Errorf("issue #%d missing from store", n)
		}
	}
	// #1 is a plain open issue; #4..#6 are PRs.
	if r := byNum[1]; r.IsPR {
		t.Error("#1 stored as PR, want issue")
	}
	for _, n := range []int64{4, 5, 6} {
		if r, ok := byNum[n]; ok && !r.IsPR {
			t.Errorf("#%d stored as issue, want PR", n)
		}
	}

	// State transitions via typed columns.
	if exists, state, isPR, merged, _ := s.IssueState(4); !exists || !isPR || state != "closed" || !merged {
		t.Errorf("#4 state: exists=%v isPR=%v state=%q merged=%v, want merged closed PR", exists, isPR, state, merged)
	}
	if exists, state, _, _, _ := s.IssueState(1); !exists || state != "open" {
		t.Errorf("#1 state=%q, want open", state)
	}

	if max, _ := s.MaxIssueNumber(); max < 6 {
		t.Errorf("MaxIssueNumber=%d, want >=6", max)
	}

	// Discussions, projects, releases present.
	discussions, _ := s.AllDiscussions()
	dseen := map[int64]bool{}
	for _, d := range discussions {
		dseen[d.Number] = true
	}
	if !dseen[7] || !dseen[8] {
		t.Errorf("discussions 7 and 8 expected in store; got %v", dseen)
	}

	if projectsSynced {
		projects, _ := s.AllProjects()
		found := false
		for _, p := range projects {
			if p.Number == 1 {
				found = true
			}
		}
		if !found {
			t.Error("project #1 missing from store")
		}
	}

	releases, _ := s.AllReleases()
	relFound := false
	for _, m := range releases {
		if tag, _ := m["tag_name"].(string); tag == "TEST-v0.0.1" {
			relFound = true
		}
	}
	if !relFound {
		t.Error("release TEST-v0.0.1 missing from store")
	}

	// A full sync records *_created events; verify at least the issue/PR ones.
	var eventCount int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM events").Scan(&eventCount); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if eventCount == 0 {
		t.Error("no events recorded by full sync")
	}
	var created int
	s.DB().QueryRow("SELECT COUNT(*) FROM events WHERE type IN ('issue_created','pr_created')").Scan(&created)
	if created < 6 {
		t.Errorf("issue/pr_created events = %d, want >=6", created)
	}
}

// --- multi-document markdown parser ---

type doc struct {
	front map[string]any
	body  string
}

// parseDocs splits a multi-document markdown file. Each document is delimited
// by a lone `---` line. The first non-empty section is YAML frontmatter; what
// follows (until the next `---` or EOF) is the body. Subsequent documents
// again start with `---` opening their frontmatter.
func parseDocs(t *testing.T, path string) []doc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")

	var docs []doc
	var cur strings.Builder
	var pendingFront string
	inFront := false

	flush := func() {
		if pendingFront == "" && cur.Len() == 0 {
			return
		}
		var m map[string]any
		if pendingFront != "" {
			if err := yaml.Unmarshal([]byte(pendingFront), &m); err != nil {
				t.Fatalf("%s: parse frontmatter: %v\n---\n%s\n---", path, err, pendingFront)
			}
		}
		docs = append(docs, doc{front: m, body: strings.TrimRight(cur.String(), "\n")})
		pendingFront = ""
		cur.Reset()
	}

	for i, ln := range lines {
		if ln == "---" {
			switch {
			case !inFront && i == 0:
				inFront = true
			case inFront:
				pendingFront = cur.String()
				cur.Reset()
				inFront = false
			default:
				flush()
				inFront = true
			}
			continue
		}
		cur.WriteString(ln)
		cur.WriteString("\n")
	}
	flush()
	return docs
}

// --- helpers ---

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

func toStringList(v any) []string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func bodiesByDocType(docs []doc, docType string) []string {
	var out []string
	for _, d := range docs {
		if d.front["document"] == docType {
			out = append(out, d.body)
		}
	}
	return out
}

func hasEvent(docs []doc, event string) bool {
	for _, d := range docs {
		if d.front["document"] == "event" && d.front["event"] == event {
			return true
		}
	}
	return false
}

func eventTypes(docs []doc) []string {
	var out []string
	for _, d := range docs {
		if d.front["document"] == "event" {
			if e, ok := d.front["event"].(string); ok {
				out = append(out, e)
			}
		}
	}
	return out
}
