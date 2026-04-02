# GitHub Data Model — Markdown Format

Sync a GitHub repository's issues, pull requests, releases, and metadata into a local `github-data/` folder. The result is a plain-text archive that an agent can read, grep, and reason about without API calls.

## Directory Structure

```
github-data/
  repo.yml                   # repository metadata + sync state
  labels.yml                 # all repository labels
  milestones.yml             # all milestones
  issues/
    0001.md                  # issue or PR — one file per number
    0002.md
    0042.md
    0043.md
  releases/
    v1.0.0.md
    v1.2.0.md
  hooks/                     # optional — prompt templates triggered by events
    triage-new-issues.md
    review-comments.md
```

Issues and pull requests share a single number space (as on GitHub). The filename is the zero-padded number. A file is self-contained: open it and you see the full thread.

## repo.yml

Repository-level metadata and sync cursor.

```yaml
owner: acme
repo: widgets
default_branch: main
synced_at: 2024-09-17T08:00:00Z
```

## labels.yml

```yaml
- name: bug
  color: d73a4a
  description: Something isn't working

- name: enhancement
  color: a2eeef
  description: New feature or request

- name: priority/high
  color: b60205
```

## milestones.yml

```yaml
- title: v2.1
  state: closed
  description: Stability release
  due_on: 2024-10-01
  closed_at: 2024-09-28

- title: v3.0
  state: open
  description: Major redesign
  due_on: 2025-03-01
```

## Issue File

YAML frontmatter holds structured metadata. The markdown body is the issue text. Comments, reviews, and events follow as additional YAML documents separated by `---`.

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
  - priority/high
milestone: v2.1
---

When passing an empty string to `parse()`, the application crashes
with a null pointer exception.

## Steps to reproduce

1. Call `parse("")`
2. Observe crash
```

## Pull Request File

Same format. The `type: pull_request` field and PR-specific frontmatter fields distinguish it from an issue.

```markdown
---
number: 43
title: Handle empty input in parser
type: pull_request
state: closed
created_at: 2024-09-15T12:00:00Z
updated_at: 2024-09-16T14:00:00Z
closed_at: 2024-09-16T14:00:00Z
author: octocat
assignees:
  - octocat
labels:
  - bugfix
milestone: v2.1
source_branch: fix/empty-input
target_branch: main
merge:
  merged: true
  merged_at: 2024-09-16T14:00:00Z
  merged_by: hubot
  commit_sha: abc123f
reviewers:
  - hubot
requested_reviewers:
  - monalisa
---

Fixes #42. Adds a guard clause to `parse()` to return early on empty input.
```

## Subsequent Documents

After the first document, each `---` starts a new document. The `document` field declares its type. All documents appear in chronological order.

### comment

```markdown
---
document: comment
id: 100
author: hubot
created_at: 2024-09-15T11:00:00Z
---

I can reproduce this. The guard clause was removed in the last refactor.
```

### review

```markdown
---
document: review
id: 200
author: hubot
state: approved
commit_sha: abc123f
submitted_at: 2024-09-16T10:00:00Z
---

Looks good. The early return is clean.
```

### review_comment

Inline code comment tied to a file, line, and review.

```markdown
---
document: review_comment
id: 201
review_id: 200
author: hubot
created_at: 2024-09-16T10:00:00Z
path: src/parser.js
line: 12
side: RIGHT
commit_sha: abc123f
---

Nit: could use `=== undefined` instead of `== null` for clarity.
```

### event

State changes. Usually no body.

```markdown
---
document: event
event: labeled
actor: octocat
created_at: 2024-09-15T10:31:00Z
label: bug
---
```

Common event types: `labeled`, `unlabeled`, `assigned`, `unassigned`, `closed`, `reopened`, `merged`, `renamed`, `milestoned`, `demilestoned`, `referenced`, `cross-referenced`, `review_requested`, `review_request_removed`, `review_dismissed`, `head_ref_force_pushed`, `head_ref_deleted`, `base_ref_changed`, `converted_to_draft`, `ready_for_review`, `locked`, `unlocked`, `pinned`, `unpinned`, `transferred`, `connected`, `disconnected`, `marked_as_duplicate`, `unmarked_as_duplicate`.

Event-specific fields are added flat in frontmatter:

| Event type          | Extra fields                                    |
| ------------------- | ----------------------------------------------- |
| `labeled/unlabeled` | `label`                                         |
| `assigned/unassigned` | `assignee`                                    |
| `milestoned/demilestoned` | `milestone`                               |
| `renamed`           | `from`, `to`                                    |
| `closed/merged/referenced` | `commit_sha`                              |
| `cross-referenced`  | `source_number`, `source_repo`                  |
| `review_requested/review_request_removed` | `reviewer`                  |
| `locked`            | `lock_reason`                                   |
| `review_dismissed`  | `dismissal_message`                             |

## Release File

```markdown
---
tag: v1.0.0
name: Version 1.0.0
draft: false
prerelease: false
author: octocat
created_at: 2024-06-01T12:00:00Z
published_at: 2024-06-01T12:00:00Z
target_commitish: main
assets:
  - name: app-v1.0.0-linux-amd64.tar.gz
    content_type: application/gzip
    size_bytes: 12345678
    download_count: 542
---

## What's New

