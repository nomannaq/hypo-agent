package main

import (
	"net/http"

	inertia "github.com/romsar/gonertia/v2"

	"nomanqureshi.dev/hpyo-agent/internal/memory"
)

func (s *server) memoryPage(w http.ResponseWriter, r *http.Request) {
	store := memory.Store{DB: s.db}
	ctx := r.Context()
	owner := currentUser(r).ID
	entries, err := store.List(ctx, owner)
	if err != nil {
		s.fail(w, err)
		return
	}
	enabled, err := store.SummariesEnabled(ctx, owner)
	if err != nil {
		s.fail(w, err)
		return
	}
	props := inertia.Props{"entries": entries, "summariesEnabled": enabled}
	s.render(w, r, "settings/memory", props)
}

func (s *server) updateMemorySettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		http.Error(w, "enabled is required", http.StatusBadRequest)
		return
	}
	store := memory.Store{DB: s.db}
	if err := store.SetSummaries(r.Context(), currentUser(r).ID, *req.Enabled); err != nil {
		s.fail(w, err)
		return
	}
	s.app.Redirect(w, r, "/settings/memory")
}

func (s *server) deleteMemory(w http.ResponseWriter, r *http.Request) {
	store := memory.Store{DB: s.db}
	deleted, err := store.Delete(r.Context(), currentUser(r).ID, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !deleted {
		http.NotFound(w, r)
		return
	}
	s.app.Redirect(w, r, "/settings/memory")
}
