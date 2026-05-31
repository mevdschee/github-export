// Package gitbackend answers GitHub "repo content" reads — branches, tags,
// commits, and file contents — straight from a local git clone, with no call to
// api.github.com. Results are mapped into GitHub's REST JSON shapes so callers
// (gh, the MCP server) see no difference. Content-type endpoints that are not
// answerable from git (check runs, Actions) are left to the proxy fallback.
package gitbackend

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Backend runs git commands against a working tree.
type Backend struct {
	dir         string
	owner, repo string
	fetch       bool
}

// New builds a Backend rooted at dir. owner/repo are used to populate URL-ish
// fields in responses. If fetch is true, Fetch() should be called before serving.
func New(dir, owner, repo string, fetch bool) *Backend {
	return &Backend{dir: dir, owner: owner, repo: repo, fetch: fetch}
}

// Available reports whether dir is inside a git work tree.
func (b *Backend) Available() bool {
	return b.git("rev-parse", "--is-inside-work-tree") == "true"
}

// Fetch runs `git fetch --all` to refresh refs (best-effort).
func (b *Backend) Fetch() error {
	if !b.fetch {
		return nil
	}
	_, err := b.run("fetch", "--all", "--quiet")
	return err
}

// run executes git in the backend's directory and returns trimmed stdout.
func (b *Backend) run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = b.dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// git is run() that swallows errors (returns "" on failure) for probe calls.
func (b *Backend) git(args ...string) string {
	out, err := b.run(args...)
	if err != nil {
		return ""
	}
	return out
}

// Branches returns the local/remote branch refs in the /repos/.../branches shape.
func (b *Backend) Branches() ([]map[string]any, error) {
	out, err := b.run("for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads")
	if err != nil {
		return nil, err
	}
	var branches []map[string]any
	for _, line := range nonEmptyLines(out) {
		name, sha, _ := strings.Cut(line, " ")
		branches = append(branches, map[string]any{
			"name":      name,
			"commit":    map[string]any{"sha": sha},
			"protected": false,
		})
	}
	return branches, nil
}

// Tags returns tag refs in the /repos/.../tags shape.
func (b *Backend) Tags() ([]map[string]any, error) {
	out, err := b.run("for-each-ref", "--format=%(refname:short) %(objectname)", "refs/tags")
	if err != nil {
		return nil, err
	}
	var tags []map[string]any
	for _, line := range nonEmptyLines(out) {
		name, sha, _ := strings.Cut(line, " ")
		tags = append(tags, map[string]any{
			"name":   name,
			"commit": map[string]any{"sha": sha},
		})
	}
	return tags, nil
}

// commitFormat is a unit-separator-delimited git log format we can parse safely.
const commitFormat = "%H%x1f%an%x1f%ae%x1f%aI%x1f%cn%x1f%ce%x1f%cI%x1f%P%x1f%s%x1f%b%x1e"

// Commits lists commits (newest first) in the /repos/.../commits shape. sha is
// an optional starting ref; perPage/page paginate.
func (b *Backend) Commits(sha string, perPage, page int) ([]map[string]any, error) {
	if perPage <= 0 {
		perPage = 30
	}
	if page <= 0 {
		page = 1
	}
	args := []string{"log", "--no-color", "--pretty=format:" + commitFormat,
		"--skip=" + strconv.Itoa((page-1)*perPage), "-n", strconv.Itoa(perPage)}
	if sha != "" {
		args = append(args, sha)
	}
	out, err := b.run(args...)
	if err != nil {
		return nil, err
	}
	var commits []map[string]any
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		commits = append(commits, b.parseCommit(rec))
	}
	return commits, nil
}

// Commit returns a single commit by ref, or ok=false if it does not resolve.
func (b *Backend) Commit(ref string) (map[string]any, bool, error) {
	out, err := b.run("log", "--no-color", "-n", "1", "--pretty=format:"+commitFormat, ref)
	if err != nil {
		return nil, false, nil // unknown ref: let the caller proxy
	}
	rec := strings.Trim(out, "\n\x1e")
	if rec == "" {
		return nil, false, nil
	}
	return b.parseCommit(rec), true, nil
}

func (b *Backend) parseCommit(rec string) map[string]any {
	f := strings.Split(rec, "\x1f")
	get := func(i int) string {
		if i < len(f) {
			return f[i]
		}
		return ""
	}
	message := get(8)
	if body := get(9); body != "" {
		message += "\n\n" + body
	}
	var parents []map[string]any
	for _, p := range strings.Fields(get(7)) {
		parents = append(parents, map[string]any{"sha": p})
	}
	sha := get(0)
	return map[string]any{
		"sha": sha,
		"commit": map[string]any{
			"author":    map[string]any{"name": get(1), "email": get(2), "date": get(3)},
			"committer": map[string]any{"name": get(4), "email": get(5), "date": get(6)},
			"message":   message,
		},
		"parents": parents,
	}
}

// Contents returns the /repos/.../contents/{path} shape for ref (default HEAD).
// A file returns a single object with base64 content; a directory returns a
// listing array (returned as []map). ok=false means the path/ref is unknown.
func (b *Backend) Contents(path, ref string) (any, bool, error) {
	if ref == "" {
		ref = "HEAD"
	}
	path = strings.Trim(path, "/")
	objType := b.git("cat-file", "-t", ref+":"+path)
	switch objType {
	case "blob":
		raw, err := b.run("show", ref+":"+path)
		if err != nil {
			return nil, false, nil
		}
		name := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			name = path[i+1:]
		}
		return map[string]any{
			"type":     "file",
			"name":     name,
			"path":     path,
			"size":     len(raw),
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString([]byte(raw)),
		}, true, nil
	case "tree":
		out, err := b.run("ls-tree", "--full-tree", ref, ensureTrailingSlash(path))
		if err != nil {
			return nil, false, nil
		}
		var entries []map[string]any
		for _, line := range nonEmptyLines(out) {
			// <mode> <type> <object>\t<file>
			meta, file, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			parts := strings.Fields(meta)
			if len(parts) < 3 {
				continue
			}
			etype := "file"
			if parts[1] == "tree" {
				etype = "dir"
			}
			name := file
			if i := strings.LastIndex(file, "/"); i >= 0 {
				name = file[i+1:]
			}
			entries = append(entries, map[string]any{
				"type": etype, "name": name, "path": file, "sha": parts[2],
			})
		}
		return entries, true, nil
	}
	return nil, false, nil
}

func ensureTrailingSlash(p string) string {
	if p == "" {
		return ""
	}
	return p + "/"
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// Grep runs `git grep` for code search, returning matches as a slice of
// {path, line_number, line} maps. ref defaults to HEAD.
func (b *Backend) Grep(pattern, ref string) ([]map[string]any, error) {
	if ref == "" {
		ref = "HEAD"
	}
	out, err := b.run("grep", "-n", "-I", "--fixed-strings", pattern, ref)
	if err != nil {
		// git grep exits non-zero when there are no matches; treat as empty.
		return nil, nil
	}
	var results []map[string]any
	for _, line := range nonEmptyLines(out) {
		// ref:path:lineno:content
		rest := strings.TrimPrefix(line, ref+":")
		path, after, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		lineno, content, _ := strings.Cut(after, ":")
		n, _ := strconv.Atoi(lineno)
		results = append(results, map[string]any{
			"path": path, "line_number": n, "line": content,
		})
	}
	return results, nil
}
