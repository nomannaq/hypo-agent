package tool

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// The browser_* tools keep one page open per run across tool calls, so the
// agent can work through a site like a person: open, look, click, type,
// scroll, go back. The run comes from the call's context (WithCall), never
// from the model.
//
// The browser session service holds these pages independently of the workers.
// Obscura scopes a tab to its CDP connection, so restarting the service or
// Obscura itself loses the tab.

const (
	sessionIdleTTL  = 10 * time.Minute // closed after this long unused
	snapshotContent = 8 << 10          // bytes of page Markdown in a snapshot
)

type browserSession struct {
	*page
	mu       sync.Mutex // one call at a time: a CDP connection is single-reader
	lastUsed time.Time
}

var sessions = struct {
	sync.Mutex
	m    map[string]*browserSession
	reap sync.Once
}{m: map[string]*browserSession{}}

// useSession locks the run's session and binds it to this call's context.
func useSession(ctx context.Context) (*browserSession, func(), error) {
	runID := RunID(ctx)
	if runID == "" {
		return nil, nil, errors.New("browser tools only work inside a run")
	}
	sessions.Lock()
	bs := sessions.m[runID]
	sessions.Unlock()
	if bs == nil {
		return nil, nil, errors.New("no page is open in this run; call browser_open first")
	}
	bs.mu.Lock()
	bs.c.bind(ctx)
	release := func() {
		bs.lastUsed = time.Now()
		bs.c.unbind()
		bs.mu.Unlock()
	}
	if err := bs.eval("1", nil); err != nil {
		release()
		closeSession(runID, bs)
		return nil, nil, fmt.Errorf("the browser session was lost (%v); call browser_open again", err)
	}
	return bs, release, nil
}

// closeSession closes bs if it is still the run's session.
func closeSession(runID string, bs *browserSession) {
	sessions.Lock()
	if sessions.m[runID] == bs {
		delete(sessions.m, runID)
	}
	sessions.Unlock()
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.dispose()
}

func (bs *browserSession) dispose() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bs.c.bind(ctx)
	bs.close()
}

// reapIdle closes sessions nobody has used for sessionIdleTTL: runs end
// without telling the tools, and a session holds a connection and a tab.
func reapIdle() {
	for range time.Tick(time.Minute) {
		sessions.Lock()
		var idle []string
		for id, bs := range sessions.m {
			if bs.mu.TryLock() {
				if time.Since(bs.lastUsed) > sessionIdleTTL {
					idle = append(idle, id)
				}
				bs.mu.Unlock()
			}
		}
		victims := make([]*browserSession, 0, len(idle))
		for _, id := range idle {
			victims = append(victims, sessions.m[id])
			delete(sessions.m, id)
		}
		sessions.Unlock()
		for _, bs := range victims {
			bs.mu.Lock()
			bs.dispose()
			bs.mu.Unlock()
		}
	}
}

