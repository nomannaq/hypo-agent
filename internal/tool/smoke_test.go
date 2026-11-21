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

// httpbin's demo form posts to /post, which echoes the submitted fields back:
// a safe target for a real submission.
func TestSmokeForms(t *testing.T) {
	base, token := os.Getenv("OBSCURA_URL"), os.Getenv("OBSCURA_CDP_TOKEN")
	if base == "" || token == "" {
		t.Skip("OBSCURA_URL or OBSCURA_CDP_TOKEN not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	forms, err := InspectForms(base, token).Run(ctx, json.RawMessage(`{"url":"https://httpbin.org/forms/post"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(forms)
	if !strings.Contains(forms, "Form 0: POST") || !strings.Contains(forms, "custname") {
		t.Errorf("custname field not listed in %q", forms)
	}

	submit := SubmitForm(base, token)
	if submit.Effect != Write {
		t.Fatal("submit_form must be a Write tool")
	}
	out, err := submit.Run(ctx, json.RawMessage(`{"url":"https://httpbin.org/forms/post","fields":{"custname":"Hypo Agent","comments":"smoke test"}}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(out)
	if !strings.Contains(out, "Hypo Agent") || !strings.Contains(out, "httpbin.org/post") {
		t.Errorf("submitted values not echoed back: %q", out)
	}

	if _, err := submit.Run(ctx, json.RawMessage(`{"url":"https://httpbin.org/forms/post","fields":{"nope":"x"}}`)); err == nil || !strings.Contains(err.Error(), "no field named nope") {
		t.Errorf("unknown field: err = %v", err)
	}
}
