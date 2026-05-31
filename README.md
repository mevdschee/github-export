# github-export

Sync all GitHub issues, pull requests, discussions, releases, labels,
milestones, and Projects (v2) from a repository into a local SQLite database
(`github.sqlite`), the primary store. Render that store to plain markdown files
on demand. Runs incrementally on subsequent invocations.

Designed so an agent can read, grep, query, and reason about GitHub data without
API access.

Blog post: https://www.tqdev.com/2026-github-export-open-source-maintenance-ai/

## Usage

The core subcommands are `sync` (GitHub → SQLite) and `export` (SQLite →
markdown). Running with no subcommand defaults to `sync`. On top of the store
sit a GitHub-compatible HTTP API (`serve`), an MCP server (`mcp`), a `gh api`
work-alike (`api`), and native read subcommands (`issue`/`pr`/`search`/`release`).

```bash
export GITHUB_TOKEN=$(gh auth token)

# 1. Sync into ./github.sqlite (the source of truth)
./github-export sync [flags] [owner/repo]

# 2. Render markdown from the database into ./github-data/ when you want it
./github-export export [--out github-data]
```

`owner/repo` is resolved from, in order: the argument, the existing database, or
the `origin` git remote of the current directory — so inside a checked-out
GitHub repo you can just run `github-export` with no arguments. On the first run
all data is fetched; on later runs only issues and PRs updated since the last
sync are re-fetched (the `synced_at` cursor lives in the database).

`export` is one-way: it reads the database and writes files, never the reverse.
The database, not the markdown, is authoritative.

### Flags

`sync`: `--max-age` (see below), `--db` (database path, default
`github.sqlite`).
`export`: `--db` (database to read), `--out` (output directory, default
`github-data`).

#### `--max-age`

`--max-age=2y` (also `6mo`, `4w`, `30d`, `12h`) only fetches issues/PRs/projects
updated within this window — useful for the first sync of very large repos
(cli/cli, kubernetes/kubernetes). It is a floor on the effective `since`: on
later runs the more-recent `synced_at` recorded in the database takes over
automatically, so you can leave the flag in your sync command. Months count as
30 days and years as 365 days (approximate, not calendar-correct).

## Serving the data (HTTP API, MCP, `gh`)

Once synced, the same store can be served three ways. All of them answer
mirrored reads from SQLite (and repo *content* — files, commits, branches — from
a local git clone), and transparently proxy anything unmirrored, plus every
write, to `api.github.com` using the server's `GITHUB_TOKEN`. They bind
localhost and trust local callers (clients present no token).

### `serve` — GitHub-compatible HTTP API + OpenAPI

```bash
./github-export serve --addr localhost:8080
#   API docs:  http://localhost:8080/docs        (Redoc over the OpenAPI 3.1 spec)
#   Spec:      http://localhost:8080/openapi.yaml
#   Status:    http://localhost:8080/status       (freshness + per-entity counts)
```

Paths and JSON field names match GitHub's REST API, so you can point `gh` at it:

```bash
GH_HOST=localhost:8080 gh issue list      # served from SQLite, offline
```

Every mirrored response carries an `X-GitHub-Export-Synced-At` freshness header.
Writes (and unmirrored reads) proxy to GitHub and then synchronously re-sync the
touched entity, so a subsequent local read is immediately consistent. Use
`--proxy=off` to run fully offline (writes/unmirrored reads then return `501`).

Flags: `--proxy on|off`, `--repo-path .` (git clone for content endpoints),
`--git-fetch`, `--auto-sync 15m` (background incremental sync), and
`--debug-compare` (answer reads locally *and* remotely, diff, and log
divergences; add `--debug-compare-file` to file deduplicated gap issues on
`mevdschee/github-export`).

> GraphQL (`POST /graphql`) is currently forwarded to GitHub. Projects v2 and
> Discussions reads therefore work through the proxy; a SQLite-backed GraphQL
> mirror is the next build-out.

### `mcp` — Model Context Protocol server

```bash
./github-export mcp                 # stdio transport (default)
./github-export mcp --http localhost:8081   # streamable-HTTP transport
./github-export mcp --read-only     # register read tools only
```

Tool names mirror GitHub's official MCP server (`get_issue`, `list_issues`,
`get_pull_request`, `search_issues`, `get_file_contents`, `create_issue`,
`add_issue_comment`, `update_issue`, …) so existing agent configs work
unchanged. Read tools resolve against the store/git clone; write tools proxy to
GitHub and re-sync.

### `api` — one-shot `gh api` work-alike

```bash
./github-export api '/repos/OWNER/REPO/issues?state=all'   # local when mirrored
./github-export api --method PATCH --input - /repos/OWNER/REPO/issues/1
```

### Native read subcommands (no `gh` needed)

```bash
./github-export issue list --state all
./github-export issue view 42 --json
./github-export pr list
./github-export search "is:pr label:bug broken"
./github-export release list
```

## What it syncs

Everything is stored in the SQLite database during `sync`; the paths below are
where `export` renders each entity.

| Data          | Exported to                       | Sync mode        |
| ------------- | --------------------------------- | ---------------- |
| Labels        | `github-data/labels.yml`          | Always full sync |
| Milestones    | `github-data/milestones.yml`      | Always full sync |
| Issues + PRs  | `github-data/issues/0042.md`      | Incremental      |
| Projects (v2) | `github-data/projects/0001.md`    | Incremental      |
| Discussions   | `github-data/discussions/0042.md` | Incremental      |
| Releases      | `github-data/releases/v1.0.0.md`  | Always full sync |
| Repo metadata | `github-data/repo.yml`            | Updated each run |
| Events        | `github-data/events/*.md`         | Since last sync  |

