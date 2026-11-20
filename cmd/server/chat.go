package main

import (
	"crypto/rand"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/memkernel"
)

const (
	maxChatMessages  = 100
	chatSystemPrompt = "You are Hypo, a helpful research assistant. Be concise and clear."
)

// chat runs one agent turn through the executor loop and returns the final
// reply. The client sends the whole conversation; nothing is persisted yet.
//
// ponytail: a fresh in-memory kernel per request, so nothing outlives the
// request and a crash loses the turn. Step 4 (temporalkernel) makes runs durable.
func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	n := len(in.Messages)
	if n == 0 || n > maxChatMessages || in.Messages[n-1].Role != "user" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	history := make([]agent.Message, 0, n-1)
	for _, m := range in.Messages {
		if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		history = append(history, agent.Message{Role: agent.Role(m.Role), Text: m.Content})
	}

	runner := &executor.Runner{Model: s.model, System: chatSystemPrompt}
	k := memkernel.New()
	k.Register("agent", runner.Workflow)
	input, _ := json.Marshal(executor.Input{History: history[:n-1], Prompt: history[n-1].Text})
	out, err := k.Run(r.Context(), rand.Text(), "agent", input)
	if err != nil {
		log.Print(err)
		http.Error(w, "the agent run failed", http.StatusInternalServerError)
		return
	}
	var st agent.State
	if err := json.Unmarshal(out, &st); err != nil {
		log.Print(err)
		http.Error(w, "the agent run failed", http.StatusInternalServerError)
		return
	}
	if st.Status != agent.Done {
		log.Printf("agent run stopped: %s", st.Err)
		http.Error(w, "the model request failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"reply": st.Messages[len(st.Messages)-1].Text})
}
