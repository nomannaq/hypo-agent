package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	inertia "github.com/romsar/gonertia/v2"
	"go.temporal.io/sdk/client"

	"nomanqureshi.dev/hpyo-agent/internal/dotenv"
	"nomanqureshi.dev/hpyo-agent/internal/runtime"
	"nomanqureshi.dev/hpyo-agent/skills"
)

type server struct {
	db        *pgxpool.Pool
	app       *inertia.ViteInstance
	temporal  client.Client
	hub       *hub
	scheduler runtime.Scheduler
	skills    *skills.Catalog
}

func main() {
	dotenv.Load(".env")
	db, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "seed" {
		if err := seed(context.Background(), db); err != nil {
			log.Fatal(err)
		}
		log.Print("database seeded")
		return
	}

	i, err := inertia.NewFromFile("web/app.html",
		inertia.WithVersionFromFile("public/build/manifest.json"))
	if err != nil {
		// No build yet (dev mode): run without asset versioning.
		i, err = inertia.NewFromFile("web/app.html")
	}
	if err != nil {
		log.Fatal(err)
	}
	i.ShareProp("name", "Hypo Agent")
	app, err := inertia.NewVite(i)
	if err != nil {
		log.Fatal(err)
	}
	// gonertia's viteReactRefresh only checks for public/hot at boot, so the page goes
	// blank if Vite (re)starts after the server. This checks on every render.
	if err := app.ShareTemplateFunc("reactRefresh", reactRefresh); err != nil {
		log.Fatal(err)
	}
	// Lazy: the app (auth, settings) works while Temporal is down; only
	// starting a run needs it.
	tc, err := client.NewLazyClient(client.Options{HostPort: cmp.Or(os.Getenv("TEMPORAL_ADDRESS"), "localhost:7233")})
	if err != nil {
		log.Fatal(err)
	}
	catalog, err := skills.Bundled()
	if err != nil {
		log.Fatalf("loading skills: %v", err)
	}
	s := &server{db: db, app: app, temporal: tc, hub: newHub(), skills: catalog}
	s.scheduler = runtime.Scheduler{DB: db, Start: s.startWorkflow, Inspect: s.inspectWorkflow, ClosedReason: s.workflowCloseReason, CancelWorkflow: s.cancelWorkflow}
	go s.scheduler.Dispatch(context.Background())
	go s.scheduler.Supervise(context.Background())
	go s.scheduler.DispatchSchedules(context.Background())
	go s.hub.listen(db.Config().ConnConfig)

	mux := http.NewServeMux()
	mux.Handle("GET /build/", http.StripPrefix("/build/", http.FileServer(http.Dir("public/build"))))

	mux.HandleFunc("GET /{$}", s.page("welcome"))
	mux.HandleFunc("GET /login", s.guestOnly(s.page("auth/login")))
	mux.HandleFunc("POST /login", s.guestOnly(s.login))
	mux.HandleFunc("GET /register", s.guestOnly(s.page("auth/register")))
	mux.HandleFunc("POST /register", s.guestOnly(s.register))
	mux.HandleFunc("POST /logout", s.logout)

	mux.HandleFunc("GET /agent", s.authOnly(s.agentPage))
	mux.HandleFunc("GET /schedules", s.authOnly(s.schedulesPage))
	mux.HandleFunc("DELETE /api/schedules/{id}", s.authOnly(s.cancelSchedule))
	mux.HandleFunc("GET /api/chat/socket", s.chatSocket)
	mux.HandleFunc("POST /api/runs", s.authOnly(s.startRun))
	mux.HandleFunc("GET /api/runs/{id}/events", s.authOnly(s.runEvents))
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.authOnly(s.requestRunCancellation))
	mux.HandleFunc("POST /api/runs/{id}/approvals", s.authOnly(s.decideApproval))
	mux.HandleFunc("GET /api/runs/{id}/artifacts/{artifact}", s.authOnly(s.artifact))
	mux.Handle("GET /settings", http.RedirectHandler("/settings/profile", http.StatusFound))
	mux.HandleFunc("GET /settings/profile", s.authOnly(s.page("settings/profile")))
	mux.HandleFunc("PATCH /settings/profile", s.authOnly(s.updateProfile))
	mux.HandleFunc("DELETE /settings/profile", s.authOnly(s.deleteAccount))
	mux.HandleFunc("GET /settings/security", s.authOnly(s.page("settings/security")))
	mux.HandleFunc("PUT /settings/password", s.authOnly(s.updatePassword))
	mux.HandleFunc("GET /settings/appearance", s.authOnly(s.page("settings/appearance")))
	mux.HandleFunc("GET /settings/memory", s.authOnly(s.memoryPage))
	mux.HandleFunc("PUT /settings/memory", s.authOnly(s.updateMemorySettings))
	mux.HandleFunc("DELETE /settings/memory/{id}", s.authOnly(s.deleteMemory))

	addr := ":" + os.Getenv("PORT")
	if addr == ":" {
		addr = ":8080"
	}
	log.Printf("listening on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, sameOrigin(app.Middleware(s.withUser(mux)))))
}

