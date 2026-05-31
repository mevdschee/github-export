package sync

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/mevdschee/github-export/internal/ghmodel"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/jsonutil"
	"github.com/mevdschee/github-export/internal/store"
)

// listProjectsQuery fetches all projectsV2 linked to a repository, with their
// field definitions. Items are fetched separately because they have their own
// pagination cursor.
const listProjectsQuery = `
query($owner: String!, $name: String!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    projectsV2(first: 20, after: $cursor) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id
        number
        title
        shortDescription
        url
        closed
        public
        createdAt
        updatedAt
        readme
        owner {
          __typename
          ... on User { login }
          ... on Organization { login }
        }
        fields(first: 50) {
          nodes {
            __typename
            ... on ProjectV2FieldCommon { name dataType }
            ... on ProjectV2SingleSelectField {
              name
              dataType
              options { name }
            }
            ... on ProjectV2IterationField {
              name
              dataType
            }
          }
        }
      }
    }
  }
}`

const listItemsQuery = `
query($projectId: ID!, $cursor: String) {
  node(id: $projectId) {
    ... on ProjectV2 {
      items(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          type
          content {
            __typename
            ... on Issue { number title repository { nameWithOwner } }
            ... on PullRequest { number title repository { nameWithOwner } }
          }
          fieldValues(first: 30) {
            nodes {
              __typename
              ... on ProjectV2ItemFieldTextValue { text field { ... on ProjectV2FieldCommon { name } } }
              ... on ProjectV2ItemFieldNumberValue { number field { ... on ProjectV2FieldCommon { name } } }
              ... on ProjectV2ItemFieldDateValue { date field { ... on ProjectV2FieldCommon { name } } }
              ... on ProjectV2ItemFieldSingleSelectValue { name field { ... on ProjectV2FieldCommon { name } } }
              ... on ProjectV2ItemFieldIterationValue { title startDate duration field { ... on ProjectV2FieldCommon { name } } }
            }
          }
        }
      }
    }
  }
}`

// Projects syncs GitHub Projects v2 linked to the repository into the store.
//
// Behavior:
//   - Lists repository.projectsV2 via GraphQL (open + closed are both fetched
//     so we can detect open→closed transitions; only open ones are stored).
//   - If since is non-empty, open projects older than since with an existing
//     stored row are skipped (no rewrite, no diff).
//   - Closed projects whose row exists are deleted; a project_closed event is
//     emitted.
//   - Returns a map of issue/PR number → project titles for the issues sync to
//     cross-link, plus any hook events.
func Projects(c *github.Client, s *store.Store, owner, repo, since string) (map[int64][]string, []hooks.Event, error) {
	log.Println("Syncing projects...")

	projects, err := fetchAllProjects(c, owner, repo)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching projects: %w", err)
	}
	log.Printf("  %d projects on repo", len(projects))

	repoSlug := owner + "/" + repo
	issueProjects := make(map[int64][]string)
	var events []hooks.Event

	for _, p := range projects {
		number := jsonutil.Int(p, "number")
		title := jsonutil.Str(p, "title")
		closed := jsonutil.Bool(p, "closed")
		updatedAt := jsonutil.Str(p, "updatedAt")
		relPath := fmt.Sprintf("projects/%04d.md", number)

		prevItemMaps, existed, err := s.ProjectItems(number)
		if err != nil {
			log.Printf("  Warning: reading project #%d: %v", number, err)
		}

		if closed {
			if existed {
				log.Printf("  Project #%d %q closed — removing", number, title)
				if err := s.DeleteProject(number); err != nil {
					log.Printf("  Warning: deleting project #%d: %v", number, err)
				}
				events = append(events, projectEvent(hooks.ProjectClosed, p, relPath, repoSlug))
			}
			continue
		}

		if since != "" && updatedAt < since && existed {
			log.Printf("  Project #%d %q skipped (unchanged)", number, title)
			continue
		}

		items, err := fetchProjectItems(c, jsonutil.Str(p, "id"))
		if err != nil {
			log.Printf("  Warning: items for project #%d: %v", number, err)
			items = nil
		}

		for _, item := range items {
			content := jsonutil.Map(item, "content")
			if content == nil {
				continue
			}
			repoName := ""
			if r := jsonutil.Map(content, "repository"); r != nil {
				repoName = jsonutil.Str(r, "nameWithOwner")
			}
			if repoName != repoSlug {
				continue
			}
			if n := jsonutil.Int(content, "number"); n > 0 {
				issueProjects[n] = append(issueProjects[n], title)
			}
		}

		if !existed {
			events = append(events, projectEvent(hooks.ProjectCreated, p, relPath, repoSlug))
		} else {
			events = append(events, diffProjectItems(p, relPath, repoSlug, buildPrevItems(prevItemMaps), items)...)
		}

		if err := s.UpsertProject(number, p, items); err != nil {
			log.Printf("  Warning: storing project #%d: %v", number, err)
		}
	}

	return issueProjects, events, nil
}

