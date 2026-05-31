package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mevdschee/github-export/internal/exporter"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/hooks"
	"github.com/mevdschee/github-export/internal/store"
	"github.com/mevdschee/github-export/internal/sync"
)

const defaultDB = "github.sqlite"

func main() {
	log.SetFlags(0)

	args := os.Args[1:]
	sub := "sync"
	switch {
	case len(args) > 0 && isSubcommand(args[0]):
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "export":
		runExport(args)
	case "serve":
		runServe(args)
	case "api":
		runAPI(args)
	case "mcp":
		runMCP(args)
	case "issue", "pr", "search", "release":
		runNative(sub, args)
	default:
		runSync(args)
	}
}

func isSubcommand(s string) bool {
	switch s {
	case "sync", "export", "serve", "api", "mcp", "issue", "pr", "search", "release":
		return true
	}
	return false
}

func runSync(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	maxAge := fs.String("max-age", "",
		"only fetch issues/PRs/projects updated within this window (e.g. 2y, 6mo, 4w, 30d, 12h); useful for very large repos on first sync")
	dbPath := fs.String("db", defaultDB, "path to the SQLite database (the primary store)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s sync [flags] [owner/repo]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Syncs GitHub issues, PRs, discussions, projects, releases, labels, and\n")
		fmt.Fprintf(os.Stderr, "milestones into a local SQLite database. Runs incrementally on later runs.\n\n")
		fmt.Fprintf(os.Stderr, "owner/repo is resolved from (1) the argument, (2) the existing database,\n")
		fmt.Fprintf(os.Stderr, "or (3) the origin git remote of the current directory.\n\n")
		fmt.Fprintf(os.Stderr, "Run 'github-export export' afterwards to render markdown files.\n\n")
		fmt.Fprintf(os.Stderr, "Requires GITHUB_TOKEN:\n  export GITHUB_TOKEN=$(gh auth token)\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "GITHUB_TOKEN environment variable is required. Run:")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  export GITHUB_TOKEN=$(gh auth token)")
		fmt.Fprintln(os.Stderr, "")
		os.Exit(1)
	}

	cutoff := ""
	if *maxAge != "" {
		t, err := parseMaxAge(*maxAge)
		if err != nil {
			log.Fatalf("--max-age: %v", err)
		}
		cutoff = t.UTC().Format(time.RFC3339)
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("opening database %s: %v", *dbPath, err)
	}
	defer s.Close()

	owner, repo := resolveOwnerRepo(fs.Arg(0), s)
	if owner == "" || repo == "" {
		log.Fatalf("could not determine owner/repo; pass it (e.g. octocat/Hello-World), run from an existing database, or from a directory whose origin remote points at GitHub")
	}

	since, err := s.SyncedAt()
	if err != nil {
		log.Fatalf("reading sync state: %v", err)
	}
	// --max-age is a floor: if it is more recent than synced_at (or there is no
	// synced_at yet), the cutoff drives the next fetch.
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

	events, err := sync.Run(client, s, owner, repo, since, syncStart)
	if err != nil {
		log.Fatalf("sync: %v", err)
	}

	log.Printf("Done. synced_at=%s, db=%s", syncStart, *dbPath)
	if len(events) > 0 {
		log.Printf("Recorded %d events — %s", len(events), summarizeEvents(events))
		log.Printf("Run 'github-export export' to write them to the events/ folder.")
	}
}

func runExport(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB, "path to the SQLite database to export from")
	out := fs.String("out", "github-data", "output directory for the markdown export")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s export [flags]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Renders the SQLite store to markdown files. One-way: reads the database\n")
		fmt.Fprintf(os.Stderr, "and writes files; it never reads the markdown back.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	if _, err := os.Stat(*dbPath); err != nil {
		log.Fatalf("database %s not found; run 'github-export sync' first", *dbPath)
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("opening database %s: %v", *dbPath, err)
	}
	defer s.Close()

	if err := exporter.Export(s, *out); err != nil {
		log.Fatalf("export: %v", err)
	}
	log.Printf("Exported %s to %s", *dbPath, *out)
}

// resolveOwnerRepo determines the target repo from, in order: the positional
// argument, the existing database, and the origin git remote.
func resolveOwnerRepo(arg string, s *store.Store) (owner, repo string) {
	if strings.Contains(arg, "/") {
		parts := strings.SplitN(arg, "/", 2)
		return parts[0], parts[1]
	}
	if o, r, err := s.OwnerRepo(); err == nil && o != "" && r != "" {
		return o, r
	}
	if o, r, ok := detectRepoFromGit(); ok {
		log.Printf("Detected %s/%s from git remote", o, r)
		return o, r
	}
	return "", ""
}

var gitRemoteRe = regexp.MustCompile(`github\.com[:/]([^/]+)/(.+?)(?:\.git)?$`)

// detectRepoFromGit parses owner/repo out of the origin remote URL.
func detectRepoFromGit() (owner, repo string, ok bool) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return "", "", false
	}
	m := gitRemoteRe.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

var maxAgeRe = regexp.MustCompile(`^(\d+)(h|d|w|mo|y)$`)

// parseMaxAge turns a human age like "2y", "6mo", "4w", "30d", "12h" into the
// UTC timestamp that age ago. Months are taken as 30 days and years as 365
// days — the cutoff is an approximate floor, not a calendar boundary.
func parseMaxAge(str string) (time.Time, error) {
	m := maxAgeRe.FindStringSubmatch(str)
	if m == nil {
		return time.Time{}, fmt.Errorf("must be Nh, Nd, Nw, Nmo or Ny (got %q)", str)
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

// summarizeEvents returns a stable "type=N, ..." breakdown sorted by descending
// count then ascending type name.
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