// act runs an action on the run's page and reports where the page is after
// it, following any navigation the action started.
func act(ctx context.Context, fn func(bs *browserSession) (string, error)) (string, error) {
	bs, release, err := useSession(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	bs.c.forget(bs.session, "Page.frameNavigated", "Page.loadEventFired")
	done, err := fn(bs)
	if err != nil {
		return "", err
	}
	bs.settle()
	return done + "\nNow on: " + bs.where(), nil
}

// BrowserOpen returns browser_open: it opens a URL in the run's browser
// session, replacing any page already open.
func BrowserOpen(cdpURL, token string) Tool {
	return Typed("browser_open",
		"Open a web page in this run's browser session (JavaScript runs; the page stays open across calls). Use it to interact with a site: then browser_snapshot to see it, and browser_click, browser_type, browser_select, browser_press, browser_scroll, browser_back, browser_wait to act. Replaces the page already open.",
		`{"type":"object","properties":{"url":{"type":"string","description":"An http or https URL"}},"required":["url"]}`,
		func(ctx context.Context, in struct {
			URL string `json:"url"`
		}) (string, error) {
			runID := RunID(ctx)
			if runID == "" {
				return "", errors.New("browser tools only work inside a run")
			}
			if err := checkURL(in.URL); err != nil {
				return "", err
			}
			sessions.reap.Do(func() { go reapIdle() })
			p, err := newPage(ctx, cdpURL, token)
			if err != nil {
				return "", err
			}
			if err := p.navigate(in.URL); err != nil {
				p.close()
				return "", err
			}
			where := p.where()
			p.c.unbind()
			bs := &browserSession{page: p, lastUsed: time.Now()}
			sessions.Lock()
			old := sessions.m[runID]
			sessions.m[runID] = bs
			sessions.Unlock()
			if old != nil {
				go closeSession(runID, old)
			}
			return "Opened " + where + ". Call browser_snapshot to see its content and elements.", nil
		})
}

// BrowserSnapshot returns browser_snapshot: the page's interactive elements,
// numbered for the action tools, then its content as Markdown.
func BrowserSnapshot() Tool {
	return Typed("browser_snapshot",
		"Show the open page: its title and URL, a numbered list of interactive elements like [3] button \"Send\", and the page content as Markdown. Element numbers change when the page changes, so take a new snapshot after acting.",
		`{"type":"object","properties":{}}`,
		func(ctx context.Context, _ struct{}) (string, error) {
			bs, release, err := useSession(ctx)
			if err != nil {
				return "", err
			}
			defer release()
			var els []struct {
				Index                        int
				Tag, Role, Name, Type, Value string
			}
			if err := bs.eval(interactiveElementsJS, &els); err != nil {
				return "", err
			}
			md, err := bs.markdown()
			if err != nil {
				return "", err
			}
			title, content, _ := strings.Cut(md, "\n\n")
			var b strings.Builder
			b.WriteString(title)
			if len(els) == 0 {
				b.WriteString("\n\nNo interactive elements.")
			} else {
				b.WriteString("\n\nInteractive elements:\n")
				for _, el := range els {
					fmt.Fprintf(&b, "[%d] %s", el.Index, cmp.Or(el.Role, el.Tag))
					if el.Name != "" {
						fmt.Fprintf(&b, " %q", el.Name)
					}
					if el.Type != "" && el.Tag == "input" {
						fmt.Fprintf(&b, " (%s)", el.Type)
					}
					if el.Value != "" {
						fmt.Fprintf(&b, " value=%q", el.Value)
					}
					b.WriteString("\n")
				}
			}
			if len(content) > snapshotContent {
				content = content[:snapshotContent] + "\n…[page content truncated; scroll or use browse for the full page]"
			}
			b.WriteString("\nPage content:\n" + content)
			return b.String(), nil
		})
}

// interactiveElementsJS numbers the page's visible interactive elements and
// tags each with data-hypo-idx so the action tools can find it again.
const interactiveElementsJS = `(() => {
  document.querySelectorAll('[data-hypo-idx]').forEach(el => el.removeAttribute('data-hypo-idx'));
  const sel = 'a[href], button, input:not([type=hidden]), textarea, select, summary, [role=button], [role=link], [role=tab], [role=menuitem], [role=checkbox], [contenteditable=true]';
  const out = [];
  for (const el of document.querySelectorAll(sel)) {
    const style = getComputedStyle(el);
    if (style.display === 'none' || style.visibility === 'hidden') continue;
    const r = el.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) continue;
    const tag = el.tagName.toLowerCase();
    const label = el.labels && el.labels.length ? el.labels[0].textContent : '';
    const name = (el.getAttribute('aria-label') || label || (tag === 'select' ? '' : el.textContent) ||
      el.getAttribute('placeholder') || el.getAttribute('title') || el.name || '').replace(/\s+/g, ' ').trim().slice(0, 80);
    let value = '';
    if (tag === 'input' && (el.type === 'checkbox' || el.type === 'radio')) value = el.checked ? 'checked' : '';
    else if (tag === 'input' || tag === 'textarea') value = el.type === 'password' && el.value ? '••••' : el.value;
    else if (tag === 'select') value = el.selectedOptions.length ? el.selectedOptions[0].text : '';
    el.setAttribute('data-hypo-idx', String(out.length));
    out.push({Index: out.length, Tag: tag, Role: el.getAttribute('role') || '', Name: name, Type: el.type || '', Value: (value || '').slice(0, 80)});
  }
  return out;
})()`

// element finds a numbered element and scrolls it into view; the JS returns
// null when the number is stale.
const findElementJS = `(idx) => {
  const el = document.querySelector('[data-hypo-idx="' + idx + '"]');
  if (el) el.scrollIntoView({block: 'center'});
  return el;
}`

func staleElement(i int) error {
	return fmt.Errorf("element %d is not on the page any more; take a new browser_snapshot", i)
}

// BrowserClick returns browser_click.
func BrowserClick() Tool {
	return Typed("browser_click",
		"Click a numbered element from the last browser_snapshot (a link, button, checkbox, tab...). It can navigate, submit a form, or change the page.",
		`{"type":"object","properties":{"element_index":{"type":"integer","minimum":0}},"required":["element_index"]}`,
		func(ctx context.Context, in struct {
			ElementIndex int `json:"element_index"`
		}) (string, error) {
			return act(ctx, func(bs *browserSession) (string, error) {
				var res struct {
					Found bool
					Name  string
				}
				if err := bs.eval(fmt.Sprintf(`((find) => {
  const el = find(%d);
  if (!el) return {Found: false};
  const name = (el.getAttribute('aria-label') || el.textContent || el.value || '').replace(/\s+/g, ' ').trim().slice(0, 80);
  el.click();
  return {Found: true, Name: name};
})(%s)`, in.ElementIndex, findElementJS), &res); err != nil {
					return "", err
				}
				if !res.Found {
					return "", staleElement(in.ElementIndex)
				}
				return fmt.Sprintf("Clicked [%d] %q.", in.ElementIndex, res.Name), nil
			})
		})
}

// BrowserType returns browser_type: it replaces a field's text by typing, so
// frameworks see real input events.
func BrowserType() Tool {
	return Typed("browser_type",
		"Type into a numbered input or textarea from the last browser_snapshot, replacing its text. Set submit to press Enter afterwards (e.g. to run a search).",
		`{"type":"object","properties":{"element_index":{"type":"integer","minimum":0},"text":{"type":"string"},"submit":{"type":"boolean","description":"Press Enter after typing"}},"required":["element_index","text"]}`,
		func(ctx context.Context, in struct {
			ElementIndex int    `json:"element_index"`
			Text         string `json:"text"`
			Submit       bool   `json:"submit"`
		}) (string, error) {
			return act(ctx, func(bs *browserSession) (string, error) {
				var found bool
				if err := bs.eval(fmt.Sprintf(`((find) => {
  const el = find(%d);
  if (!el) return false;
  el.focus();
  if ('value' in el) { el.value = ''; el.dispatchEvent(new Event('input', {bubbles: true})); }
  else if (el.isContentEditable) el.textContent = '';
  return true;
})(%s)`, in.ElementIndex, findElementJS), &found); err != nil {
					return "", err
				}
				if !found {
					return "", staleElement(in.ElementIndex)
				}
				if err := bs.c.call(bs.session, "Input.insertText", map[string]any{"text": in.Text}, nil); err != nil {
					return "", err
				}
				_ = bs.eval(`document.activeElement && document.activeElement.dispatchEvent(new Event('change', {bubbles: true}))`, nil)
				if in.Submit {
					if err := pressKey(bs, "Enter"); err != nil {
						return "", err
					}
					return fmt.Sprintf("Typed into [%d] and pressed Enter.", in.ElementIndex), nil
				}
				return fmt.Sprintf("Typed into [%d].", in.ElementIndex), nil
			})
		})
}

// BrowserSelect returns browser_select.
func BrowserSelect() Tool {
	return Typed("browser_select",
		"Choose an option in a numbered dropdown (select) from the last browser_snapshot, by its visible text or value.",
		`{"type":"object","properties":{"element_index":{"type":"integer","minimum":0},"option":{"type":"string"}},"required":["element_index","option"]}`,
		func(ctx context.Context, in struct {
			ElementIndex int    `json:"element_index"`
			Option       string `json:"option"`
		}) (string, error) {
			return act(ctx, func(bs *browserSession) (string, error) {
				opt, _ := json.Marshal(in.Option)
				var res struct {
					Found   bool
					Chosen  string
					Options []string
				}
				if err := bs.eval(fmt.Sprintf(`((find, want) => {
  const el = find(%d);
  if (!el || el.tagName !== 'SELECT') return {Found: false};
  const norm = s => s.replace(/\s+/g, ' ').trim().toLowerCase();
  const o = Array.from(el.options).find(o => o.value === want || norm(o.text) === norm(want));
  if (!o) return {Found: true, Options: Array.from(el.options).map(o => o.text.trim()).slice(0, 30)};
  el.value = o.value;
  el.dispatchEvent(new Event('input', {bubbles: true}));
  el.dispatchEvent(new Event('change', {bubbles: true}));
  return {Found: true, Chosen: o.text.trim()};
})(%s, %s)`, in.ElementIndex, findElementJS, opt), &res); err != nil {
					return "", err
				}
				switch {
				case !res.Found:
					return "", fmt.Errorf("element %d is not a dropdown on this page; take a new browser_snapshot", in.ElementIndex)
				case res.Chosen == "":
					return "", fmt.Errorf("no option %q; the options are: %s", in.Option, strings.Join(res.Options, " | "))
				}
				return fmt.Sprintf("Selected %q in [%d].", res.Chosen, in.ElementIndex), nil
			})
		})
}

// keyCodes are the keys browser_press accepts, with their Windows virtual key
// codes (CDP needs them for keys to have their default effect, like Enter
// submitting a form).
var keyCodes = map[string]int{
	"Enter": 13, "Tab": 9, "Escape": 27, "Backspace": 8, "Delete": 46, "Space": 32,
	"ArrowUp": 38, "ArrowDown": 40, "ArrowLeft": 37, "ArrowRight": 39,
	"PageUp": 33, "PageDown": 34, "Home": 36, "End": 35,
}

func pressKey(bs *browserSession, key string) error {
	code, ok := keyCodes[key]
	if !ok {
		names := make([]string, 0, len(keyCodes))
		for k := range keyCodes {
			names = append(names, k)
		}
		return fmt.Errorf("unsupported key %q; use one of %s", key, strings.Join(names, ", "))
	}
	down := map[string]any{"type": "keyDown", "key": key, "code": key, "windowsVirtualKeyCode": code}
	switch key {
	case "Enter":
		down["text"] = "\r"
	case "Space":
		down["key"], down["text"] = " ", " "
	}
	if err := bs.c.call(bs.session, "Input.dispatchKeyEvent", down, nil); err != nil {
		return err
	}
	return bs.c.call(bs.session, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": down["key"], "code": key, "windowsVirtualKeyCode": code}, nil)
}

// BrowserPress returns browser_press.
func BrowserPress() Tool {
	return Typed("browser_press",
		"Press a key on the focused element: Enter, Tab, Escape, Backspace, Delete, Space, ArrowUp/Down/Left/Right, PageUp, PageDown, Home, End.",
		`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}`,
		func(ctx context.Context, in struct {
			Key string `json:"key"`
		}) (string, error) {
			return act(ctx, func(bs *browserSession) (string, error) {
				if err := pressKey(bs, in.Key); err != nil {
					return "", err
				}
				return "Pressed " + in.Key + ".", nil
			})
		})
}

// BrowserScroll returns browser_scroll.
func BrowserScroll() Tool {
	return Typed("browser_scroll",
		"Scroll the open page: down or up by most of a screen, or to the top or bottom. Pages that load more content on scroll show it in the next snapshot.",
		`{"type":"object","properties":{"direction":{"type":"string","enum":["down","up","top","bottom"]}},"required":["direction"]}`,
		func(ctx context.Context, in struct {
			Direction string `json:"direction"`
		}) (string, error) {
			moves := map[string]string{
				"down": "window.scrollBy(0, innerHeight * 0.8)", "up": "window.scrollBy(0, -innerHeight * 0.8)",
				"top": "window.scrollTo(0, 0)", "bottom": "window.scrollTo(0, document.documentElement.scrollHeight)",
			}
			move, ok := moves[in.Direction]
			if !ok {
				return "", fmt.Errorf("direction must be down, up, top or bottom, got %q", in.Direction)
			}
			return act(ctx, func(bs *browserSession) (string, error) {
				var pos struct{ Y, Height int }
				if err := bs.eval(`(async () => { `+move+`; await new Promise(r => setTimeout(r, 300));
  return {Y: Math.round(scrollY + innerHeight), Height: document.documentElement.scrollHeight}; })()`, &pos); err != nil {
					return "", err
				}
				return fmt.Sprintf("Scrolled %s: showing up to %dpx of %dpx.", in.Direction, pos.Y, pos.Height), nil
			})
		})
}

// BrowserBack returns browser_back.
func BrowserBack() Tool {
	return Typed("browser_back",
		"Go back to the previous page in this run's browser session.",
		`{"type":"object","properties":{}}`,
		func(ctx context.Context, _ struct{}) (string, error) {
			return act(ctx, func(bs *browserSession) (string, error) {
				var had bool
				if err := bs.eval(`history.length > 1 ? (history.back(), true) : false`, &had); err != nil {
					return "", err
				}
				if !had {
					return "", errors.New("there is no previous page")
				}
				return "Went back.", nil
			})
		})
}

// BrowserWait returns browser_wait: it waits for text to appear, for pages
// that fill in after loading.
func BrowserWait() Tool {
	return Typed("browser_wait",
		"Wait until some text appears on the open page (up to 20 seconds), for content that loads after the page does.",
		`{"type":"object","properties":{"text":{"type":"string"},"seconds":{"type":"integer","minimum":1,"maximum":20,"description":"How long to wait (default 10)"}},"required":["text"]}`,
		func(ctx context.Context, in struct {
			Text    string `json:"text"`
			Seconds int    `json:"seconds"`
		}) (string, error) {
			secs := min(cmp.Or(max(in.Seconds, 0), 10), 20)
			text, _ := json.Marshal(in.Text)
			bs, release, err := useSession(ctx)
			if err != nil {
				return "", err
			}
			defer release()
			var found bool
			if err := bs.eval(fmt.Sprintf(`new Promise(resolve => {
  const want = %s, until = Date.now() + %d;
  (function poll() {
    if (document.body && document.body.innerText.includes(want)) return resolve(true);
    if (Date.now() > until) return resolve(false);
    setTimeout(poll, 250);
  })();
})`, text, secs*1000), &found); err != nil {
				return "", err
			}
			if !found {
				return fmt.Sprintf("%q did not appear within %ds.", in.Text, secs), nil
			}
			return fmt.Sprintf("%q is on the page.", in.Text), nil
		})
}

// SaveArtifact stores a file a tool produced for the user (a screenshot) and
// returns its ID. key is the call's idempotency key, so a retried call
// replaces rather than duplicates.
type SaveArtifact func(ctx context.Context, runID, key, mime string, data []byte) (id string, err error)

// BrowserScreenshot returns browser_screenshot: a PNG of the open page,
// stored as an artifact the chat shows the user. The model gets its ID, not
// the image.
func BrowserScreenshot(save SaveArtifact) Tool {
	return Typed("browser_screenshot",
		"Take a screenshot of the open page and show it to the user in the chat (you don't see the image). Use it when the user would want to see the page, or as evidence of what was on screen.",
		`{"type":"object","properties":{"full_page":{"type":"boolean","description":"Capture the whole page, not just the visible part"}}}`,
		func(ctx context.Context, in struct {
			FullPage bool `json:"full_page"`
		}) (string, error) {
			bs, release, err := useSession(ctx)
			if err != nil {
				return "", err
			}
			defer release()
			var shot struct {
				Data string `json:"data"`
			}
			if err := bs.c.call(bs.session, "Page.captureScreenshot", map[string]any{
				"format": "png", "captureBeyondViewport": in.FullPage,
			}, &shot); err != nil {
				return "", err
			}
			png, err := base64.StdEncoding.DecodeString(shot.Data)
			if err != nil {
				return "", fmt.Errorf("screenshot: %w", err)
			}
			id, err := save(ctx, RunID(ctx), CallKey(ctx), "image/png", png)
			if err != nil {
				return "", fmt.Errorf("saving the screenshot: %w", err)
			}
			return fmt.Sprintf("Screenshot of %s shown to the user. [artifact:%s]", bs.where(), id), nil
		})
}
