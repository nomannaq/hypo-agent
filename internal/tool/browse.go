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
// DevTools Protocol at cdpURL. Every call opens its page in a fresh browser
// context and disposes of it, so calls share no state: the model passes a URL
// each time, and a retried read is just a re-read.
//
// Obscura refuses private and loopback addresses (its SSRF guard), so pages
// can't reach the database or Temporal next to it.

const urlSchema = `"url":{"type":"string","description":"An http or https URL"}`

// Browse returns the browse tool: the rendered page as Markdown.
func Browse(cdpURL, token string) Tool {
	return Typed("browse",
		"Open a web page in a headless browser (JavaScript runs) and return its content as Markdown. Use it to read pages found with web_search.",
		`{"type":"object","properties":{`+urlSchema+`},"required":["url"]}`,
		func(ctx context.Context, in struct {
			URL string `json:"url"`
		}) (string, error) {
			p, err := openPage(ctx, cdpURL, token, in.URL)
			if err != nil {
				return "", err
			}
			defer p.close()
			return p.markdown()
		})
}

// InspectForms returns the inspect_forms tool: the page's forms and their
// fields, which is what submit_form needs to fill one in.
func InspectForms(cdpURL, token string) Tool {
	return Typed("inspect_forms",
		"List the forms on a web page with their fields (name, type, label, required). Use before submit_form.",
		`{"type":"object","properties":{`+urlSchema+`},"required":["url"]}`,
		func(ctx context.Context, in struct {
			URL string `json:"url"`
		}) (string, error) {
			p, err := openPage(ctx, cdpURL, token, in.URL)
			if err != nil {
				return "", err
			}
			defer p.close()
			var forms []struct {
				Action, Method string
				Fields         []struct {
					Name, Type, Label string
					Required          bool
					Options           []string
				}
			}
			if err := p.eval(listFormsJS, &forms); err != nil {
				return "", err
			}
			if len(forms) == 0 {
				return "No forms on " + in.URL + ".", nil
			}
			var b strings.Builder
			for i, f := range forms {
				fmt.Fprintf(&b, "Form %d: %s %s\n", i, f.Method, f.Action)
				for _, fl := range f.Fields {
					fmt.Fprintf(&b, "  - %s (%s", fl.Name, fl.Type)
					if fl.Required {
						b.WriteString(", required")
					}
					b.WriteString(")")
					if fl.Label != "" {
						fmt.Fprintf(&b, ": %s", fl.Label)
					}
					if len(fl.Options) > 0 {
						fmt.Fprintf(&b, " [options: %s]", strings.Join(fl.Options, ", "))
					}
					b.WriteString("\n")
				}
			}
			return strings.TrimRight(b.String(), "\n"), nil
		})
}

// SubmitForm returns the submit_form tool. It has side effects (it sends
// whatever the form sends), so it is a Write tool: the run asks the user
// before calling it, and the ledger keeps a retry from submitting twice.
func SubmitForm(cdpURL, token string) Tool {
	t := Typed("submit_form",
		"Fill in and submit a form on a web page (e.g. a contact form), then return the page it leads to. The user is asked to approve every submission. Find the form and field names with inspect_forms first.",
		`{"type":"object","properties":{`+urlSchema+`,`+
			`"form":{"type":"integer","minimum":0,"description":"Form index from inspect_forms (default 0)"},`+
			`"fields":{"type":"object","additionalProperties":{"type":"string"},"description":"Field name to value"}},`+
			`"required":["url","fields"]}`,
		func(ctx context.Context, in struct {
			URL    string            `json:"url"`
			Form   int               `json:"form"`
			Fields map[string]string `json:"fields"`
		}) (string, error) {
			p, err := openPage(ctx, cdpURL, token, in.URL)
			if err != nil {
				return "", err
			}
			defer p.close()
			fields, _ := json.Marshal(in.Fields)
			p.c.forget(p.session, "Page.loadEventFired")
			if err := p.eval(fmt.Sprintf("(%s)(%d, %s)", fillAndSubmitJS, in.Form, fields), nil); err != nil {
				return "", err
			}
			// A form that navigates fires a load event; one that submits with
			// fetch doesn't, so give it a moment and read the page either way.
			if !p.c.waitEventFor(p.session, "Page.loadEventFired", 10*time.Second) {
				time.Sleep(2 * time.Second)
			}
			page, err := p.markdown()
			if err != nil {
				return "", fmt.Errorf("the form was submitted, but reading the result failed: %w", err)
			}
			return fmt.Sprintf("Submitted form %d on %s. The page after submitting:\n\n%s", in.Form, in.URL, page), nil
		})
	t.Effect = Write
	return t
}

