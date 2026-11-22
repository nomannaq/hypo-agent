package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistry(t *testing.T) {
	echo := Typed("echo", "echoes", `{"type":"object"}`, func(_ context.Context, in struct{ S string }) (string, error) {
		return in.S, nil
	})
	r := Registry{echo, WebSearch(Searcher{SearXNG: "http://unused"})}

	specs := r.Specs()
	if len(specs) != 2 || specs[0].Function.Name != "echo" || specs[1].Function.Name != "web_search" || specs[0].Type != "function" {
		t.Fatalf("specs = %+v", specs)
	}
	if !json.Valid(specs[1].Function.Parameters) {
		t.Errorf("web_search schema is not valid JSON")
	}
	if out, err := r.Call(t.Context(), "echo", json.RawMessage(`{"S":"hi"}`)); err != nil || out != "hi" {
		t.Errorf("echo = %q, %v", out, err)
	}
	if _, err := r.Call(t.Context(), "nope", nil); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("unknown tool: err = %v", err)
	}
	if _, err := r.Call(t.Context(), "echo", json.RawMessage(`[1]`)); err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("bad args: err = %v", err)
	}
}

// searxng fakes SearXNG's JSON API with n results.
func searxng(t *testing.T, n int) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("format") != "json" || r.URL.Query().Get("q") == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var res []map[string]string
		for i := range n {
			res = append(res, map[string]string{"title": fmt.Sprintf("T%d", i+1), "url": fmt.Sprintf("https://e.com/%d", i+1), "content": "snippet"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": res})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWebSearch(t *testing.T) {
	run := func(base, args string) (string, error) {
		return WebSearch(Searcher{SearXNG: base}).Run(t.Context(), json.RawMessage(args))
	}
	srv := searxng(t, 20)

	out, err := run(srv.URL, `{"query":"caffeine"}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "1. T1\n   https://e.com/1\n   snippet\n"; !strings.HasPrefix(out, want) {
		t.Errorf("output starts %q, want %q", out, want)
	}
	if strings.Count(out, "https://") != defaultResults {
		t.Errorf("default limit: %d results, want %d", strings.Count(out, "https://"), defaultResults)
	}
	out, _ = run(srv.URL, `{"query":"caffeine","limit":50}`)
	if got := strings.Count(out, "https://"); got != maxResults {
		t.Errorf("limit 50: %d results, want the cap %d", got, maxResults)
	}
	if out, err := run(searxng(t, 0).URL, `{"query":"zzz"}`); err != nil || !strings.HasPrefix(out, "No results") {
		t.Errorf("empty: %q, %v", out, err)
	}
	if _, err := run(srv.URL, `{"query":"  "}`); err == nil {
		t.Error("blank query accepted")
	}
}

func TestBrowseRejectsNonWebURLs(t *testing.T) {
	b := Browse("http://127.0.0.1:1", "t") // never reached
	for _, u := range []string{"file:///etc/passwd", "javascript:alert(1)", "ftp://x.org", "example.com", ""} {
		if _, err := b.Run(t.Context(), json.RawMessage(`{"url":"`+u+`"}`)); err == nil || !strings.Contains(err.Error(), "http or https") {
			t.Errorf("%q: err = %v", u, err)
		}
	}
}

// Browser session tools never trust a model-supplied run: without one from
// the executor they refuse, and with one but no open page they say so.
func TestBrowserSessionNeedsARunAndAPage(t *testing.T) {
	if _, err := BrowserSnapshot().Run(t.Context(), json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "inside a run") {
		t.Errorf("no run: err = %v", err)
	}
	ctx := WithCall(t.Context(), "r-none", "r-none/tool/0/c1")
	if _, err := BrowserClick().Run(ctx, json.RawMessage(`{"element_index":0}`)); err == nil || !strings.Contains(err.Error(), "browser_open first") {
		t.Errorf("no page: err = %v", err)
	}
	// A run_id in the arguments is ignored, not used to find a session.
	if _, err := BrowserSnapshot().Run(t.Context(), json.RawMessage(`{"run_id":"someone-else"}`)); err == nil || !strings.Contains(err.Error(), "inside a run") {
		t.Errorf("model-supplied run_id: err = %v", err)
	}
}

func TestWebSearchFallsBackAndCaches(t *testing.T) {
	var braveCalls int
	brave := func(status, n int) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			braveCalls++
			if r.Header.Get("X-Subscription-Token") != "key" || r.URL.Query().Get("q") == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if status != http.StatusOK {
				http.Error(w, "limited", status)
				return
			}
			var res []map[string]string
			for i := range n {
				res = append(res, map[string]string{"title": fmt.Sprintf("B%d", i+1), "url": fmt.Sprintf("https://b.com/%d", i+1), "description": "<strong>brave</strong> hit"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"web": map[string]any{"results": res}})
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	run := func(s Searcher, query string) (string, error) {
		return WebSearch(s).Run(t.Context(), json.RawMessage(`{"query":`+fmt.Sprintf("%q", query)+`}`))
	}

	// Brave answers first when a key is set; its markup is stripped.
	if out, err := run(Searcher{SearXNG: searxng(t, 3).URL, BraveKey: "key", BraveURL: brave(200, 2).URL}, "q"); err != nil ||
		!strings.HasPrefix(out, "1. B1\n   https://b.com/1\n   brave hit") {
		t.Fatalf("brave = %q, %v", out, err)
	}
	// A rate-limited or empty Brave falls back to SearXNG.
	for _, b := range []*httptest.Server{brave(http.StatusTooManyRequests, 0), brave(200, 0)} {
		if out, err := run(Searcher{SearXNG: searxng(t, 3).URL, BraveKey: "key", BraveURL: b.URL}, "q"); err != nil || !strings.HasPrefix(out, "1. T1") {
			t.Fatalf("fallback = %q, %v", out, err)
		}
	}
	// Both empty is "no results"; one failing reports the failure.
	if out, err := run(Searcher{SearXNG: searxng(t, 0).URL, BraveKey: "key", BraveURL: brave(200, 0).URL}, "q"); err != nil || !strings.HasPrefix(out, "No results") {
		t.Fatalf("both empty = %q, %v", out, err)
	}
	if _, err := run(Searcher{SearXNG: searxng(t, 0).URL, BraveKey: "key", BraveURL: brave(http.StatusTooManyRequests, 0).URL}, "q"); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("a failed backend must be reported: %v", err)
	}

	// The cache keys on the normalized query and never stores empty answers.
	cache := map[string]string{}
	var keys []string
	s := Searcher{SearXNG: searxng(t, 3).URL, BraveKey: "key", BraveURL: brave(200, 2).URL,
		Cache: func(_ context.Context, key string, fetch func() (string, error)) (string, error) {
			keys = append(keys, key)
			if out, ok := cache[key]; ok {
				return out, nil
			}
			out, err := fetch()
			if err == nil {
				cache[key] = out
			}
			return out, err
		}}
	braveCalls = 0
	first, _ := run(s, "CDP  ROI")
	second, _ := run(s, " cdp roi ")
	if first != second || braveCalls != 1 || keys[0] != "5|cdp roi" || keys[1] != keys[0] {
		t.Fatalf("cache: calls=%d keys=%q", braveCalls, keys)
	}
	s.SearXNG, s.BraveURL = searxng(t, 0).URL, brave(200, 0).URL
	if out, _ := run(s, "nothing"); !strings.HasPrefix(out, "No results") || len(cache) != 1 {
		t.Fatalf("empty answer cached: %q, %v", out, cache)
	}
}
