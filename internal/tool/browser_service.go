package tool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	browserCallTimeout = 110 * time.Second
	browserRequestMax  = 256 << 10
	browserReplyMax    = 24 << 20 // CDP frame limit plus JSON encoding
	pendingArtifact    = "[artifact:pending]"
)

type browserCall struct {
	RunID string          `json:"run_id"`
	Key   string          `json:"key"`
	Name  string          `json:"name"`
	Args  json.RawMessage `json:"args"`
}

type browserReply struct {
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
	PNG    []byte `json:"png,omitempty"`
}

type browserReceipt struct {
	name  string
	args  [sha256.Size]byte
	reply browserReply
}

type browserRun struct {
	gate        chan struct{}
	receipts    map[string]browserReceipt
	screenshot  string
	lastUsed    time.Time
	activeCalls int
	expiry      *time.Timer
}

// BrowserService owns the CDP connections. Temporal workers can come and go
// while this process keeps the exact open tab and its in-memory page state.
func BrowserService(cdpURL, token string) http.Handler {
	var mu sync.Mutex
	runs := map[string]*browserRun{}
	registry := sessionTools(cdpURL, token, func(ctx context.Context, _, _, _ string, data []byte) (string, error) {
		reply := ctx.Value(browserReplyKey{}).(*browserReply)
		reply.PNG = data
		return "pending", nil
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /call", func(w http.ResponseWriter, req *http.Request) {
		if token == "" || subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var call browserCall
		body := http.MaxBytesReader(w, req.Body, browserRequestMax)
		if err := json.NewDecoder(body).Decode(&call); err != nil ||
			call.RunID == "" || len(call.RunID) > 256 || len(call.Key) > 512 ||
			!strings.HasPrefix(call.Key, call.RunID+"/tool/") {
			http.Error(w, "invalid browser call", http.StatusBadRequest)
			return
		}

		mu.Lock()
		run := runs[call.RunID]
		if run == nil {
			run = &browserRun{gate: make(chan struct{}, 1), receipts: map[string]browserReceipt{}}
			runs[call.RunID] = run
		}
		run.activeCalls++
		mu.Unlock()
		defer func() {
			mu.Lock()
			run.activeCalls--
			run.lastUsed = time.Now()
			if run.expiry == nil {
				run.expiry = time.AfterFunc(sessionIdleTTL, func() {
					mu.Lock()
					defer mu.Unlock()
					if run.activeCalls == 0 && time.Since(run.lastUsed) >= sessionIdleTTL {
						delete(runs, call.RunID)
					}
				})
			} else {
				run.expiry.Reset(sessionIdleTTL)
			}
			mu.Unlock()
		}()

		select {
		case run.gate <- struct{}{}:
			defer func() { <-run.gate }()
		case <-req.Context().Done():
			return
		}
		if req.Context().Err() != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		hash := sha256.Sum256(call.Args)
		reply, exists := run.receipts[call.Key]
		if exists {
			if call.Name != reply.name || hash != reply.args {
				http.Error(w, "browser call key reused with different arguments", http.StatusConflict)
				return
			}
			_ = json.NewEncoder(w).Encode(reply.reply)
			return
		}

		// Finish and remember an accepted action if the worker disconnects
		// before receiving its result. A retry uses the same call key.
		var result browserReply
		ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), browserCallTimeout)
		defer cancel()
		ctx = WithCall(context.WithValue(ctx, browserReplyKey{}, &result), call.RunID, call.Key)
		var err error
		result.Output, err = registry.Call(ctx, call.Name, call.Args)
		if err != nil {
			result.Error = err.Error()
		}
		if len(result.PNG) > 0 {
			// Only the latest screenshot needs its bytes kept for retry.
			// Keep the older call's receipt so it cannot capture again.
			if previous, ok := run.receipts[run.screenshot]; ok {
				previous.reply = browserReply{Error: "This screenshot was superseded. Take a new screenshot if needed."}
				run.receipts[run.screenshot] = previous
			}
			run.screenshot = call.Key
		}
		run.receipts[call.Key] = browserReceipt{name: call.Name, args: hash, reply: result}
		_ = json.NewEncoder(w).Encode(result)
	})
	return mux
}

type browserReplyKey struct{}

// RemoteBrowserTools offers the existing tool definitions and runs them
// through the service. The workflow's catalog and activity names stay stable.
func RemoteBrowserTools(address, token string, save SaveArtifact) Registry {
	registry := sessionTools("", "", nil)
	for i, spec := range registry {
		registry[i].Run = func(ctx context.Context, args json.RawMessage) (string, error) {
			id, key := RunID(ctx), CallKey(ctx)
			input, err := json.Marshal(browserCall{RunID: id, Key: key, Name: spec.Name, Args: args})
			if err != nil {
				return "", err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(address, "/")+"/call", bytes.NewReader(input))
			if err != nil {
				return "", err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return "", fmt.Errorf("browser session service unavailable: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return "", fmt.Errorf("browser session service: %s", resp.Status)
			}
			var result browserReply
			if err := json.NewDecoder(io.LimitReader(resp.Body, browserReplyMax)).Decode(&result); err != nil {
				return "", fmt.Errorf("reading browser session result: %w", err)
			}
			if result.Error != "" {
				return "", errors.New(result.Error)
			}
			if len(result.PNG) > 0 {
				artifact, err := save(ctx, id, key, "image/png", result.PNG)
				if err != nil {
					return "", fmt.Errorf("saving the screenshot: %w", err)
				}
				result.Output = strings.TrimSuffix(result.Output, pendingArtifact) + "[artifact:" + artifact + "]"
			}
			return result.Output, nil
		}
	}
	return registry
}

func sessionTools(cdpURL, token string, save SaveArtifact) Registry {
	return Registry{
		BrowserOpen(cdpURL, token), BrowserSnapshot(), BrowserClick(), BrowserType(),
		BrowserSelect(), BrowserPress(), BrowserScroll(), BrowserBack(), BrowserWait(), BrowserScreenshot(save),
	}
}
