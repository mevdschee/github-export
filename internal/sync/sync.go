package sync

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"

	"github.com/mevdschee/github-export/internal/ghmodel"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/jsonutil"
	"github.com/mevdschee/github-export/internal/store"
)

// Repo fetches repository-level metadata, stores it, and records the sync
// timestamp in the store's meta table. syncedAt should be captured before any
// other sync call so the next incremental run picks up anything updated during
// this one.
func Repo(c *github.Client, s *store.Store, owner, repo, syncedAt string) error {
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
	if err := s.UpsertRepo(owner, repo, data); err != nil {
		return fmt.Errorf("storing repo metadata: %w", err)
	}
	return s.SetMeta("synced_at", syncedAt)
}

func Labels(c *github.Client, s *store.Store, owner, repo string) error {
	log.Println("Syncing labels...")
	url := fmt.Sprintf("%s/repos/%s/%s/labels?per_page=%d", github.API, owner, repo, github.PerPage)
	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return fmt.Errorf("fetching labels: %w", err)
	}
	maps := unmarshalMaps(items)
	if err := s.ReplaceLabels(maps); err != nil {
		return fmt.Errorf("storing labels: %w", err)
	}
	log.Printf("  %d labels", len(maps))
	return nil
}

func Milestones(c *github.Client, s *store.Store, owner, repo string) error {
	log.Println("Syncing milestones...")
	url := fmt.Sprintf("%s/repos/%s/%s/milestones?state=all&per_page=%d", github.API, owner, repo, github.PerPage)
	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return fmt.Errorf("fetching milestones: %w", err)
	}
	maps := unmarshalMaps(items)
	if err := s.ReplaceMilestones(maps); err != nil {
		return fmt.Errorf("storing milestones: %w", err)
	}
	log.Printf("  %d milestones", len(maps))
	return nil
}

func Releases(c *github.Client, s *store.Store, owner, repo string) ([]hooks.Event, error) {
	log.Println("Syncing releases...")
	url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=%d", github.API, owner, repo, github.PerPage)
	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching releases: %w", err)
	}
	repoSlug := owner + "/" + repo

	var events []hooks.Event
	for _, m := range unmarshalMaps(items) {
		tag := jsonutil.Str(m, "tag_name")
		if tag == "" {
			continue
		}

		existed, prevPrerelease, err := s.ReleaseState(tag)
		if err != nil {
			return events, fmt.Errorf("reading release state %q: %w", tag, err)
		}
		if err := s.UpsertRelease(m); err != nil {
			log.Printf("  Warning: storing release %q: %v", tag, err)
			continue
		}

		if jsonutil.Bool(m, "draft") {
			continue
		}

		prerelease := jsonutil.Bool(m, "prerelease")
		relPath := filepath.Join("releases", ghmodel.SafeTag(tag)+".md")
		base := hooks.Event{
			Number: jsonutil.Int(m, "id"),
			Title:  jsonutil.Str(m, "name"),
			Author: jsonutil.UserLogin(m, "author"),
			State:  "published",
			File:   relPath,
			Repo:   repoSlug,
			URL:    jsonutil.Str(m, "html_url"),
		}
		if prerelease {
			base.State = "prerelease"
		}
		extra := map[string]string{"tag": tag}

		switch {
		case !existed:
			ev := base
			ev.Type = hooks.ReleasePublished
			ev.Body = jsonutil.Str(m, "body")
			ev.Extra = extra
			events = append(events, ev)
		case prevPrerelease && !prerelease:
			ev := base
			ev.Type = hooks.PrereleasePromoted
			ev.Extra = extra
			events = append(events, ev)
		}
	}

	log.Printf("  %d releases", len(items))
	return events, nil
}

// unmarshalMaps decodes a slice of raw JSON objects into maps, skipping any that
// fail to parse.
func unmarshalMaps(items []json.RawMessage) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err == nil {
			out = append(out, m)
		}
	}
	return out
}
