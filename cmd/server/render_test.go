package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	inertia "github.com/romsar/gonertia/v2"
)

func TestRenderMergesPageAndSharedProps(t *testing.T) {
	app, err := inertia.New("<html></html>")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{app: &inertia.ViteInstance{Inertia: app}}
	r := httptest.NewRequest("GET", "/agent", nil)
	r.Header.Set("X-Inertia", "true")
	ctx := inertia.SetProp(r.Context(), "auth", map[string]any{"user": &user{ID: 1}})
	w := httptest.NewRecorder()
	s.render(w, r.WithContext(ctx), "agent", inertia.Props{"conversationID": "chat-1"})
	var result struct {
		Props struct {
			ConversationID string
			Auth           struct {
				User *user
			}
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Props.ConversationID != "chat-1" || result.Props.Auth.User == nil || result.Props.Auth.User.ID != 1 {
		t.Fatalf("missing page or shared props: %s", w.Body.String())
	}
}
