// Package model is the model gateway: the Nebius client plus retry, rate
// limiting, and usage metering. Called from inside Step bodies in
// internal/executor so every model call is journaled exactly like a tool
// call.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultBaseURL = "https://api.tokenfactory.nebius.com/v1"
	DefaultModel   = "deepseek-ai/DeepSeek-V4.1-Flash-1017"
)

// Message is the OpenAI-compatible chat message Nebius speaks. Assistant
// messages may carry ToolCalls; tool messages answer one via ToolCallID.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // a JSON object, encoded as a string
}

// Tool describes a function the model may call.
type Tool struct {
	Type     string       `json:"type"` // always "function"
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type Request struct {
	Messages  []Message
	Tools     []Tool
	MaxTokens int `json:",omitempty"`
}

type Response struct {
	Content   string
	ToolCalls []ToolCall
	Usage     Usage
}

type Client struct {
	APIKey  string
	BaseURL string
	Model   string
	HTTP    *http.Client

	// MaxRetries is how many times a failed call is retried (network errors,
	// 429 and 5xx). RetryBase is the first backoff; it doubles each attempt.
	MaxRetries int
	RetryBase  time.Duration

	// OnUsage, if set, is called once per successful call: the metering hook.
	OnUsage func(model string, u Usage)
}

func NewClient(apiKey, baseURL, model string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if model == "" {
		model = DefaultModel
	}
	return &Client{
		APIKey:     apiKey,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Model:      model,
		HTTP:       &http.Client{Timeout: 2 * time.Minute},
		MaxRetries: 3,
		RetryBase:  500 * time.Millisecond,
	}
}

// Chat sends the conversation and returns the reply text. It is Complete
// without tools, kept for callers that only want text.
func (c *Client) Chat(ctx context.Context, messages []Message) (string, Usage, error) {
	resp, err := c.Complete(ctx, Request{Messages: messages})
	return resp.Content, resp.Usage, err
}

// Complete runs one chat completion, retrying transient failures.
func (c *Client) Complete(ctx context.Context, req Request) (Response, error) {
	if c.APIKey == "" {
		return Response{}, errors.New("model: NEBIUS_TOKEN_FACTORY is not set")
	}
	payload := map[string]any{"model": c.Model, "messages": req.Messages}
	if len(req.Tools) > 0 {
		payload["tools"] = req.Tools
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, err
	}

	for attempt := 0; ; attempt++ {
		resp, retryAfter, err := c.do(ctx, body)
		if err == nil {
			if c.OnUsage != nil {
				c.OnUsage(c.Model, resp.Usage)
			}
			return resp, nil
		}
		var re *retryable
		if !errors.As(err, &re) || attempt >= c.MaxRetries {
			return Response{}, err
		}
		wait := retryAfter
		if wait == 0 {
			// Exponential backoff with full jitter, capped at 8s.
			wait = time.Duration(rand.Int64N(int64(min(c.RetryBase<<attempt, 8*time.Second)) + 1))
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
	}
}

// retryable marks errors worth another attempt.
type retryable struct{ err error }

func (r *retryable) Error() string { return r.err.Error() }
func (r *retryable) Unwrap() error { return r.err }

func (c *Client) do(ctx context.Context, body []byte) (Response, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, 0, ctx.Err()
		}
		return Response{}, 0, &retryable{err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Response{}, 0, &retryable{err}
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("model: nebius returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return Response{}, retryAfter(resp.Header.Get("Retry-After")), &retryable{err}
		}
		return Response{}, 0, err
	}
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, 0, fmt.Errorf("model: decoding response: %w", err)
	}
	if len(out.Choices) == 0 {
		return Response{}, 0, errors.New("model: empty response")
	}
	m := out.Choices[0].Message
	return Response{Content: m.Content, ToolCalls: m.ToolCalls, Usage: out.Usage}, 0, nil
}

// retryAfter reads a Retry-After header given in seconds, capped at 30s.
func retryAfter(h string) time.Duration {
	s, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || s <= 0 {
		return 0
	}
	return min(time.Duration(s)*time.Second, 30*time.Second)
}
