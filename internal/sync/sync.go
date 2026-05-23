package sync

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/mevdschee/github-export/internal/config"
	"github.com/mevdschee/github-export/internal/document"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/jsonutil"

	"gopkg.in/yaml.v3"
)

// Repo fetches repository-level metadata and writes it to repo.yml. The
// syncedAt timestamp should be captured before any other sync calls so the
// next incremental run picks up anything updated during this one.
func Repo(c *github.Client, owner, repo, outDir, syncedAt string) error {
	log.Println("Syncing repo metadata...")
	url := fmt.Sprintf("%s/repos/%s/%s", github.API, owner, repo)
	raw, err := c.GetJSON(url, nil)
	if err != nil {
		return fmt.Errorf("fetching repo metadata: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("parsing repo metadata: %w", err)
	}

	cfg := &config.RepoConfig{
		Owner:          owner,
		Repo:           repo,
		DefaultBranch:  jsonutil.Str(data, "default_branch"),
		Description:    jsonutil.Str(data, "description"),
		Homepage:       jsonutil.Str(data, "homepage"),
		Visibility:     jsonutil.Str(data, "visibility"),
		Language:       jsonutil.Str(data, "language"),
		License:        jsonutil.Str(jsonutil.Map(data, "license"), "name"),
		Topics:         stringList(jsonutil.List(data, "topics")),
		Archived:       jsonutil.Bool(data, "archived"),
		HasIssues:      jsonutil.Bool(data, "has_issues"),
		HasProjects:    jsonutil.Bool(data, "has_projects"),
		HasWiki:        jsonutil.Bool(data, "has_wiki"),
		HasPages:       jsonutil.Bool(data, "has_pages"),
		HasDiscussions: jsonutil.Bool(data, "has_discussions"),
		CreatedAt:      jsonutil.Str(data, "created_at"),
		UpdatedAt:      jsonutil.Str(data, "updated_at"),
		PushedAt:       jsonutil.Str(data, "pushed_at"),
		SyncedAt:       syncedAt,
	}
	return config.WriteRepoConfig(filepath.Join(outDir, "repo.yml"), cfg)
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

func Labels(c *github.Client, owner, repo, outDir string) error {
	log.Println("Syncing labels...")
	url := fmt.Sprintf("%s/repos/%s/%s/labels?per_page=%d", github.API, owner, repo, github.PerPage)
	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return fmt.Errorf("fetching labels: %w", err)
	}

	type Label struct {
		Name        string `yaml:"name"`
		Color       string `yaml:"color"`
		Description string `yaml:"description,omitempty"`
	}

	var labels []Label
	for _, raw := range items {
		var m map[string]any
		json.Unmarshal(raw, &m)
		labels = append(labels, Label{
			Name:        jsonutil.Str(m, "name"),
			Color:       jsonutil.Str(m, "color"),
			Description: jsonutil.Str(m, "description"),
		})
	}

	data, err := yaml.Marshal(labels)
	if err != nil {
		return err
	}
	log.Printf("  %d labels", len(labels))
	return os.WriteFile(filepath.Join(outDir, "labels.yml"), data, 0644)
}

func Milestones(c *github.Client, owner, repo, outDir string) error {
	log.Println("Syncing milestones...")
	url := fmt.Sprintf("%s/repos/%s/%s/milestones?state=all&per_page=%d", github.API, owner, repo, github.PerPage)
	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return fmt.Errorf("fetching milestones: %w", err)
	}

	type Milestone struct {
		Title       string `yaml:"title"`
		State       string `yaml:"state"`
		Description string `yaml:"description,omitempty"`
		DueOn       string `yaml:"due_on,omitempty"`
		ClosedAt    string `yaml:"closed_at,omitempty"`
	}

	var milestones []Milestone
	for _, raw := range items {
		var m map[string]any
		json.Unmarshal(raw, &m)
		milestones = append(milestones, Milestone{
			Title:       jsonutil.Str(m, "title"),
			State:       jsonutil.Str(m, "state"),
			Description: jsonutil.Str(m, "description"),
			DueOn:       jsonutil.Str(m, "due_on"),
			ClosedAt:    jsonutil.Str(m, "closed_at"),
		})
	}

	data, err := yaml.Marshal(milestones)
	if err != nil {
		return err
	}
	log.Printf("  %d milestones", len(milestones))
	return os.WriteFile(filepath.Join(outDir, "milestones.yml"), data, 0644)
}