### Issues and pull requests

Each issue or PR is stored as a single markdown file with YAML frontmatter.
Comments, reviews, review comments, and events follow as additional YAML
documents separated by `---`, in chronological order.

For pull requests, the program fetches branch info and merge status from the
Pulls REST API, and all PR reviews in one paginated GraphQL query (one query per
~100 PRs instead of one REST call per PR). PRs with more than 100 reviews log a
warning and only the first 100 are exported.

### Projects (v2)

Each project linked to the repository is exported as a single markdown file
under `github-data/projects/`, named by the project number (`0001.md`). The
frontmatter has the project's title, owner, URL, description, and field
definitions (e.g. the `Status` column's options). Each item (issue or PR) on the
project follows as an `item` sub-document with its current field values.

Only **open** projects are written. Closed ones are removed on next sync and
emit a `project_closed` event. Draft issues (project-only items with no real
issue number) are skipped.

When an issue or PR is on a project, the issue's frontmatter gains a `projects:`
list. This is populated on the next sync that re-fetches the issue — for issues
that haven't changed since the last sync, the field will appear the first time
the issue is touched on GitHub.

Projects v2 is GraphQL-only, so this section uses the GitHub GraphQL endpoint
instead of REST.

### Discussions

One file per discussion at `github-data/discussions/<number>.md`. Frontmatter
includes `category`, `state`, `state_reason`, and — for Q&A categories where a
reply has been marked as the answer — `answer_id`, `answer_chosen_at`,
`answer_chosen_by`. Top-level comments are emitted as `document: comment` and
nested replies as `document: reply` with a `parent_id` field linking back to the
comment they reply to.

Discussions are GraphQL-only. The list is fetched newest-first
(`UPDATED_AT DESC`) and pagination stops when an item is older than the
effective `since`, so `--max-age` is fully respected. Each page pulls up to 50
discussions with up to 100 comments and 50 replies per comment; if a single
discussion exceeds those limits a warning is logged and only the first N entries
are exported.

### Incremental sync

On second run, the program reads `synced_at` from the database and passes it as
`?since=` to the GitHub Issues API. Only issues with `updated_at >= synced_at`
are re-fetched. Each touched issue is fully rebuilt in the store from the
timeline endpoint. Change detection diffs the incoming data against the existing
row, so events are emitted without re-reading any markdown.

When `--max-age` is set on the first sync, the same `?since=` filter is used
with the cutoff timestamp. The PR list is fetched sorted newest-first and
pagination stops once it crosses the cutoff, so very large repos are practical
to sync.

### Rate limiting

Automatically sleeps when `X-RateLimit-Remaining` drops below 100, resuming when
the reset window passes. A typical incremental sync of 50 updated issues costs
~200 API requests, well within the 5,000/hour limit.

### Events

Each sync detects changes (new issues, closed PRs, new comments, etc.) and
exports them as individual markdown files in `github-data/events/`. These files
are designed as a handoff point: an agent reads them, acts on them, and deletes
them. No configuration is needed; events are always exported when detected.

Event types:

- Issues and PRs: `issue_created`, `issue_closed`, `issue_reopened`,
  `pr_created`, `pr_merged`, `pr_closed`, `pr_reopened`, `comment_created`
- PR review loop: `pr_review_requested`, `pr_reviewed` (with `review_state`),
  `pr_ready_for_review`
- Triage / activity: `assigned`, `unassigned`, `label_added`, `label_removed`,
  `mentioned`, `linked_to_pr`, `duplicate_marked`
- Discussions: `discussion_created`, `discussion_closed`, `discussion_answered`,
  `discussion_comment_created`
- Projects: `project_created`, `project_closed`, `item_added`, `item_removed`,
  `item_status_changed`, `item_field_changed`
- Releases: `release_published`, `prerelease_promoted`

Timeline-derived events (`comment_created`, `assigned`, `label_added`,
`pr_reviewed`, …) only fire on incremental syncs. The first full sync emits
`*_created` events for each entity but skips the per-timeline history that
already happened — otherwise it would flood `events/` with years of activity.

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

See [docs/data-model.md](docs/data-model.md) for the full format specification.

Example issue file:

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

## Testing

```bash
go test -short ./...                       # unit tests only
GITHUB_TOKEN=$(gh auth token) go test ./...   # full end-to-end suite
```

The end-to-end suite (`e2e_test.go`) runs a real sync of this repository's
TEST-prefixed fixtures (issues `#1–#3`, PRs `#4–#6`, discussions `#7–#8`,
project, releases, milestones) into a SQLite store, asserts on the store
contents, then renders a markdown export and asserts on the rendered files. It
auto-skips when `GITHUB_TOKEN` is unset or `-short` is passed; project
assertions are skipped when the token lacks the `read:project` scope. The
`internal/store` package has standalone unit tests that need no token.

## Installation

```bash
go build .
sudo install -m 755 github-export /usr/local/bin/
```

Requires Go 1.22+.

## Requirements

- `GITHUB_TOKEN` environment variable with repo read access
- Get one via `gh auth token` (GitHub CLI) or create a personal access token
