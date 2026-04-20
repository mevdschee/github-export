package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mevdschee/github-export/internal/document"
)

const (
	IssueCreated   = "issue_created"
	PRCreated      = "pr_created"
	IssueClosed    = "issue_closed"
	IssueReopened  = "issue_reopened"
	PRMerged       = "pr_merged"
	PRClosed       = "pr_closed"
	CommentCreated = "comment_created"
)

type Event struct {
	Type   string
	Number int64
	Title  string
	Author string
	State  string
	Labels []string
	File   string // relative path to the issue/PR markdown file (e.g. "github-data/issues/0042.md")
	Repo   string // "owner/repo"
	Body   string // event-specific content: issue body for created, comment text for comments, empty for state changes
}

// Export writes each event as a markdown file in the events/ directory.
// Files are named with a timestamp and event type so agents can pick them up
// and remove them after handling.
func Export(eventsDir string, events []Event) error {
	if len(events) == 0 {
		return nil
	}

	if err := os.MkdirAll(eventsDir, 0755); err != nil {
		return fmt.Errorf("creating events directory: %w", err)
	}

	now := time.Now().UTC()
	for i, ev := range events {
		d := &document.Writer{}
		d.KV("event", ev.Type)
		d.KV("number", ev.Number)
		d.KV("title", ev.Title)
		d.KV("author", ev.Author)
		d.KV("state", ev.State)
		d.List("labels", ev.Labels)
		d.KV("file", ev.File)
		d.KV("repo", ev.Repo)
		d.KV("url", fmt.Sprintf("https://github.com/%s/issues/%d", ev.Repo, ev.Number))
		d.KV("exported_at", now.Format(time.RFC3339))

		// Timestamp with sub-second index to guarantee unique filenames
		name := fmt.Sprintf("%s-%03d-%s-%d.md",
			now.Format("20060102-150405"), i, ev.Type, ev.Number)
		path := filepath.Join(eventsDir, name)

		body := ev.Body

		content := formatEventFile(d.String(), body)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("writing event %s: %w", name, err)
		}
	}

	return nil
}

func formatEventFile(frontmatter, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(frontmatter)
	b.WriteString("---\n")
	if body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	return b.String()
}
