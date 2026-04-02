package hooks

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	IssueCreated   = "issue_created"
	PRCreated      = "pr_created"
	IssueClosed    = "issue_closed"
	IssueReopened  = "issue_reopened"
	PRMerged       = "pr_merged"
	PRClosed       = "pr_closed"
	CommentCreated = "comment_created"
)

type Event struct {
	Type   string
	Number int64
	Title  string
	Author string
	State  string
	Labels []string
	File   string // absolute path to the issue/PR markdown file
	Repo   string // "owner/repo"
}

// Hook is a parsed markdown template file from the hooks/ directory.
type Hook struct {
	Event    string // event type from frontmatter
	Template string // markdown body with {{issue.*}} placeholders
	Path     string // source file path (for logging)
}

// LoadHooks reads all .md files from the hooks directory.
// Each file has YAML frontmatter with an "event" field and a markdown body
// containing {{issue.number}}, {{issue.title}}, etc. placeholders.
// Returns an empty slice if the directory doesn't exist.
func LoadHooks(dir string) ([]Hook, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var hooks []Hook
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		hook, err := parseHookFile(path)
		if err != nil {
			log.Printf("Warning: skipping %s: %v", path, err)
			continue
		}
		hooks = append(hooks, hook)
	}
	return hooks, nil
}

// parseHookFile reads a markdown file with YAML frontmatter.
// Expects:
//
//	---
//	event: issue_created
//	---
//	<template body>
func parseHookFile(path string) (Hook, error) {
	f, err := os.Open(path)
	if err != nil {
		return Hook{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)

	// Expect opening ---
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return Hook{}, fmt.Errorf("missing frontmatter")
	}

	// Read frontmatter lines
	event := ""
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
	}
	if event == "" {
		return Hook{}, fmt.Errorf("missing event field in frontmatter")
	}

	// Read body
	var body strings.Builder
	for scanner.Scan() {
		if body.Len() > 0 {
			body.WriteByte('\n')
		}
		body.WriteString(scanner.Text())
	}

	return Hook{
		Event:    event,
		Template: strings.TrimSpace(body.String()),
		Path:     path,
	}, nil
}

// render replaces {{issue.*}} placeholders in the template with event values.
func render(template string, ev Event) string {
	absFile := ev.File
	if abs, err := filepath.Abs(ev.File); err == nil {
		absFile = abs
	}

	r := strings.NewReplacer(
		"{{issue.number}}", fmt.Sprintf("%d", ev.Number),
		"{{issue.title}}", ev.Title,
		"{{issue.author}}", ev.Author,
		"{{issue.state}}", ev.State,
		"{{issue.labels}}", strings.Join(ev.Labels, ", "),
		"{{issue.file}}", absFile,
		"{{issue.repo}}", ev.Repo,
		"{{issue.url}}", fmt.Sprintf("https://github.com/%s/issues/%d", ev.Repo, ev.Number),
		"{{event.type}}", ev.Type,
	)
	return r.Replace(template)
}

// Run executes matching hooks for each event. Each hook's rendered markdown
// body is passed to "claude -p" with the issue file attached via --file.
func Run(hooks []Hook, events []Event, dryRun bool) {
	if len(hooks) == 0 || len(events) == 0 {
		return
	}

	for _, ev := range events {
		for _, hook := range hooks {
			if hook.Event != ev.Type {
				continue
			}

			prompt := render(hook.Template, ev)

			absFile := ev.File
			if abs, err := filepath.Abs(ev.File); err == nil {
				absFile = abs
			}

			if dryRun {
				log.Printf("  [dry-run] %s #%d (%s):\n%s", ev.Type, ev.Number, filepath.Base(hook.Path), prompt)
				continue
			}

			log.Printf("  hook %s #%d (%s)", ev.Type, ev.Number, filepath.Base(hook.Path))

			cmd := exec.Command("claude", "-p", prompt, "--file", absFile)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			if err := cmd.Run(); err != nil {
				log.Printf("  hook failed: %v", err)
			}
		}
	}
}
