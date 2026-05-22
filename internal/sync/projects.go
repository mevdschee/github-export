package sync

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mevdschee/github-export/internal/document"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/jsonutil"

	"gopkg.in/yaml.v3"
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

// Projects exports GitHub Projects v2 linked to the repository.
//
// Behavior:
//   - Lists repository.projectsV2 via GraphQL (open + closed are both fetched
//     so we can detect open→closed transitions; only open ones are written).
//   - If since is non-empty, projects whose updatedAt is older than since are
//     skipped (no rewrite, no diff).
//   - Closed projects whose file exists are deleted; a project_closed event is
//     emitted.
//   - Each kept project produces github-data/projects/<number>.md with the
//     project header + one document per item.
//   - Returns a map of issue/PR number → project titles for the issues sync to
//     cross-link, plus any hook events.
func Projects(c *github.Client, owner, repo, outDir, since string) (map[int64][]string, []hooks.Event, error) {
	log.Println("Syncing projects...")

	projects, err := fetchAllProjects(c, owner, repo)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching projects: %w", err)
	}
	log.Printf("  %d projects on repo", len(projects))

	repoSlug := owner + "/" + repo
	projectsDir := filepath.Join(outDir, "projects")
	if err := os.MkdirAll(projectsDir, 0755); err != nil {
		return nil, nil, fmt.Errorf("creating projects dir: %w", err)
	}

	issueProjects := make(map[int64][]string)
	var events []hooks.Event

	for _, p := range projects {
		number := jsonutil.Int(p, "number")
		title := jsonutil.Str(p, "title")
		closed := jsonutil.Bool(p, "closed")
		updatedAt := jsonutil.Str(p, "updatedAt")
		path := filepath.Join(projectsDir, fmt.Sprintf("%04d.md", number))
		fileExists := fileExistsOrFalse(path)

		// Closed: delete file if it existed, emit project_closed
		if closed {
			if fileExists {
				log.Printf("  Project #%d %q closed — removing file", number, title)
				os.Remove(path)
				events = append(events, projectEvent(hooks.ProjectClosed, p, path, repoSlug))
			}
			continue
		}

		// Incremental: skip unchanged open projects
		if since != "" && updatedAt < since && fileExists {
			continue
		}

		// Fetch items for this project
		items, err := fetchProjectItems(c, jsonutil.Str(p, "id"))
		if err != nil {
			log.Printf("  Warning: items for project #%d: %v", number, err)
			items = nil
		}

		// Track which issues/PRs this project contains (for the cross-link)
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

		// Detect events: project_created (new file) or item_added (diff against prev)
		if !fileExists {
			events = append(events, projectEvent(hooks.ProjectCreated, p, path, repoSlug))
		} else {
			prevItems := readPrevProjectItems(path)
			for _, item := range items {
				content := jsonutil.Map(item, "content")
				if content == nil {
					continue
				}
				n := jsonutil.Int(content, "number")
				if n == 0 || prevItems[n] {
					continue
				}
				ev := projectEvent(hooks.ItemAdded, p, path, repoSlug)
				ev.Extra = map[string]string{
					"item_number": fmt.Sprintf("%d", n),
					"item_title":  jsonutil.Str(content, "title"),
					"item_type":   strings.ToLower(jsonutil.Str(item, "type")),
				}
				if r := jsonutil.Map(content, "repository"); r != nil {
					ev.Extra["item_repo"] = jsonutil.Str(r, "nameWithOwner")
				}
				events = append(events, ev)
			}
		}

		if err := writeProjectFile(path, p, items); err != nil {
			log.Printf("  Warning: writing project #%d: %v", number, err)
		}
	}

	return issueProjects, events, nil
}