// listFormsJS describes document.forms. Hidden and button inputs are left
// out: the model can't usefully set them.
const listFormsJS = `Array.from(document.forms).map(f => ({
  Action: f.action || location.href,
  Method: (f.method || 'get').toUpperCase(),
  Fields: Array.from(f.elements)
    .filter(e => e.name && !['submit', 'button', 'reset', 'hidden', 'image'].includes(e.type))
    .map(e => ({
      Name: e.name,
      Type: e.type || e.tagName.toLowerCase(),
      Required: !!e.required,
      Label: ((e.labels && e.labels.length ? e.labels[0].textContent : '') || e.placeholder || e.getAttribute('aria-label') || '').trim().slice(0, 80),
      Options: e.tagName === 'SELECT' ? Array.from(e.options).map(o => o.value).slice(0, 20)
        : ['radio', 'checkbox'].includes(e.type) ? Array.from(f.elements).filter(x => x.name === e.name).map(x => x.value).slice(0, 20)
        : null,
    }))
    // radio buttons share a name: list the group once
    .filter((x, i, all) => all.findIndex(y => y.Name === x.Name) === i),
}))`

// fillAndSubmitJS sets each field (radio groups through their RadioNodeList),
// fires the events frameworks listen for, and submits like a user would
// (requestSubmit runs validation and submit handlers).
const fillAndSubmitJS = `(index, fields) => {
  const form = document.forms[index];
  if (!form) throw new Error('there is no form ' + index + ' on this page');
  for (const [name, value] of Object.entries(fields)) {
    const el = form.elements.namedItem(name);
    if (!el) throw new Error('form ' + index + ' has no field named ' + name);
    if (el.type === 'checkbox') el.checked = value !== '' && value !== 'false';
    else el.value = value;
    if (el.dispatchEvent) {
      el.dispatchEvent(new Event('input', {bubbles: true}));
      el.dispatchEvent(new Event('change', {bubbles: true}));
    }
  }
  if (form.requestSubmit) form.requestSubmit(); else form.submit();
}`

// page is one open tab in its own browser context.
type page struct {
	c       *cdp
	ctxID   string
	session string
}

// openPage creates an isolated browser context and a tab in it, navigates to
// rawURL and waits for the load event.
func openPage(ctx context.Context, cdpURL, token, rawURL string) (*page, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("needs an http or https URL, got %q", rawURL)
	}
	c, err := dialCDP(ctx, cdpURL, token)
	if err != nil {
		return nil, err
	}
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
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	if err := c.call(p.session, "Page.navigate", map[string]any{"url": u.String()}, &nav); err != nil {
		return nil, err
	}
	if nav.ErrorText != "" {
		return nil, fmt.Errorf("loading %s: %s", u, nav.ErrorText)
	}
	if err := c.waitEvent(p.session, "Page.loadEventFired"); err != nil {
		return nil, err
	}
	ok = true
	return p, nil
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
		msg := cmp.Or(res.Exception.Exception.Description, res.Exception.Text)
		return fmt.Errorf("page script failed: %s", msg)
	}
	if out == nil || len(res.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(res.Result.Value, out)
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
// so waitEvent can find ones that came early.
type cdp struct {
	ws       *websocket.Conn
	ctx      context.Context
	stop     func() bool // detaches the context watcher
	deadline time.Time   // the context's deadline; zero if none
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
	c := &cdp{ws: ws, ctx: ctx, seen: map[string]bool{}}
	// The client reads synchronously, so the context's end must unblock it.
	if d, ok := ctx.Deadline(); ok {
		c.deadline = d
		_ = ws.SetDeadline(d)
	}
	c.stop = context.AfterFunc(ctx, func() { _ = ws.SetDeadline(time.Now()) })
	return c, nil
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

func (c *cdp) waitEvent(session, method string) error {
	for !c.seen[session+"\x00"+method] {
		if _, err := c.read(); err != nil {
			return fmt.Errorf("waiting for %s: %w", method, slow(err))
		}
	}
	return nil
}

// waitEventFor is waitEvent bounded by d (and the context): it reports
// whether the event came.
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
	return c.waitEvent(session, method) == nil
}

// forget clears a seen event, so the next wait is for a new one.
func (c *cdp) forget(session, method string) { delete(c.seen, session+"\x00"+method) }

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
