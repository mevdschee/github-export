package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github-export/internal/config"
	"github-export/internal/github"
	"github-export/internal/hooks"
	"github-export/internal/jsonutil"
	"github-export/internal/sync"
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
		log.Fatal("GITHUB_TOKEN environment variable is required (try: export GITHUB_TOKEN=$(gh auth token))")
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

	syncStart := time.Now().UTC()
	client := github.NewClient(token)

	// Fetch repo metadata for default_branch
	defaultBranch := ""
	repoURL := fmt.Sprintf("%s/repos/%s/%s", github.API, owner, repo)
	if repoRaw, err := client.GetJSON(repoURL, nil); err != nil {
		log.Printf("Warning: fetching repo metadata: %v", err)
	} else {
		var repoData map[string]any
		json.Unmarshal(repoRaw, &repoData)
		defaultBranch = jsonutil.Str(repoData, "default_branch")
	}

	// Sync all entities
	if err := sync.Labels(client, owner, repo, outDir); err != nil {
		log.Printf("Warning: %v", err)
	}
	if err := sync.Milestones(client, owner, repo, outDir); err != nil {
		log.Printf("Warning: %v", err)
	}
	events, err := sync.Issues(client, owner, repo, outDir, since)
	if err != nil {
		log.Printf("Warning: %v", err)
	}
	if err := sync.Releases(client, owner, repo, outDir); err != nil {
		log.Printf("Warning: %v", err)
	}

	// Update repo.yml
	newCfg := &config.RepoConfig{
		Owner:         owner,
		Repo:          repo,
		DefaultBranch: defaultBranch,
		SyncedAt:      syncStart.Format(time.RFC3339),
	}
	if err := config.WriteRepoConfig(configPath, newCfg); err != nil {
		log.Fatalf("Writing repo.yml: %v", err)
	}

	log.Printf("Done. synced_at=%s", newCfg.SyncedAt)

	// Export events as markdown files for agents to pick up
	if len(events) > 0 {
		eventsDir := filepath.Join(outDir, "events")
		log.Printf("Exporting %d events to %s", len(events), eventsDir)
		if err := hooks.Export(eventsDir, events); err != nil {
			log.Printf("Warning: exporting events: %v", err)
		}
	}
}
