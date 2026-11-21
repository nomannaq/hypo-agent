package tool

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

// The browser tools drive Obscura, a headless browser speaking the Chrome
// DevTools Protocol at cdpURL. browse (here) is stateless: each call opens its
// page in a fresh browser context and disposes of it, so it is safe to repeat
// and to run from research subagents. The browser_* tools (session.go) keep a
// page open across calls within a run.
//
// Obscura refuses private and loopback addresses (its SSRF guard), so pages
// can't reach the database or Temporal next to it.

// Browse returns the browse tool: the rendered page as Markdown.
func Browse(cdpURL, token string) Tool {
	return Typed("browse",
		"Read a web page: open it in a fresh headless browser (JavaScript runs) and return its content as Markdown. Use it to read pages found with web_search. To click, type or follow a multi-step flow, use browser_open and the other browser_* tools instead.",
		`{"type":"object","properties":{"url":{"type":"string","description":"An http or https URL"}},"required":["url"]}`,
		func(ctx context.Context, in struct {
			URL string `json:"url"`
		}) (string, error) {
			if err := checkURL(in.URL); err != nil {
				return "", err
			}
			p, err := newPage(ctx, cdpURL, token)
			if err != nil {
				return "", err
			}
			defer p.close()
			if err := p.navigate(in.URL); err != nil {
				return "", err
			}
			return p.markdown()
		})
}

// page is one tab in its own browser context.
type page struct {
	c       *cdp
	ctxID   string
	session string
}

// newPage creates an isolated browser context with one blank tab. The
// connection is bound to ctx (deadline and cancellation) until unbind or
// close; a session unbinds it and rebinds it to each tool call.
func newPage(ctx context.Context, cdpURL, token string) (*page, error) {
	c, err := dialCDP(ctx, cdpURL, token)
	if err != nil {
		return nil, err
	}
	c.bind(ctx)
	p := &page{c: c}
	ok := false
	defer func() {
		if !ok {
			p.close()
		}
	}()
	var bc struct {
		ID string `json:"browserContextId"`
	}
	if err := c.call("", "Target.createBrowserContext", nil, &bc); err != nil {
		return nil, err
	}
	p.ctxID = bc.ID
	var target struct {
		ID string `json:"targetId"`
	}
	if err := c.call("", "Target.createTarget", map[string]any{"url": "about:blank", "browserContextId": bc.ID}, &target); err != nil {
		return nil, err
	}
	var att struct {
		Session string `json:"sessionId"`
	}
	if err := c.call("", "Target.attachToTarget", map[string]any{"targetId": target.ID, "flatten": true}, &att); err != nil {
		return nil, err
	}
	p.session = att.Session
	if err := c.call(p.session, "Page.enable", nil, nil); err != nil {
		return nil, err
	}
	ok = true
	return p, nil
}

// navigate loads rawURL (http or https only) and waits for its load event.
// A page whose load event never comes (a long-polling SPA) is used as it is
// after 30s rather than failing the call.
func (p *page) navigate(rawURL string) error {
	if err := checkURL(rawURL); err != nil {
		return err
	}
	p.c.forget(p.session, "Page.loadEventFired")
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	if err := p.c.call(p.session, "Page.navigate", map[string]any{"url": rawURL}, &nav); err != nil {
		return err
	}
	if nav.ErrorText != "" {
		return fmt.Errorf("loading %s: %s", rawURL, nav.ErrorText)
	}
	p.c.waitEventFor(p.session, "Page.loadEventFired", 30*time.Second)
	return p.c.ctx.Err()
}

// checkURL accepts absolute http and https URLs only: no file:, javascript:
// or relative URLs reach the browser.
func checkURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("needs an http or https URL, got %q", rawURL)
	}
	return nil
}

// settle follows an action that may have started a navigation (a click, a
// key, history.back): if one starts within a second, it waits for its load.
// Navigations that finish inside the action's own CDP call are already seen.
func (p *page) settle() {
	if p.c.waitEventFor(p.session, "Page.frameNavigated", time.Second) {
		p.c.waitEventFor(p.session, "Page.loadEventFired", 15*time.Second)
	}
}

func (p *page) close() {
	if p.ctxID != "" {
		_ = p.c.call("", "Target.disposeBrowserContext", map[string]any{"browserContextId": p.ctxID}, nil)
	}
	p.c.close()
}

// eval runs a JavaScript expression in the page and decodes its value (a
// returned promise is awaited). A thrown exception is an error.
func (p *page) eval(expr string, out any) error {
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := p.c.call(p.session, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true,
	}, &res); err != nil {
		return err
	}
	if res.Exception != nil {
		return fmt.Errorf("page script failed: %s", cmp.Or(res.Exception.Exception.Description, res.Exception.Text))
	}
	if out == nil || len(res.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(res.Result.Value, out)
}

