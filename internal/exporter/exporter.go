// Package exporter renders the store's contents to the markdown export layout
// on demand. It is one-way: it reads from the store and writes files, and never
// parses markdown back. The store remains the source of truth.
package exporter

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/mevdschee/github-export/internal/config"
	"github.com/mevdschee/github-export/internal/ghmodel"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/jsonutil"
	"github.com/mevdschee/github-export/internal/render"
	"github.com/mevdschee/github-export/internal/store"

	"gopkg.in/yaml.v3"
)

// dataDirs are recreated on each export so the output reflects exactly what the
// store holds (e.g. a closed project's file does not linger). The events/ dir is
// deliberately preserved — it is a handoff inbox the caller may have partly
// consumed.
var dataDirs = []string{"issues", "projects", "discussions", "releases"}

// Export writes the full markdown view of the store into outDir.
func Export(s *store.Store, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", outDir, err)
	}
	for _, d := range dataDirs {
		p := filepath.Join(outDir, d)
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("clearing %s: %w", p, err)
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", p, err)
		}
	}

	if err := exportRepoMeta(s, outDir); err != nil {
		return err
	}
	if err := exportLabels(s, outDir); err != nil {
		return err
	}
	if err := exportMilestones(s, outDir); err != nil {
		return err
	}
	if err := exportIssues(s, outDir); err != nil {
		return err
	}
	if err := exportProjects(s, outDir); err != nil {
		return err
	}
	if err := exportDiscussions(s, outDir); err != nil {
		return err
	}
	if err := exportReleases(s, outDir); err != nil {
		return err
	}
	return exportEvents(s, outDir)
}

func exportRepoMeta(s *store.Store, outDir string) error {
	owner, repo, m, syncedAt, err := s.ExportRepo()
	if err != nil {
		return fmt.Errorf("reading repo metadata: %w", err)
	}
	if m == nil {
		return nil
	}
	cfg := &config.RepoConfig{
		Owner:          owner,
		Repo:           repo,
		DefaultBranch:  jsonutil.Str(m, "default_branch"),
		Description:    jsonutil.Str(m, "description"),
		Homepage:       jsonutil.Str(m, "homepage"),
		Visibility:     jsonutil.Str(m, "visibility"),
		Language:       jsonutil.Str(m, "language"),
		License:        jsonutil.Str(jsonutil.Map(m, "license"), "name"),
		Topics:         stringList(jsonutil.List(m, "topics")),
		Archived:       jsonutil.Bool(m, "archived"),
		HasIssues:      jsonutil.Bool(m, "has_issues"),
		HasProjects:    jsonutil.Bool(m, "has_projects"),
		HasWiki:        jsonutil.Bool(m, "has_wiki"),
		HasPages:       jsonutil.Bool(m, "has_pages"),
		HasDiscussions: jsonutil.Bool(m, "has_discussions"),
		CreatedAt:      jsonutil.Str(m, "created_at"),
		UpdatedAt:      jsonutil.Str(m, "updated_at"),
		PushedAt:       jsonutil.Str(m, "pushed_at"),
		SyncedAt:       syncedAt,
	}
	return config.WriteRepoConfig(filepath.Join(outDir, "repo.yml"), cfg)
}

func exportLabels(s *store.Store, outDir string) error {
	maps, err := s.AllLabels()
	if err != nil {
		return fmt.Errorf("reading labels: %w", err)
	}
	type label struct {
		Name        string `yaml:"name"`
		Color       string `yaml:"color"`
		Description string `yaml:"description,omitempty"`
	}
	out := make([]label, 0, len(maps))
	for _, m := range maps {
		out = append(out, label{
			Name:        jsonutil.Str(m, "name"),
			Color:       jsonutil.Str(m, "color"),
			Description: jsonutil.Str(m, "description"),
		})
	}
	return writeYAML(filepath.Join(outDir, "labels.yml"), out)
}

