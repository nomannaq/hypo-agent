package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	inertia "github.com/romsar/gonertia/v2"

	"nomanqureshi.dev/hpyo-agent/internal/runtime"
)

func (s *server) schedulesPage(w http.ResponseWriter, r *http.Request) {
	entries, err := s.scheduler.ListSchedules(r.Context(), currentUser(r).ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "schedules", inertia.Props{"schedules": entries})
}

func (s *server) cancelSchedule(w http.ResponseWriter, r *http.Request) {
	entry, err := s.scheduler.CancelSchedule(r.Context(), currentUser(r).ID, r.PathValue("id"))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		http.NotFound(w, r)
	case err != nil:
		s.fail(w, err)
	case entry.Status == runtime.ScheduleCancelled:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(entry)
	}
}
