package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mevdschee/github-export/internal/gitbackend"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/graphqlmirror"
	"github.com/mevdschee/github-export/internal/httpapi"
	"github.com/mevdschee/github-export/internal/query"
	"github.com/mevdschee/github-export/internal/shadow"
	"github.com/mevdschee/github-export/internal/store"
	"github.com/mevdschee/github-export/internal/sync"
	"github.com/mevdschee/github-export/internal/writeproxy"
)

// projectRepoSlug is where shadow-compare files parity-gap issues.
const projectRepoSlug = "mevdschee/github-export"

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "address to bind (localhost only; no client auth)")
	dbPath := fs.String("db", defaultDB, "path to the SQLite database to serve")
	proxyMode := fs.String("proxy", "on", "proxy fallback to api.github.com for unmirrored reads and writes: on|off")
	repoPath := fs.String("repo-path", ".", "local git clone for content endpoints (files/commits/branches); empty to disable")
	gitFetch := fs.Bool("git-fetch", false, "git fetch before serving content endpoints")
	autoSync := fs.Duration("auto-sync", 0, "if set, run an incremental sync on this interval (e.g. 15m)")
	debugCompare := fs.Bool("debug-compare", false, "answer reads locally AND remotely, diff, and log divergences (doubles request cost)")
	compareFile := fs.Bool("debug-compare-file", false, "with --debug-compare, file a deduplicated issue on "+projectRepoSlug+" per divergence")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s serve [flags]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Serves the SQLite store over a GitHub-compatible HTTP API. Mirrored reads\n")
		fmt.Fprintf(os.Stderr, "are answered locally; unmirrored reads and all writes proxy to GitHub.\n")
		fmt.Fprintf(os.Stderr, "Binds localhost and trusts local callers (no client token needed).\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	s, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("opening database %s: %v", *dbPath, err)
	}
	defer s.Close()

	owner, repo, err := s.OwnerRepo()
	if err != nil || owner == "" || repo == "" {
		log.Fatalf("database %s has no synced repo; run 'github-export sync' first", *dbPath)
	}
	syncedAt, _ := s.SyncedAt()

	token := os.Getenv("GITHUB_TOKEN")
	var client *github.Client
	if token != "" {
		client = github.NewClient(token)
	}

	proxy := writeproxy.New(token, func(kind string, number int64) error {
		if client == nil {
			return fmt.Errorf("no GITHUB_TOKEN: cannot re-sync after write")
		}
		if kind == "issue" && number > 0 {
			return sync.ResyncIssue(client, s, owner, repo, number)
		}
		return nil // collection writes are picked up by the next full sync
	})
	if *proxyMode == "off" {
		proxy.Disabled = true
	} else if token == "" {
		log.Println("Warning: GITHUB_TOKEN not set; proxy fallback and writes will fail (mirrored reads still work)")
	}

	var git *gitbackend.Backend
	if *repoPath != "" {
		git = gitbackend.New(*repoPath, owner, repo, *gitFetch)
		if !git.Available() {
			log.Printf("Note: %s is not a git work tree; content endpoints disabled", *repoPath)
			git = nil
		} else if *gitFetch {
			if err := git.Fetch(); err != nil {
				log.Printf("Warning: git fetch: %v", err)
			}
		}
	}

	var comparator *shadow.Comparator
	if *debugCompare {
		if client == nil {
			log.Println("Warning: --debug-compare needs GITHUB_TOKEN for the remote leg; disabling")
		} else {
			fetch := func(ctx context.Context, path string) (int, []byte, error) {
				return proxy.Request(ctx, "GET", path, nil)
			}
			var fileIssue func(title, body string) error
			if *compareFile {
				fileIssue = func(title, body string) error {
					payload := fmt.Sprintf(`{"title":%q,"body":%q,"labels":["shadow-compare"]}`, title, body)
					status, resp, err := proxy.Request(context.Background(), "POST",
						"/repos/"+projectRepoSlug+"/issues", strings.NewReader(payload))
					if err != nil {
						return err
					}
					if status >= 400 {
						return fmt.Errorf("filing issue: %d %s", status, string(resp))
					}
					return nil
				}
			}
			comparator = shadow.New(fetch, fileIssue, projectRepoSlug)
			log.Printf("Shadow-compare enabled (file-issues=%v)", *compareFile)
		}
	}

	q := query.New(s)
	srv := httpapi.New(httpapi.Config{
		Query: q, Proxy: proxy, Git: git, Compare: comparator,
		GraphQL: graphqlmirror.New(q, owner, repo),
		Owner:   owner, Repo: repo, SyncedAt: syncedAt,
	})

	if *autoSync > 0 && client != nil {
		go autoSyncLoop(client, s, owner, repo, *autoSync)
	}

	httpServer := &http.Server{Addr: *addr, Handler: srv.Handler()}
	go func() {
		log.Printf("Serving %s/%s from %s on http://%s", owner, repo, *dbPath, *addr)
		log.Printf("  API docs:  http://%s/docs", *addr)
		log.Printf("  Status:    http://%s/status", *addr)
		log.Printf("  Point gh:  GH_HOST=%s gh issue list", *addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)
}

// autoSyncLoop runs an incremental sync on a fixed interval.
func autoSyncLoop(client *github.Client, s *store.Store, owner, repo string, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for range ticker.C {
		since, _ := s.SyncedAt()
		start := time.Now().UTC().Format(time.RFC3339)
		if _, err := sync.Run(client, s, owner, repo, since, start); err != nil {
			log.Printf("auto-sync: %v", err)
		} else {
			log.Printf("auto-sync complete at %s", start)
		}
	}
}
