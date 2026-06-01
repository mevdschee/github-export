package main

import (
	"os"
	"os/exec"
	"strings"
)

// resolveToken finds a GitHub API token without making the user export one by
// hand. It prefers $GITHUB_TOKEN and otherwise falls back to the GitHub CLI's
// stored credentials (`gh auth token`).
//
// On success it returns the token and a short note describing where it came
// from (empty when it came straight from the environment, so callers can stay
// quiet in the common case). When no token can be found it returns an empty
// token and an actionable hint that distinguishes "gh not installed" from "gh
// installed but not logged in", so the user knows exactly what to do next.
func resolveToken() (token, note string) {
	return resolveTokenFrom(os.Getenv, exec.LookPath, ghAuthToken)
}

// ghAuthToken shells out to `gh auth token`, returning the stored credential.
func ghAuthToken() (string, error) {
	out, err := exec.Command("gh", "auth", "token").Output()
	return strings.TrimSpace(string(out)), err
}

// resolveTokenFrom is the dependency-injected core of resolveToken, kept
// separate so the resolution logic can be tested without a real environment or
// a real gh binary.
func resolveTokenFrom(
	getenv func(string) string,
	lookPath func(string) (string, error),
	ghToken func() (string, error),
) (token, note string) {
	if t := strings.TrimSpace(getenv("GITHUB_TOKEN")); t != "" {
		return t, ""
	}
	if _, err := lookPath("gh"); err != nil {
		return "", "No GITHUB_TOKEN set and the GitHub CLI (gh) was not found.\n" +
			"Install it from https://cli.github.com and run 'gh auth login', or set the\n" +
			"token yourself:\n\n  export GITHUB_TOKEN=$(gh auth token)"
	}
	if t, err := ghToken(); err == nil {
		if t = strings.TrimSpace(t); t != "" {
			return t, "Using token from the GitHub CLI (gh auth token)"
		}
	}
	return "", "No GITHUB_TOKEN set and the GitHub CLI (gh) is not logged in.\n" +
		"Run 'gh auth login' first, or set the token yourself:\n\n" +
		"  export GITHUB_TOKEN=$(gh auth token)"
}