// where is the page's title and URL, for action results.
func (p *page) where() string {
	var meta struct{ Title, URL string }
	if err := p.eval(`({Title: document.title, URL: location.href})`, &meta); err != nil {
		return "(the page can't be read: " + err.Error() + ")"
	}
	if meta.Title == "" {
		return meta.URL
	}
	return fmt.Sprintf("%q — %s", meta.Title, meta.URL)
}

// markdown returns the page as "# title\nURL\n\nMarkdown".
func (p *page) markdown() (string, error) {
	var meta struct{ Title, URL string }
	if err := p.eval(`({Title: document.title, URL: location.href})`, &meta); err != nil {
		return "", err
	}
	var md struct {
		Markdown string `json:"markdown"`
	}
	if err := p.c.call(p.session, "LP.getMarkdown", nil, &md); err != nil {
		return "", err
	}
	return fmt.Sprintf("# %s\n%s\n\n%s", meta.Title, meta.URL, strings.TrimSpace(md.Markdown)), nil
}

// cdp is a minimal, single-goroutine CDP client: call sends a command and
// reads messages until its response arrives, remembering the events it passes
// so waitEvent can find ones that came early. Its reads are bounded by the
// context it is bound to.
type cdp struct {
	ws       *websocket.Conn
	ctx      context.Context // the bound call's context; Background when unbound
	stop     func() bool     // detaches the bound context's watcher
	deadline time.Time       // the bound context's deadline; zero if none
	id       int
	seen     map[string]bool // session + "\x00" + event method
}

func dialCDP(ctx context.Context, cdpURL, token string) (*cdp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cdpURL, "/")+"/json/version", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("browser unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("browser unavailable: %s", resp.Status)
	}
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("browser unavailable: %w", err)
	}
	// Obscura reports its own view of the address (localhost inside the
	// container); keep its path but dial the host we reached it on.
	wsURL, err := url.Parse(v.WS)
	if err != nil {
		return nil, err
	}
	base, _ := url.Parse(cdpURL)
	wsURL.Host = base.Host

	cfg, err := websocket.NewConfig(wsURL.String(), "http://"+base.Host)
	if err != nil {
		return nil, err
	}
	cfg.Header.Set("Authorization", "Bearer "+token)
	ws, err := cfg.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("browser unavailable: %w", err)
	}
	ws.MaxPayloadBytes = 16 << 20
	return &cdp{ws: ws, ctx: context.Background(), stop: func() bool { return false }, seen: map[string]bool{}}, nil
}

// bind makes ctx bound every read and write until unbind: its deadline
// becomes the connection's, and its cancellation unblocks a read in progress.
func (c *cdp) bind(ctx context.Context) {
	c.ctx = ctx
	c.deadline, _ = ctx.Deadline()
	_ = c.ws.SetDeadline(c.deadline)
	c.stop = context.AfterFunc(ctx, func() { _ = c.ws.SetDeadline(time.Now()) })
}

func (c *cdp) unbind() {
	c.stop()
	c.ctx, c.deadline = context.Background(), time.Time{}
	_ = c.ws.SetDeadline(time.Time{})
}

func (c *cdp) close() {
	c.stop()
	_ = c.ws.Close()
}

type cdpMessage struct {
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Session string          `json:"sessionId"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *cdp) call(session, method string, params, out any) error {
	c.id++
	msg := map[string]any{"id": c.id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	if err := websocket.JSON.Send(c.ws, msg); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	for {
		m, err := c.read()
		if err != nil {
			return fmt.Errorf("%s: %w", method, slow(err))
		}
		if m.ID != c.id {
			continue
		}
		if m.Error != nil {
			return fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(m.Result, out)
	}
}

// waitEventFor waits up to d (and no longer than the bound context) for an
// event, and reports whether it came.
func (c *cdp) waitEventFor(session, method string, d time.Duration) bool {
	limit := time.Now().Add(d)
	if !c.deadline.IsZero() && c.deadline.Before(limit) {
		limit = c.deadline
	}
	_ = c.ws.SetReadDeadline(limit)
	defer func() {
		if c.ctx.Err() != nil {
			_ = c.ws.SetReadDeadline(time.Now()) // keep a cancellation in force
		} else {
			_ = c.ws.SetReadDeadline(c.deadline)
		}
	}()
	for !c.seen[session+"\x00"+method] {
		if _, err := c.read(); err != nil {
			return false
		}
	}
	return true
}

// forget clears a seen event, so the next wait is for a new one.
func (c *cdp) forget(session string, methods ...string) {
	for _, m := range methods {
		delete(c.seen, session+"\x00"+m)
	}
}

func (c *cdp) read() (cdpMessage, error) {
	var m cdpMessage
	if err := websocket.JSON.Receive(c.ws, &m); err != nil {
		return m, err
	}
	if m.Method != "" {
		c.seen[m.Session+"\x00"+m.Method] = true
	}
	return m, nil
}

// slow turns a read deadline into an error the model can act on.
func slow(err error) error {
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("the page took too long to respond")
	}
	return err
}
