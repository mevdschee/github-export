// Package writeproxy forwards requests we cannot (or must not) answer locally to
// api.github.com: all writes, and any read the local mirror does not implement.
// The upstream response is streamed back verbatim. After a successful write to a
// known entity it triggers a synchronous targeted re-sync so the local store is
// consistent the moment the call returns.
package writeproxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const upstream = "https://api.github.com"

// ReSyncFunc re-pulls a single entity into the store after a write. number is 0
// for whole-collection writes (e.g. creating a label). It runs synchronously on
// the write path; errors are logged, not fatal.
type ReSyncFunc func(kind string, number int64) error

// Proxy forwards to GitHub using the server's token.
type Proxy struct {
	Token    string
	Client   *http.Client
	ReSync   ReSyncFunc
	Disabled bool // --proxy=off: refuse to forward (offline-only mode)
}

// New builds a Proxy.
func New(token string, resync ReSyncFunc) *Proxy {
	return &Proxy{
		Token:  token,
		Client: &http.Client{Timeout: 60 * time.Second},
		ReSync: resync,
	}
}

// hopHeaders are managed by the transport and must not be copied.
var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true,
}

// Forward proxies r to api.github.com and writes the response to w. It returns
// the upstream status code (or 0 on a transport error already written to w).
func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request) int {
	if p.Disabled {
		http.Error(w, `{"message":"proxy disabled (--proxy=off); endpoint not mirrored locally"}`, http.StatusNotImplemented)
		return http.StatusNotImplemented
	}

	target := upstream + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		http.Error(w, `{"message":"building upstream request: `+err.Error()+`"}`, http.StatusBadGateway)
		return 0
	}
	for k, vs := range r.Header {
		if hopHeaders[http.CanonicalHeaderKey(k)] || strings.EqualFold(k, "Authorization") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := p.Client.Do(req)
	if err != nil {
		http.Error(w, `{"message":"upstream error: `+err.Error()+`"}`, http.StatusBadGateway)
		return 0
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-GitHub-Export-Proxied", "true")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)

	// Read-after-write consistency: re-sync the touched entity on a successful
	// write so a subsequent local read reflects it.
	if r.Method != http.MethodGet && r.Method != http.MethodHead &&
		resp.StatusCode >= 200 && resp.StatusCode < 300 && p.ReSync != nil {
		if kind, number, ok := classifyWrite(r.Method, r.URL.Path); ok {
			if err := p.ReSync(kind, number); err != nil {
				log.Printf("write-proxy: re-sync %s #%d failed: %v", kind, number, err)
				w.Header().Set("X-GitHub-Export-Resync", "failed")
			}
		}
	}
	return resp.StatusCode
}

// Request performs a programmatic call to api.github.com (used by the MCP write
// tools, which have no http.ResponseWriter to stream into). On a successful
// write to a known entity it triggers the same synchronous re-sync as Forward.
func (p *Proxy) Request(ctx context.Context, method, path string, body io.Reader) (status int, respBody []byte, err error) {
	if p.Disabled {
		return http.StatusNotImplemented, nil, fmt.Errorf("proxy disabled (--proxy=off)")
	}
	req, err := http.NewRequestWithContext(ctx, method, upstream+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if method != http.MethodGet && resp.StatusCode >= 200 && resp.StatusCode < 300 && p.ReSync != nil {
		if kind, number, ok := classifyWrite(method, path); ok {
			if rerr := p.ReSync(kind, number); rerr != nil {
				log.Printf("write-proxy: re-sync %s #%d failed: %v", kind, number, rerr)
			}
		}
	}
	return resp.StatusCode, respBody, nil
}

var (
	reIssue        = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/(\d+)$`)
	reIssueCreate  = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues$`)
	reIssueComment = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/(\d+)/comments$`)
	rePull         = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/(\d+)$`)
	rePullCreate   = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls$`)
	rePullMerge    = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/(\d+)/merge$`)
	reLabels       = regexp.MustCompile(`^/repos/[^/]+/[^/]+/labels`)
)

// classifyWrite maps a write path to (entity-kind, number) for targeted re-sync.
// number is 0 when the write creates a new entity (its number is unknown until
// the response body is parsed; the caller re-syncs the whole collection or the
// next sync picks it up).
func classifyWrite(method, path string) (kind string, number int64, ok bool) {
	switch {
	case reIssueComment.MatchString(path):
		m := reIssueComment.FindStringSubmatch(path)
		return "issue", atoi(m[1]), true
	case reIssue.MatchString(path):
		m := reIssue.FindStringSubmatch(path)
		return "issue", atoi(m[1]), true
	case rePullMerge.MatchString(path):
		m := rePullMerge.FindStringSubmatch(path)
		return "issue", atoi(m[1]), true
	case rePull.MatchString(path):
		m := rePull.FindStringSubmatch(path)
		return "issue", atoi(m[1]), true
	case reIssueCreate.MatchString(path) && method == http.MethodPost:
		return "issues", 0, true
	case rePullCreate.MatchString(path) && method == http.MethodPost:
		return "issues", 0, true
	case reLabels.MatchString(path):
		return "labels", 0, true
	}
	return "", 0, false
}

func atoi(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
