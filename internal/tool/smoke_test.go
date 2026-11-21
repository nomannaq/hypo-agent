package tool

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Live checks against the compose services (`make up`): run with `make smoke`.
// They skip unless the service's address is set.

func TestSmokeSearch(t *testing.T) {
	base := os.Getenv("SEARXNG_URL")
	if base == "" {
		t.Skip("SEARXNG_URL not set")
	}
	out, err := WebSearch(base).Run(t.Context(), json.RawMessage(`{"query":"caffeine short-term memory","limit":3}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(out)
	if !strings.HasPrefix(out, "1. ") || !strings.Contains(out, "http") {
		t.Errorf("no results in %q", out)
	}
}

func TestSmokeBrowse(t *testing.T) {
	base, token := os.Getenv("OBSCURA_URL"), os.Getenv("OBSCURA_CDP_TOKEN")
	if base == "" || token == "" {
		t.Skip("OBSCURA_URL or OBSCURA_CDP_TOKEN not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	browse := Browse(base, token)

	out, err := browse.Run(ctx, json.RawMessage(`{"url":"https://example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(out)
	if !strings.Contains(out, "Example Domain") {
		t.Errorf("page content missing from %q", out)
	}

	// Obscura's SSRF guard: private and loopback addresses are refused.
	for _, u := range []string{"http://127.0.0.1:8080", "http://db:5432"} {
		if out, err := browse.Run(ctx, json.RawMessage(`{"url":"`+u+`"}`)); err == nil {
			t.Errorf("browse %s succeeded: %q", u, out)
		} else {
			t.Logf("browse %s: %v", u, err)
		}
	}
}

// The session tools walk a real flow on httpbin's demo form, which posts to
// /post and echoes the fields back: open, snapshot, type, click submit, read
// the result, go back, scroll, wait, screenshot.
func TestSmokeBrowserSession(t *testing.T) {
	base, token := os.Getenv("OBSCURA_URL"), os.Getenv("OBSCURA_CDP_TOKEN")
	if base == "" || token == "" {
		t.Skip("OBSCURA_URL or OBSCURA_CDP_TOKEN not set")
	}
	ctx, cancel := context.WithTimeout(WithCall(t.Context(), "smoke-"+t.Name(), "smoke/1"), 90*time.Second)
	defer cancel()
	run := func(tl Tool, args string) string {
		t.Helper()
		out, err := tl.Run(ctx, json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s %s: %v", tl.Name, args, err)
		}
		t.Logf("%s %s:\n%s", tl.Name, args, out)
		return out
	}
	index := func(snapshot, contains string) string {
		for _, line := range strings.Split(snapshot, "\n") {
			if strings.HasPrefix(line, "[") && strings.Contains(line, contains) {
				return line[1:strings.Index(line, "]")]
			}
		}
		t.Fatalf("no element %q in snapshot", contains)
		return ""
	}

	run(BrowserOpen(base, token), `{"url":"https://httpbin.org/forms/post"}`)
	snap := run(BrowserSnapshot(), `{}`)
	run(BrowserType(), `{"element_index":`+index(snap, "Customer name")+`,"text":"Hypo Agent"}`)
	if snap = run(BrowserSnapshot(), `{}`); !strings.Contains(snap, `value="Hypo Agent"`) {
		t.Errorf("typed value not in snapshot")
	}
	if out := run(BrowserClick(), `{"element_index":`+index(snap, "Submit order")+`}`); !strings.Contains(out, "httpbin.org/post") {
		t.Errorf("click didn't submit: %q", out)
	}
	if snap = run(BrowserSnapshot(), `{}`); !strings.Contains(snap, "Hypo Agent") {
		t.Errorf("submitted value not echoed")
	}
	if out := run(BrowserBack(), `{}`); !strings.Contains(out, "forms/post") {
		t.Errorf("back didn't return to the form: %q", out)
	}
	run(BrowserScroll(), `{"direction":"bottom"}`)
	if out := run(BrowserWait(), `{"text":"Delivery instructions","seconds":3}`); !strings.Contains(out, "is on the page") {
		t.Errorf("wait: %q", out)
	}
	var saved []byte
	out := run(BrowserScreenshot(func(_ context.Context, runID, key, mime string, data []byte) (string, error) {
		saved = data
		return "a1", nil
	}), `{}`)
	if !strings.Contains(out, "[artifact:a1]") || len(saved) < 100 || string(saved[1:4]) != "PNG" {
		t.Errorf("screenshot: %q, %d bytes", out, len(saved))
	}
	if _, err := BrowserClick().Run(ctx, json.RawMessage(`{"element_index":999}`)); err == nil || !strings.Contains(err.Error(), "new browser_snapshot") {
		t.Errorf("stale element: err = %v", err)
	}
}
