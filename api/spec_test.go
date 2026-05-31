package api

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// openapiDoc is a minimal view of the structure we assert on. It intentionally
// avoids a heavyweight OpenAPI library to keep the project's dependency set
// small; it validates the invariants that matter for our hand-written spec.
type openapiDoc struct {
	OpenAPI string `yaml:"openapi"`
	Info    struct {
		Title   string `yaml:"title"`
		Version string `yaml:"version"`
	} `yaml:"info"`
	Paths map[string]map[string]struct {
		Responses map[string]any `yaml:"responses"`
	} `yaml:"paths"`
}

func loadSpec(t *testing.T) openapiDoc {
	t.Helper()
	var doc openapiDoc
	if err := yaml.Unmarshal(OpenAPISpec, &doc); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}
	return doc
}

func TestSpecIsOpenAPI31(t *testing.T) {
	doc := loadSpec(t)
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("openapi version = %q, want 3.1.x", doc.OpenAPI)
	}
	if doc.Info.Title == "" || doc.Info.Version == "" {
		t.Errorf("info.title/version must be set: %+v", doc.Info)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("spec declares no paths")
	}
}

func TestEveryPathHasAnOperationWithResponses(t *testing.T) {
	doc := loadSpec(t)
	methods := map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true}
	for path, item := range doc.Paths {
		ops := 0
		for method, op := range item {
			if !methods[method] {
				continue
			}
			ops++
			if len(op.Responses) == 0 {
				t.Errorf("%s %s declares no responses", strings.ToUpper(method), path)
			}
		}
		if ops == 0 {
			t.Errorf("path %s has no HTTP operations", path)
		}
	}
}

// TestDocumentedMirrorPathsPresent guards against the spec drifting away from
// the endpoints the server actually mirrors. If a handler path is renamed, this
// list and the spec must be updated together.
func TestDocumentedMirrorPathsPresent(t *testing.T) {
	doc := loadSpec(t)
	want := []string{
		"/repos/{owner}/{repo}",
		"/repos/{owner}/{repo}/issues",
		"/repos/{owner}/{repo}/issues/{number}",
		"/repos/{owner}/{repo}/issues/{number}/comments",
		"/repos/{owner}/{repo}/pulls",
		"/repos/{owner}/{repo}/pulls/{number}",
		"/repos/{owner}/{repo}/pulls/{number}/reviews",
		"/repos/{owner}/{repo}/labels",
		"/repos/{owner}/{repo}/releases",
		"/repos/{owner}/{repo}/commits",
		"/repos/{owner}/{repo}/contents/{path}",
		"/search/issues",
		"/status",
	}
	for _, p := range want {
		if _, ok := doc.Paths[p]; !ok {
			t.Errorf("OpenAPI spec is missing documented path %q", p)
		}
	}
}
