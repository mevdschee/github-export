# Plan: SQLite-backed store, on-demand Markdown export, OpenAPI + MCP, gh/GitHub-MCP parity

## 1. Goals

1. **SQLite as the primary store.** All synced GitHub content lives in a single
   SQLite database as the source of truth. The sync engine writes to SQLite, not
   to markdown.
2. **Markdown export becomes on demand and one-way.** Markdown files are a
   *derived, one-way view* produced only when the user explicitly asks
   (`github-export export`). SQLite is the sole source of truth; markdown is
   never read back. The export format is free to evolve — it is not contractually
   pinned to today's byte layout.
3. **OpenAPI spec.** Ship a machine-readable OpenAPI 3.1 description of a local
   HTTP API that serves the synced data.
4. **MCP server.** Expose the data through a Model Context Protocol server whose
   tool names/shapes match GitHub's official MCP server.
5. **Parity with `gh` CLI + GitHub MCP for reads/queries; proxy for writes.**
   The local API mirrors GitHub's own REST/GraphQL surface closely enough that
   reads can be served from SQLite offline, while any write (and any read we do
   not yet mirror) is transparently proxied to `api.github.com`.

### Non-goals (initial scope)

- Re-implementing GitHub's *entire* REST surface by hand. We mirror the slices
  we already sync; everything else falls through to the proxy. "100% match" is
  achieved by the **proxy fallback**, not by hand-writing every endpoint.
- Becoming a write-through cache with conflict resolution. Writes go to GitHub;
  we re-sync the affected entity afterward. No offline write queue.
