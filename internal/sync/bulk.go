package sync

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github-export/internal/github"
	"github-export/internal/jsonutil"
)

// issueNumberFromURL extracts the issue/PR number from a GitHub API URL
// e.g. "https://api.github.com/repos/owner/repo/issues/42" → 42
func issueNumberFromURL(url string) int64 {
	idx := strings.LastIndex(url, "/")
	if idx < 0 || idx == len(url)-1 {
		return 0
	}
	n, err := strconv.ParseInt(url[idx+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// fetchAllIssueComments fetches all issue comments for the repo using the
// bulk endpoint GET /repos/{owner}/{repo}/issues/comments.
// Each comment is normalized with event="commented" for timeline compatibility.
// Returns comments grouped by issue number.
func fetchAllIssueComments(c *github.Client, owner, repo, since string) (map[int64][]map[string]any, error) {
	log.Println("  Fetching all issue comments (bulk)...")
	url := fmt.Sprintf("%s/repos/%s/%s/issues/comments?per_page=%d&sort=created&direction=asc",
		github.API, owner, repo, github.PerPage)
	if since != "" {
		url += "&since=" + since
	}
	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching issue comments: %w", err)
	}
	log.Printf("    %d comments", len(items))

	result := make(map[int64][]map[string]any)
	for _, raw := range items {
		var m map[string]any
		json.Unmarshal(raw, &m)
		num := issueNumberFromURL(jsonutil.Str(m, "issue_url"))
		if num == 0 {
			continue
		}
		m["event"] = "commented"
		result[num] = append(result[num], m)
	}
	return result, nil
}

// fetchAllIssueEvents fetches all issue events for the repo using the
// bulk endpoint GET /repos/{owner}/{repo}/issues/events.
// Returns events grouped by issue number.
func fetchAllIssueEvents(c *github.Client, owner, repo string) (map[int64][]map[string]any, error) {
	log.Println("  Fetching all issue events (bulk)...")
	url := fmt.Sprintf("%s/repos/%s/%s/issues/events?per_page=%d",
		github.API, owner, repo, github.PerPage)
	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching issue events: %w", err)
	}
	log.Printf("    %d events", len(items))

	result := make(map[int64][]map[string]any)
	for _, raw := range items {
		var m map[string]any
		json.Unmarshal(raw, &m)
		issue := jsonutil.Map(m, "issue")
		if issue == nil {
			continue
		}
		num := jsonutil.Int(issue, "number")
		if num == 0 {
			continue
		}
		delete(m, "issue") // remove embedded issue object to save memory
		result[num] = append(result[num], m)
	}
	return result, nil
}

// fetchAllPRs fetches all pull requests for the repo using the
// bulk endpoint GET /repos/{owner}/{repo}/pulls?state=all.
// Returns PR details mapped by PR number.
// Note: the list endpoint doesn't include "merged" (boolean) or "merged_by",
// so we infer "merged" from "merged_at".
func fetchAllPRs(c *github.Client, owner, repo string) (map[int64]map[string]any, error) {
	log.Println("  Fetching all pull requests (bulk)...")
	url := fmt.Sprintf("%s/repos/%s/%s/pulls?state=all&per_page=%d",
		github.API, owner, repo, github.PerPage)
	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching pull requests: %w", err)
	}
	log.Printf("    %d pull requests", len(items))

	result := make(map[int64]map[string]any)
	for _, raw := range items {
		var m map[string]any
		json.Unmarshal(raw, &m)
		num := jsonutil.Int(m, "number")
		if num == 0 {
			continue
		}
		// Infer merged boolean from merged_at (list endpoint omits "merged" field)
		m["merged"] = jsonutil.Str(m, "merged_at") != ""
		result[num] = m
	}
	return result, nil
}

// fetchAllReviewComments fetches all pull request review comments using the
// bulk endpoint GET /repos/{owner}/{repo}/pulls/comments.
// Returns review comments grouped by PR number.
func fetchAllReviewComments(c *github.Client, owner, repo, since string) (map[int64][]map[string]any, error) {
	log.Println("  Fetching all review comments (bulk)...")
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/comments?per_page=%d&sort=created&direction=asc",
		github.API, owner, repo, github.PerPage)
	if since != "" {
		url += "&since=" + since
	}
	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching review comments: %w", err)
	}
	log.Printf("    %d review comments", len(items))

	result := make(map[int64][]map[string]any)
	for _, raw := range items {
		var m map[string]any
		json.Unmarshal(raw, &m)
		num := issueNumberFromURL(jsonutil.Str(m, "pull_request_url"))
		if num == 0 {
			continue
		}
		result[num] = append(result[num], m)
	}
	return result, nil
}

// fetchReviews fetches reviews for a single PR.
// Still per-PR since there is no bulk reviews endpoint.
func fetchReviews(c *github.Client, owner, repo string, number int64) ([]map[string]any, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews?per_page=%d",
		github.API, owner, repo, number, github.PerPage)
	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return nil, err
	}
	var reviews []map[string]any
	for _, raw := range items {
		var m map[string]any
		json.Unmarshal(raw, &m)
		reviews = append(reviews, m)
	}
	return reviews, nil
}

// timelineTimestamp returns the most appropriate timestamp for sorting a timeline entry.
func timelineTimestamp(m map[string]any) string {
	if t := jsonutil.Str(m, "submitted_at"); t != "" {
		return t
	}
	return jsonutil.Str(m, "created_at")
}

// buildTimeline merges comments, events, review comments, and reviews from
// separate bulk endpoints into a single chronologically sorted timeline.
func buildTimeline(comments, events []map[string]any, reviewComments []map[string]any, reviews []map[string]any) []map[string]any {
	var timeline []map[string]any
	timeline = append(timeline, comments...)
	timeline = append(timeline, events...)

	// Add reviews as "reviewed" timeline events
	for _, r := range reviews {
		r["event"] = "reviewed"
		timeline = append(timeline, r)
	}

	// Group review comments by pull_request_review_id into "line-commented" events.
	// This matches the structure returned by the per-issue timeline endpoint.
	reviewGroups := make(map[int64][]any)
	var ungrouped []any
	for _, rc := range reviewComments {
		reviewID := jsonutil.Int(rc, "pull_request_review_id")
		if reviewID > 0 {
			reviewGroups[reviewID] = append(reviewGroups[reviewID], rc)
		} else {
			ungrouped = append(ungrouped, rc)
		}
	}

	for _, group := range reviewGroups {
		earliest := ""
		for _, c := range group {
			cm := c.(map[string]any)
			t := jsonutil.Str(cm, "created_at")
			if earliest == "" || t < earliest {
				earliest = t
			}
		}
		timeline = append(timeline, map[string]any{
			"event":      "line-commented",
			"comments":   group,
			"created_at": earliest,
		})
	}

	for _, rc := range ungrouped {
		timeline = append(timeline, map[string]any{
			"event":      "line-commented",
			"comments":   []any{rc},
			"created_at": jsonutil.Str(rc.(map[string]any), "created_at"),
		})
	}

	// Sort chronologically
	sort.Slice(timeline, func(i, j int) bool {
		return timelineTimestamp(timeline[i]) < timelineTimestamp(timeline[j])
	})

	return timeline
}
