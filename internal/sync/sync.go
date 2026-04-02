package sync

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github-export/internal/document"
	"github-export/internal/github"
	"github-export/internal/jsonutil"

	"gopkg.in/yaml.v3"
)

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

func Releases(c *github.Client, owner, repo, outDir string) error {
	log.Println("Syncing releases...")
	url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=%d", github.API, owner, repo, github.PerPage)
	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return fmt.Errorf("fetching releases: %w", err)
	}

	relDir := filepath.Join(outDir, "releases")
	os.MkdirAll(relDir, 0755)

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

		f, err := os.Create(path)
		if err != nil {
			log.Printf("  Warning: cannot create %s: %v", path, err)
			continue
		}
		document.WriteFirstDoc(f, d.String(), jsonutil.Str(m, "body"))
		f.Close()
	}

	log.Printf("  %d releases", len(items))
	return nil
}
