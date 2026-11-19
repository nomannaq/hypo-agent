package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"nomanqureshi.dev/hpyo-agent/internal/model"
)

const (
	maxChatMessages  = 100
	chatSystemPrompt = "You are Hypo, a helpful research assistant. Be concise and clear."
)

// chat is a plain request/response completion. The client sends the whole
// conversation; nothing is persisted yet.
func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Messages []model.Message `json:"messages"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	if len(in.Messages) == 0 || len(in.Messages) > maxChatMessages {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	msgs := []model.Message{{Role: "system", Content: chatSystemPrompt}}
	for _, m := range in.Messages {
		if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		msgs = append(msgs, m)
	}
	reply, _, err := s.model.Chat(r.Context(), msgs)
	if err != nil {
		log.Print(err)
		http.Error(w, "the model request failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"reply": reply})
}
