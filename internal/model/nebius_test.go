package model

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Transient failures (503, 429) are retried; the eventual tool-call reply is
// parsed and metered once.
func TestCompleteRetriesAndParsesToolCalls(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []Tool `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Tools) != 1 {
			t.Errorf("request carried %d tools, want 1", len(body.Tools))
		}
		switch hits.Add(1) {
		case 1:
			http.Error(w, "busy", http.StatusServiceUnavailable)
		case 2:
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]}}],
				"usage":{"prompt_tokens":11,"completion_tokens":4}}`))
		}
	}))
	defer srv.Close()

	c := NewClient("key", srv.URL, "m")
	c.RetryBase = time.Millisecond
	var metered []Usage
	c.OnUsage = func(_ string, u Usage) { metered = append(metered, u) }

	resp, err := c.Complete(t.Context(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []Tool{{Type: "function", Function: ToolFunction{Name: "search"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 3 {
		t.Errorf("%d attempts, want 3", hits.Load())
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "search" || resp.ToolCalls[0].Function.Arguments != `{"q":"x"}` {
		t.Errorf("tool calls parsed as %+v", resp.ToolCalls)
	}
	if len(metered) != 1 || metered[0].PromptTokens != 11 {
		t.Errorf("metered %+v, want one call with 11 prompt tokens", metered)
	}
}

func TestCompleteSendsOutputLimitOnlyWhenRequested(t *testing.T) {
	for _, limit := range []int{0, 16384} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				MaxTokens *int `json:"max_tokens"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if limit == 0 && body.MaxTokens != nil || limit > 0 && (body.MaxTokens == nil || *body.MaxTokens != limit) {
				t.Errorf("max_tokens = %v for limit %d", body.MaxTokens, limit)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}))
		c := NewClient("key", srv.URL, "m")
		_, err := c.Complete(t.Context(), Request{MaxTokens: limit})
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompleteDoesNotRetryClientErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "bad model", http.StatusBadRequest)
	}))
	defer srv.Close()
	c := NewClient("key", srv.URL, "m")
	c.RetryBase = time.Millisecond
	if _, err := c.Complete(t.Context(), Request{}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v, want a 400 error", err)
	}
	if hits.Load() != 1 {
		t.Errorf("%d attempts, want 1", hits.Load())
	}
}

// TestSmokeNebius calls the real API. It is skipped unless the key is set:
//
//	make smoke
func TestSmokeNebius(t *testing.T) {
	key := os.Getenv("NEBIUS_TOKEN_FACTORY")
	if key == "" {
		t.Skip("NEBIUS_TOKEN_FACTORY not set")
	}
	c := NewClient(key, os.Getenv("NEBIUS_BASE_URL"), os.Getenv("NEBIUS_MODEL"))

	reply, usage, err := c.Chat(t.Context(), []Message{{Role: "user", Content: "Reply with the single word: pong"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(reply) == "" || usage.PromptTokens == 0 {
		t.Fatalf("reply %q, usage %+v", reply, usage)
	}
	t.Logf("chat: %q (%+v)", reply, usage)

	// The executor depends on tool calling; check the wire format round-trips.
	resp, err := c.Complete(t.Context(), Request{
		Messages: []Message{{Role: "user", Content: "What is the weather in Paris? Use the tool."}},
		Tools: []Tool{{Type: "function", Function: ToolFunction{
			Name:        "get_weather",
			Description: "Current weather for a city",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) == 0 {
		t.Fatalf("model did not call the tool; replied %q", resp.Content)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(resp.ToolCalls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("tool arguments are not JSON: %q", resp.ToolCalls[0].Function.Arguments)
	}
	t.Logf("tool call: %s(%v)", resp.ToolCalls[0].Function.Name, args)
}
