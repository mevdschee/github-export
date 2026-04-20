package sync

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mevdschee/github-export/internal/document"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/jsonutil"
)

func padWidth(maxNumber int64) int {
	switch {
	case maxNumber >= 100000:
		return 6
	case maxNumber >= 10000:
		return 5
	default:
		return 4
	}
}

func issueFilename(number int64, pad int) string {
	return fmt.Sprintf("%0*d.md", pad, number)
}

func Issues(c *github.Client, owner, repo, outDir, since string) ([]hooks.Event, error) {
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

	issueDir := filepath.Join(outDir, "issues")
	os.MkdirAll(issueDir, 0755)

	// Parse issue items
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
	// Also check existing files for pad width
	if entries, err := os.ReadDir(issueDir); err == nil {
		for _, e := range entries {
			name := strings.TrimSuffix(e.Name(), ".md")
			if n, err := strconv.ParseInt(name, 10, 64); err == nil && n > maxNum {
				maxNum = n
			}
		}
	}
	pad := padWidth(maxNum)

	if len(issues) == 0 {
		log.Println("  No issues to process")
		return nil, nil
	}

	log.Printf("  %d issues/PRs to process", len(issues))

	if since == "" {
		return syncIssuesFull(c, owner, repo, issueDir, pad, issues)
	}
	return syncIssuesIncremental(c, owner, repo, issueDir, pad, issues, since)
}

// detectEvents compares the current API data with the previous on-disk state
// and returns any hook events that should fire for this issue/PR.
func detectEvents(path string, issue map[string]any, isPR bool, pr map[string]any, owner, repo string) []hooks.Event {
	number := jsonutil.Int(issue, "number")
	state := jsonutil.Str(issue, "state")
	repoSlug := owner + "/" + repo

	base := hooks.Event{
		Number: number,
		Title:  jsonutil.Str(issue, "title"),
		Author: jsonutil.UserLogin(issue, "user"),
		State:  state,
		Labels: jsonutil.LabelNames(issue, "labels"),
		File:   path,
		Repo:   repoSlug,
	}

	prev := readPrevState(path)

	var events []hooks.Event

	if prev == nil {
		// File doesn't exist — new issue or PR
		ev := base
		ev.Body = jsonutil.Str(issue, "body")
		if isPR {
			ev.Type = hooks.PRCreated
		} else {
			ev.Type = hooks.IssueCreated
		}
		events = append(events, ev)
		return events
	}

	// State transitions
	if isPR {
		merged := pr != nil && jsonutil.Bool(pr, "merged")
		if merged && !prev.Merge.Merged {
			ev := base
			ev.Type = hooks.PRMerged
			events = append(events, ev)
		} else if state == "closed" && prev.State == "open" && !merged {
			ev := base
			ev.Type = hooks.PRClosed
			events = append(events, ev)
		}
	} else {
		if state == "closed" && prev.State == "open" {
			ev := base
			ev.Type = hooks.IssueClosed
			events = append(events, ev)
		}
		if state == "open" && prev.State == "closed" {
			ev := base
			ev.Type = hooks.IssueReopened
			events = append(events, ev)
		}
	}

	return events
}

// detectCommentEvents finds new comments in the timeline created since the last sync.
func detectCommentEvents(path string, issue map[string]any, isPR bool, timeline []map[string]any, since, owner, repo string) []hooks.Event {
	if since == "" {
		return nil
	}

	number := jsonutil.Int(issue, "number")
	repoSlug := owner + "/" + repo

	var events []hooks.Event
	for _, ev := range timeline {
		evType := jsonutil.Str(ev, "event")
		if evType != "commented" {
			continue
		}
		createdAt := jsonutil.Str(ev, "created_at")
		if createdAt >= since {
			events = append(events, hooks.Event{
				Type:   hooks.CommentCreated,
				Number: number,
				Title:  jsonutil.Str(issue, "title"),
				Author: jsonutil.UserLogin(ev, "user"),
				State:  jsonutil.Str(issue, "state"),
				Labels: jsonutil.LabelNames(issue, "labels"),
				File:   path,
				Repo:   repoSlug,
				Body:   jsonutil.Str(ev, "body"),
			})
		}
	}
	return events
}

