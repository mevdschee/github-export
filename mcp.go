package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mevdschee/github-export/internal/gitbackend"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/mcp"
	"github.com/mevdschee/github-export/internal/query"
	"github.com/mevdschee/github-export/internal/store"
	"github.com/mevdschee/github-export/internal/sync"
	"github.com/mevdschee/github-export/internal/writeproxy"
)

func runMCP(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB, "path to the SQLite database")
	readOnly := fs.Bool("read-only", false, "register read tools only (no write/proxy tools)")
	proxyMode := fs.String("proxy", "on", "proxy fallback for writes: on|off")
	repoPath := fs.String("repo-path", ".", "local git clone for content/code tools")
	httpAddr := fs.String("http", "", "serve the streamable-HTTP transport on this address instead of stdio (e.g. localhost:8081)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s mcp [flags]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Runs a Model Context Protocol server over stdio (default) whose tool names\n")
		fmt.Fprintf(os.Stderr, "match GitHub's official MCP server. Reads come from the local store/git;\n")
		fmt.Fprintf(os.Stderr, "writes proxy to GitHub and re-sync.\n\n")
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
		if client != nil && kind == "issue" && number > 0 {
			return sync.ResyncIssue(client, s, owner, repo, number)
		}
		return nil
	})
	if *proxyMode == "off" {
		proxy.Disabled = true
	}

	var git *gitbackend.Backend
	if *repoPath != "" {
		git = gitbackend.New(*repoPath, owner, repo, false)
		if !git.Available() {
			git = nil
		}
	}

	srv := mcp.NewServer(mcp.Deps{
		Query: query.New(s), Git: git, Proxy: proxy,
		Owner: owner, Repo: repo, SyncedAt: syncedAt, ReadOnly: *readOnly,
	})

	ctx := context.Background()
	if *httpAddr != "" {
		handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
		log.Printf("MCP streamable-HTTP server on http://%s", *httpAddr)
		if err := http.ListenAndServe(*httpAddr, handler); err != nil {
			log.Fatalf("mcp http: %v", err)
		}
		return
	}

	if err := srv.Run(ctx, &mcpsdk.StdioTransport{}); err != nil {
		log.Fatalf("mcp: %v", err)
	}
}
