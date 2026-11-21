package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	defaultResults = 5
	maxResults     = 10
)

// WebSearch returns the web_search tool, backed by a SearXNG instance's JSON
// API at baseURL.
func WebSearch(baseURL string) Tool {
	return Typed("web_search",
		"Search the web. Returns titles, URLs and snippets; open a result with browse to read it.",
		`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":10,"description":"Number of results (default 5)"}},"required":["query"]}`,
		func(ctx context.Context, in struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}) (string, error) {
			if strings.TrimSpace(in.Query) == "" {
				return "", errors.New("web_search needs a query")
			}
			limit := in.Limit
			if limit <= 0 {
				limit = defaultResults
			}
			limit = min(limit, maxResults)
			return search(ctx, baseURL, in.Query, limit)
		})
}

func search(ctx context.Context, baseURL, query string, limit int) (string, error) {
	u := strings.TrimRight(baseURL, "/") + "/search?" + url.Values{"q": {query}, "format": {"json"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("search unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("search unavailable: %s", resp.Status)
	}
	var body struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("search: bad response: %w", err)
	}
	if len(body.Results) == 0 {
		return fmt.Sprintf("No results for %q.", query), nil
	}
	var b strings.Builder
	for i, r := range body.Results[:min(limit, len(body.Results))] {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if s := strings.TrimSpace(r.Content); s != "" {
			fmt.Fprintf(&b, "   %s\n", s)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