func exportMilestones(s *store.Store, outDir string) error {
	maps, err := s.AllMilestones()
	if err != nil {
		return fmt.Errorf("reading milestones: %w", err)
	}
	type milestone struct {
		Title       string `yaml:"title"`
		State       string `yaml:"state"`
		Description string `yaml:"description,omitempty"`
		DueOn       string `yaml:"due_on,omitempty"`
		ClosedAt    string `yaml:"closed_at,omitempty"`
	}
	out := make([]milestone, 0, len(maps))
	for _, m := range maps {
		out = append(out, milestone{
			Title:       jsonutil.Str(m, "title"),
			State:       jsonutil.Str(m, "state"),
			Description: jsonutil.Str(m, "description"),
			DueOn:       jsonutil.Str(m, "due_on"),
			ClosedAt:    jsonutil.Str(m, "closed_at"),
		})
	}
	return writeYAML(filepath.Join(outDir, "milestones.yml"), out)
}

func exportIssues(s *store.Store, outDir string) error {
	rows, err := s.AllIssues()
	if err != nil {
		return fmt.Errorf("reading issues: %w", err)
	}
	var maxNum int64
	for _, r := range rows {
		if r.Number > maxNum {
			maxNum = r.Number
		}
	}
	pad := ghmodel.PadWidth(maxNum)
	dir := filepath.Join(outDir, "issues")
	for _, r := range rows {
		path := filepath.Join(dir, ghmodel.IssueFilename(r.Number, pad))
		if err := render.IssueFile(path, r.Issue, r.IsPR, r.PR, r.Timeline, r.Projects); err != nil {
			log.Printf("Warning: exporting issue #%d: %v", r.Number, err)
		}
	}
	return nil
}

func exportProjects(s *store.Store, outDir string) error {
	rows, err := s.AllProjects()
	if err != nil {
		return fmt.Errorf("reading projects: %w", err)
	}
	dir := filepath.Join(outDir, "projects")
	for _, r := range rows {
		path := filepath.Join(dir, fmt.Sprintf("%04d.md", r.Number))
		if err := render.ProjectFile(path, r.Project, r.Items); err != nil {
			log.Printf("Warning: exporting project #%d: %v", r.Number, err)
		}
	}
	return nil
}

func exportDiscussions(s *store.Store, outDir string) error {
	nodes, err := s.AllDiscussions()
	if err != nil {
		return fmt.Errorf("reading discussions: %w", err)
	}
	dir := filepath.Join(outDir, "discussions")
	for _, d := range nodes {
		path := filepath.Join(dir, fmt.Sprintf("%04d.md", d.Number))
		if err := render.DiscussionFile(path, d); err != nil {
			log.Printf("Warning: exporting discussion #%d: %v", d.Number, err)
		}
	}
	return nil
}

func exportReleases(s *store.Store, outDir string) error {
	maps, err := s.AllReleases()
	if err != nil {
		return fmt.Errorf("reading releases: %w", err)
	}
	dir := filepath.Join(outDir, "releases")
	for _, m := range maps {
		tag := jsonutil.Str(m, "tag_name")
		if tag == "" {
			continue
		}
		path := filepath.Join(dir, ghmodel.SafeTag(tag)+".md")
		if err := render.ReleaseFile(path, m); err != nil {
			log.Printf("Warning: exporting release %q: %v", tag, err)
		}
	}
	return nil
}

// exportEvents renders not-yet-exported events to events/*.md and stamps them so
// re-running export does not re-create files the caller may have consumed.
func exportEvents(s *store.Store, outDir string) error {
	pending, err := s.PendingEvents()
	if err != nil {
		return fmt.Errorf("reading events: %w", err)
	}
	if len(pending) == 0 {
		return nil
	}
	events := make([]hooks.Event, 0, len(pending))
	ids := make([]int64, 0, len(pending))
	for _, pe := range pending {
		ev := pe.Event
		// The stored file path is repo-relative (e.g. issues/0042.md); point it
		// at this export's output location.
		if ev.File != "" {
			ev.File = filepath.Join(outDir, ev.File)
		}
		events = append(events, ev)
		ids = append(ids, pe.ID)
	}
	eventsDir := filepath.Join(outDir, "events")
	if err := hooks.Export(eventsDir, events); err != nil {
		return fmt.Errorf("writing events: %w", err)
	}
	return s.MarkEventsExported(ids, time.Now().UTC().Format(time.RFC3339))
}

func writeYAML(path string, v any) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func stringList(xs []any) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
