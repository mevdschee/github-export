package sync

import (
	"encoding/json"
	"fmt"

	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/store"
)

// ResyncIssue re-pulls a single issue/PR (and its timeline) into the store. It
// is used for read-after-write consistency by the write proxy: after a write to
// issue #n succeeds upstream, the local row is refreshed before the call
// returns. The issue's project cross-links are preserved from the existing row.
func ResyncIssue(c *github.Client, s *store.Store, owner, repo string, number int64) error {
	issueURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d", github.API, owner, repo, number)
	rawIssue, err := c.GetJSON(issueURL, nil)
	if err != nil {
		return fmt.Errorf("fetching issue #%d: %w", number, err)
	}
	var issue map[string]any
	if err := json.Unmarshal(rawIssue, &issue); err != nil {
		return fmt.Errorf("parsing issue #%d: %w", number, err)
	}
	isPR := issue["pull_request"] != nil

	var pr map[string]any
	if isPR {
		prURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", github.API, owner, repo, number)
		if rawPR, err := c.GetJSON(prURL, nil); err == nil {
			json.Unmarshal(rawPR, &pr)
		}
	}

	timelineURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/timeline?per_page=%d",
		github.API, owner, repo, number, github.PerPage)
	var timeline []map[string]any
	if timelineRaw, err := c.GetPaginated(timelineURL, nil); err == nil {
		for _, r := range timelineRaw {
			var m map[string]any
			json.Unmarshal(r, &m)
			timeline = append(timeline, m)
		}
	}

	// Preserve existing project cross-links (not re-fetched here).
	projects, _ := s.ProjectsForIssue(number)

	return s.UpsertIssue(number, isPR, issue, pr, timeline, projects)
}
