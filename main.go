package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mevdschee/github-export/internal/config"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/sync"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <owner/repo> [output-dir]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nExports GitHub issues, PRs, releases, labels, and milestones\n")
		fmt.Fprintf(os.Stderr, "to a local directory in markdown format.\n")
		fmt.Fprintf(os.Stderr, "\nRuns incrementally on subsequent invocations.\n")
		fmt.Fprintf(os.Stderr, "\nRequires GITHUB_TOKEN environment variable.\n")
		fmt.Fprintf(os.Stderr, "  export GITHUB_TOKEN=$(gh auth token)\n")
		os.Exit(1)
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "GITHUB_TOKEN environment variable is required. Run:")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  export GITHUB_TOKEN=$(gh auth token)")
		fmt.Fprintln(os.Stderr, "")
		os.Exit(1)
	}

	var owner, repo, outDir string

	arg := os.Args[1]
	if strings.Contains(arg, "/") {
		parts := strings.SplitN(arg, "/", 2)
		owner = parts[0]
		repo = parts[1]
	}

	if len(os.Args) >= 3 {
		outDir = os.Args[2]
	} else {
		outDir = "github-data"
	}

	// Create output directories
	os.MkdirAll(filepath.Join(outDir, "issues"), 0755)
	os.MkdirAll(filepath.Join(outDir, "releases"), 0755)

	// Read existing config for incremental sync
	configPath := filepath.Join(outDir, "repo.yml")
	cfg, err := config.ReadRepoConfig(configPath)
	if err != nil {
		log.Fatalf("Reading repo.yml: %v", err)
	}
	if cfg != nil && owner == "" {
		owner = cfg.Owner
		repo = cfg.Repo
	}
	if owner == "" || repo == "" {
		log.Fatal("owner/repo must be specified as first argument (e.g., octocat/Hello-World)")
	}

	since := ""
	if cfg != nil {
		since = cfg.SyncedAt
	}
	if since != "" {
		log.Printf("Incremental sync since %s", since)
	} else {
		log.Println("Full sync (first run)")
	}

	syncStart := time.Now().UTC().Format(time.RFC3339)
	client := github.NewClient(token)

	// Sync all entities
	if err := sync.Labels(client, owner, repo, outDir); err != nil {
		log.Printf("Warning: %v", err)
	}
	if err := sync.Milestones(client, owner, repo, outDir); err != nil {
		log.Printf("Warning: %v", err)
	}
	issueProjects, projectEvents, err := sync.Projects(client, owner, repo, outDir, since)
	if err != nil {
		log.Printf("Warning: %v", err)
	}
	events, err := sync.Issues(client, owner, repo, outDir, since, issueProjects)
	if err != nil {
		log.Printf("Warning: %v", err)
	}
	events = append(events, projectEvents...)
	if err := sync.Releases(client, owner, repo, outDir); err != nil {
		log.Printf("Warning: %v", err)
	}
	if err := sync.Repo(client, owner, repo, outDir, syncStart); err != nil {
		log.Fatalf("Writing repo.yml: %v", err)
	}

	log.Printf("Done. synced_at=%s", syncStart)

	// Export events as markdown files for agents to pick up
	if len(events) > 0 {
		eventsDir := filepath.Join(outDir, "events")
		log.Printf("Exporting %d events to %s — %s", len(events), eventsDir, summarizeEvents(events))
		if err := hooks.Export(eventsDir, events); err != nil {
			log.Printf("Warning: exporting events: %v", err)
		}
	}
}

// summarizeEvents returns a stable "type=N, type=N, ..." breakdown of an event
// slice, sorted by descending count then ascending type name.
func summarizeEvents(events []hooks.Event) string {
	counts := map[string]int{}
	for _, ev := range events {
		counts[ev.Type]++
	}
	type kv struct {
		k string
		n int
	}
	pairs := make([]kv, 0, len(counts))
	for k, n := range counts {
		pairs = append(pairs, kv{k, n})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].n != pairs[j].n {
			return pairs[i].n > pairs[j].n
		}
		return pairs[i].k < pairs[j].k
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("%s=%d", p.k, p.n)
	}
	return strings.Join(parts, ", ")
}