- Multi-repo databases in v1 (one DB = one repo, matching today's model). The
  schema leaves room for it (see §4).

---

## 2. Where we are today

- Go 1.22, single binary, only dep is `gopkg.in/yaml.v3`.
- `main.go` orchestrates `sync.Labels/Milestones/Projects/Issues/Releases/Discussions/Repo`.
- Each `sync.*` function fetches from REST/GraphQL (`internal/github/client.go`),
  parses JSON into `map[string]any` (`internal/jsonutil`), and **writes markdown
  directly** (`internal/document`).
- Incremental state is a single `synced_at` in `repo.yml`.
- Change detection (`internal/hooks`) diffs fresh API data against the previous
  on-disk markdown frontmatter and emits `events/*.md`.

The key structural fact: today **fetch, model, and render-to-markdown are fused**
inside each `sync.*` function. The core refactor is to split them into
**fetch → store (SQLite) → render**, so SQLite sits in the middle and markdown
becomes one of several renderers.

---

## 3. Target architecture

```
                 GitHub REST + GraphQL (api.github.com)
                   ▲  pull (sync)         ▲  push (write proxy)
                   │                      │
   ┌───────────────┴──────────────────────┴────────────────┐
   │                      core                              │
   │  internal/github    – API client (unchanged)           │
   │  internal/sync      – fetch → UPSERT into store         │
   │  internal/store     – SQLite: schema, upserts, queries │  ← source of truth
   │  internal/query     – high-level read/query API         │
   │  internal/gitbackend– repo content from a local git repo│ ← files/commits/code-search
   └───┬───────────────┬───────────────┬───────────────┬────┘
       │               │               │               │
   exporter        httpapi          mcp             writeproxy
   (markdown,      (REST+GraphQL+    (MCP tools,     (POST/PATCH/…
    on demand)      OpenAPI, reads    reads from      → GitHub,
                    from query/git;   query/git;      then sync.
                    writes via proxy) writes proxy)   re-sync)
```

**Centerpiece — the GitHub-API-compatible read proxy.** One HTTP handler speaks
GitHub's API shape (same paths/queries, same JSON field names) over three
read backends, with a proxy fallback:

- **SQLite** (`internal/query`) for synced collaboration data — issues, PRs,
  comments, reviews, timelines, labels, milestones, releases, **projects and
  discussions** (the last two via GraphQL — see §7).
- **Local git** (`internal/gitbackend`) for repo *content* — file contents,
  commits, blobs, branches/tags, diffs, blame, and code search — answered
  straight from a local clone, no GitHub API needed (your decision on §6/4).
- **Proxy fallback** → `api.github.com` for anything neither backend can answer
  (e.g. check runs, Actions, anything we don't mirror) **and all writes**
  (response streamed back, then the affected entity re-synced into SQLite).

This single component is simultaneously: the thing the **OpenAPI spec
describes**, the backend the **MCP server** calls, and the endpoint you point
**`gh`** at. We build the compatibility surface once and reuse it three ways.

---

## 4. Component 1 — SQLite store (`internal/store`)

### Library

Use **`modernc.org/sqlite`** (pure-Go, cgo-free) to preserve today's
"`go build .` with no toolchain" property. (`mattn/go-sqlite3` is faster but
needs cgo — rejected for that reason; revisit only if perf demands it.)

### Schema principles

- **Dual storage per entity: typed columns + `raw_json`.** Typed columns
  (`number`, `state`, `author`, timestamps, …) power querying and the API;
  `raw_json` holds the full upstream payload so markdown export and API
  responses are full-fidelity and round-trip-safe. This mirrors how `jsonutil`
  already pulls fields off `map[string]any`.
- Use **SQLite JSON1** for flexible filtering and **FTS5** for body/comment text
  search (this is what powers `gh search` / MCP `search_*` offline).
- Schema versioned via `meta` + a tiny migration runner (embedded `.sql` files,
  `PRAGMA user_version`).
- `repo_id` column on every table from day one so a future multi-repo DB is an
  additive change, not a rewrite.

### Tables (initial)

| Table | Key columns | Notes |
|---|---|---|
| `meta` | `key, value` | `owner`, `repo`, `synced_at`, `schema_version` (replaces `repo.yml` state) |
| `repositories` | `id`, owner, name, default_branch, visibility, … `raw_json` | repo metadata |
| `users` | `id`, `login`, type, `raw_json` | dedup actors/authors |
| `issues` | `number` PK, `is_pull_request`, title, state, state_reason, locked, author, milestone, created/updated/closed_at, body, `raw_json` | issues + PRs unified, as today |
| `pull_request_details` | `number` FK, draft, source/target_branch, source_repo, merged, merged_at, merged_by, merge_commit_sha | PR-only fields |
| `issue_labels` | `issue_number, label` | join |
| `issue_assignees` | `issue_number, login` | join |
| `issue_projects` | `issue_number, project_number` | drives the `projects:` frontmatter list |
| `comments` | `id` PK, issue_number, author, created/updated_at, body, `raw_json` | |
| `reviews` | `id` PK, pr_number, author, state, commit_sha, submitted_at, body | |
| `review_comments` | `id` PK, review_id, pr_number, author, path, line, side, commit_sha, created_at, body | |
| `timeline_events` | `id` PK, issue_number, event, actor, created_at, `extra_json` | labeled/assigned/renamed/… |
| `labels` | `name` PK, color, description | |
| `milestones` | `number` PK, title, state, description, due_on, closed_at | |
| `projects` | `number` PK, title, state, public, owner, url, description, created/updated_at, readme, `fields_json` | Projects v2 |
| `project_items` | `project_number, item_number`, type, title, repo, `fields_json` | |
| `discussions` | `number` PK, title, category, state, state_reason, author, created/updated/closed_at, answer_id, answer_chosen_at, answer_chosen_by, body | |
| `discussion_comments` | `id` PK, discussion_number, parent_id, author, created_at, is_answer, body | comment + reply, `parent_id` for nesting |
| `releases` | `tag` PK, name, draft, prerelease, author, created_at, published_at, target_commitish, body | |
| `release_assets` | `release_tag, name`, content_type, size_bytes, download_count | |
| `fts_content` | FTS5 virtual table | bodies of issues/comments/discussions for search |

### Store API (sketch)

```go
type Store struct{ db *sql.DB }

func Open(path string) (*Store, error)          // opens + runs migrations
func (s *Store) Meta() (owner, repo, syncedAt string, err error)
func (s *Store) Tx(fn func(*Txn) error) error   // one transaction per sync pass

// upserts take the raw map already parsed by jsonutil
func (t *Txn) UpsertIssue(m map[string]any) error
func (t *Txn) UpsertComment(issueNum int64, m map[string]any) error
func (t *Txn) ReplaceTimeline(issueNum int64, events []map[string]any) error
// … one per entity, each writing typed columns + raw_json
```

Each sync pass runs in a single transaction for atomicity and speed (SQLite is
much faster with batched writes; set `PRAGMA journal_mode=WAL`,
`synchronous=NORMAL`).

---

## 5. Component 2 — Sync engine refactor (`internal/sync`)

The refactor is mechanical and low-risk because the fetch + JSON-parse logic
stays; only the *sink* changes from "write markdown" to "upsert into store".

1. Add a `*store.Txn` (or `*store.Store`) parameter to each `sync.*` function.
2. Replace `document.Writer` / file-writing tails with `txn.Upsert*` calls.
3. Move incremental state from `repo.yml` to `meta.synced_at`.
4. **Change detection moves to the store.** Today `hooks` diffs API data vs.
   on-disk markdown. Instead, diff the incoming entity against its **current
   SQLite row** (the previous `raw_json` / typed columns) *before* the upsert,
   inside the transaction. This is cleaner and removes the markdown dependency
   from event detection. Events are written to an `events` table (and rendered
   to `events/*.md` only on export, or emitted live by `serve`/`mcp`).
5. `main.go`'s `sync` path becomes: open store → `Tx(func(txn){ Labels; Milestones;
   Projects; Issues; Releases; Discussions; Repo })` → done. No markdown by default.

The existing `--max-age` / `since` logic is unchanged — it still drives the
GitHub `?since=` filter; `synced_at` now comes from `meta`.

---

## 6. Component 3 — Markdown exporter (`internal/exporter`)

- New subcommand `github-export export [--out github-data/]`.
- **One-way and read-only against the store.** Export reads rows from SQLite and
  renders files; nothing ever parses markdown back. The store is authoritative.
- Reuses `internal/document` as the rendering primitive. Writer functions are
  fed by rows read back from the store (`raw_json` rehydrates the map shape, so
  `writeIssueFile` etc. need minimal change).
- Produces a familiar layout (`issues/NNNN.md`, `projects/`, `discussions/`,
  `releases/`, `labels.yml`, `milestones.yml`, `repo.yml`, `events/`), but the
  format is **not** frozen — it may evolve as the store gains fields. We do not
  maintain byte-for-byte compatibility with the pre-SQLite output.
- Optional `export --since` to write only entities changed since a timestamp.

---

## 7. Component 4 — Read/query layer + HTTP API + OpenAPI

### `internal/query`

Backend-agnostic read functions over the store, shaped to serve all three
consumers (HTTP, MCP, exporter):

```go
ListIssues(ListIssuesOpts) ([]Issue, error)   // state, labels, author, since, sort, page
GetIssue(number) (Issue, error)
ListPullRequests(...) / GetPullRequest(...)
SearchIssues(query) (...)                      // FTS5-backed
ListDiscussions / GetDiscussion / ListReleases / ListProjects / ...
```

Return values serialize to **GitHub's REST JSON shape** (straight from
`raw_json` where possible) so the HTTP layer is a thin adapter.

### `internal/httpapi` — `github-export serve [--addr :8080]`

A `net/http` server. **No auth: it binds localhost only and trusts any local
caller** (your decision). The proxy fallback and writes spend the server's own
`GITHUB_TOKEN`; clients never present a token.

It serves three read backends plus a fallback:

1. **REST mirror from SQLite:**
   - `GET /repos/{owner}/{repo}/issues` (+ `state`, `labels`, `since`, `sort`, `per_page`, `page`)
   - `GET /repos/{owner}/{repo}/issues/{n}` and `/comments`, `/timeline`
   - `GET /repos/{owner}/{repo}/pulls`, `/pulls/{n}`, `/pulls/{n}/reviews`, `/comments`
   - `GET /repos/{owner}/{repo}/labels`, `/milestones`, `/releases`
   - `GET /search/issues`, `GET /search/code` (FTS5 + git, see search below)
   - `GET /repos/{owner}/{repo}` (metadata)
   - Pagination via `Link` headers, matching GitHub.
2. **GraphQL mirror from SQLite** (`POST /graphql`) — **required for read
   parity**, because Projects v2 and Discussions are GraphQL-only and `gh` /
   GitHub MCP issue many reads as GraphQL. See "GraphQL read parity" below.
3. **Repo content from local git** (`internal/gitbackend`):
   - `GET /repos/{o}/{r}/contents/{path}`, `/git/blobs/{sha}`, `/git/trees/{sha}`
   - `GET /repos/{o}/{r}/commits`, `/commits/{sha}`, `/compare/{base}...{head}`
   - `GET /repos/{o}/{r}/branches`, `/tags`
   - answered from a local clone (`--repo-path`, default: the cwd's git repo);
     no GitHub API call.
4. **Proxy fallback** (Component 5) for everything else (check runs, Actions,
   unmirrored paths) **and all writes**.

Response includes a freshness header (`X-GitHub-Export-Synced-At`) so callers
know how stale the local copy is.

### GraphQL read parity (`internal/graphqlmirror`)

`gh` and the GitHub MCP lean heavily on GraphQL, so a REST-only mirror is not
enough. Approach:

- Parse incoming GraphQL queries (use a Go GraphQL parser, e.g.
  `github.com/vektah/gqlparser`) against a **schema subset** covering the
  fields we actually store (repository → issues/pullRequests/discussions/
  projectsV2 and their nested connections, plus `search`).
- Resolve fields from SQLite via `internal/query`; honor `first`/`after`
  cursor pagination by encoding offsets the way the GraphQL connection spec
  expects.
- Any query touching a field outside the supported schema **falls through to the
  proxy** (`POST /graphql` → `api.github.com`), so coverage is "everything",
  with the mirrored subset served offline and the rest forwarded.
- This is the larger build item in the API phase; scope it to the exact queries
  `gh` and the GitHub MCP emit first (capture them with the shadow-compare mode
  below), then widen.

### OpenAPI 3.1 (`api/openapi.yaml`)

- Describes exactly the **mirrored** read endpoints (the offline-serveable
  subset) plus the proxied write endpoints we officially support.
- Author by hand initially (the surface is small and stable), reusing
  component schemas lifted from GitHub's published OpenAPI for fidelity.
- Validate in CI with a linter (e.g. `vacuum`/`redocly`) and, ideally, generate
  request/response model structs from it so handlers and spec cannot drift.
- Serve the spec at `GET /openapi.yaml` and a Swagger/Redoc page at `/docs`.

### Local git backend (`internal/gitbackend`)

Repo *content* endpoints (file contents, blobs, trees, commits, compare/diffs,
branches, tags, blame, **code search**) are answered from a **local git clone**
rather than the GitHub API or SQLite — your decision that "point 4 can be
answered by git directly."

- `--repo-path` selects the working tree (default: discover the git repo of the
  cwd; fall back to a managed clone under the DB's directory if absent).
- Shell out to `git` (or use `go-git`) for: `git show <sha>:<path>` (contents),
  `git cat-file` (blobs), `git log`/`rev-list` (commits), `git diff`/`merge-base`
  (compare), `git for-each-ref` (branches/tags), `git grep` (code search).
- Map results into GitHub's REST JSON shapes so `gh` / MCP see no difference.
- `serve --git-fetch` optionally `git fetch` before answering, to stay current;
  otherwise content is as fresh as the local checkout.
- **Not git-answerable** (check runs, Actions, deployments) stay proxy-only.

### Search — full coverage

Search must "support everything" (your decision):

- Issue/PR/discussion search → FTS5 over bodies/titles + a qualifier parser
  (`is:`, `state:`, `label:`, `author:`, `assignee:`, `milestone:`, `in:`,
  date ranges, `sort:`) translated to SQL.
- Code search → `git grep` via the git backend.
- Any query using a qualifier we don't yet translate **falls through to the
  proxy**, so the surface is complete even before every qualifier is local.
  The shadow-compare mode (below) is how we find the gaps to close.

### Shadow-compare debug mode (`--debug-compare`)

A self-verifying parity harness, on `serve`/`mcp`/`api`:

- For each **read**, answer it **both** locally (SQLite/git) **and** remotely
  (proxy to `api.github.com`), then **diff the two JSON results**.
- On a meaningful difference (after normalizing volatile fields — rate-limit
  headers, ephemeral URLs, ordering where GitHub is unordered), **automatically
  open an issue on this project's own repo** (`mevdschee/github-export`) with the
  request, both responses, and the diff — deduplicated by a fingerprint of
  (endpoint + diff shape) so the same gap files once, not every request.
- The locally-computed answer is still what's returned to the caller; remote is
  only for comparison. Off by default (it doubles request cost and needs a
  token with issue-write scope on the project repo).
- Doubles as the data source for the §9 gh-parity matrix and the §7 GraphQL
  "capture the queries gh actually emits" step.

---

## 8. Component 5 — Write proxy (`internal/writeproxy`)

- Any non-GET request (and unmirrored GETs) is forwarded to `api.github.com`
  with the `GITHUB_TOKEN`, preserving method, path, query, body, and relevant
  headers. Response (status, body, rate-limit headers) is streamed back
  verbatim.
- **Read-after-write consistency (synchronous):** when a write targets a known
  entity (`POST .../issues`, `PATCH .../issues/{n}`, `POST .../issues/{n}/comments`,
  merges, label edits, …), run a **synchronous targeted re-sync** of just that
  entity into SQLite *before returning to the caller* (your decision). The local
  store is therefore guaranteed consistent the moment a write call returns — at
  the cost of one extra GET on the write path. Map write paths → re-sync action
  in a small table. If the re-sync GET fails, the write still succeeded upstream;
  return success with a warning header and mark the entity stale.
- Configurable modes: `--proxy=on` (default), `--proxy=off` (offline-only;
  writes and unmirrored reads return `501`), so the tool can run fully air-gapped.
- Surface this in `serve`, `mcp`, and a `github-export api <method> <path>`
  passthrough command (a `gh api` work-alike).

---

## 9. Component 6 — `gh` CLI parity strategy

"100% match the `gh` read/query API" is delivered in two complementary ways,
because `gh` itself is just a client of GitHub's API:

1. **Proxy redirection (the 100% mechanism).** Document and support pointing
   `gh` at our local server:
   - `gh api` and most commands honor `GH_HOST` / a base-URL override; with the
     compatibility proxy, every `gh api <GET path>` is served from SQLite when
     mirrored and transparently proxied otherwise. Writes proxy through. This
     gives full surface coverage without re-implementing `gh`.
   - Ship a wrapper/env snippet (`GH_HOST=localhost:8080` or
     `gh --hostname`) and verify against a matrix of common commands
     (`gh issue list/view`, `gh pr list/view/diff`, `gh search issues`,
     `gh release list`, `gh api ...`).
2. **Native convenience subcommands (ergonomic subset).** A handful of
   first-class read commands for offline use without `gh` installed:
   `github-export issue list/view`, `pr list/view`, `search`, `release list`,
   mirroring `gh`'s flags and (optionally) its `--json` field selection. These
   are thin wrappers over `internal/query`.

A parity test matrix (Phase 4) runs the same query against (a) real `gh` →
GitHub and (b) `gh` → our proxy, and diffs the JSON to measure and track
coverage.

---

## 10. Component 7 — MCP server (`internal/mcp` / `github-export mcp`)

- Use the **official Go MCP SDK** (`github.com/modelcontextprotocol/go-sdk`).
- Serve over stdio by default (`github-export mcp`), optional `--http` for the
  streamable-HTTP transport.
- **Tool names/shapes mirror GitHub's official MCP server** so existing agent
  configs work unchanged. Read tools resolve against `internal/query` (SQLite);
  write tools call `internal/writeproxy`:

  | Tool (GitHub MCP name) | Backed by |
  |---|---|
  | `get_issue`, `list_issues`, `search_issues` | SQLite (`query`) |
  | `get_pull_request`, `list_pull_requests`, `get_pull_request_files`, `get_pull_request_reviews` | SQLite |
  | `list_commits`, `get_file_contents`, `search_code` | proxy (not synced) → GitHub |
  | `create_issue`, `add_issue_comment`, `update_issue`, `create_pull_request`, `merge_pull_request` | write proxy → GitHub + re-sync |

- Each read tool returns the same JSON the GitHub MCP server returns (from
  `raw_json`), plus a `synced_at` note so agents can reason about staleness.
- A capability/config flag chooses **read-only mode** (no write tools
  registered) vs. **read-write** (write tools proxy out).

---

## 11. CLI surface redesign

Move from "flags only" to subcommands (keep the old bare form working for one
release with a deprecation notice):

```
github-export sync   [--max-age=…] [owner/repo] [--db github.sqlite]      # → SQLite (default action)
github-export export [--out github-data/] [--since=…]                     # SQLite → markdown (one-way)
github-export serve  [--addr :8080] [--proxy=on|off] [--repo-path .]      # HTTP API + OpenAPI
                     [--git-fetch] [--debug-compare] [--auto-sync=15m]
