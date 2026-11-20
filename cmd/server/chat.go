package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/temporalkernel"
)

const maxChatMessages = 100

// chat starts one agent turn as a durable run on Temporal and waits for its
// final reply. The run lives on the worker, not in this request: if the client
// disconnects or the server restarts, it still finishes (Step 3 will let the UI
// reattach to it).
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

	input, _ := json.Marshal(executor.Input{History: history[:n-1], Prompt: history[n-1].Text})
	run, err := s.temporal.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{
		ID:                       "run-" + rand.Text(),
		TaskQueue:                temporalkernel.TaskQueue,
		WorkflowExecutionTimeout: 30 * time.Minute, // backstop; the executor's guards end runs long before
	}, executor.WorkflowName, input)
	if err != nil {
		log.Printf("starting run: %v", err)
		var unavailable *serviceerror.Unavailable
		if errors.As(err, &unavailable) {
			http.Error(w, "the agent runtime is unavailable", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "the agent run failed", http.StatusInternalServerError)
		return
	}
	var out []byte
	if err := temporalkernel.Result(run.Get(r.Context(), &out)); err != nil {
		log.Printf("run %s: %v", run.GetID(), err)
		http.Error(w, "the agent run failed", http.StatusInternalServerError)
		return
	}
	var st agent.State
	if err := json.Unmarshal(out, &st); err != nil {
		log.Printf("run %s: %v", run.GetID(), err)
		http.Error(w, "the agent run failed", http.StatusInternalServerError)
		return
	}
	if st.Status != agent.Done {
		log.Printf("run %s stopped: %s", run.GetID(), st.Err)
		http.Error(w, "the model request failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"reply": st.Messages[len(st.Messages)-1].Text})
}
