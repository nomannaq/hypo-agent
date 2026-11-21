package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Session-based browser tools keep a browser page open across multiple tool
// calls within a single run. Each run gets its own browser session, isolated
// from other runs. If the worker crashes and loses the session, the next tool
// call detects it and returns an error telling the model to call browser_open
// again.

var sessionStore sync.Map // map[string]*browserSession

type browserSession struct {
	cdp     *cdp
	ctxID   string
	session string
}

func (bs *browserSession) eval(expr string, out any) error {
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
	if err := bs.cdp.call(bs.session, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true,
	}, &res); err != nil {
		return err
	}
	if res.Exception != nil {
		msg := res.Exception.Exception.Description
		if msg == "" {
			msg = res.Exception.Text
		}
		return fmt.Errorf("page script failed: %s", msg)
	}
	if out == nil || len(res.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(res.Result.Value, out)
}

func (bs *browserSession) markdown() (string, error) {
	var meta struct{ Title, URL string }
	if err := bs.eval(`({Title: document.title, URL: location.href})`, &meta); err != nil {
		return "", err
	}
	var md struct {
		Markdown string `json:"markdown"`
	}
	if err := bs.cdp.call(bs.session, "LP.getMarkdown", nil, &md); err != nil {
		return "", err
	}
	return fmt.Sprintf("# %s\n%s\n\n%s", meta.Title, meta.URL, strings.TrimSpace(md.Markdown)), nil
}

func (bs *browserSession) close() {
	if bs.ctxID != "" {
		_ = bs.cdp.call("", "Target.disposeBrowserContext", map[string]any{"browserContextId": bs.ctxID}, nil)
	}
	bs.cdp.close()
}

func getOrCreateSession(ctx context.Context, runID, cdpURL, token string) (*browserSession, error) {
	if val, ok := sessionStore.Load(runID); ok {
		return val.(*browserSession), nil
	}
	bs, err := createSession(ctx, cdpURL, token)
	if err != nil {
		return nil, err
	}
	sessionStore.Store(runID, bs)
	return bs, nil
}

func createSession(ctx context.Context, cdpURL, token string) (*browserSession, error) {
	// Use a background context for the CDP connection so it persists across
	// tool calls. The tool's context is only for this call's timeout; the
	// session lives until the run ends or is explicitly closed.
	c, err := dialCDP(context.Background(), cdpURL, token)
	if err != nil {
		return nil, err
	}
	bs := &browserSession{cdp: c}
	ok := false
	defer func() {
		if !ok {
			bs.close()
		}
	}()
	var bc struct {
		ID string `json:"browserContextId"`
	}
	if err := c.call("", "Target.createBrowserContext", nil, &bc); err != nil {
		return nil, err
	}
	bs.ctxID = bc.ID
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
	bs.session = att.Session
	if err := c.call(bs.session, "Page.enable", nil, nil); err != nil {
		return nil, err
	}
	ok = true
	return bs, nil
}

// CloseSession closes and removes the browser session for the given run ID.
// It is safe to call even if no session exists for that run.
func CloseSession(runID string) {
	if val, ok := sessionStore.LoadAndDelete(runID); ok {
		val.(*browserSession).close()
	}
}

// closeSession is the internal wrapper used by browser_open to replace
// an existing session. It uses the exported CloseSession.
func closeSession(runID string) {
	CloseSession(runID)
}

// lookupSession retrieves the session for a run, or returns a session-error
// if the session is missing (worker restart, not yet opened). It also checks
// whether the underlying CDP connection is still alive; if the WebSocket has
// dropped, it evicts the dead entry and tells the model to call browser_open.
func lookupSession(runID string) (*browserSession, error) {
	val, ok := sessionStore.Load(runID)
	if !ok {
		return nil, fmt.Errorf("no browser session for this run; call browser_open first")
	}
	bs := val.(*browserSession)
	// Check if the CDP connection is still alive by pinging with a no-op eval.
	// If it fails, the session is stale (Obscura restarted, WebSocket dropped).
	if err := bs.eval("1", nil); err != nil {
		closeSession(runID)
		return nil, fmt.Errorf("browser session lost; call browser_open to reconnect: %w", err)
	}
	return bs, nil
}

