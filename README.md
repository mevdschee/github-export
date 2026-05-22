# github-export

Export all GitHub issues, pull requests, releases, labels, milestones, and
Projects (v2) from a repository into a local `github-data/` folder as plain
markdown files. Runs incrementally on subsequent invocations.

Designed so an agent can read, grep, and reason about GitHub data without API
access.

Blog post: https://www.tqdev.com/2026-github-export-open-source-maintenance-ai/

## Usage

```bash
export GITHUB_TOKEN=$(gh auth token)
./github-export owner/repo [output-dir]
```

The output directory defaults to `github-data/`. On the first run, all data is
fetched. On subsequent runs, only issues and PRs updated since the last sync are
re-fetched.

## What it syncs

| Data          | Output                           | Sync mode        |
| ------------- | -------------------------------- | ---------------- |
| Labels        | `github-data/labels.yml`         | Always full sync |
| Milestones    | `github-data/milestones.yml`     | Always full sync |
| Issues + PRs  | `github-data/issues/0042.md`     | Incremental      |
| Projects (v2) | `github-data/projects/0001.md`   | Incremental      |
| Releases      | `github-data/releases/v1.0.0.md` | Always full sync |
| Repo metadata | `github-data/repo.yml`           | Updated each run |
| Events        | `github-data/events/*.md`        | Since last sync  |

### Issues and pull requests

Each issue or PR is stored as a single markdown file with YAML frontmatter.
Comments, reviews, review comments, and events follow as additional YAML
documents separated by `---`, in chronological order.

For pull requests, the program fetches branch info, merge status, and reviews
from the Pulls API.

### Projects (v2)

Each project linked to the repository is exported as a single markdown file
under `github-data/projects/`, named by the project number (`0001.md`). The
frontmatter has the project's title, owner, URL, description, and field
definitions (e.g. the `Status` column's options). Each item (issue or PR) on the
project follows as an `item` sub-document with its current field values.

Only **open** projects are written. Closed ones are removed on next sync and
emit a `project_closed` event. Draft issues (project-only items with no real
issue number) are skipped.

When an issue or PR is on a project, the issue's frontmatter gains a
`projects:` list. This is populated on the next sync that re-fetches the issue
— for issues that haven't changed since the last sync, the field will appear
the first time the issue is touched on GitHub.

Projects v2 is GraphQL-only, so this section uses the GitHub GraphQL endpoint
instead of REST.

### Incremental sync

On second run, the program reads `synced_at` from `repo.yml` and passes it as
`?since=` to the GitHub Issues API. Only issues with `updated_at >= synced_at`
are re-fetched. Each touched issue file is fully rebuilt from the timeline
endpoint.

### Rate limiting

Automatically sleeps when `X-RateLimit-Remaining` drops below 100, resuming when
the reset window passes. A typical incremental sync of 50 updated issues costs
~200 API requests, well within the 5,000/hour limit.

### Events

Each sync detects changes (new issues, closed PRs, new comments, etc.) and
exports them as individual markdown files in `github-data/events/`. These files
are designed as a handoff point: an agent reads them, acts on them, and deletes
them. No configuration is needed; events are always exported when detected.

Event types: `issue_created`, `issue_closed`, `issue_reopened`, `pr_created`,
`pr_merged`, `pr_closed`, `comment_created`, `project_created`,
`project_closed`, `item_added`.

Example event file (`events/20240916-140000-000-issue_created-42.md`):

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

Whenever I ...
```

Event detection adds no extra API calls — it compares the data already fetched
during sync against the previous on-disk state.

## Output format

See [data-model.md](data-model.md) for the full format specification. Example
issue file:

```markdown
---
number: 42
title: Fix crash on empty input
state: closed
state_reason: completed
created_at: 2024-09-15T10:30:00Z
updated_at: 2024-09-16T14:00:00Z
closed_at: 2024-09-16T14:00:00Z
author: octocat
assignees:
  - hubot
labels:
  - bug
milestone: v2.1
---

When passing an empty string to `parse()`, the application crashes.

---
document: comment
id: 100
author: hubot
created_at: 2024-09-15T11:00:00Z
---

I can reproduce this.

---
document: event
event: closed
actor: hubot
created_at: 2024-09-16T14:00:00Z
commit_sha: abc123f
---
```

## Installation

```bash
go build .
sudo install -m 755 github-export /usr/local/bin/
```

Requires Go 1.22+.

## Requirements

- `GITHUB_TOKEN` environment variable with repo read access
- Get one via `gh auth token` (GitHub CLI) or create a personal access token