// syncIssuesFull uses bulk endpoints to minimize API calls for full syncs.
//
// API calls: ~5 paginated fetches + 1 per PR (reviews only).
// Compared to the per-issue approach (3 calls per issue), this is dramatically
// fewer calls for repos with many issues.
func syncIssuesFull(c *github.Client, owner, repo, issueDir string, pad int, issues []map[string]any) ([]hooks.Event, error) {
	// Bulk fetch all supporting data
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

	prDetails, err := fetchAllPRs(c, owner, repo)
	if err != nil {
		log.Printf("  Warning: %v", err)
		prDetails = make(map[int64]map[string]any)
	}

	reviewComments, err := fetchAllReviewComments(c, owner, repo, "")
	if err != nil {
		log.Printf("  Warning: %v", err)
		reviewComments = make(map[int64][]map[string]any)
	}

	// Fetch reviews per PR (no bulk endpoint exists)
	allReviews := make(map[int64][]map[string]any)
	prCount := 0
	for _, issue := range issues {
		if issue["pull_request"] == nil {
			continue
		}
		prCount++
		number := jsonutil.Int(issue, "number")
		reviews, err := fetchReviews(c, owner, repo, number)
		if err != nil {
			log.Printf("  Warning: reviews for #%d: %v", number, err)
			continue
		}
		allReviews[number] = reviews
	}
	if prCount > 0 {
		log.Printf("    %d PR review fetches", prCount)
	}

	// Write files and detect events
	var hookEvents []hooks.Event
	for i, issue := range issues {
		number := jsonutil.Int(issue, "number")
		isPR := issue["pull_request"] != nil

		if (i+1)%100 == 0 || i == len(issues)-1 {
			log.Printf("  Writing %d/%d (#%d)", i+1, len(issues), number)
		}

		var pr map[string]any
		var rc []map[string]any
		var reviews []map[string]any
		if isPR {
			pr = prDetails[number]
			rc = reviewComments[number]
			reviews = allReviews[number]
		}

		timeline := buildTimeline(comments[number], ghEvents[number], rc, reviews)

		path := filepath.Join(issueDir, issueFilename(number, pad))

		// Detect events before writing (so we can compare with previous state)
		hookEvents = append(hookEvents, detectEvents(path, issue, isPR, pr, owner, repo)...)

		if err := writeIssueFile(path, issue, isPR, pr, timeline); err != nil {
			log.Printf("  Warning: writing #%d: %v", number, err)
		}
	}

	return hookEvents, nil
}

// syncIssuesIncremental uses the per-issue timeline endpoint for changed issues
// (gives complete comment+event history in one call) combined with bulk PR
// details (saves N per-PR detail calls).
//
// API calls: N timeline + ~few pages bulk PRs (no separate PR detail or review calls).
func syncIssuesIncremental(c *github.Client, owner, repo, issueDir string, pad int, issues []map[string]any, since string) ([]hooks.Event, error) {
	// Bulk fetch PR details (replaces N per-PR detail calls with ~few paginated pages)
	prDetails, err := fetchAllPRs(c, owner, repo)
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

		// Fetch per-issue timeline (gives complete comment+event+review history)
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

		// PR details from bulk fetch
		var pr map[string]any
		if isPR {
			pr = prDetails[number]
		}

		path := filepath.Join(issueDir, issueFilename(number, pad))

		// Detect events before writing (so we can compare with previous state)
		hookEvents = append(hookEvents, detectEvents(path, issue, isPR, pr, owner, repo)...)
		hookEvents = append(hookEvents, detectCommentEvents(path, issue, isPR, timeline, since, owner, repo)...)

		if err := writeIssueFile(path, issue, isPR, pr, timeline); err != nil {
			log.Printf("  Warning: writing #%d: %v", number, err)
		}
	}

	return hookEvents, nil
}

