package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/mevdschee/github-export/internal/query"
	"github.com/mevdschee/github-export/internal/store"
)

// runNative implements the offline read subcommands (issue/pr/search/release)
// over internal/query. They mirror common `gh` reads for use without `gh` or a
// running server. Use --json for raw GitHub-shaped JSON.
func runNative(sub string, args []string) {
	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	dbPath := fs.String("db", defaultDB, "path to the SQLite database")
	asJSON := fs.Bool("json", false, "emit raw GitHub-shaped JSON")
	state := fs.String("state", "open", "filter by state: open|closed|all")
	limit := fs.Int("limit", 30, "maximum number of results")
	// Allow flags and positionals to be interspersed (e.g. `issue list --state all`,
	// `issue view 1 --json`), which Go's flag package does not do on its own.
	positional := parseInterspersed(fs, args)

	s, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("opening database %s: %v", *dbPath, err)
	}
	defer s.Close()
	q := query.New(s)

	rest := positional
	action := "list"
	if len(rest) > 0 {
		action = rest[0]
		rest = rest[1:]
	}

	switch sub {
	case "issue":
		nativeIssueOrPR(q, false, action, rest, *state, *limit, *asJSON)
	case "pr":
		nativeIssueOrPR(q, true, action, rest, *state, *limit, *asJSON)
	case "release":
		nativeReleases(q, *asJSON)
	case "search":
		nativeSearch(q, positional, *limit, *asJSON)
	}
}

// parseInterspersed parses flags from args while collecting positional
// arguments, allowing the two to be mixed in any order (unlike flag.Parse,
// which stops at the first positional).
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return positional
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func nativeIssueOrPR(q *query.Query, isPR bool, action string, rest []string, state string, limit int, asJSON bool) {
	switch action {
	case "view":
		if len(rest) == 0 {
			log.Fatalf("usage: view <number>")
		}
		n, _ := strconv.ParseInt(rest[0], 10, 64)
		var doc json.RawMessage
		var ok bool
		var err error
		if isPR {
			doc, ok, err = q.GetPull(n)
		} else {
			doc, ok, err = q.GetIssue(n)
		}
		if err != nil {
			log.Fatal(err)
		}
		if !ok {
			log.Fatalf("#%d not found in local store", n)
		}
		if asJSON {
			os.Stdout.Write(doc)
			fmt.Println()
			return
		}
		printIssueDetail(doc)
	default: // list
		o := query.ListIssuesOpts{State: state, PerPage: limit, Page: 1, OnlyPulls: isPR, OnlyIssues: !isPR}
		items, _, err := q.ListIssues(o)
		if err != nil {
			log.Fatal(err)
		}
		if asJSON {
			emitJSONArray(items)
			return
		}
		printIssueTable(items)
	}
}

func nativeReleases(q *query.Query, asJSON bool) {
	items, err := q.ListReleases()
	if err != nil {
		log.Fatal(err)
	}
	if asJSON {
		emitJSONArray(items)
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TAG\tNAME\tPUBLISHED")
	for _, it := range items {
		var m map[string]any
		json.Unmarshal(it, &m)
		fmt.Fprintf(tw, "%s\t%s\t%s\n", s(m, "tag_name"), s(m, "name"), s(m, "published_at"))
	}
	tw.Flush()
}

func nativeSearch(q *query.Query, args []string, limit int, asJSON bool) {
	queryStr := ""
	for _, a := range args {
		queryStr += a + " "
	}
	items, total, err := q.SearchIssues(queryStr, limit, 1)
	if err != nil {
		log.Fatal(err)
	}
	if asJSON {
		emitJSONArray(items)
		return
	}
	fmt.Printf("Found %d result(s):\n", total)
	printIssueTable(items)
}

func printIssueTable(items []json.RawMessage) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NUMBER\tSTATE\tTITLE\tAUTHOR")
	for _, it := range items {
		var m map[string]any
		json.Unmarshal(it, &m)
		author := ""
		if u, ok := m["user"].(map[string]any); ok {
			author, _ = u["login"].(string)
		}
		fmt.Fprintf(tw, "#%s\t%s\t%s\t%s\n", numStr(m), s(m, "state"), truncate(s(m, "title"), 60), author)
	}
	tw.Flush()
}

func printIssueDetail(doc json.RawMessage) {
	var m map[string]any
	json.Unmarshal(doc, &m)
	author := ""
	if u, ok := m["user"].(map[string]any); ok {
		author, _ = u["login"].(string)
	}
	fmt.Printf("#%s  %s\n", numStr(m), s(m, "title"))
	fmt.Printf("State: %s   Author: %s   Created: %s\n\n", s(m, "state"), author, s(m, "created_at"))
	fmt.Println(s(m, "body"))
}

func emitJSONArray(items []json.RawMessage) {
	if items == nil {
		items = []json.RawMessage{}
	}
	b, _ := json.MarshalIndent(items, "", "  ")
	os.Stdout.Write(b)
	fmt.Println()
}

func s(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

func numStr(m map[string]any) string {
	if f, ok := m["number"].(float64); ok {
		return strconv.FormatInt(int64(f), 10)
	}
	return ""
}

func truncate(str string, n int) string {
	if len(str) <= n {
		return str
	}
	return str[:n-1] + "…"
}
