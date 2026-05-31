package sync

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"

	"github.com/mevdschee/github-export/internal/ghmodel"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/store"
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

// Discussions syncs GitHub Discussions into the store. Mirrors the PR-bulk
// behavior: paginates updated_at-descending and stops once a page's oldest item
// is older than `since`. Emits discussion_created / _closed / _answered /
// _comment_created events by diffing against the stored discussion.
func Discussions(c *github.Client, s *store.Store, owner, repo, since string) ([]hooks.Event, error) {
	log.Println("Syncing discussions...")

	repoSlug := owner + "/" + repo

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
					Nodes []ghmodel.DiscussionNode `json:"nodes"`
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

			relPath := filepath.Join("discussions", fmt.Sprintf("%04d.md", d.Number))
			prev, existed, err := s.DiscussionNode(d.Number)
			if err != nil {
				log.Printf("  Warning: reading discussion #%d: %v", d.Number, err)
			}

			events = append(events, detectDiscussionEvents(d, existed, prev, relPath, repoSlug)...)

			if err := s.UpsertDiscussion(d); err != nil {
				log.Printf("  Warning: storing discussion #%d: %v", d.Number, err)
				continue
			}
			processed++

			warnDiscussionTruncation(d)
		}

		if hitCutoff || !resp.Repository.Discussions.PageInfo.HasNextPage {
			break
		}
		cursor = resp.Repository.Discussions.PageInfo.EndCursor
	}

	log.Printf("  %d discussions processed", processed)
	return events, nil
}

func warnDiscussionTruncation(d ghmodel.DiscussionNode) {
	if d.Comments.PageInfo.HasNextPage {
		log.Printf("  Warning: discussion #%d has more than 100 top-level comments — only first 100 stored", d.Number)
	}
	for _, c := range d.Comments.Nodes {
		if c.Replies.PageInfo.HasNextPage {
			log.Printf("  Warning: discussion #%d comment %d has more than 50 replies — only first 50 stored", d.Number, c.DatabaseID)
		}
	}
}

func detectDiscussionEvents(d ghmodel.DiscussionNode, existed bool, prev ghmodel.DiscussionNode, relPath, repoSlug string) []hooks.Event {
	state := ghmodel.DiscussionState(d)
	base := hooks.Event{
		Number: d.Number,
		Title:  d.Title,
		Author: d.Author.Login,
		State:  state,
		File:   relPath,
		Repo:   repoSlug,
		URL:    d.URL,
	}

	var events []hooks.Event

	if !existed {
		created := base
		created.Type = hooks.DiscussionCreated
		created.Body = d.Body
		return append(events, created)
	}

	if state == "closed" && ghmodel.DiscussionState(prev) == "open" {
		ev := base
		ev.Type = hooks.DiscussionClosed
		events = append(events, ev)
	}

	if d.AnswerID() != 0 && prev.AnswerID() == 0 {
		ev := base
		ev.Type = hooks.DiscussionAnswered
		extra := map[string]string{"answer_id": fmt.Sprintf("%d", d.AnswerID())}
		if d.AnswerChosenAt != "" {
			extra["answer_chosen_at"] = d.AnswerChosenAt
		}
		if d.AnswerChosenBy != nil && d.AnswerChosenBy.Login != "" {
			extra["answer_chosen_by"] = d.AnswerChosenBy.Login
		}
		ev.Extra = extra
		events = append(events, ev)
	}

	prevComments := map[int64]bool{}
	for _, c := range prev.Comments.Nodes {
		prevComments[c.DatabaseID] = true
	}
	for _, c := range d.Comments.Nodes {
		if prevComments[c.DatabaseID] {
			continue
		}
		ev := base
		ev.Type = hooks.DiscussionCommentCreated
		ev.Author = c.Author.Login
		ev.Body = c.Body
		ev.Extra = map[string]string{
			"comment_id": fmt.Sprintf("%d", c.DatabaseID),
			"created_at": c.CreatedAt,
		}
		events = append(events, ev)
	}

	return events
}
