package github

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
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

func (c *Client) GetPaginated(url string, headers map[string]string) ([]json.RawMessage, error) {
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

		url = ""
		if link := resp.Header.Get("Link"); link != "" {
			if m := linkNextRe.FindStringSubmatch(link); m != nil {
				url = m[1]
			}
		}
	}
	return all, nil
}