// BrowserSnapshot returns the browser_snapshot tool: current page info plus
// an indexed list of interactive elements.
func BrowserSnapshot() Tool {
	return Typed("browser_snapshot",
		"Return the current page: title, URL, Markdown content, and an indexed list of interactive elements like [3] button \"Send\". Call this after navigation or after an action to see the updated page.",
		`{"type":"object","properties":`+
			`{"run_id":{"type":"string","description":"The run ID for session isolation"}},`+
			`"required":["run_id"]}`,
		func(ctx context.Context, in struct {
			RunID string `json:"run_id"`
		}) (string, error) {
			bs, err := lookupSession(in.RunID)
			if err != nil {
				return "", err
			}
			md, err := bs.markdown()
			if err != nil {
				return "", err
			}
			var elements []struct {
				Tag   string `json:"tag"`
				Name  string `json:"name"`
				Role  string `json:"role"`
				Type  string `json:"type"`
				Value string `json:"value"`
				Index int    `json:"index"`
			}
			if err := bs.eval(interactiveElementsJS, &elements); err != nil {
				return "", err
			}
			var sb strings.Builder
			sb.WriteString(md)
			if len(elements) == 0 {
				sb.WriteString("\n\nNo interactive elements on this page.")
				return sb.String(), nil
			}
			sb.WriteString("\n\nInteractive elements:\n")
			for _, el := range elements {
				role := el.Role
				if role == "" {
					role = el.Tag
				}
				fmt.Fprintf(&sb, "[%d] %s", el.Index, role)
				if el.Name != "" {
					fmt.Fprintf(&sb, " %q", el.Name)
				}
				if el.Type != "" && el.Tag == "input" {
					fmt.Fprintf(&sb, " (type: %s)", el.Type)
				}
				// Show the current value for input/textarea/select so the agent
				// can verify what it typed, since Markdown from LP.getMarkdown
				// only shows placeholder text.
				if el.Value != "" && (el.Tag == "input" || el.Tag == "textarea" || el.Tag == "select") {
					fmt.Fprintf(&sb, " value: %q", el.Value)
				}
				sb.WriteString("\n")
			}
			return strings.TrimRight(sb.String(), "\n"), nil
		})
}

// interactiveElementsJS collects all interactive elements and marks them with
// a data attribute so click/type can find them by index.
const interactiveElementsJS = `(() => {
  const roles = ['button', 'a', 'input', 'textarea', 'select', 'form'];
  const els = Array.from(document.querySelectorAll(roles.join(',') + ', [role="button"], [role="link"]'));
  const result = [];
  let idx = 0;
  for (const el of els) {
    // Use getBoundingClientRect and getComputedStyle instead of offsetParent
    // so fixed-position elements (modals, navbars, FABs) are not skipped.
    const style = getComputedStyle(el);
    if (style.display === 'none' || style.visibility === 'hidden') continue;
    const rect = el.getBoundingClientRect();
    if (rect.width === 0 && rect.height === 0) continue;
    const tag = el.tagName.toLowerCase();
    const role = el.getAttribute('role') || '';
    const name = el.textContent?.trim().slice(0, 80)
      || el.getAttribute('aria-label') || el.getAttribute('placeholder') || el.name || '';
    // Capture the current value for form fields so the snapshot reflects what
    // was typed, not just placeholder text.
    let value = '';
    if (tag === 'input' || tag === 'textarea') {
      value = el.value || '';
    } else if (tag === 'select') {
      const opt = el.options[el.selectedIndex];
      value = opt ? opt.text : '';
    }
    el.setAttribute('data-hypo-idx', String(idx));
    result.push({ tag, name, role, type: el.type || '', value, index: idx });
    idx++;
  }
  return result;
})()`

