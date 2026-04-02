# Export Plan: GitHub to github-data/

Incrementally sync a GitHub repository's issues, PRs, releases, labels, and
milestones into the `github-data/` markdown format defined in `data-model.md`.

## Tool

Go binary (`github-export`) using the GitHub REST API directly via `net/http`.
The client (`internal/github/client.go`) handles authentication
(`GITHUB_TOKEN`), pagination (follows `Link: rel="next"` headers), and rate
limiting (auto-sleeps when remaining < 100).

## API Endpoints

### Per-issue endpoints (used for incremental sync)

| Data           | Endpoint                                                          | Incremental?     |
| -------------- | ----------------------------------------------------------------- | ---------------- |
| Issues + PRs   | `GET /repos/{owner}/{repo}/issues?state=all&since=T&sort=updated` | Yes, via `since` |
| Issue timeline | `GET /repos/{owner}/{repo}/issues/{n}/timeline`                   | Per issue        |
| Labels         | `GET /repos/{owner}/{repo}/labels`                                | Always full      |
| Milestones     | `GET /repos/{owner}/{repo}/milestones?state=all`                  | Always full      |
| Releases       | `GET /repos/{owner}/{repo}/releases`                              | Always full      |

### Bulk endpoints (used for full sync)

| Data                | Endpoint                                            | Notes                                               |
| ------------------- | --------------------------------------------------- | --------------------------------------------------- |
| All issue comments  | `GET /repos/{owner}/{repo}/issues/comments?since=T` | Supports `since`, sorted by `created`               |
| All issue events    | `GET /repos/{owner}/{repo}/issues/events`           | No `since` support                                  |
| All PRs             | `GET /repos/{owner}/{repo}/pulls?state=all`         | List endpoint (omits `merged` boolean, `merged_by`) |
| All review comments | `GET /repos/{owner}/{repo}/pulls/comments?since=T`  | Supports `since`, sorted by `created`               |
| PR reviews          | `GET /repos/{owner}/{repo}/pulls/{n}/reviews`       | Still per-PR (no bulk endpoint)                     |

### Key API Behaviors

- **Issues endpoint returns PRs too.** If the response has a `pull_request`
  field, it's a PR. No need for a separate PR list.
- **`since` parameter** filters by `updated_at >= T`. This is the incremental
  sync mechanism — only issues modified since last sync are returned.
- **Pagination**: `per_page=100` (max), follow `Link: <url>; rel="next"`
  headers. The Go client handles this via `GetPaginated`.
- **Rate limit**: 5,000 requests/hour for authenticated users. The client sleeps
  automatically when `X-RateLimit-Remaining` drops below 100.
- **Bulk PR list omits fields**: The `pulls?state=all` list endpoint does not
  return `merged` (boolean) or `merged_by`. `merged` is inferred from
  `merged_at != ""`. `merged_by` is extracted from the timeline's `merged`
  event.
- **Timeline endpoint** (`application/vnd.github.mockingbird-preview+json`)
  returns comments and events interleaved chronologically. Used only during
  incremental sync (for the small number of changed issues).
- **Review comments are grouped** by `pull_request_review_id` into
  `line-commented` pseudo-events to match the timeline format.

## Sync Strategy

Two modes: **full sync** (first run, no `synced_at`) and **incremental sync**
(subsequent runs). Both share the same file-writing logic but differ in how they
gather timeline data.

### Step 1: repo.yml

Read or create `github-data/repo.yml`. Extract `synced_at` timestamp (or empty
for first run).

### Step 2: Labels + Milestones (always full sync)

These are small collections. Fetch all and overwrite into
`github-data/labels.yml` and `github-data/milestones.yml`.

### Step 3: Issues + PRs

Fetch issues updated since last sync (or all on first run):

```
GET /repos/{owner}/{repo}/issues?state=all&sort=updated&direction=asc&per_page=100[&since=T]
```

This returns both issues and PRs. The sync then diverges by mode:

#### Full sync (bulk method)

Uses repo-wide bulk endpoints to gather all supporting data in ~5 paginated
fetches instead of 1-3 calls per issue:

1. **Bulk fetch all issue comments** —
   `GET /repos/{owner}/{repo}/issues/comments?sort=created&direction=asc&per_page=100`.
   Group by issue number (extracted from `issue_url`). Normalize each with
   `event="commented"`.
2. **Bulk fetch all issue events** —
   `GET /repos/{owner}/{repo}/issues/events?per_page=100`. Group by issue number
   (extracted from embedded `issue` object, then removed to save memory).
3. **Bulk fetch all PRs** —
   `GET /repos/{owner}/{repo}/pulls?state=all&per_page=100`. Map by PR number.
   Infer `merged` from `merged_at != ""` (list endpoint omits the boolean).
4. **Bulk fetch all review comments** —
   `GET /repos/{owner}/{repo}/pulls/comments?sort=created&direction=asc&per_page=100`.
   Group by PR number (extracted from `pull_request_url`). Group into
   `line-commented` pseudo-events by `pull_request_review_id`.
5. **Fetch reviews per PR** — `GET /repos/{owner}/{repo}/pulls/{n}/reviews` for
   each PR (no bulk endpoint exists).
6. **Build timeline** per issue by merging comments + events + review comments +
   reviews, sorted chronologically.
7. **Write each issue/PR file.**

#### Incremental sync (per-issue timeline)

For the smaller number of changed issues, uses the per-issue timeline endpoint
(which returns complete comment+event+review history in one call):

1. **Bulk fetch all PRs** — same as full sync (replaces N individual PR detail
   fetches with a few paginated pages).
2. **Per changed issue**: fetch
   `GET /repos/{owner}/{repo}/issues/{n}/timeline?per_page=100`. This gives the
   full chronological record including comments, events, and reviews.
3. **Write each issue/PR file** using the timeline directly.

### Step 4: Releases (always full sync)

Write each release as `github-data/releases/{tag_name}.md` (slashes in tag names
replaced with `-`).

### Step 5: Update repo.yml

Set `synced_at` to the current timestamp.

## Rate Budget Estimate

### Full sync (bulk method)

Fixed overhead regardless of issue count:

| Request                | Count              |
| ---------------------- | ------------------ |
| Issues list            | ceil(N/100) pages  |
| Issue comments (bulk)  | ceil(C/100) pages  |
| Issue events (bulk)    | ceil(E/100) pages  |
| PRs list (bulk)        | ceil(P/100) pages  |
| Review comments (bulk) | ceil(RC/100) pages |
| PR reviews (per PR)    | 1 per PR           |
| Labels                 | 1                  |
| Milestones             | 1                  |
| Releases               | 1+ (paginated)     |

A repo with 1,000 issues (300 PRs, 5,000 comments, 2,000 events, 1,000 review
comments): ~10 + 50 + 20 + 3 + 10 + 300 + 3 = **~396 requests** (vs ~3,000-4,000
with the old per-issue method).

### Incremental sync

| Request                  | Count               |
| ------------------------ | ------------------- |
| Issues list (with since) | 1+ (paginated)      |
| PRs list (bulk)          | ceil(P/100) pages   |
| Timeline (per changed)   | 1 per changed issue |
| Labels                   | 1                   |
| Milestones               | 1                   |
| Releases                 | 1+ (paginated)      |

A repo with 50 changed issues since last sync: ~3 + 50 + 3 = **~56 requests**.

## Field Mapping: API Response → Frontmatter

### Issue

| API field               | Frontmatter field              |
| ----------------------- | ------------------------------ |
| `number`                | `number`                       |
| `title`                 | `title`                        |
| `state`                 | `state`                        |
| `state_reason`          | `state_reason`                 |
| `locked`                | `locked` (omit if false)       |
| `active_lock_reason`    | (omit)                         |
| `created_at`            | `created_at`                   |
| `updated_at`            | `updated_at`                   |
| `closed_at`             | `closed_at`                    |
| `user.login`            | `author`                       |
| `assignees[].login`     | `assignees`                    |
| `labels[].name`         | `labels`                       |
| `milestone.title`       | `milestone`                    |
| `body`                  | markdown body                  |
| `pull_request` (exists) | `type: pull_request`           |
| `reactions` (counts)    | `reactions` (omit if all zero) |