func Releases(c *github.Client, owner, repo, outDir string) ([]hooks.Event, error) {
	log.Println("Syncing releases...")
	url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=%d", github.API, owner, repo, github.PerPage)
	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching releases: %w", err)
	}

	relDir := filepath.Join(outDir, "releases")
	os.MkdirAll(relDir, 0755)
	repoSlug := owner + "/" + repo

	var events []hooks.Event
	for _, raw := range items {
		var m map[string]any
		json.Unmarshal(raw, &m)

		tag := jsonutil.Str(m, "tag_name")
		if tag == "" {
			continue
		}

		d := &document.Writer{}
		d.KV("tag", tag)
		d.KV("name", jsonutil.Str(m, "name"))
		d.KV("draft", jsonutil.Bool(m, "draft"))
		d.KV("prerelease", jsonutil.Bool(m, "prerelease"))
		d.KV("author", jsonutil.UserLogin(m, "author"))
		d.KV("created_at", jsonutil.Str(m, "created_at"))
		d.KV("published_at", jsonutil.Str(m, "published_at"))
		d.KV("target_commitish", jsonutil.Str(m, "target_commitish"))

		assets := jsonutil.List(m, "assets")
		if len(assets) > 0 {
			fmt.Fprint(d.Buf(), "assets:\n")
			for _, a := range assets {
				asset, _ := a.(map[string]any)
				if asset == nil {
					continue
				}
				fmt.Fprintf(d.Buf(), "  - name: %s\n", document.YamlScalar(jsonutil.Str(asset, "name")))
				if ct := jsonutil.Str(asset, "content_type"); ct != "" {
					fmt.Fprintf(d.Buf(), "    content_type: %s\n", document.YamlScalar(ct))
				}
				if sz := jsonutil.Int(asset, "size"); sz > 0 {
					fmt.Fprintf(d.Buf(), "    size_bytes: %d\n", sz)
				}
				if dc := jsonutil.Int(asset, "download_count"); dc > 0 {
					fmt.Fprintf(d.Buf(), "    download_count: %d\n", dc)
				}
			}
		}

		safeTag := strings.ReplaceAll(tag, "/", "-")
		path := filepath.Join(relDir, safeTag+".md")
		prev := readPrevRelease(path)

		f, err := os.Create(path)
		if err != nil {
			log.Printf("  Warning: cannot create %s: %v", path, err)
			continue
		}
		document.WriteFirstDoc(f, d.String(), jsonutil.Str(m, "body"))
		f.Close()

		if jsonutil.Bool(m, "draft") {
			continue
		}

		prerelease := jsonutil.Bool(m, "prerelease")
		base := hooks.Event{
			Number: jsonutil.Int(m, "id"),
			Title:  jsonutil.Str(m, "name"),
			Author: jsonutil.UserLogin(m, "author"),
			State:  "published",
			File:   path,
			Repo:   repoSlug,
			URL:    jsonutil.Str(m, "html_url"),
		}
		if prerelease {
			base.State = "prerelease"
		}
		extra := map[string]string{"tag": tag}

		if !prev.exists {
			ev := base
			ev.Type = hooks.ReleasePublished
			ev.Body = jsonutil.Str(m, "body")
			ev.Extra = extra
			events = append(events, ev)
		} else if prev.prerelease && !prerelease {
			ev := base
			ev.Type = hooks.PrereleasePromoted
			ev.Extra = extra
			events = append(events, ev)
		}
	}

	log.Printf("  %d releases", len(items))
	return events, nil
}

type prevReleaseState struct {
	exists     bool
	prerelease bool
}

func readPrevRelease(path string) prevReleaseState {
	f, err := os.Open(path)
	if err != nil {
		return prevReleaseState{}
	}
	defer f.Close()
	out := prevReleaseState{exists: true}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return out
	}
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		lines = append(lines, line)
	}
	var fm struct {
		Prerelease bool `yaml:"prerelease"`
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &fm); err == nil {
		out.prerelease = fm.Prerelease
	}
	return out
}