// diffProjectItems emits item_added / item_removed / item_status_changed /
// item_field_changed events by comparing stored items against fresh ones.
func diffProjectItems(p map[string]any, relPath, repoSlug string, prevItems map[int64]*prevProjectItem, items []map[string]any) []hooks.Event {
	var events []hooks.Event
	currentNumbers := map[int64]bool{}
	for _, item := range items {
		content := jsonutil.Map(item, "content")
		if content == nil {
			continue
		}
		n := jsonutil.Int(content, "number")
		if n == 0 {
			continue
		}
		currentNumbers[n] = true
		currFields := ghmodel.ExtractItemFields(item)
		extra := itemExtra(n, content, item)

		prev, existed := prevItems[n]
		if !existed {
			ev := projectEvent(hooks.ItemAdded, p, relPath, repoSlug)
			ev.Extra = extra
			events = append(events, ev)
			continue
		}
		keys := map[string]bool{}
		for k := range prev.Fields {
			keys[k] = true
		}
		for k := range currFields {
			keys[k] = true
		}
		for k := range keys {
			if prev.Fields[k] == currFields[k] {
				continue
			}
			evType := hooks.ItemFieldChanged
			if k == "Status" {
				evType = hooks.ItemStatusChanged
			}
			ev := projectEvent(evType, p, relPath, repoSlug)
			ee := map[string]string{}
			for ek, ev2 := range extra {
				ee[ek] = ev2
			}
			ee["field_name"] = k
			if v := prev.Fields[k]; v != "" {
				ee["from_value"] = v
			}
			if v := currFields[k]; v != "" {
				ee["to_value"] = v
			}
			ev.Extra = ee
			events = append(events, ev)
		}
	}
	for n, prev := range prevItems {
		if currentNumbers[n] {
			continue
		}
		ev := projectEvent(hooks.ItemRemoved, p, relPath, repoSlug)
		ee := map[string]string{"item_number": fmt.Sprintf("%d", n)}
		if prev.Title != "" {
			ee["item_title"] = prev.Title
		}
		if prev.Type != "" {
			ee["item_type"] = prev.Type
		}
		if prev.Repo != "" {
			ee["item_repo"] = prev.Repo
		}
		ev.Extra = ee
		events = append(events, ev)
	}
	return events
}

func fetchAllProjects(c *github.Client, owner, repo string) ([]map[string]any, error) {
	var all []map[string]any
	cursor := ""
	for {
		vars := map[string]any{"owner": owner, "name": repo}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		raw, err := c.GraphQL(listProjectsQuery, vars)
		if err != nil {
			return all, err
		}
		var resp struct {
			Repository struct {
				ProjectsV2 struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []map[string]any `json:"nodes"`
				} `json:"projectsV2"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return all, fmt.Errorf("parsing projects response: %w", err)
		}
		all = append(all, resp.Repository.ProjectsV2.Nodes...)
		if !resp.Repository.ProjectsV2.PageInfo.HasNextPage {
			break
		}
		cursor = resp.Repository.ProjectsV2.PageInfo.EndCursor
	}
	return all, nil
}

func fetchProjectItems(c *github.Client, projectID string) ([]map[string]any, error) {
	var all []map[string]any
	cursor := ""
	for {
		vars := map[string]any{"projectId": projectID}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		raw, err := c.GraphQL(listItemsQuery, vars)
		if err != nil {
			return all, err
		}
		var resp struct {
			Node struct {
				Items struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []map[string]any `json:"nodes"`
				} `json:"items"`
			} `json:"node"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return all, fmt.Errorf("parsing items response: %w", err)
		}
		all = append(all, resp.Node.Items.Nodes...)
		if !resp.Node.Items.PageInfo.HasNextPage {
			break
		}
		cursor = resp.Node.Items.PageInfo.EndCursor
	}
	return all, nil
}

func projectEvent(eventType string, p map[string]any, relPath, repoSlug string) hooks.Event {
	number := jsonutil.Int(p, "number")
	state := "open"
	if jsonutil.Bool(p, "closed") {
		state = "closed"
	}
	return hooks.Event{
		Type:   eventType,
		Number: number,
		Title:  jsonutil.Str(p, "title"),
		Author: ghmodel.OwnerLogin(p),
		State:  state,
		File:   relPath,
		Repo:   repoSlug,
		URL:    jsonutil.Str(p, "url"),
		Body:   jsonutil.Str(p, "shortDescription"),
	}
}

// prevProjectItem captures the prior state of a project item for diffing.
type prevProjectItem struct {
	Title  string
	Type   string
	Repo   string
	Fields map[string]string
}

// itemExtra builds the standard extra-fields map describing a project item.
func itemExtra(n int64, content, item map[string]any) map[string]string {
	out := map[string]string{
		"item_number": fmt.Sprintf("%d", n),
		"item_title":  jsonutil.Str(content, "title"),
		"item_type":   strings.ToLower(jsonutil.Str(item, "type")),
	}
	if r := jsonutil.Map(content, "repository"); r != nil {
		out["item_repo"] = jsonutil.Str(r, "nameWithOwner")
	}
	return out
}

// buildPrevItems converts stored item maps into the diff-friendly prev map keyed
// by issue/PR number.
func buildPrevItems(items []map[string]any) map[int64]*prevProjectItem {
	out := map[int64]*prevProjectItem{}
	for _, item := range items {
		content := jsonutil.Map(item, "content")
		if content == nil {
			continue
		}
		n := jsonutil.Int(content, "number")
		if n == 0 {
			continue
		}
		repoName := ""
		if r := jsonutil.Map(content, "repository"); r != nil {
			repoName = jsonutil.Str(r, "nameWithOwner")
		}
		out[n] = &prevProjectItem{
			Title:  jsonutil.Str(content, "title"),
			Type:   strings.ToLower(jsonutil.Str(item, "type")),
			Repo:   repoName,
			Fields: ghmodel.ExtractItemFields(item),
		}
	}
	return out
}
