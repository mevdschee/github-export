package sync

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/mevdschee/github-export/internal/ghmodel"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/jsonutil"
	"github.com/mevdschee/github-export/internal/store"
)

// prevIssue is the prior on-store state of an issue/PR needed to detect
// transitions.
type prevIssue struct {
	exists bool
	state  string
	merged bool
}

func issueRelPath(number int64, pad int) string {
	return filepath.Join("issues", ghmodel.IssueFilename(number, pad))
}

func Issues(c *github.Client, s *store.Store, owner, repo, since string, issueProjects map[int64][]string) ([]hooks.Event, error) {
	log.Println("Syncing issues and pull requests...")

	url := fmt.Sprintf("%s/repos/%s/%s/issues?state=all&sort=updated&direction=asc&per_page=%d",
		github.API, owner, repo, github.PerPage)
	if since != "" {
		url += "&since=" + since
	}

	items, err := c.GetPaginated(url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching issues: %w", err)
	}

	var issues []map[string]any
	var maxNum int64
	for _, raw := range items {
		var m map[string]any
		json.Unmarshal(raw, &m)
		issues = append(issues, m)
		if n := jsonutil.Int(m, "number"); n > maxNum {
			maxNum = n
		}
	}
	// Existing stored issues may have a higher number than this (incremental)
	// page; keep the pad width stable across runs.
	if storeMax, err := s.MaxIssueNumber(); err == nil && storeMax > maxNum {
		maxNum = storeMax
	}
	pad := ghmodel.PadWidth(maxNum)

	if len(issues) == 0 {
		log.Println("  No issues to process")
		return nil, nil
	}

	log.Printf("  %d issues/PRs to process", len(issues))

	if since == "" {
		return syncIssuesFull(c, s, owner, repo, pad, issues, issueProjects)
	}
	return syncIssuesIncremental(c, s, owner, repo, pad, issues, since, issueProjects)
}

// detectEvents compares current API data with the prior stored state and
// returns hook events for this issue/PR.
func detectEvents(relPath string, issue map[string]any, isPR bool, pr map[string]any, owner, repo string, prev prevIssue) []hooks.Event {
	number := jsonutil.Int(issue, "number")
	state := jsonutil.Str(issue, "state")
	repoSlug := owner + "/" + repo

	base := hooks.Event{
		Number: number,
		Title:  jsonutil.Str(issue, "title"),
		Author: jsonutil.UserLogin(issue, "user"),
		State:  state,
		Labels: jsonutil.LabelNames(issue, "labels"),
		File:   relPath,
		Repo:   repoSlug,
	}

	var events []hooks.Event

	if !prev.exists {
		ev := base
		ev.Body = jsonutil.Str(issue, "body")
		if isPR {
			ev.Type = hooks.PRCreated
		} else {
			ev.Type = hooks.IssueCreated
		}
		return append(events, ev)
	}

	if isPR {
		merged := pr != nil && jsonutil.Bool(pr, "merged")
		if merged && !prev.merged {
			ev := base
			ev.Type = hooks.PRMerged
			events = append(events, ev)
		} else if state == "closed" && prev.state == "open" && !merged {
			ev := base
			ev.Type = hooks.PRClosed
			events = append(events, ev)
		} else if state == "open" && prev.state == "closed" {
			ev := base
			ev.Type = hooks.PRReopened
			events = append(events, ev)
		}
	} else {
		if state == "closed" && prev.state == "open" {
			ev := base
			ev.Type = hooks.IssueClosed
			events = append(events, ev)
		}
		if state == "open" && prev.state == "closed" {
			ev := base
			ev.Type = hooks.IssueReopened
			events = append(events, ev)
		}
	}

	return events
}

