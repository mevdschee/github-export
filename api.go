package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/mevdschee/github-export/internal/gitbackend"
	"github.com/mevdschee/github-export/internal/github"
	"github.com/mevdschee/github-export/internal/graphqlmirror"
	"github.com/mevdschee/github-export/internal/httpapi"
	"github.com/mevdschee/github-export/internal/query"
	"github.com/mevdschee/github-export/internal/store"
	"github.com/mevdschee/github-export/internal/sync"
	"github.com/mevdschee/github-export/internal/writeproxy"
)

// runAPI is a `gh api`-style passthrough: it answers GET reads from the local
// store/git when mirrored and proxies everything else to GitHub. It reuses the
// exact serve handler in-process, so behaviour matches `serve`.
func runAPI(args []string) {
	fs := flag.NewFlagSet("api", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB, "path to the SQLite database")
	method := fs.String("method", "GET", "HTTP method (or pass --method shorthand -X)")
	input := fs.String("input", "", "request body file ('-' for stdin)")
	proxyMode := fs.String("proxy", "on", "proxy fallback: on|off")
	repoPath := fs.String("repo-path", ".", "local git clone for content endpoints")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s api [flags] <path>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Makes a single GitHub-API request, served locally when mirrored and proxied\n")
		fmt.Fprintf(os.Stderr, "otherwise. Example: %s api /repos/OWNER/REPO/issues?state=all\n\n", os.Args[0])
		fs.PrintDefaults()
	}
	fs.Parse(args)

	path := fs.Arg(0)
	if path == "" {
		fs.Usage()
		os.Exit(2)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	var body io.Reader
	if *input == "-" {
		body = os.Stdin
		if *method == "GET" {
			*method = "POST"
		}
	} else if *input != "" {
		f, err := os.Open(*input)
		if err != nil {
			log.Fatalf("opening --input %s: %v", *input, err)
		}
		defer f.Close()
		body = f
		if *method == "GET" {
			*method = "POST"
		}
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("opening database %s: %v", *dbPath, err)
	}
	defer s.Close()
	owner, repo, _ := s.OwnerRepo()
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

	q := query.New(s)
	srv := httpapi.New(httpapi.Config{
		Query: q, Proxy: proxy, Git: git,
		GraphQL: graphqlmirror.New(q, owner, repo),
		Owner:   owner, Repo: repo, SyncedAt: syncedAt,
	})

	req := httptest.NewRequest(*method, path, body)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	res := rec.Result()
	io.Copy(os.Stdout, res.Body)
	if res.StatusCode >= 400 {
		os.Exit(1)
	}
}
