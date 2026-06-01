package main

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveTokenFrom(t *testing.T) {
	const ghToken = "gho_fromcli"
	notFound := func(string) (string, error) { return "", errors.New("not found") }
	found := func(string) (string, error) { return "/usr/bin/gh", nil }
	ghOK := func() (string, error) { return ghToken, nil }
	ghErr := func() (string, error) { return "", errors.New("not logged in") }
	noEnv := func(string) string { return "" }

	t.Run("env wins", func(t *testing.T) {
		env := func(k string) string {
			if k == "GITHUB_TOKEN" {
				return "  env_token  " // surrounding space is trimmed
			}
			return ""
		}
		// gh would error, but the env var means we never consult it.
		tok, note := resolveTokenFrom(env, found, ghErr)
		if tok != "env_token" {
			t.Errorf("token = %q, want env_token", tok)
		}
		if note != "" {
			t.Errorf("note = %q, want empty for env-sourced token", note)
		}
	})

	t.Run("falls back to gh", func(t *testing.T) {
		tok, note := resolveTokenFrom(noEnv, found, ghOK)
		if tok != ghToken {
			t.Errorf("token = %q, want %q", tok, ghToken)
		}
		if note == "" {
			t.Error("expected a note indicating the token came from gh")
		}
	})

	t.Run("gh missing", func(t *testing.T) {
		tok, note := resolveTokenFrom(noEnv, notFound, ghOK)
		if tok != "" {
			t.Errorf("token = %q, want empty when gh is missing", tok)
		}
		if !strings.Contains(note, "was not found") {
			t.Errorf("hint should explain gh is missing, got %q", note)
		}
	})

	t.Run("gh not logged in", func(t *testing.T) {
		tok, note := resolveTokenFrom(noEnv, found, ghErr)
		if tok != "" {
			t.Errorf("token = %q, want empty when gh is not logged in", tok)
		}
		if !strings.Contains(note, "not logged in") {
			t.Errorf("hint should explain gh is not logged in, got %q", note)
		}
	})

	t.Run("gh returns empty token", func(t *testing.T) {
		ghEmpty := func() (string, error) { return "   ", nil }
		tok, note := resolveTokenFrom(noEnv, found, ghEmpty)
		if tok != "" {
			t.Errorf("token = %q, want empty when gh yields nothing", tok)
		}
		if !strings.Contains(note, "not logged in") {
			t.Errorf("hint should treat empty token as not logged in, got %q", note)
		}
	})
}
