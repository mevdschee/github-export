# Event Export Infrastructure

## Context

The `github-export` tool syncs GitHub data (issues, PRs, releases) to local
markdown files. When the sync detects changes (new issue, closed PR, new
comment, etc.), it exports each event as a standalone markdown file in the
`events/` directory. Agents can watch this directory, act on new events, and
remove the files after handling them.

No configuration is needed — events are always exported when detected.

## Design

### Event Detection

During sync, compare the state before and after writing each issue file:

1. Check if the issue file already exists on disk → `issue_created` /
   `pr_created`
2. Check `state` field vs previous frontmatter → `issue_closed`,
   `issue_reopened`, `pr_closed`, `pr_merged`
3. Check `created_at` vs `synced_at` for timeline entries → `comment_created`

### Event Types

| Event             | Trigger condition                                                 |
| ----------------- | ----------------------------------------------------------------- |
| `issue_created`   | Issue file doesn't exist on disk, `pull_request` field is nil     |
| `pr_created`      | Issue file doesn't exist on disk, `pull_request` field is present |
| `issue_closed`    | `state == "closed"` and previous file had `state: open`           |
| `issue_reopened`  | `state == "open"` and previous file had `state: closed`           |
| `pr_merged`       | PR with `merged == true` and previous file had `merged: false`    |
| `pr_closed`       | PR `state == "closed"` and not merged, previous had `state: open` |
| `comment_created` | New comments in timeline with `created_at >= synced_at`           |

### Event File Format

Each event is written as a markdown file with YAML frontmatter:

```markdown
---
event: issue_created
number: 42
title: Fix crash on empty input
author: octocat
state: open
labels:
  - bug
file: github-data/issues/0042.md
repo: owner/repo
url: https://github.com/owner/repo/issues/42
exported_at: 2024-09-16T14:00:00Z
---

See [0042.md](github-data/issues/0042.md) for full details.
```

Files are named `{timestamp}-{index}-{event_type}-{number}.md` to guarantee
uniqueness and sort chronologically.

### Directory Structure

```
github-data/
  events/
    20240916-140000-000-issue_created-42.md
    20240916-140000-001-issue_closed-15.md
    20240916-140000-002-comment_created-42.md
```

### Implementation

#### Files

- `internal/hooks/hooks.go` — Event types, Event struct, Export function
- `internal/sync/issues.go` — Detects events during sync by comparing API data
  with previous on-disk state
- `internal/sync/state.go` — Reads previous frontmatter state from existing
  issue files

#### Sync Flow

```
1. Sync labels, milestones
2. Sync issues/PRs → collect []Event
3. Sync releases
4. Update repo.yml
5. Export events to events/ directory
```

Events are exported at the end (not inline during sync) to keep the sync fast
and ensure all issue files are written before event files reference them.

#### Event Struct

```go
type Event struct {
    Type   string   // e.g. "issue_created"
    Number int64
    Title  string
    Author string
    State  string
    Labels []string
    File   string   // absolute path to written .md file
    Repo   string   // "owner/repo"
}
```

## Verification

1. Run a full sync on a small repo — verify event files appear in `events/` for
   every issue (all are "new" on first sync)
2. Create a new issue on GitHub, run incremental sync — verify only the new
   issue produces an `issue_created` event file
3. Close an issue, re-sync — verify `issue_closed` event file appears
4. Verify event files can be removed without affecting subsequent syncs
5. Test with no prior `events/` directory — it should be created automatically
