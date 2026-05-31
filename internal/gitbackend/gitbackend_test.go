package gitbackend

import (
	"encoding/base64"
	"strings"
	"testing"
)

// These tests run against the github-export repo itself (tests execute inside
// the package directory, which is within the work tree).

func newBackend() *Backend { return New(".", "mevdschee", "github-export", false) }

func TestAvailable(t *testing.T) {
	if !newBackend().Available() {
		t.Skip("not in a git work tree")
	}
}

func TestBranches(t *testing.T) {
	b := newBackend()
	if !b.Available() {
		t.Skip("no git")
	}
	branches, err := b.Branches()
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) == 0 {
		t.Fatal("expected at least one branch")
	}
	if _, ok := branches[0]["commit"].(map[string]any); !ok {
		t.Errorf("branch missing commit object: %v", branches[0])
	}
}

func TestCommits(t *testing.T) {
	b := newBackend()
	if !b.Available() {
		t.Skip("no git")
	}
	commits, err := b.Commits("", 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) == 0 {
		t.Fatal("expected commits")
	}
	c := commits[0]
	if c["sha"] == "" {
		t.Errorf("commit missing sha: %v", c)
	}
	commit, _ := c["commit"].(map[string]any)
	if commit == nil || commit["message"] == "" {
		t.Errorf("commit missing message: %v", c)
	}
}

func TestContentsFile(t *testing.T) {
	b := newBackend()
	if !b.Available() {
		t.Skip("no git")
	}
	doc, ok, err := b.Contents("go.mod", "")
	if err != nil || !ok {
		t.Fatalf("Contents go.mod ok=%v err=%v", ok, err)
	}
	m := doc.(map[string]any)
	if m["type"] != "file" || m["encoding"] != "base64" {
		t.Errorf("unexpected file shape: %v", m)
	}
	raw, err := base64.StdEncoding.DecodeString(m["content"].(string))
	if err != nil || !strings.Contains(string(raw), "module github.com/mevdschee/github-export") {
		t.Errorf("decoded content wrong: err=%v", err)
	}
}

func TestContentsDir(t *testing.T) {
	b := newBackend()
	if !b.Available() {
		t.Skip("no git")
	}
	doc, ok, err := b.Contents("internal", "")
	if err != nil || !ok {
		t.Fatalf("Contents internal ok=%v err=%v", ok, err)
	}
	entries, isList := doc.([]map[string]any)
	if !isList || len(entries) == 0 {
		t.Fatalf("expected directory listing, got %T", doc)
	}
	foundDir := false
	for _, e := range entries {
		if e["type"] == "dir" {
			foundDir = true
		}
	}
	if !foundDir {
		t.Errorf("expected at least one subdir in internal/: %v", entries)
	}
}

func TestContentsMiss(t *testing.T) {
	b := newBackend()
	if !b.Available() {
		t.Skip("no git")
	}
	if _, ok, _ := b.Contents("does/not/exist.xyz", ""); ok {
		t.Error("expected miss for nonexistent path")
	}
}