func fileExistsOrFalse(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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

func projectEvent(eventType string, p map[string]any, path, repoSlug string) hooks.Event {
	number := jsonutil.Int(p, "number")
	state := "open"
	if jsonutil.Bool(p, "closed") {
		state = "closed"
	}
	return hooks.Event{
		Type:   eventType,
		Number: number,
		Title:  jsonutil.Str(p, "title"),
		Author: ownerLogin(p),
		State:  state,
		File:   path,
		Repo:   repoSlug,
		URL:    jsonutil.Str(p, "url"),
		Body:   jsonutil.Str(p, "shortDescription"),
	}
}

func ownerLogin(p map[string]any) string {
	o := jsonutil.Map(p, "owner")
	if o == nil {
		return ""
	}
	return jsonutil.Str(o, "login")
}

func writeProjectFile(path string, p map[string]any, items []map[string]any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	d := &document.Writer{}
	d.KV("number", jsonutil.Int(p, "number"))
	d.KV("title", jsonutil.Str(p, "title"))
	d.KV("state", "open")
	if jsonutil.Bool(p, "public") {
		d.KV("public", true)
	}
	d.KV("url", jsonutil.Str(p, "url"))
	d.KV("owner", ownerLogin(p))
	if desc := jsonutil.Str(p, "shortDescription"); desc != "" {
		d.KV("description", desc)
	}
	d.KV("created_at", jsonutil.Str(p, "createdAt"))
	d.KV("updated_at", jsonutil.Str(p, "updatedAt"))

	// Field definitions
	fieldsMap := jsonutil.Map(p, "fields")
	fieldNodes := jsonutil.List(fieldsMap, "nodes")
	if len(fieldNodes) > 0 {
		fmt.Fprint(d.Buf(), "fields:\n")
		for _, fn := range fieldNodes {
			fm, _ := fn.(map[string]any)
			if fm == nil {
				continue
			}
			name := jsonutil.Str(fm, "name")
			if name == "" {
				continue
			}
			fmt.Fprintf(d.Buf(), "  - name: %s\n", document.YamlScalar(name))
			if dt := jsonutil.Str(fm, "dataType"); dt != "" {
				fmt.Fprintf(d.Buf(), "    type: %s\n", document.YamlScalar(dt))
			}
			if opts := jsonutil.List(fm, "options"); len(opts) > 0 {
				fmt.Fprint(d.Buf(), "    options:\n")
				for _, o := range opts {
					om, _ := o.(map[string]any)
					if om == nil {
						continue
					}
					if optName := jsonutil.Str(om, "name"); optName != "" {
						fmt.Fprintf(d.Buf(), "      - %s\n", document.YamlScalar(optName))
					}
				}
			}
		}
	}

	document.WriteFirstDoc(f, d.String(), jsonutil.Str(p, "readme"))

	for _, item := range items {
		writeProjectItem(f, item)
	}
	return nil
}

func writeProjectItem(f *os.File, item map[string]any) {
	content := jsonutil.Map(item, "content")
	if content == nil {
		return
	}
	typename := jsonutil.Str(content, "__typename")
	var itemType string
	switch typename {
	case "Issue":
		itemType = "issue"
	case "PullRequest":
		itemType = "pull_request"
	default:
		// Skip drafts and anything else we don't model
		return
	}

	d := &document.Writer{}
	d.KV("document", "item")
	d.KV("type", itemType)
	d.KV("number", jsonutil.Int(content, "number"))
	d.KV("title", jsonutil.Str(content, "title"))
	if r := jsonutil.Map(content, "repository"); r != nil {
		d.KV("repo", jsonutil.Str(r, "nameWithOwner"))
	}

	// Field values
	fvMap := jsonutil.Map(item, "fieldValues")
	fvNodes := jsonutil.List(fvMap, "nodes")
	var pairs [][2]string
	for _, fv := range fvNodes {
		fvm, _ := fv.(map[string]any)
		if fvm == nil {
			continue
		}
		fieldName := ""
		if fld := jsonutil.Map(fvm, "field"); fld != nil {
			fieldName = jsonutil.Str(fld, "name")
		}
		if fieldName == "" {
			continue
		}
		value := fieldValueString(fvm)
		if value == "" {
			continue
		}
		pairs = append(pairs, [2]string{fieldName, value})
	}
	if len(pairs) > 0 {
		sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
		fmt.Fprint(d.Buf(), "fields:\n")
		for _, kv := range pairs {
			fmt.Fprintf(d.Buf(), "  %s: %s\n", document.YamlScalar(kv[0]), document.YamlScalar(kv[1]))
		}
	}

	document.WriteSubDoc(f, d.String(), "")
}

func fieldValueString(fv map[string]any) string {
	switch jsonutil.Str(fv, "__typename") {
	case "ProjectV2ItemFieldTextValue":
		return jsonutil.Str(fv, "text")
	case "ProjectV2ItemFieldNumberValue":
		return jsonutil.Str(fv, "number")
	case "ProjectV2ItemFieldDateValue":
		return jsonutil.Str(fv, "date")
	case "ProjectV2ItemFieldSingleSelectValue":
		return jsonutil.Str(fv, "name")
	case "ProjectV2ItemFieldIterationValue":
		return jsonutil.Str(fv, "title")
	}
	return ""
}

// prevProjectItems lists which issue/PR numbers were previously recorded as
// items of a project file. Used to detect newly-added items.
func readPrevProjectItems(path string) map[int64]bool {
	out := map[int64]bool{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()

	// A project file looks like:
	//   ---
	//   <frontmatter>
	//   ---
	//   <readme body>
	//   ---
	//   document: item
	//   ...
	//   number: 42
	//   ---
	// We walk by document and parse each one whose document field is "item".
	scanner := bufio.NewScanner(f)
	// Allow long lines (readmes can be big).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var current []string
	inFrontmatter := false
	flush := func() {
		if len(current) == 0 {
			return
		}
		var doc struct {
			Document string `yaml:"document"`
			Number   int64  `yaml:"number"`
		}
		if err := yaml.Unmarshal([]byte(strings.Join(current, "\n")), &doc); err == nil {
			if doc.Document == "item" && doc.Number > 0 {
				out[doc.Number] = true
			}
		}
		current = nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			if inFrontmatter {
				flush()
				inFrontmatter = false
			} else {
				inFrontmatter = true
			}
			continue
		}
		if inFrontmatter {
			current = append(current, line)
		}
	}
	return out
}