github-export mcp    [--http] [--read-only] [--proxy=on|off] [--repo-path .] [--debug-compare]
github-export api    <METHOD> <path> [--input -] [--debug-compare]        # gh-api-style passthrough
github-export issue|pr|search|release …                                   # native read subcommands
```

- Default DB path: `./github.sqlite` (or `--db`). One repo per DB.
- `serve`/`mcp` bind **localhost and require no token from clients** (§7); the
  server's `GITHUB_TOKEN` is used only for proxy fallback and writes.
- `--repo-path` points the git backend at a local clone (default: cwd's repo).
- **Fold in the TODO UX win:** when no `owner/repo` and no DB/`repo.yml` is
  present, auto-detect from `git remote get-url origin` (parse
  `github.com:OWNER/REPO(.git)`). Removes the most common first-run failure.
- Bare invocation with an existing DB ⇒ `sync` (incremental), matching today's
  "run in the export dir to update" behavior. One repo per DB.

---

## 12. Consistency & freshness model

- The store is a **read cache of GitHub**, explicitly possibly-stale.
- Every API/MCP read advertises `synced_at`. A `GET /status` (and MCP `status`
  tool) reports last sync time and per-entity counts.
- Optional `serve --auto-sync=15m` background incremental sync; otherwise the
  data is exactly as fresh as the last `sync`.
- Writes trigger targeted re-sync (§8) so the entity you just changed is
  immediately consistent locally.

---

## 13. Migration & backward compatibility

- **No markdown import.** Markdown is a one-way output; we never parse it back.
  Existing users get a fresh SQLite store by re-running `sync` against GitHub
  (the API is the source, not their old `github-data/` folder).
- **Sync state:** the only state worth carrying over is `synced_at`. On first
  `sync`, if a legacy `repo.yml` is present in the working dir, read its
  `synced_at` into the `meta` table so the first run stays incremental rather
  than re-pulling everything. Otherwise a full sync runs as today.
- `export` output is allowed to differ from the pre-SQLite layout (§6); we do
  not promise byte-stability to downstream markdown consumers.

---

## 14. Testing strategy

- **Refactor guard (Phase 1):** reframe `e2e_test.go` as sync→SQLite, asserting
  on **store contents** (row counts, key fields, timeline ordering, event
  detection) rather than markdown bytes. Since export is one-way and the format
  may evolve, data integrity in the store — not file layout — is the contract.
  A lighter `export` smoke test checks files render and parse as valid
  markdown/YAML, without pinning exact bytes.
- **Store unit tests:** upsert idempotency, incremental diffing, FTS queries,
  migration runner.
- **API tests:** golden JSON for mirrored endpoints; pagination/`Link` headers;
  proxy fallback (httptest fake upstream); freshness headers.
- **OpenAPI:** lint spec in CI; contract-test that every mirrored handler
  validates against the spec.
- **gh parity matrix (Phase 4):** diff `gh→GitHub` vs `gh→proxy` JSON for a
  command list; track coverage %.
- **MCP:** tool-list snapshot vs. GitHub MCP names; per-tool I/O tests against
  the store; write tools hit a fake upstream.
- Keep cgo-free: CI builds with `CGO_ENABLED=0`.

---

## 15. Phased roadmap

**Phase 0 — Foundations**
- Add `modernc.org/sqlite`; `internal/store` skeleton, migration runner, schema
  v1, `Open`/`Meta`/`Tx`.

**Phase 1 — SQLite as source of truth (highest value, de-risks everything)**
- Refactor `sync.*` to upsert into the store; move `synced_at` to `meta`
  (seeding from a legacy `repo.yml` if present).
- Move change detection to store-diff; events → `events` table.
- Build `internal/exporter` + one-way `export` subcommand reusing `internal/document`.
- Reframe e2e test as sync→SQLite asserting on store contents; add an `export`
  smoke test (valid markdown/YAML, not byte-pinned).
- *Deliverable:* default run populates SQLite; `export` renders markdown on demand.

**Phase 2 — Read/query + REST mirror + OpenAPI**
- `internal/query`; `serve` with the REST mirror; `api/openapi.yaml`; `/docs` +
  spec endpoint; freshness headers; `/status`. (REST only; GraphQL in Phase 3.)

**Phase 3 — GraphQL mirror + git backend + write proxy**
- `internal/graphqlmirror` (`POST /graphql` served from SQLite, proxy
  fallthrough) — needed for Projects/Discussions read parity.
- `internal/gitbackend` (files/commits/blobs/compare/code-search from a local
  clone); decide `go-git` vs shell-out.
- `internal/writeproxy`; proxy fallback in `serve`; `github-export api`
  passthrough; **synchronous** re-sync after writes; `--proxy` modes.

**Phase 4 — gh parity + shadow-compare**
- Document `gh`→proxy redirection; native `issue/pr/search/release` subcommands
  with `gh`-style `--json`.
- `--debug-compare` shadow mode (local vs remote diff → auto-file issue on
  `mevdschee/github-export`); use it to capture the real `gh`/MCP query set and
  drive the parity matrix + coverage report.

**Phase 5 — MCP server**
- `internal/mcp` with GitHub-MCP-matching tool names; read tools over `query`,
  write tools over `writeproxy`; stdio + HTTP transports; read-only flag.

**Phase 6 — Polish**
- `serve --auto-sync`; multi-repo DB (optional); perf pass (indexes, batched
  upserts); docs (`README`, `docs/` updates), and update
  `docs/data-model.md`/`export-plan.md` to describe the SQLite schema.

---

## 16. Decisions (settled)

1. **SQLite driver:** `modernc.org/sqlite` (cgo-free), to keep single-binary,
   no-toolchain builds. (Revisit `mattn/go-sqlite3` only if perf forces it.)
2. **Markdown is on-demand and one-way:** `sync` never writes markdown; `export`
   is explicit and reads only from the store. Export format may evolve — no
   byte-for-byte compatibility promise.
3. **One repo per DB**, default `./github.sqlite`. Schema still carries `repo_id`
   so a multi-repo DB would be additive, not a rewrite.
4. **No markdown import:** the store is rebuilt from GitHub, not from old files;
   only `synced_at` is carried over from a legacy `repo.yml` if present.
5. **MCP SDK:** official `github.com/modelcontextprotocol/go-sdk`.
6. **OpenAPI scope = mirrored reads + supported writes**, not all of GitHub.
7. **"100% parity"** is realized by the transparent proxy fallback (any
   read/write we don't mirror is forwarded), with mirrored endpoints serving the
   hot path offline — not by re-implementing every endpoint by hand.
8. **GraphQL is mirrored for reads**, not just proxied — required because
   Projects/Discussions and much of `gh`/MCP are GraphQL (§7). Unsupported
   fields fall through to the proxy.
9. **Local auth: none.** `serve`/`mcp` bind localhost and trust local callers;
   only the server's `GITHUB_TOKEN` reaches GitHub.
10. **Repo content (files/commits/blobs/code-search) comes from local git**, not
    the API; check runs/Actions remain proxy-only.
11. **Search supports everything** — local for parsed qualifiers + code search
    via git, proxy fallthrough for the rest.
12. **Writes re-sync synchronously** before returning (read-after-write
    consistency).
13. **Shadow-compare debug mode** answers locally + remotely, diffs, and
    auto-files an issue on `mevdschee/github-export` on divergence (off by
    default; also drives the parity matrix and GraphQL query capture).

## 17. New dependencies

- `modernc.org/sqlite` (store)
- `github.com/modelcontextprotocol/go-sdk` (MCP server)
- A GraphQL parser for the read mirror (`github.com/vektah/gqlparser`)
- `go-git` (`github.com/go-git/go-git/v5`) *or* shell out to the `git` binary
  for the git backend — decide in Phase 3 (shell-out is simpler and matches
  today's zero-heavy-deps spirit; `go-git` avoids a runtime `git` dependency).
- Dev/CI only: an OpenAPI linter (`redocly`/`vacuum`)
- (Still no cgo; `net/http`, `database/sql`, `yaml.v3` cover the rest.)
```
