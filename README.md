# github-export

Export all GitHub issues, pull requests, releases, labels, and milestones from a repository into a local `github-data/` folder as plain markdown files. Runs incrementally on subsequent invocations.

Designed so an agent can read, grep, and reason about GitHub data without API access.

## Usage

```bash
export GITHUB_TOKEN=$(gh auth token)
./github-export owner/repo [output-dir]
```

The output directory defaults to `github-data/`. On the first run, all data is fetched. On subsequent runs, only issues and PRs updated since the last sync are re-fetched.

## What it syncs

| Data              | Output                          | Sync mode           |
| ----------------- | ------------------------------- | ------------------- |
| Labels            | `github-data/labels.yml`        | Always full sync    |
| Milestones        | `github-data/milestones.yml`    | Always full sync    |
| Issues + PRs      | `github-data/issues/0042.md`    | Incremental         |
| Releases          | `github-data/releases/v1.0.0.md`| Always full sync   |
| Repo metadata     | `github-data/repo.yml`          | Updated each run    |

### Issues and pull requests

Each issue or PR is stored as a single markdown file with YAML frontmatter. Comments, reviews, review comments, and events follow as additional YAML documents separated by `---`, in chronological order.

For pull requests, the program fetches branch info, merge status, and reviews from the Pulls API.

### Incremental sync

On second run, the program reads `synced_at` from `repo.yml` and passes it as `?since=` to the GitHub Issues API. Only issues with `updated_at >= synced_at` are re-fetched. Each touched issue file is fully rebuilt from the timeline endpoint.

### Rate limiting

Automatically sleeps when `X-RateLimit-Remaining` drops below 100, resuming when the reset window passes. A typical incremental sync of 50 updated issues costs ~200 API requests, well within the 5,000/hour limit.

## Output format

See [data-model.md](data-model.md) for the full format specification. Example issue file:

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

## Building

```bash
go build -o github-export .
```

Requires Go 1.22+.

## Requirements

- `GITHUB_TOKEN` environment variable with repo read access
- Get one via `gh auth token` (GitHub CLI) or create a personal access token