- Initial stable release
- Full parser support
- CLI interface
```

## Complete Example: issues/0042.md

A full issue file showing the chronological thread.

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
  - priority/high
milestone: v2.1
---

When passing an empty string to `parse()`, the application crashes
with a null pointer exception.

---
document: event
event: labeled
actor: octocat
created_at: 2024-09-15T10:31:00Z
label: bug
---

---
document: event
event: labeled
actor: octocat
created_at: 2024-09-15T10:31:00Z
label: priority/high
---

---
document: comment
id: 100
author: hubot
created_at: 2024-09-15T11:00:00Z
---

I can reproduce this. The guard clause was removed in the last refactor.

---
document: event
event: assigned
actor: octocat
created_at: 2024-09-15T11:05:00Z
assignee: hubot
---

---
document: comment
id: 101
author: octocat
created_at: 2024-09-15T14:00:00Z
---

Fixed in PR #43.

---
document: event
event: closed
actor: hubot
created_at: 2024-09-16T14:00:00Z
commit_sha: abc123f
---
```

## Frontmatter Reference

### Issue / Pull Request (first document)

| Field               | Type        | Notes                                  |
| ------------------- | ----------- | -------------------------------------- |
| `number`            | integer     | Required. Unique within repo           |
| `title`             | string      | Required                               |
| `type`              | string      | `pull_request` if PR, omit for issues  |
| `state`             | string      | `open` or `closed`                     |
| `state_reason`      | string      | `completed`, `not_planned`, `reopened` |
| `locked`            | boolean     | Omit if false                          |
| `created_at`        | ISO-8601    | Required                               |
| `updated_at`        | ISO-8601    | Required                               |
| `closed_at`         | ISO-8601    | Present when closed                    |
| `author`            | string      | GitHub username                        |
| `assignees`         | string list | Usernames                              |
| `labels`            | string list | Label names                            |
| `milestone`         | string      | Milestone title                        |
| `reactions`         | map         | `{"+1": 2, "heart": 1}`, omit if none |

### PR-only fields (when `type: pull_request`)

| Field                 | Type        | Notes                          |
| --------------------- | ----------- | ------------------------------ |
| `draft`               | boolean     | Omit if false                  |
| `source_branch`       | string      |                                |
| `target_branch`       | string      |                                |
| `source_repo`         | string      | Only for cross-repo PRs       |
| `merge.merged`        | boolean     |                                |
| `merge.merged_at`     | ISO-8601    |                                |
| `merge.merged_by`     | string      | Username                       |
| `merge.commit_sha`    | string      |                                |
| `reviewers`           | string list | Completed reviewers            |
| `requested_reviewers` | string list | Pending reviewers              |

### Subsequent documents

| Field      | Type     | Notes                                                                      |
| ---------- | -------- | -------------------------------------------------------------------------- |
| `document` | string   | Required. `comment`, `review`, `review_comment`, `event`                   |
| `id`       | integer  | Required for comments and reviews                                          |
| `author`   | string   | For comments/reviews                                                       |
| `actor`    | string   | For events                                                                 |
| `created_at` | ISO-8601 | Required                                                                 |

Type-specific fields are added flat — see examples above.

## Agent Usage

This format is designed so an agent with standard file tools (read, glob, grep) can work with GitHub data without API access.

**Find open bugs:**
```
grep -l "state: open" github-data/issues/*.md | xargs grep -l "bug"
```

**Read a specific issue thread:**
```
cat github-data/issues/0042.md
```

**Find issues mentioning a file:**
```
grep -rl "parser.js" github-data/issues/
```

**Find all PRs merged to main:**
```
grep -l "target_branch: main" github-data/issues/*.md | xargs grep -l "merged: true"
```

**List releases:**
```
ls github-data/releases/
```

**Check sync freshness:**
```
cat github-data/repo.yml
```

## Sync Behavior

- **Full sync**: Uses bulk API endpoints (repo-wide comments, events, PRs, review comments) to fetch all data in a few paginated requests instead of per-issue calls. Only PR reviews require per-PR fetches (no bulk endpoint).
- **Incremental sync**: Uses `synced_at` from `repo.yml`. Fetches only items updated since last sync via the `since` parameter. Uses per-issue timeline endpoint for changed issues (gives complete history in one call) plus bulk PR list.
- **Deleted items**: GitHub doesn't hard-delete issues. Transferred or spam-deleted issues are left as-is (the `state` and timeline tell the story).
- **File naming**: Zero-padded to 4 digits (`0042.md`). Repos with >9999 issues use 5+ digits.
- **Idempotent**: Running sync twice produces the same files. Safe to re-run.

## Design Decisions

**Why `github-data/` inside the repo?** The agent already has the repo checked out. Colocating the data means no extra paths to configure. Add `github-data/` to `.gitignore` if you don't want it committed.

**Why one file per issue?** An agent can read a single file to get the full picture. Grep works across all issues. No database, no joins, no query language.

**Why multi-document markdown?** The thread reads top-to-bottom like a conversation. Frontmatter is parseable; the body is readable. Standard YAML parsers handle multi-document streams.

**Why usernames instead of user objects?** Keeps files readable and greppable. A username is enough to identify who did what. Full user profiles (email, avatar) are rarely needed for reasoning.

**Why flat event fields?** `label: bug` is simpler than `label: { name: bug, color: d73a4a }`. The label details live in `labels.yml` if you need them.

**Why chronological order?** Events and comments interleaved in time order tell the story of what happened. An agent can read top-to-bottom without sorting.