// BrowserClick returns the browser_click tool: clicks an element by its
// indexed position (from the last snapshot).
func BrowserClick() Tool {
	t := Typed("browser_click",
		"Click the element at the given index (from the last snapshot). This can trigger navigation, form submission, or JavaScript actions.",
		`{"type":"object","properties":`+
			`{"run_id":{"type":"string","description":"The run ID for session isolation"},`+
			`"element_index":{"type":"integer","minimum":0,"description":"Element index from the last snapshot"}},`+
			`"required":["run_id","element_index"]}`,
		func(ctx context.Context, in struct {
			RunID        string `json:"run_id"`
			ElementIndex int    `json:"element_index"`
		}) (string, error) {
			bs, err := lookupSession(in.RunID)
			if err != nil {
				return "", err
			}
			var result struct {
				Found bool   `json:"found"`
				Role  string `json:"role"`
				Name  string `json:"name"`
			}
			if err := bs.eval(fmt.Sprintf("(%s)(%d)", clickJS, in.ElementIndex), &result); err != nil {
				return "", err
			}
			if !result.Found {
				return "", fmt.Errorf("element %d not found; call browser_snapshot for fresh indices", in.ElementIndex)
			}
			return fmt.Sprintf("Clicked element %d (%s %q)", in.ElementIndex, result.Role, result.Name), nil
		})
	t.Effect = Read
	return t
}

// clickJS finds the element by data-hypo-idx, scrolls it into view, clicks
// it, and reports back. Scrolling ensures fixed-position and off-screen
// elements are visible before interaction.
const clickJS = `(function(idx) {
  const el = document.querySelector('[data-hypo-idx="' + idx + '"]');
  if (!el) return { found: false };
  el.scrollIntoView({ block: 'center', inline: 'nearest' });
  const role = el.getAttribute('role') || el.tagName.toLowerCase();
  const name = el.textContent?.trim().slice(0, 80) || el.getAttribute('aria-label') || '';
  el.click();
  return { found: true, role, name };
})`

// BrowserType returns the browser_type tool: types text into an element by
// its indexed position.
func BrowserType() Tool {
	t := Typed("browser_type",
		"Type text into the element at the given index (from the last snapshot). Use for inputs, textareas, and search fields.",
		`{"type":"object","properties":`+
			`{"run_id":{"type":"string","description":"The run ID for session isolation"},`+
			`"element_index":{"type":"integer","minimum":0,"description":"Element index from the last snapshot"},`+
			`"text":{"type":"string","description":"Text to type into the element"}},`+
			`"required":["run_id","element_index","text"]}`,
		func(ctx context.Context, in struct {
			RunID        string `json:"run_id"`
			ElementIndex int    `json:"element_index"`
			Text         string `json:"text"`
		}) (string, error) {
			bs, err := lookupSession(in.RunID)
			if err != nil {
				return "", err
			}
			var result struct {
				Found bool   `json:"found"`
				Tag   string `json:"tag"`
			}
			// JSON-encode the text so special characters (backslashes, newlines,
			// quotes) don't break the JavaScript expression.
			textJSON, err := json.Marshal(in.Text)
			if err != nil {
				return "", err
			}
			if err := bs.eval(fmt.Sprintf("(%s)(%d, %s)", typeJS, in.ElementIndex, textJSON), &result); err != nil {
				return "", err
			}
			if !result.Found {
				return "", fmt.Errorf("element %d not found; call browser_snapshot for fresh indices", in.ElementIndex)
			}
			return fmt.Sprintf("Typed into element %d (%s)", in.ElementIndex, result.Tag), nil
		})
	t.Effect = Read
	return t
}