// reactRefresh emits the @vitejs/plugin-react preamble while the Vite dev server runs.
func reactRefresh() template.HTML {
	hot, err := os.ReadFile("public/hot")
	if err != nil {
		return ""
	}
	return template.HTML(fmt.Sprintf(`<script type="module">
  import RefreshRuntime from "%s/@react-refresh"
  RefreshRuntime.injectIntoGlobalHook(window)
  window.$RefreshReg$ = () => {}
  window.$RefreshSig$ = () => (type) => type
  window.__vite_plugin_react_preamble_installed__ = true
</script>`, strings.TrimSpace(string(hot))))
}

func (s *server) page(component string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.render(w, r, component) }
}

func (s *server) render(w http.ResponseWriter, r *http.Request, component string, props ...inertia.Props) {
	if err := s.app.Render(w, r, component, props...); err != nil {
		s.fail(w, err)
	}
}

// invalid re-renders the form page with errors; Inertia keeps the form state on non-GET visits.
func (s *server) invalid(w http.ResponseWriter, r *http.Request, component string, errs inertia.ValidationErrors) {
	s.render(w, r.WithContext(inertia.SetValidationErrors(r.Context(), errs)), component)
}

func (s *server) fail(w http.ResponseWriter, err error) {
	log.Print(err)
	http.Error(w, "server error", http.StatusInternalServerError)
}

func (s *server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *server) updateProfile(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Email string }
	if !s.decode(w, r, &in) {
		return
	}
	errs := inertia.ValidationErrors{}
	if strings.TrimSpace(in.Name) == "" {
		errs["name"] = "The name field is required."
	}
	if !strings.Contains(in.Email, "@") {
		errs["email"] = "The email field must be a valid email address."
	}
	if len(errs) > 0 {
		s.invalid(w, r, "settings/profile", errs)
		return
	}
	tag, err := s.db.Exec(r.Context(), `
		UPDATE users SET name = $1, email = $2, updated_at = now() WHERE id = $3
		AND NOT EXISTS (SELECT 1 FROM users WHERE email = $2 AND id <> $3)`,
		strings.TrimSpace(in.Name), normEmail(in.Email), currentUser(r).ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		s.invalid(w, r, "settings/profile", inertia.ValidationErrors{"email": "The email has already been taken."})
		return
	}
	s.app.Redirect(w, r, "/settings/profile")
}

func (s *server) updatePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current  string `json:"current_password"`
		Password string `json:"password"`
		Confirm  string `json:"password_confirmation"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	errs := inertia.ValidationErrors{}
	if !s.passwordMatches(r, in.Current) {
		errs["current_password"] = "The password is incorrect."
	}
	validatePassword(errs, in.Password, in.Confirm)
	if len(errs) > 0 {
		s.invalid(w, r, "settings/security", errs)
		return
	}
	if _, err := s.db.Exec(r.Context(), `UPDATE users SET password_hash = $1, updated_at = now() WHERE id = $2`,
		hashPassword(in.Password), currentUser(r).ID); err != nil {
		s.fail(w, err)
		return
	}
	s.app.Redirect(w, r, "/settings/security")
}

func (s *server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	var in struct{ Password string }
	if !s.decode(w, r, &in) {
		return
	}
	if !s.passwordMatches(r, in.Password) {
		s.invalid(w, r, "settings/profile", inertia.ValidationErrors{"password": "The password is incorrect."})
		return
	}
	if _, err := s.db.Exec(r.Context(), `DELETE FROM users WHERE id = $1`, currentUser(r).ID); err != nil {
		s.fail(w, err)
		return
	}
	s.endSession(w, r)
	s.app.Redirect(w, r, "/")
}

func (s *server) passwordMatches(r *http.Request, pw string) bool {
	var hash string
	err := s.db.QueryRow(r.Context(), `SELECT password_hash FROM users WHERE id = $1`, currentUser(r).ID).Scan(&hash)
	return err == nil && checkPassword(hash, pw)
}