// detectTimelineEvents walks the timeline and emits one hook event per entry
// that landed after the last sync. Only fires on incremental syncs (since != "").
func detectTimelineEvents(relPath string, issue map[string]any, isPR bool, timeline []map[string]any, since, owner, repo string) []hooks.Event {
	if since == "" {
		return nil
	}

	number := jsonutil.Int(issue, "number")
	title := jsonutil.Str(issue, "title")
	state := jsonutil.Str(issue, "state")
	labels := jsonutil.LabelNames(issue, "labels")
	repoSlug := owner + "/" + repo

	base := func(actor string) hooks.Event {
		return hooks.Event{
			Number: number,
			Title:  title,
			Author: actor,
			State:  state,
			Labels: labels,
			File:   relPath,
			Repo:   repoSlug,
		}
	}

	var events []hooks.Event
	for _, ev := range timeline {
		evType := jsonutil.Str(ev, "event")
		createdAt := jsonutil.Str(ev, "created_at")
		if evType == "reviewed" {
			createdAt = jsonutil.Str(ev, "submitted_at")
		}
		if createdAt == "" || createdAt < since {
			continue
		}

		actor := jsonutil.UserLogin(ev, "actor")

		switch evType {
		case "commented":
			e := base(jsonutil.UserLogin(ev, "user"))
			e.Type = hooks.CommentCreated
			e.Body = jsonutil.Str(ev, "body")
			events = append(events, e)

		case "assigned", "unassigned":
			e := base(actor)
			if evType == "assigned" {
				e.Type = hooks.Assigned
			} else {
				e.Type = hooks.Unassigned
			}
			if assignee := jsonutil.UserLogin(ev, "assignee"); assignee != "" {
				e.Extra = map[string]string{"assignee": assignee}
			}
			events = append(events, e)

		case "labeled", "unlabeled":
			label := jsonutil.Map(ev, "label")
			if label == nil {
				continue
			}
			name := jsonutil.Str(label, "name")
			if name == "" {
				continue
			}
			e := base(actor)
			if evType == "labeled" {
				e.Type = hooks.LabelAdded
			} else {
				e.Type = hooks.LabelRemoved
			}
			e.Extra = map[string]string{"label": name}
			events = append(events, e)

		case "mentioned":
			e := base(actor)
			e.Type = hooks.Mentioned
			events = append(events, e)

		case "review_requested":
			if !isPR {
				continue
			}
			e := base(actor)
			e.Type = hooks.PRReviewRequested
			if reviewer := jsonutil.UserLogin(ev, "requested_reviewer"); reviewer != "" {
				e.Extra = map[string]string{"reviewer": reviewer}
			}
			events = append(events, e)

		case "reviewed":
			if !isPR {
				continue
			}
			e := base(jsonutil.UserLogin(ev, "user"))
			e.Type = hooks.PRReviewed
			e.Body = jsonutil.Str(ev, "body")
			if rs := strings.ToLower(jsonutil.Str(ev, "state")); rs != "" {
				e.Extra = map[string]string{"review_state": rs}
			}
			events = append(events, e)

		case "ready_for_review":
			if !isPR {
				continue
			}
			e := base(actor)
			e.Type = hooks.PRReadyForReview
			events = append(events, e)

		case "cross-referenced":
			source := jsonutil.Map(ev, "source")
			if source == nil {
				continue
			}
			si := jsonutil.Map(source, "issue")
			if si == nil || si["pull_request"] == nil {
				continue
			}
			e := base(actor)
			e.Type = hooks.LinkedToPR
			extra := map[string]string{}
			if n := jsonutil.Int(si, "number"); n > 0 {
				extra["source_number"] = fmt.Sprintf("%d", n)
			}
			if sr := jsonutil.Map(si, "repository"); sr != nil {
				if fn := jsonutil.Str(sr, "full_name"); fn != "" {
					extra["source_repo"] = fn
				}
			}
			if len(extra) > 0 {
				e.Extra = extra
			}
			events = append(events, e)

		case "connected":
			e := base(actor)
			e.Type = hooks.LinkedToPR
			events = append(events, e)

		case "marked_as_duplicate":
			e := base(actor)
			e.Type = hooks.DuplicateMarked
			events = append(events, e)
		}
	}
	return events
}

// readPrevIssue reads the prior stored state for change detection.
func readPrevIssue(s *store.Store, number int64) prevIssue {
	exists, state, _, merged, err := s.IssueState(number)
	if err != nil {
		log.Printf("  Warning: reading issue #%d state: %v", number, err)
		return prevIssue{}
	}
	return prevIssue{exists: exists, state: state, merged: merged}
}

