# Hook Infrastructure for Reacting to GitHub Events During Sync

## Context

The `github-export` tool syncs GitHub data (issues, PRs, releases) to local markdown files. Currently it's a silent exporter — it writes files but doesn't notify anything about what changed. The goal is to add a configurable hook system so that when the sync detects events (new issue created, issue closed, new comment, etc.), it runs user-defined commands. This enables Claude (or any tool) to react to GitHub activity by wiring up hooks in a config file.

## Design

### Event Detection

During sync, compare the state before and after writing each issue file:
- **New issue/PR**: The markdown file did not exist before sync wrote it (i.e., `os.Stat` returns `ErrNotExist` before `writeIssueFile`)
- **Updated issue/PR**: The file existed but its content changed (compare old vs new bytes, or just always fire for updated items during incremental sync)
- **Closed/reopened/merged**: Parse the `state` field from the GitHub API response and compare to the previous frontmatter state (read from existing file)
- **New comment**: During incremental sync, the timeline contains new comments not present in the old file

To keep it simple for v1, detect events from the **API response data directly** rather than diffing files:
1. Check if the issue file already exists on disk → `issue_created` / `pr_created`
2. Check `state` field → `issue_closed`, `issue_reopened`, `pr_closed`, `pr_merged`
3. Check `created_at` vs `synced_at` for timeline entries → `comment_created`

### Event Types (v1)

| Event | Trigger condition |
|---|---|
| `issue_created` | Issue file doesn't exist on disk, `pull_request` field is nil |
| `pr_created` | Issue file doesn't exist on disk, `pull_request` field is present |
| `issue_closed` | `state == "closed"` and previous file had `state: open` |
| `issue_reopened` | `state == "open"` and previous file had `state: closed` |
| `pr_merged` | PR with `merged == true` and previous file had `merged: false` |
| `pr_closed` | PR `state == "closed"` and not merged, previous had `state: open` |
| `comment_created` | New comments in timeline with `created_at >= synced_at` |

### Hook Configuration

Hooks are markdown template files stored in a `hooks/` subdirectory of the output directory. Each `.md` file has YAML frontmatter specifying which event triggers it, and a markdown body that serves as a prompt template with placeholders.

```
github-data/
  hooks/
    triage-new-issues.md
    review-comments.md
```

Example hook file (`hooks/triage-new-issues.md`):

```markdown
---
event: issue_created
---

Triage this new issue #{{issue.number}}: "{{issue.title}}" by {{issue.author}}.

Labels: {{issue.labels}}

Read the issue file and categorize it by priority and area.
```

### Placeholders

| Placeholder | Description |
|---|---|
| `{{issue.number}}` | Issue/PR number |
| `{{issue.title}}` | Issue/PR title |
| `{{issue.author}}` | Author login |
| `{{issue.state}}` | Current state (open/closed) |
| `{{issue.labels}}` | Comma-separated label names |
| `{{issue.file}}` | Absolute path to the issue/PR markdown file |
| `{{issue.repo}}` | `owner/repo` |
| `{{issue.url}}` | GitHub web URL for the issue/PR |
| `{{event.type}}` | Event type (e.g., `issue_created`) |

### Hook Execution

- Hooks run **after** the issue file has been written to disk (so the file is up-to-date when the hook reads it)
- Hooks run **sequentially** per event, but all events are collected during sync and executed at the end
- Hook failures are logged but don't abort the sync
- A `--dry-run` flag shows which hooks would fire (prints rendered prompt) without executing them
- Each rendered prompt is passed to `claude -p "<prompt>" --file <issue_file>` — no shell commands or env variables needed

### Implementation

#### New files

- `internal/hooks/hooks.go` — Hook loading, event types, template rendering, execution
- `hooks/` dir in output dir — User-created `.md` template files (not auto-generated)

#### Modified files

- `main.go` — Load hooks config, pass hook runner to sync, execute collected events after sync
- `internal/sync/issues.go` — Detect events during `syncIssuesFull` and `syncIssuesIncremental`, return a list of events
- `internal/config/config.go` — Add hooks config reading (or put in hooks package)

#### Changes to sync flow

1. `main.go` loads `.md` files from `hooks/` dir (if it exists, no error if missing)
2. `sync.Issues()` signature changes to return `([]hooks.Event, error)` instead of just `error`
3. Inside `syncIssuesFull` / `syncIssuesIncremental`, before calling `writeIssueFile`:
   - Check if file exists → detect `issue_created` / `pr_created`
   - Read existing file's state line → detect close/reopen/merge transitions
4. After all sync operations complete in `main.go`, iterate collected events and run matching hooks
5. Log each hook execution and its exit code

#### Event struct

```go
type Event struct {
    Type   string            // e.g. "issue_created"
    Number int64
    Title  string
    Author string
    State  string
    Labels []string
    File   string            // absolute path to written .md file
    Repo   string            // "owner/repo"
}
```

### Execution order

```
1. Load hooks.yml
2. Sync labels, milestones
3. Sync issues/PRs → collect []Event
4. Sync releases
5. Update repo.yml
6. For each event, run matching hooks
```

Running hooks at the end (not inline during sync) keeps the sync fast and ensures all files are written before any hook reads them.

## Verification

1. Create a test hook `hooks/triage.md` with `event: issue_created` and a simple prompt
2. Run a full sync on a small repo — verify hooks fire for every issue (all are "new" on first sync)
3. Create a new issue on GitHub, run incremental sync — verify only the new issue triggers `issue_created`
4. Close an issue, re-sync — verify `issue_closed` fires
5. Test with `--dry-run` flag to see rendered prompts without execution
6. Test with no `hooks/` directory — sync should work exactly as before (backward compatible)