### PR (from `GET /pulls/{n}`)

| API field                     | Frontmatter field       |
| ----------------------------- | ----------------------- |
| `head.ref`                    | `source_branch`         |
| `base.ref`                    | `target_branch`         |
| `head.repo.full_name`         | `source_repo` (if fork) |
| `draft`                       | `draft` (omit if false) |
| `merged`                      | `merge.merged`          |
| `merged_at`                   | `merge.merged_at`       |
| `merged_by.login`             | `merge.merged_by`       |
| `merge_commit_sha`            | `merge.commit_sha`      |
| `requested_reviewers[].login` | `requested_reviewers`   |

### Timeline → Subsequent Documents

The timeline endpoint returns events and comments interleaved. Map each item by
its `event` field:

| Timeline event     | Document type    | Key fields                                                 |
| ------------------ | ---------------- | ---------------------------------------------------------- |
| `commented`        | `comment`        | `id`, `user.login`, `body`, `created_at`                   |
| `reviewed`         | `review`         | `id`, `user.login`, `state`, `body`, `commit_id`           |
| `line-commented`   | `review_comment` | `id`, `user.login`, `path`, `line`, `body`, `commit_id`    |
| `labeled`          | `event`          | `actor.login`, `label.name`                                |
| `unlabeled`        | `event`          | `actor.login`, `label.name`                                |
| `assigned`         | `event`          | `actor.login`, `assignee.login`                            |
| `unassigned`       | `event`          | `actor.login`, `assignee.login`                            |
| `milestoned`       | `event`          | `actor.login`, `milestone.title`                           |
| `demilestoned`     | `event`          | `actor.login`, `milestone.title`                           |
| `renamed`          | `event`          | `actor.login`, `rename.from`, `rename.to`                  |
| `closed`           | `event`          | `actor.login`, `commit_id`                                 |
| `reopened`         | `event`          | `actor.login`                                              |
| `merged`           | `event`          | `actor.login`, `commit_id`                                 |
| `referenced`       | `event`          | `actor.login`, `commit_id`                                 |
| `cross-referenced` | `event`          | `source.issue.number`, `source.issue.repository.full_name` |
| Other events       | `event`          | `actor.login` + event-specific fields                      |

## Incremental Sync: What Gets Re-fetched

When a synced issue is updated (new comment, label change, close, etc.), GitHub
bumps its `updated_at`. The `since` parameter catches it.

**On re-fetch, overwrite the entire file.** Rebuilding from scratch is simpler
and more reliable than trying to diff/merge individual documents. The timeline
endpoint gives us the full chronological record every time.

This means incremental sync is incremental in _which issues to fetch_, not in
_how much data per issue_. For most repos this is fine — an issue with 100
comments is a few KB of markdown.

## Edge Cases

- **Deleted issues**: GitHub doesn't hard-delete issues. They may become
  inaccessible (404) if transferred or spam-removed. Skip 404s during sync.
- **Very long issues** (1000+ comments): Timeline pagination handles this. The
  resulting file may be large but is still a valid markdown file.
- **Cross-repo PRs**: `source_repo` is set when `head.repo.full_name` differs
  from the target repo.
- **Bot accounts**: Stored as regular usernames. The `user.type` field (`User`,
  `Bot`) could be preserved but isn't essential for readability.
- **Reactions**: The issues list returns reaction counts. Store as
  `reactions: {"+1": 2, "heart": 1}` in frontmatter, omit if all zeros.
- **Images/attachments in body**: Markdown body may contain
  `![image](https://user-images...)` URLs. These point to GitHub's CDN and
  remain valid. Optionally download and store locally for true offline access.

## File Naming

- Issues/PRs: `{number}.md` — zero-padded to 4 digits for repos with <10k items,
  5+ digits otherwise. Padding width is determined on first sync and stays
  consistent.
- Releases: `{tag_name}.md` — tag name as-is (e.g., `v1.0.0.md`).