func writeIssueFile(path string, issue map[string]any, isPR bool, pr map[string]any, timeline []map[string]any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	d := &document.Writer{}
	d.KV("number", jsonutil.Int(issue, "number"))
	d.KV("title", jsonutil.Str(issue, "title"))
	if isPR {
		d.KV("type", "pull_request")
	}
	d.KV("state", jsonutil.Str(issue, "state"))
	if reason := jsonutil.Str(issue, "state_reason"); reason != "" {
		d.KV("state_reason", reason)
	}
	if isPR && pr != nil && jsonutil.Bool(pr, "draft") {
		d.KV("draft", true)
	}
	if jsonutil.Bool(issue, "locked") {
		d.KV("locked", true)
	}
	d.KV("created_at", jsonutil.Str(issue, "created_at"))
	d.KV("updated_at", jsonutil.Str(issue, "updated_at"))
	d.KV("closed_at", jsonutil.Str(issue, "closed_at"))
	d.KV("author", jsonutil.UserLogin(issue, "user"))
	d.List("assignees", jsonutil.Logins(issue, "assignees"))
	d.List("labels", jsonutil.LabelNames(issue, "labels"))
	if ms := jsonutil.Map(issue, "milestone"); ms != nil {
		d.KV("milestone", jsonutil.Str(ms, "title"))
	}

	// PR-specific fields
	if isPR && pr != nil {
		if head := jsonutil.Map(pr, "head"); head != nil {
			d.KV("source_branch", jsonutil.Str(head, "ref"))
		}
		if base := jsonutil.Map(pr, "base"); base != nil {
			d.KV("target_branch", jsonutil.Str(base, "ref"))
		}
		// Cross-repo?
		head := jsonutil.Map(pr, "head")
		base := jsonutil.Map(pr, "base")
		if head != nil && base != nil {
			headRepo := jsonutil.Map(head, "repo")
			baseRepo := jsonutil.Map(base, "repo")
			if headRepo != nil && baseRepo != nil {
				hf := jsonutil.Str(headRepo, "full_name")
				bf := jsonutil.Str(baseRepo, "full_name")
				if hf != "" && bf != "" && hf != bf {
					d.KV("source_repo", hf)
				}
			}
		}

		// Merge info
		merged := jsonutil.Bool(pr, "merged")
		fmt.Fprint(d.Buf(), "merge:\n")
		d.KVIndent("  ", "merged", merged)
		if merged {
			d.KVIndent("  ", "merged_at", jsonutil.Str(pr, "merged_at"))
			mergedBy := jsonutil.UserLogin(pr, "merged_by")
			if mergedBy == "" {
				// Bulk PR list endpoint omits merged_by; extract from timeline
				for _, ev := range timeline {
					if jsonutil.Str(ev, "event") == "merged" {
						mergedBy = jsonutil.UserLogin(ev, "actor")
						break
					}
				}
			}
			if mergedBy != "" {
				d.KVIndent("  ", "merged_by", mergedBy)
			}
			d.KVIndent("  ", "commit_sha", jsonutil.Str(pr, "merge_commit_sha"))
		}

		// Reviewers (deduplicated from reviewed events in timeline)
		reviewerSet := map[string]bool{}
		for _, ev := range timeline {
			if jsonutil.Str(ev, "event") == "reviewed" {
				if login := jsonutil.UserLogin(ev, "user"); login != "" {
					reviewerSet[login] = true
				}
			}
		}
		if len(reviewerSet) > 0 {
			var reviewers []string
			for login := range reviewerSet {
				reviewers = append(reviewers, login)
			}
			sort.Strings(reviewers)
			d.List("reviewers", reviewers)
		}
		d.List("requested_reviewers", jsonutil.Logins(pr, "requested_reviewers"))
	}

	// Reactions
	if reactions := buildReactions(issue); len(reactions) > 0 {
		fmt.Fprint(d.Buf(), "reactions:\n")
		for _, r := range reactions {
			fmt.Fprintf(d.Buf(), "  %s: %d\n", document.YamlScalar(r.key), r.count)
		}
	}

	document.WriteFirstDoc(f, d.String(), jsonutil.Str(issue, "body"))

	// Timeline documents
	for _, event := range timeline {
		writeTimelineDoc(f, event)
	}

	return nil
}

type reaction struct {
	key   string
	count int
}

func buildReactions(issue map[string]any) []reaction {
	r := jsonutil.Map(issue, "reactions")
	if r == nil {
		return nil
	}
	keys := []string{"+1", "-1", "laugh", "hooray", "confused", "heart", "rocket", "eyes"}
	var out []reaction
	for _, k := range keys {
		if count := int(jsonutil.Int(r, k)); count > 0 {
			out = append(out, reaction{k, count})
		}
	}
	return out
}

