// Package shadow implements the --debug-compare parity harness. For each
// mirrored read it also fetches the same path from api.github.com, normalizes
// volatile fields out of both JSON payloads, and diffs them. On a meaningful
// divergence it logs the gap and (optionally) files a deduplicated issue on the
// project's own repo so coverage holes get tracked. The locally-computed answer
// is always what the caller receives; the remote fetch is comparison-only.
package shadow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Fetcher is the minimal upstream-GET dependency (a function keeps the proxy
// coupling loose).
type Fetcher func(ctx context.Context, path string) (status int, body []byte, err error)

// Comparator diffs local responses against upstream and tracks reported gaps.
type Comparator struct {
	fetch     Fetcher
	fileIssue func(title, body string) error
	repoSlug  string
	mu        sync.Mutex
	seen      map[string]bool // fingerprint → already reported
	volatile  map[string]bool
}

// New builds a Comparator. fileIssue may be nil to only log divergences.
func New(fetch Fetcher, fileIssue func(title, body string) error, repoSlug string) *Comparator {
	return &Comparator{
		fetch:     fetch,
		fileIssue: fileIssue,
		repoSlug:  repoSlug,
		seen:      map[string]bool{},
		volatile: map[string]bool{
			// Fields GitHub fills that we cannot (or need not) mirror byte-for-byte.
			"node_id": true, "url": true, "html_url": true, "events_url": true,
			"labels_url": true, "comments_url": true, "timeline_url": true,
			"repository_url": true, "avatar_url": true, "gravatar_id": true,
			"followers_url": true, "following_url": true, "gists_url": true,
			"starred_url": true, "subscriptions_url": true, "organizations_url": true,
			"repos_url": true, "received_events_url": true, "performed_via_github_app": true,
			"author_association": true, "reactions": true, "score": true,
		},
	}
}

// Check compares localBody against the upstream answer for path. It runs the
// comparison synchronously in a goroutine-friendly way; callers typically invoke
// it in a goroutine so the response is not delayed.
func (c *Comparator) Check(path string, localBody []byte) {
	status, remoteBody, err := c.fetch(context.Background(), path)
	if err != nil || status != http.StatusOK {
		return // upstream unavailable or not a 200: nothing meaningful to diff
	}
	localNorm := c.normalize(localBody)
	remoteNorm := c.normalize(remoteBody)
	if bytes.Equal(localNorm, remoteNorm) {
		return
	}
	fp := fingerprint(path, localNorm, remoteNorm)
	c.mu.Lock()
	already := c.seen[fp]
	c.seen[fp] = true
	c.mu.Unlock()
	if already {
		return
	}

	log.Printf("shadow-compare: divergence on %s (fingerprint %s)", path, fp)
	if c.fileIssue == nil {
		return
	}
	title := fmt.Sprintf("[shadow-compare] divergence on %s", path)
	body := fmt.Sprintf("The local mirror diverged from api.github.com.\n\n"+
		"**Path:** `%s`\n\n**Local (normalized):**\n```json\n%s\n```\n\n"+
		"**Remote (normalized):**\n```json\n%s\n```\n\n_Filed automatically at %s._",
		path, truncate(localNorm), truncate(remoteNorm), time.Now().UTC().Format(time.RFC3339))
	if err := c.fileIssue(title, body); err != nil {
		log.Printf("shadow-compare: filing issue failed: %v", err)
	}
}

// normalize parses JSON, strips volatile keys recursively, and re-marshals with
// sorted keys so the diff is order-independent.
func (c *Comparator) normalize(b []byte) []byte {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b
	}
	v = c.strip(v)
	out, err := marshalSorted(v)
	if err != nil {
		return b
	}
	return out
}

func (c *Comparator) strip(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range t {
			if c.volatile[k] {
				continue
			}
			out[k] = c.strip(val)
		}
		return out
	case []any:
		for i := range t {
			t[i] = c.strip(t[i])
		}
		return t
	default:
		return v
	}
}

// marshalSorted marshals with map keys sorted (encoding/json already sorts map
// keys, so a plain Marshal suffices; kept as a seam for future tuning).
func marshalSorted(v any) ([]byte, error) {
	return json.Marshal(v)
}

func fingerprint(path string, local, remote []byte) string {
	// A coarse fingerprint: the path plus the sorted set of top-level keys that
	// differ. This collapses "same endpoint, same shape of gap" into one issue.
	lk := topKeys(local)
	rk := topKeys(remote)
	all := map[string]bool{}
	for _, k := range lk {
		all[k] = true
	}
	for _, k := range rk {
		all[k] = true
	}
	var keys []string
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return path + "|" + fmt.Sprint(keys)
}

func topKeys(b []byte) []string {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func truncate(b []byte) string {
	const max = 4000
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "\n… (truncated)"
}
