package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	API     = "https://api.github.com"
	PerPage = 100
)

type Client struct {
	token  string
	client *http.Client
}

func NewClient(token string) *Client {
	return &Client{
		token:  token,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

var linkNextRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func apiError(url string, status int, body []byte) error {
	msg := fmt.Sprintf("GET %s: %d %s", url, status, string(body))
	if status == 401 || status == 403 {
		msg += "\n\nYour token may be expired or lack the required scopes. Run:\n\n  export GITHUB_TOKEN=$(gh auth token)"
	} else if status == 404 {
		msg += "\n\nRepository not found. Check the owner/repo name and that your token has access."
	}
	return fmt.Errorf("%s", msg)
}

func (c *Client) do(url string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}

	// Rate limit handling
	if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining != "" {
		if rem, err := strconv.Atoi(remaining); err == nil && rem < 100 {
			if resetStr := resp.Header.Get("X-RateLimit-Reset"); resetStr != "" {
				if resetUnix, err := strconv.ParseInt(resetStr, 10, 64); err == nil {
					sleepDur := time.Until(time.Unix(resetUnix, 0))
					if sleepDur > 0 {
						log.Printf("Rate limit low (%d remaining), sleeping %s", rem, sleepDur.Round(time.Second))
						time.Sleep(sleepDur + time.Second)
					}
				}
			}
		}
	}

	return resp, nil
}

func (c *Client) GetJSON(url string, headers map[string]string) (json.RawMessage, error) {
	resp, err := c.do(url, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, apiError(url, resp.StatusCode, body)
	}
	data, err := io.ReadAll(resp.Body)
	return json.RawMessage(data), err
}

// GraphQL posts a query to the GitHub GraphQL endpoint. Variables may be nil.
// Returns the contents of the "data" field. If the response contains "errors",
// returns them joined as a single error (along with any partial data).
func (c *Client) GraphQL(query string, variables map[string]any) (json.RawMessage, error) {
	const endpoint = "https://api.github.com/graphql"

	payload, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Rate limit handling (GraphQL uses the same headers)
	if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining != "" {
		if rem, err := strconv.Atoi(remaining); err == nil && rem < 100 {
			if resetStr := resp.Header.Get("X-RateLimit-Reset"); resetStr != "" {
				if resetUnix, err := strconv.ParseInt(resetStr, 10, 64); err == nil {
					sleepDur := time.Until(time.Unix(resetUnix, 0))
					if sleepDur > 0 {
						log.Printf("GraphQL rate limit low (%d remaining), sleeping %s", rem, sleepDur.Round(time.Second))
						time.Sleep(sleepDur + time.Second)
					}
				}
			}
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, apiError(endpoint, resp.StatusCode, body)
	}

	var result struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Path    []any  `json:"path"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing graphql response: %w", err)
	}
	if len(result.Errors) > 0 {
		var msgs []string
		for _, e := range result.Errors {
			msgs = append(msgs, e.Message)
		}
		return result.Data, fmt.Errorf("graphql: %s", strings.Join(msgs, "; "))
	}
	return result.Data, nil
}

func (c *Client) GetPaginated(url string, headers map[string]string) ([]json.RawMessage, error) {
	return c.GetPaginatedUntil(url, headers, nil)
}

// GetPaginatedUntil is like GetPaginated but stops requesting further pages
// once stop(currentPage) returns true. The current page is included in the
// result. Pass nil to fetch all pages.
func (c *Client) GetPaginatedUntil(url string, headers map[string]string, stop func([]json.RawMessage) bool) ([]json.RawMessage, error) {
	var all []json.RawMessage
	for url != "" {
		resp, err := c.do(url, headers)
		if err != nil {
			return all, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return all, apiError(url, resp.StatusCode, body)
		}
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil {
			return all, fmt.Errorf("parsing response from %s: %w", url, err)
		}
		all = append(all, items...)

		if stop != nil && stop(items) {
			return all, nil
		}

		url = ""
		if link := resp.Header.Get("Link"); link != "" {
			if m := linkNextRe.FindStringSubmatch(link); m != nil {
				url = m[1]
			}
		}
	}
	return all, nil
}