func writeTimelineDoc(w io.Writer, event map[string]any) {
	eventType := jsonutil.Str(event, "event")

	switch eventType {
	case "commented":
		d := &document.Writer{}
		d.KV("document", "comment")
		d.KV("id", jsonutil.Int(event, "id"))
		d.KV("author", jsonutil.UserLogin(event, "user"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		document.WriteSubDoc(w, d.String(), jsonutil.Str(event, "body"))

	case "reviewed":
		d := &document.Writer{}
		d.KV("document", "review")
		d.KV("id", jsonutil.Int(event, "id"))
		d.KV("author", jsonutil.UserLogin(event, "user"))
		d.KV("state", strings.ToLower(jsonutil.Str(event, "state")))
		d.KV("commit_sha", jsonutil.Str(event, "commit_id"))
		d.KV("submitted_at", jsonutil.Str(event, "submitted_at"))
		document.WriteSubDoc(w, d.String(), jsonutil.Str(event, "body"))

	case "line-commented":
		comments := jsonutil.List(event, "comments")
		for _, c := range comments {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			d := &document.Writer{}
			d.KV("document", "review_comment")
			d.KV("id", jsonutil.Int(cm, "id"))
			if rid := jsonutil.Int(cm, "pull_request_review_id"); rid > 0 {
				d.KV("review_id", rid)
			}
			d.KV("author", jsonutil.UserLogin(cm, "user"))
			d.KV("created_at", jsonutil.Str(cm, "created_at"))
			d.KV("path", jsonutil.Str(cm, "path"))
			if line := jsonutil.Int(cm, "line"); line > 0 {
				d.KV("line", line)
			} else if origLine := jsonutil.Int(cm, "original_line"); origLine > 0 {
				d.KV("line", origLine)
			}
			if side := jsonutil.Str(cm, "side"); side != "" {
				d.KV("side", side)
			}
			d.KV("commit_sha", jsonutil.Str(cm, "commit_id"))
			document.WriteSubDoc(w, d.String(), jsonutil.Str(cm, "body"))
		}

	case "labeled", "unlabeled":
		label := jsonutil.Map(event, "label")
		if label == nil {
			return
		}
		d := &document.Writer{}
		d.KV("document", "event")
		d.KV("event", eventType)
		d.KV("actor", jsonutil.UserLogin(event, "actor"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		d.KV("label", jsonutil.Str(label, "name"))
		document.WriteSubDoc(w, d.String(), "")

	case "assigned", "unassigned":
		d := &document.Writer{}
		d.KV("document", "event")
		d.KV("event", eventType)
		d.KV("actor", jsonutil.UserLogin(event, "actor"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		d.KV("assignee", jsonutil.UserLogin(event, "assignee"))
		document.WriteSubDoc(w, d.String(), "")

	case "milestoned", "demilestoned":
		d := &document.Writer{}
		d.KV("document", "event")
		d.KV("event", eventType)
		d.KV("actor", jsonutil.UserLogin(event, "actor"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		if ms := jsonutil.Map(event, "milestone"); ms != nil {
			d.KV("milestone", jsonutil.Str(ms, "title"))
		}
		document.WriteSubDoc(w, d.String(), "")

	case "renamed":
		d := &document.Writer{}
		d.KV("document", "event")
		d.KV("event", eventType)
		d.KV("actor", jsonutil.UserLogin(event, "actor"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		if rename := jsonutil.Map(event, "rename"); rename != nil {
			d.KV("from", jsonutil.Str(rename, "from"))
			d.KV("to", jsonutil.Str(rename, "to"))
		}
		document.WriteSubDoc(w, d.String(), "")

	case "cross-referenced":
		d := &document.Writer{}
		d.KV("document", "event")
		d.KV("event", eventType)
		d.KV("actor", jsonutil.UserLogin(event, "actor"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		if source := jsonutil.Map(event, "source"); source != nil {
			if si := jsonutil.Map(source, "issue"); si != nil {
				if n := jsonutil.Int(si, "number"); n > 0 {
					d.KV("source_number", n)
				}
				if sr := jsonutil.Map(si, "repository"); sr != nil {
					d.KV("source_repo", jsonutil.Str(sr, "full_name"))
				}
			}
		}
		document.WriteSubDoc(w, d.String(), "")

	case "committed":
		// Skip individual commit entries — commit info is in PR details
		return

	case "closed", "reopened", "merged", "referenced",
		"locked", "unlocked",
		"head_ref_force_pushed", "head_ref_deleted", "base_ref_changed",
		"converted_to_draft", "ready_for_review",
		"review_requested", "review_request_removed",
		"review_dismissed",
		"pinned", "unpinned", "transferred",
		"connected", "disconnected",
		"marked_as_duplicate", "unmarked_as_duplicate":
		d := &document.Writer{}
		d.KV("document", "event")
		d.KV("event", eventType)
		d.KV("actor", jsonutil.UserLogin(event, "actor"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		if sha := jsonutil.Str(event, "commit_id"); sha != "" {
			d.KV("commit_sha", sha)
		}
		if reviewer := jsonutil.UserLogin(event, "requested_reviewer"); reviewer != "" {
			d.KV("reviewer", reviewer)
		}
		if reason := jsonutil.Str(event, "lock_reason"); reason != "" {
			d.KV("lock_reason", reason)
		}
		if dismissal := jsonutil.Str(event, "dismissal_message"); dismissal != "" {
			d.KV("dismissal_message", dismissal)
		}
		document.WriteSubDoc(w, d.String(), "")

	default:
		// Unknown event — still record if it has an event type
		if eventType != "" {
			d := &document.Writer{}
			d.KV("document", "event")
			d.KV("event", eventType)
			d.KV("actor", jsonutil.UserLogin(event, "actor"))
			d.KV("created_at", jsonutil.Str(event, "created_at"))
			document.WriteSubDoc(w, d.String(), "")
		}
	}
}