// typeJS finds the element by data-hypo-idx, scrolls it into view, sets its
// value using the native property descriptor (so React/Vue controlled inputs
// register the change), and fires input/change events.
const typeJS = `(function(idx, text) {
  const el = document.querySelector('[data-hypo-idx="' + idx + '"]');
  if (!el) return { found: false };
  const tag = el.tagName.toLowerCase();
  el.scrollIntoView({ block: 'center', inline: 'nearest' });
  el.focus();
  // Use the native value setter descriptor so React/Vue controlled inputs
  // register the change. Plain assignment bypasses framework tracking.
  const nativeInputValueSetter = Object.getOwnPropertyDescriptor(
    window.HTMLInputElement.prototype, 'value'
  ).set;
  if (tag === 'input' && nativeInputValueSetter) {
    nativeInputValueSetter.call(el, text);
  } else {
    el.value = text;
  }
  if (el.dispatchEvent) {
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
  }
  return { found: true, tag };
})`

// BrowserPress returns the browser_press tool: presses a keyboard key.
func BrowserPress() Tool {
	t := Typed("browser_press",
		"Press a keyboard key (Enter, Tab, Escape, ArrowDown, etc.).",
		`{"type":"object","properties":`+
			`{"run_id":{"type":"string","description":"The run ID for session isolation"},`+
			`"key":{"type":"string","description":"Key name (Enter, Tab, Escape, ArrowDown, etc.)"}},`+
			`"required":["run_id","key"]}`,
		func(ctx context.Context, in struct {
			RunID string `json:"run_id"`
			Key   string `json:"key"`
		}) (string, error) {
			bs, err := lookupSession(in.RunID)
			if err != nil {
				return "", err
			}
			// JSON-encode the key so special characters don't break the JavaScript.
			keyJSON, err := json.Marshal(in.Key)
			if err != nil {
				return "", err
			}
			if err := bs.eval(fmt.Sprintf("(%s)%s", pressJS, keyJSON), nil); err != nil {
				return "", err
			}
			return fmt.Sprintf("Pressed key: %s", in.Key), nil
		})
	t.Effect = Read
	return t
}

// pressJS dispatches a keydown/keyup for the given key name.
const pressJS = `(function(key) {
  const target = document.activeElement || document.body;
  target.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true }));
  target.dispatchEvent(new KeyboardEvent('keyup', { key, bubbles: true }));
})`

// BrowserOpen returns the browser_open tool: opens a URL in a persistent
// browser session for the given run.
func BrowserOpen(cdpURL, token string) Tool {
	return Typed("browser_open",
		"Open a web page in a persistent browser session. Use this first, then act on the page with other browser tools.",
		`{"type":"object","properties":`+
			`{"run_id":{"type":"string","description":"The run ID for session isolation"},`+
			`"url":{"type":"string","description":"An http or https URL"}},`+
			`"required":["run_id","url"]}`,
		func(ctx context.Context, in struct {
			RunID string `json:"run_id"`
			URL   string `json:"url"`
		}) (string, error) {
			u, err := url.Parse(in.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return "", fmt.Errorf("needs an http or https URL, got %q", in.URL)
			}
			// Close existing session for this run, if any, and create a new one.
			closeSession(in.RunID)
			bs, err := createSession(ctx, cdpURL, token)
			if err != nil {
				return "", err
			}
			ok := false
			defer func() {
				if !ok {
					bs.close()
				}
			}()
			var nav struct {
				ErrorText string `json:"errorText"`
			}
			if err := bs.cdp.call(bs.session, "Page.navigate", map[string]any{"url": u.String()}, &nav); err != nil {
				return "", err
			}
			if nav.ErrorText != "" {
				return "", fmt.Errorf("loading %s: %s", u, nav.ErrorText)
			}
			// Use a bounded wait so SPA pages or fast cached loads don't hang
			// the entire run. If the load event doesn't fire within 30 seconds,
			// the page is likely still usable (client-side routing) or stuck.
			if !bs.cdp.waitEventFor(bs.session, "Page.loadEventFired", 30*time.Second) {
				// Page may still be usable (SPA) — don't fail, just continue.
			}
			sessionStore.Store(in.RunID, bs)
			ok = true
			var meta struct{ Title, URL string }
			if err := bs.eval(`({Title: document.title, URL: location.href})`, &meta); err != nil {
				return "", err
			}
			return fmt.Sprintf("Opened %s: %s", meta.URL, meta.Title), nil
		})
}