// syncIssuesFull uses bulk endpoints to minimize API calls for full syncs.
func syncIssuesFull(c *github.Client, s *store.Store, owner, repo string, pad int, issues []map[string]any, issueProjects map[int64][]string) ([]hooks.Event, error) {
	comments, err := fetchAllIssueComments(c, owner, repo, "")
	if err != nil {
		log.Printf("  Warning: %v", err)
		comments = make(map[int64][]map[string]any)
	}
	ghEvents, err := fetchAllIssueEvents(c, owner, repo)
	if err != nil {
		log.Printf("  Warning: %v", err)
		ghEvents = make(map[int64][]map[string]any)
	}
	prDetails, err := fetchAllPRs(c, owner, repo, "")
	if err != nil {
		log.Printf("  Warning: %v", err)
		prDetails = make(map[int64]map[string]any)
	}
	reviewComments, err := fetchAllReviewComments(c, owner, repo, "")
	if err != nil {
		log.Printf("  Warning: %v", err)
		reviewComments = make(map[int64][]map[string]any)
	}
	allReviews, err := fetchAllReviewsGraphQL(c, owner, repo)
	if err != nil {
		log.Printf("  Warning: %v", err)
		allReviews = make(map[int64][]map[string]any)
	}

	var hookEvents []hooks.Event
	for i, issue := range issues {
		number := jsonutil.Int(issue, "number")
		isPR := issue["pull_request"] != nil

		if (i+1)%100 == 0 || i == len(issues)-1 {
			log.Printf("  Storing %d/%d (#%d)", i+1, len(issues), number)
		}

		var pr map[string]any
		var rc, reviews []map[string]any
		if isPR {
			pr = prDetails[number]
			rc = reviewComments[number]
			reviews = allReviews[number]
		}

		timeline := buildTimeline(comments[number], ghEvents[number], rc, reviews)
		relPath := issueRelPath(number, pad)

		prev := readPrevIssue(s, number)
		hookEvents = append(hookEvents, detectEvents(relPath, issue, isPR, pr, owner, repo, prev)...)

		if err := s.UpsertIssue(number, isPR, issue, pr, timeline, issueProjects[number]); err != nil {
			log.Printf("  Warning: storing #%d: %v", number, err)
		}
	}

	return hookEvents, nil
}

// syncIssuesIncremental uses the per-issue timeline endpoint for changed issues
// combined with bulk PR details.
func syncIssuesIncremental(c *github.Client, s *store.Store, owner, repo string, pad int, issues []map[string]any, since string, issueProjects map[int64][]string) ([]hooks.Event, error) {
	prDetails, err := fetchAllPRs(c, owner, repo, since)
	if err != nil {
		log.Printf("  Warning: %v", err)
		prDetails = make(map[int64]map[string]any)
	}

	var hookEvents []hooks.Event
	for i, issue := range issues {
		number := jsonutil.Int(issue, "number")
		isPR := issue["pull_request"] != nil

		if (i+1)%50 == 0 || i == len(issues)-1 {
			log.Printf("  Processing %d/%d (#%d)", i+1, len(issues), number)
		}

		timelineURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/timeline?per_page=%d",
			github.API, owner, repo, number, github.PerPage)
		timelineRaw, err := c.GetPaginated(timelineURL, nil)
		if err != nil {
			log.Printf("  Warning: timeline for #%d: %v", number, err)
			timelineRaw = nil
		}
		var timeline []map[string]any
		for _, r := range timelineRaw {
			var m map[string]any
			json.Unmarshal(r, &m)
			timeline = append(timeline, m)
		}

		var pr map[string]any
		if isPR {
			pr = prDetails[number]
		}

		relPath := issueRelPath(number, pad)

		prev := readPrevIssue(s, number)
		hookEvents = append(hookEvents, detectEvents(relPath, issue, isPR, pr, owner, repo, prev)...)
		hookEvents = append(hookEvents, detectTimelineEvents(relPath, issue, isPR, timeline, since, owner, repo)...)

		if err := s.UpsertIssue(number, isPR, issue, pr, timeline, issueProjects[number]); err != nil {
			log.Printf("  Warning: storing #%d: %v", number, err)
		}
	}

	return hookEvents, nil
}
