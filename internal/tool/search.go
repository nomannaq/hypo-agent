package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	defaultResults = 5
	maxResults     = 10
	braveEndpoint  = "https://api.search.brave.com/res/v1/web/search"
)

// Searcher is where web_search looks: the Brave Search API when BraveKey is
// set, then the SearXNG instance at SearXNG. Cache, if set, keeps formatted
// results by normalized query so repeated queries skip both backends.
type Searcher struct {
	SearXNG  string
	BraveKey string
	BraveURL string // tests; empty means the public API
	Cache    func(ctx context.Context, key string, fetch func() (string, error)) (string, error)
}

// result is one search hit in the shape both backends are reduced to.
type result struct{ Title, URL, Snippet string }

// errNoResults lets an empty answer fall through to the next backend and
// keeps it out of the cache.
var errNoResults = errors.New("no results")

// WebSearch returns the web_search tool. The name, description and schema
// are part of the specs every workflow offers, so they stay unchanged.
func WebSearch(s Searcher) Tool {
	return Typed("web_search",
		"Search the web. Returns titles, URLs and snippets; open a result with browse to read it.",
		`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":10,"description":"Number of results (default 5)"}},"required":["query"]}`,
		func(ctx context.Context, in struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}) (string, error) {
			query := strings.Join(strings.Fields(in.Query), " ")
			if query == "" {
				return "", errors.New("web_search needs a query")
			}
			limit := in.Limit
			if limit <= 0 {
				limit = defaultResults
			}
			limit = min(limit, maxResults)
			fetch := func() (string, error) { return s.search(ctx, query, limit) }
			var out string
			var err error
			if s.Cache != nil {
				out, err = s.Cache(ctx, strconv.Itoa(limit)+"|"+strings.ToLower(query), fetch)
			} else {
				out, err = fetch()
			}
			if errors.Is(err, errNoResults) {
				return fmt.Sprintf("No results for %q.", query), nil
			}
			return out, err
		})
}

// search tries Brave, then SearXNG. It reports "no results" only when every
// backend answered and found nothing; any failure is reported instead.
func (s Searcher) search(ctx context.Context, query string, limit int) (string, error) {
	var failures []error
	if s.BraveKey != "" {
		results, err := s.brave(ctx, query, limit)
		if err == nil {
			return format(results, limit), nil
		}
		if !errors.Is(err, errNoResults) {
			failures = append(failures, fmt.Errorf("brave: %w", err))
		}
	}
	results, err := fromSearXNG(ctx, s.SearXNG, query)
	if err == nil {
		return format(results, limit), nil
	}
	if !errors.Is(err, errNoResults) {
		failures = append(failures, err)
	}
	if len(failures) == 0 {
		return "", errNoResults
	}
	return "", fmt.Errorf("search unavailable: %w", errors.Join(failures...))
}

func (s Searcher) brave(ctx context.Context, query string, limit int) ([]result, error) {
	endpoint := s.BraveURL
	if endpoint == "" {
		endpoint = braveEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		endpoint+"?"+url.Values{"q": {query}, "count": {strconv.Itoa(limit)}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", s.BraveKey)
	var body struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := getJSON(req, &body); err != nil {
		return nil, err
	}
	var results []result
	for _, r := range body.Web.Results {
		results = append(results, result{r.Title, r.URL, stripTags(r.Description)})
	}
	return nonEmpty(results)
}

func fromSearXNG(ctx context.Context, baseURL, query string) ([]result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/search?"+url.Values{"q": {query}, "format": {"json"}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var body struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := getJSON(req, &body); err != nil {
		return nil, fmt.Errorf("searxng: %w", err)
	}
	var results []result
	for _, r := range body.Results {
		results = append(results, result{r.Title, r.URL, r.Content})
	}
	return nonEmpty(results)
}

func getJSON(req *http.Request, into any) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("bad response: %w", err)
	}
	return nil
}

func nonEmpty(results []result) ([]result, error) {
	if len(results) == 0 {
		return nil, errNoResults
	}
	return results, nil
}

func format(results []result, limit int) string {
	var b strings.Builder
	for i, r := range results[:min(limit, len(results))] {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if s := strings.TrimSpace(r.Snippet); s != "" {
			fmt.Fprintf(&b, "   %s\n", s)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// Brave marks query terms in snippets with <strong>.
func stripTags(s string) string {
	return strings.NewReplacer("<strong>", "", "</strong>", "").Replace(s)
}
