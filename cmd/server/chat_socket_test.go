package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestChatSocketRequiresSessionAndSameOrigin(t *testing.T) {
	s := &server{}
	for _, test := range []struct {
		name   string
		owner  *user
		origin string
		want   int
	}{
		{"unauthenticated", nil, "http://hypo.test", http.StatusUnauthorized},
		{"foreign origin", &user{ID: 1}, "https://elsewhere.test", http.StatusForbidden},
		{"missing origin", &user{ID: 1}, "", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://hypo.test/api/chat/socket", nil)
			request.Header.Set("Origin", test.origin)
			request = request.WithContext(context.WithValue(request.Context(), userKey{}, test.owner))
			w := httptest.NewRecorder()
			s.chatSocket(w, request)
			if w.Code != test.want {
				t.Fatalf("response = %d, want %d", w.Code, test.want)
			}
		})
	}
}

func TestChatSocketRejectsInvalidSubmissionAndStaysOpen(t *testing.T) {
	s := &server{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.chatSocket(w, r.WithContext(context.WithValue(r.Context(), userKey{}, &user{ID: 1})))
	}))
	defer server.Close()
	ws, err := websocket.Dial("ws"+strings.TrimPrefix(server.URL, "http"), "", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.SetDeadline(time.Now().Add(3 * time.Second))
	exchange := func(cmd chatCommand) runFrame {
		t.Helper()
		if err := websocket.JSON.Send(ws, cmd); err != nil {
			t.Fatal(err)
		}
		var frame runFrame
		if err := websocket.JSON.Receive(ws, &frame); err != nil {
			t.Fatal(err)
		}
		return frame
	}
	if frame := exchange(chatCommand{Type: "submit", runRequest: runRequest{RequestID: "req-1"}}); frame.Type != "rejected" || frame.RequestID != "req-1" {
		t.Fatalf("unexpected rejection: %+v", frame)
	}
	if frame := exchange(chatCommand{Type: "ping"}); frame.Type != "pong" {
		t.Fatalf("unexpected heartbeat: %+v", frame)
	}
}
