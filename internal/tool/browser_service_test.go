package tool

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestBrowserServiceKeepsTabAndDeduplicatesActions(t *testing.T) {
	clicked, finishClick := make(chan struct{}), make(chan struct{})
	var clicks, connections atomic.Int32
	cdp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/version" {
			_ = json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": "ws://" + r.Host + "/cdp"})
			return
		}
		websocket.Handler(func(ws *websocket.Conn) {
			connections.Add(1)
			url := "https://example.test/form"
			for {
				var cmd struct {
					ID      int
					Method  string
					Session string `json:"sessionId"`
					Params  struct{ Expression string }
				}
				if websocket.JSON.Receive(ws, &cmd) != nil {
					return
				}
				result := map[string]any{}
				switch cmd.Method {
				case "Target.createBrowserContext":
					result["browserContextId"] = "context"
				case "Target.createTarget":
					result["targetId"] = "page"
				case "Target.attachToTarget":
					result["sessionId"] = "session"
				case "Runtime.evaluate":
					var value any = map[string]string{"Title": "Form", "URL": url}
					switch {
					case strings.Contains(cmd.Params.Expression, "el.click()"):
						if clicks.Add(1) == 1 {
							close(clicked)
							<-finishClick
						}
						url = "https://example.test/submitted"
						value = map[string]any{"Found": true, "Name": "Submit"}
					case strings.Contains(cmd.Params.Expression, "const out = []"):
						value = []map[string]any{{"Index": 0, "Name": "Submit", "Tag": "button"}}
					}
					result["result"] = map[string]any{"value": value}
				case "LP.getMarkdown":
					result["markdown"] = "Form contents"
				case "Page.captureScreenshot":
					result["data"] = "cG5n"
				}
				if err := websocket.JSON.Send(ws, map[string]any{"id": cmd.ID, "result": result}); err != nil {
					return
				}
				if cmd.Method == "Page.navigate" {
					_ = websocket.JSON.Send(ws, map[string]any{"sessionId": cmd.Session, "method": "Page.loadEventFired"})
				}
			}
		}).ServeHTTP(w, r)
	}))
	defer cdp.Close()
	server := httptest.NewServer(BrowserService(cdp.URL, "secret"))
	defer server.Close()

	workerA := RemoteBrowserTools(server.URL, "secret", nil)
	workerB := RemoteBrowserTools(server.URL, "secret", func(_ context.Context, id, key, mime string, png []byte) (string, error) {
		if mime != "image/png" || string(png) != "png" || !strings.HasPrefix(key, id+"/tool/") {
			t.Errorf("invalid screenshot receipt for %s", key)
		}
		return "saved", nil
	})
	runID := "recovery-" + rand.Text()
	defer func() {
		sessions.Lock()
		bs := sessions.m[runID]
		sessions.Unlock()
		if bs != nil {
			closeSession(runID, bs)
		}
	}()
	ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()

	inRun := func(ctx context.Context, step string) context.Context {
		return WithCall(ctx, runID, runID+"/tool/"+step)
	}
	if _, err := workerA.Call(inRun(ctx, "open"), "browser_open", json.RawMessage(`{"url":"https://example.test/form"}`)); err != nil {
		t.Fatal(err)
	}
	workerCtx, workerLost := context.WithCancel(ctx)
	defer workerLost()
	args := json.RawMessage(`{"element_index":0}`)
	first := make(chan error, 1)
	go func() {
		_, err := workerA.Call(inRun(workerCtx, "click"), "browser_click", args)
		first <- err
	}()
	select {
	case <-clicked:
	case <-ctx.Done():
		t.Fatal("click never reached the browser")
	}
	workerLost()
	close(finishClick)
	if err := <-first; err == nil {
		t.Fatal("disconnected worker received the click result")
	}
	out, err := workerB.Call(inRun(ctx, "click"), "browser_click", args)
	if err != nil || !strings.Contains(out, "/submitted") || clicks.Load() != 1 {
		t.Fatalf("retried click = %q, %v, clicks=%d", out, err, clicks.Load())
	}
	if _, err := workerB.Call(inRun(ctx, "click"), "browser_click", json.RawMessage(`{"element_index":1}`)); err == nil {
		t.Fatal("changed arguments reused a completed key")
	}
	out, err = workerB.Call(inRun(ctx, "snapshot"), "browser_snapshot", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out, "/submitted") || connections.Load() != 1 {
		t.Fatalf("snapshot = %q, %v, CDP connections=%d", out, err, connections.Load())
	}
	out, err = workerB.Call(inRun(ctx, "screenshot"), "browser_screenshot", json.RawMessage(`{}`))
	if err != nil || !strings.HasSuffix(out, "[artifact:saved]") {
		t.Fatalf("screenshot = %q, %v", out, err)
	}
	if _, err := workerB.Call(WithCall(ctx, "another-run", "another-run/tool/snapshot"), "browser_snapshot", json.RawMessage(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "no page is open") {
		t.Fatalf("another run could address the page: %v", err)
	}
	unauthenticated := RemoteBrowserTools(server.URL, "wrong", nil)
	if _, err := unauthenticated.Call(inRun(ctx, "unauthorized"), "browser_snapshot", json.RawMessage(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Fatalf("incorrect token accepted: %v", err)
	}
}
