package main

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	inertia "github.com/romsar/gonertia/v2"
)

const (
	sessionCookie = "session"
	sessionTTL    = 30 * 24 * time.Hour
	hashIter      = 600_000 // OWASP 2023 recommendation for PBKDF2-SHA256
)

type user struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type userKey struct{}

func currentUser(r *http.Request) *user {
	u, _ := r.Context().Value(userKey{}).(*user)
	return u
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, _ := pbkdf2.Key(sha256.New, pw, salt, hashIter, 32)
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", hashIter, b64(salt), b64(key))
}

func checkPassword(hash, pw string) bool {
	var iter int
	parts := strings.Split(hash, "$")
	if len(parts) != 4 {
		return false
	}
	if _, err := fmt.Sscan(parts[1], &iter); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// startSession logs the user in. Without remember the cookie dies with the browser.
func (s *server) startSession(w http.ResponseWriter, r *http.Request, userID int64, remember bool) error {
	token := rand.Text()
	expires := time.Now().Add(sessionTTL)
	if _, err := s.db.Exec(r.Context(),
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash(token), userID, expires); err != nil {
		return err
	}
	c := &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
	}
	if remember {
		c.Expires = expires
	}
	http.SetCookie(w, c)
	return nil
}

func (s *server) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.db.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash = $1`, tokenHash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
}

// withUser loads the session user and shares it with every page as `auth.user`.
func (s *server) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var u *user
		if c, err := r.Cookie(sessionCookie); err == nil {
			u = &user{}
			err := s.db.QueryRow(r.Context(), `
				SELECT u.id, u.name, u.email FROM sessions s JOIN users u ON u.id = s.user_id
				WHERE s.token_hash = $1 AND s.expires_at > now()`, tokenHash(c.Value),
			).Scan(&u.ID, &u.Name, &u.Email)
			if err != nil {
				u = nil
			}
		}
		ctx := context.WithValue(r.Context(), userKey{}, u)
		ctx = inertia.SetProp(ctx, "auth", map[string]any{"user": u})
		// Sidebar stays as the user left it (the sidebar component writes this cookie).
		sidebar, err := r.Cookie("sidebar_state")
		ctx = inertia.SetProp(ctx, "sidebarOpen", err != nil || sidebar.Value == "true")
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sameOrigin rejects cross-site writes (CSRF). SameSite=Lax cookies cover most of it already.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host && o != "https://"+r.Host {
				http.Error(w, "cross-origin request blocked", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) authOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			s.app.Redirect(w, r, "/login")
			return
		}
		h(w, r)
	}
}

func (s *server) guestOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) != nil {
			s.app.Redirect(w, r, "/agent")
			return
		}
		h(w, r)
	}
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email, Password string
		Remember        any // checkbox: "on" when ticked
	}
	if !s.decode(w, r, &in) {
		return
	}
	var id int64
	var hash string
	err := s.db.QueryRow(r.Context(), `SELECT id, password_hash FROM users WHERE email = $1`,
		normEmail(in.Email)).Scan(&id, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, err)
		return
	}
	if err != nil || !checkPassword(hash, in.Password) {
		s.invalid(w, r, "auth/login", inertia.ValidationErrors{"email": "These credentials do not match our records."})
		return
	}
	if err := s.startSession(w, r, id, in.Remember == "on" || in.Remember == true); err != nil {
		s.fail(w, err)
		return
	}
	s.app.Redirect(w, r, "/agent")
}

func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name, Email, Password string
		PasswordConfirmation  string `json:"password_confirmation"`
	}
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
	validatePassword(errs, in.Password, in.PasswordConfirmation)
	if len(errs) > 0 {
		s.invalid(w, r, "auth/register", errs)
		return
	}
	var id int64
	err := s.db.QueryRow(r.Context(),
		`INSERT INTO users (name, email, password_hash) VALUES ($1, $2, $3)
		 ON CONFLICT (email) DO NOTHING RETURNING id`,
		strings.TrimSpace(in.Name), normEmail(in.Email), hashPassword(in.Password)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		s.invalid(w, r, "auth/register", inertia.ValidationErrors{"email": "The email has already been taken."})
		return
	}
	if err == nil {
		err = s.startSession(w, r, id, false)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.app.Redirect(w, r, "/agent")
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	s.endSession(w, r)
	s.app.Redirect(w, r, "/")
}

func validatePassword(errs inertia.ValidationErrors, pw, confirm string) {
	switch {
	case len(pw) < 8:
		errs["password"] = "The password field must be at least 8 characters."
	case pw != confirm:
		errs["password"] = "The password field confirmation does not match."
	}
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }
