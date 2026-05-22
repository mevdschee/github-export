package sync

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/mevdschee/github-export/internal/document"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/hooks"
)

// listDiscussionsQuery fetches discussions sorted by updated_at descending so
// the caller can stop pagination once it crosses a cutoff. Per-page node-cost
// budget (~500k): 50 discussions × (100 comments × (1 + 50 replies)) ≈ 256k.
const listDiscussionsQuery = `
query($owner: String!, $name: String!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    discussions(first: 50, after: $cursor, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        title
        body
        createdAt
        updatedAt
        closed
        closedAt
        stateReason
        locked
        url
        author { login }
        category { name emoji isAnswerable }
        labels(first: 20) { nodes { name } }
        answer { databaseId }
        answerChosenAt
        answerChosenBy { login }
        comments(first: 100) {
          pageInfo { hasNextPage }
          nodes {
            databaseId
            createdAt
            body
            author { login }
            replies(first: 50) {
              pageInfo { hasNextPage }
              nodes {
                databaseId
                createdAt
                body
                author { login }
              }
            }
          }
        }
      }
    }
  }
}`

// Discussions exports GitHub Discussions linked to the repository. Mirrors the
// PR-bulk-reviews behavior: paginates updated_at-descending and stops once a
// page's oldest item is older than `since`.
//
// File layout: one markdown file per discussion at
// `github-data/discussions/<number>.md`. Top-level comments are emitted as
// `document: comment` sub-documents; nested replies as `document: reply` with
// a `parent_id` pointing at the comment they reply to.
//
// Hook events: emits `discussion_created` for any discussion whose file did
// not previously exist on disk.
func Discussions(c *github.Client, owner, repo, outDir, since string) ([]hooks.Event, error) {
	log.Println("Syncing discussions...")

	repoSlug := owner + "/" + repo
	dir := filepath.Join(outDir, "discussions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating discussions dir: %w", err)
	}

	var events []hooks.Event
	var processed int
	cursor := ""

	for {
		vars := map[string]any{"owner": owner, "name": repo}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		raw, err := c.GraphQL(listDiscussionsQuery, vars)
		if err != nil {
			return events, fmt.Errorf("fetching discussions: %w", err)
		}
		var resp struct {
			Repository struct {
				Discussions struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []discussionNode `json:"nodes"`
				} `json:"discussions"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return events, fmt.Errorf("parsing discussions response: %w", err)
		}

		nodes := resp.Repository.Discussions.Nodes
		hitCutoff := false
		for _, d := range nodes {
			if since != "" && d.UpdatedAt < since {
				hitCutoff = true
				continue
			}
			path := filepath.Join(dir, fmt.Sprintf("%04d.md", d.Number))
			isNew := !fileExistsOrFalse(path)

			if err := writeDiscussionFile(path, d); err != nil {
				log.Printf("  Warning: writing discussion #%d: %v", d.Number, err)
				continue
			}
			processed++

			if isNew {
				events = append(events, hooks.Event{
					Type:   hooks.DiscussionCreated,
					Number: d.Number,
					Title:  d.Title,
					Author: d.Author.Login,
					State:  discussionState(d),
					File:   path,
					Repo:   repoSlug,
					URL:    d.URL,
					Body:   d.Body,
				})
			}
		}

		if hitCutoff || !resp.Repository.Discussions.PageInfo.HasNextPage {
			break
		}
		cursor = resp.Repository.Discussions.PageInfo.EndCursor
	}

	log.Printf("  %d discussions processed", processed)
	return events, nil
}

type discussionAuthor struct {
	Login string `json:"login"`
}

type discussionCategory struct {
	Name         string `json:"name"`
	Emoji        string `json:"emoji"`
	IsAnswerable bool   `json:"isAnswerable"`
}

type discussionReply struct {
	DatabaseID int64            `json:"databaseId"`
	CreatedAt  string           `json:"createdAt"`
	Body       string           `json:"body"`
	Author     discussionAuthor `json:"author"`
}

type discussionComment struct {
	DatabaseID int64            `json:"databaseId"`
	CreatedAt  string           `json:"createdAt"`
	Body       string           `json:"body"`
	Author     discussionAuthor `json:"author"`
	Replies    struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []discussionReply `json:"nodes"`
	} `json:"replies"`
}

type discussionNode struct {
	Number         int64              `json:"number"`
	Title          string             `json:"title"`
	Body           string             `json:"body"`
	CreatedAt      string             `json:"createdAt"`
	UpdatedAt      string             `json:"updatedAt"`
	Closed         bool               `json:"closed"`
	ClosedAt       string             `json:"closedAt"`
	StateReason    string             `json:"stateReason"`
	Locked         bool               `json:"locked"`
	URL            string             `json:"url"`
	Author         discussionAuthor   `json:"author"`
	Category       discussionCategory `json:"category"`
	Labels         struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Answer *struct {
		DatabaseID int64 `json:"databaseId"`
	} `json:"answer"`
	AnswerChosenAt string            `json:"answerChosenAt"`
	AnswerChosenBy *discussionAuthor `json:"answerChosenBy"`
	Comments       struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []discussionComment `json:"nodes"`
	} `json:"comments"`
}

func discussionState(d discussionNode) string {
	if d.Closed {
		return "closed"
	}
	return "open"
}

func writeDiscussionFile(path string, d discussionNode) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := &document.Writer{}
	w.KV("number", d.Number)
	w.KV("title", d.Title)
	w.KV("type", "discussion")
	w.KV("state", discussionState(d))
	if d.StateReason != "" {
		w.KV("state_reason", strings.ToLower(d.StateReason))
	}
	if d.Locked {
		w.KV("locked", true)
	}
	w.KV("created_at", d.CreatedAt)
	w.KV("updated_at", d.UpdatedAt)
	if d.ClosedAt != "" {
		w.KV("closed_at", d.ClosedAt)
	}
	w.KV("author", d.Author.Login)
	if d.Category.Name != "" {
		w.KV("category", d.Category.Name)
	}
	var labels []string
	for _, l := range d.Labels.Nodes {
		if l.Name != "" {
			labels = append(labels, l.Name)
		}
	}
	w.List("labels", labels)
	if d.Answer != nil {
		w.KV("answer_id", d.Answer.DatabaseID)
		if d.AnswerChosenAt != "" {
			w.KV("answer_chosen_at", d.AnswerChosenAt)
		}
		if d.AnswerChosenBy != nil && d.AnswerChosenBy.Login != "" {
			w.KV("answer_chosen_by", d.AnswerChosenBy.Login)
		}
	}

	document.WriteFirstDoc(f, w.String(), d.Body)

	// Warn on truncation rather than fall back (PR-reviews policy).
	if d.Comments.PageInfo.HasNextPage {
		log.Printf("  Warning: discussion #%d has more than 100 top-level comments — only first 100 exported", d.Number)
	}

	var answerID int64
	if d.Answer != nil {
		answerID = d.Answer.DatabaseID
	}

	for _, c := range d.Comments.Nodes {
		cw := &document.Writer{}
		cw.KV("document", "comment")
		cw.KV("id", c.DatabaseID)
		cw.KV("author", c.Author.Login)
		cw.KV("created_at", c.CreatedAt)
		if answerID != 0 && c.DatabaseID == answerID {
			cw.KV("is_answer", true)
		}
		document.WriteSubDoc(f, cw.String(), c.Body)

		if c.Replies.PageInfo.HasNextPage {
			log.Printf("  Warning: discussion #%d comment %d has more than 50 replies — only first 50 exported", d.Number, c.DatabaseID)
		}
		for _, r := range c.Replies.Nodes {
			rw := &document.Writer{}
			rw.KV("document", "reply")
			rw.KV("id", r.DatabaseID)
			rw.KV("parent_id", c.DatabaseID)
			rw.KV("author", r.Author.Login)
			rw.KV("created_at", r.CreatedAt)
			document.WriteSubDoc(f, rw.String(), r.Body)
		}
	}

	return nil
}
