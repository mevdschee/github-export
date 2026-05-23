package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mevdschee/github-export/internal/config"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/sync"
)

func main() {
	maxAge := flag.String("max-age", "",
		"only fetch issues/PRs/projects updated within this window (e.g. 2y, 6mo, 4w, 30d, 12h); useful for very large repos on first sync")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] [owner/repo] [output-dir]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Exports GitHub issues, PRs, releases, labels, and milestones\n")
		fmt.Fprintf(os.Stderr, "to a local directory in markdown format.\n\n")
		fmt.Fprintf(os.Stderr, "Runs incrementally on subsequent invocations.\n\n")
		fmt.Fprintf(os.Stderr, "If invoked with no arguments, the current directory is treated\n")
		fmt.Fprintf(os.Stderr, "as an existing export and owner/repo are read from ./repo.yml.\n\n")
		fmt.Fprintf(os.Stderr, "Requires GITHUB_TOKEN environment variable.\n")
		fmt.Fprintf(os.Stderr, "  export GITHUB_TOKEN=$(gh auth token)\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "GITHUB_TOKEN environment variable is required. Run:")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  export GITHUB_TOKEN=$(gh auth token)")
		fmt.Fprintln(os.Stderr, "")
		os.Exit(1)
	}

	var owner, repo, outDir string
	switch {
	case len(args) == 0:
		// Deduce mode: current directory must already hold a repo.yml from
		// a previous sync. owner/repo are filled in from cfg below.
		outDir = "."
	case len(args) == 1:
		if strings.Contains(args[0], "/") {
			parts := strings.SplitN(args[0], "/", 2)
			owner = parts[0]
			repo = parts[1]
		}
		outDir = "github-data"
	default:
		if strings.Contains(args[0], "/") {
			parts := strings.SplitN(args[0], "/", 2)
			owner = parts[0]
			repo = parts[1]
		}
		outDir = args[1]
	}

	cutoff := ""
	if *maxAge != "" {
		t, err := parseMaxAge(*maxAge)
		if err != nil {
			log.Fatalf("--max-age: %v", err)
		}
		cutoff = t.UTC().Format(time.RFC3339)
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
		if len(args) == 0 {
			log.Fatalf("no owner/repo given and no repo.yml found in %s; pass owner/repo (e.g., octocat/Hello-World) or run from a directory that already has an export", outDir)
		}
		log.Fatal("owner/repo must be specified as first argument (e.g., octocat/Hello-World)")
	}

	since := ""
	if cfg != nil {
		since = cfg.SyncedAt
	}
	// Treat --max-age as a floor: if it's more recent than synced_at (or
	// synced_at is empty), the cutoff drives the next fetch.
	if cutoff != "" && cutoff > since {
		since = cutoff
	}
	switch {
	case since != "" && cutoff == since:
		log.Printf("First sync limited to items updated since %s (--max-age=%s)", since, *maxAge)
	case since != "":
		log.Printf("Incremental sync since %s", since)
	default:
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
	releaseEvents, err := sync.Releases(client, owner, repo, outDir)
	if err != nil {
		log.Printf("Warning: %v", err)
	}
	events = append(events, releaseEvents...)
	discussionEvents, err := sync.Discussions(client, owner, repo, outDir, since)
	if err != nil {
		log.Printf("Warning: %v", err)
	}
	events = append(events, discussionEvents...)
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

var maxAgeRe = regexp.MustCompile(`^(\d+)(h|d|w|mo|y)$`)

// parseMaxAge turns a human age like "2y", "6mo", "4w", "30d", "12h" into the
// UTC timestamp that age ago. Months are taken as 30 days and years as 365
// days — the cutoff is an approximate floor, not a calendar boundary.
func parseMaxAge(s string) (time.Time, error) {
	m := maxAgeRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("must be Nh, Nd, Nw, Nmo or Ny (got %q)", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return time.Time{}, fmt.Errorf("must be a positive integer (got %q)", m[1])
	}
	var d time.Duration
	switch m[2] {
	case "h":
		d = time.Duration(n) * time.Hour
	case "d":
		d = time.Duration(n) * 24 * time.Hour
	case "w":
		d = time.Duration(n) * 7 * 24 * time.Hour
	case "mo":
		d = time.Duration(n) * 30 * 24 * time.Hour
	case "y":
		d = time.Duration(n) * 365 * 24 * time.Hour
	}
	return time.Now().Add(-d), nil
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
