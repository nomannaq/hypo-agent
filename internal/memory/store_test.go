package memory

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"nomanqureshi.dev/hpyo-agent/db"
)

func TestStorePostgres(t *testing.T) {
	dsn := os.Getenv("HYPO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set HYPO_TEST_DATABASE_URL to test the memory store against pgvector")
	}
	ctx := t.Context()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, cfg.ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := pgx.Identifier{"memory_test_" + strings.ToLower(rand.Text())}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ", public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, name := range []string{
		"20261119191023_create_users.sql",
		"20261120223657_create_run_events.sql",
		"20261122004308_durable_conversations.sql",
		"20261122005314_add_memory.sql",
	} {
		body, err := db.Migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(body), "-- +goose Down")
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users (id, name, email, password_hash) VALUES (1,'One','one@test','-'), (2,'Two','two@test','-');
		INSERT INTO conversations (id,user_id,title) VALUES ('chat-one',1,'One'), ('chat-two',2,'Two');
		INSERT INTO agent_runs (id,user_id,conversation_id,status,prompt) VALUES
			('first',1,'chat-one','succeeded','Compare cloud forests'),
			('other',2,'chat-two','succeeded','Second account'),
			('query',1,NULL,'running','Search my chats');
		INSERT INTO run_events (run_id,seq,event) VALUES
			('first',1,'{"kind":"tool_result","text":"browser-secret"}'),
			('first',2,'{"kind":"model_reply","text":"Fog supports epiphytes"}'),
			('other',1,'{"kind":"model_reply","text":"Private epiphytes"}')`)

	store := Store{DB: pool}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if _, err := store.remember(ctx, "first", "first/tool/1/c1", Preference, "Prefer short cloud forest reports"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	entries, err := store.List(ctx, 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("remember wrote duplicate entries: %+v, %v", entries, err)
	}
	note := entries[0]
	if _, err := store.remember(ctx, "other", "other/tool/1/c1", Note, "Private cloud forest data"); err != nil {
		t.Fatal(err)
	}
	out, err := store.Recall(ctx, "query", "cloud")
	if err != nil || !strings.Contains(out, note.Content) || strings.Contains(out, "Private") {
		t.Fatalf("owner-scoped recall: %q, %v", out, err)
	}
	if _, err := store.remember(ctx, "other", "first/tool/1/c1", Note, "forged note"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign receipt lookup: %v", err)
	}
	if deleted, err := store.Delete(ctx, 2, note.ID); deleted || err != nil {
		t.Fatalf("foreign deletion: %t, %v", deleted, err)
	}
	out, err = store.sessionSearch(ctx, "query", "epiphytes")
	if err != nil || !strings.Contains(out, "supports") || !strings.Contains(out, "/agent?chat=chat-one#first") || strings.Contains(out, "Private") {
		t.Fatalf("transcript search: %q, %v", out, err)
	}
	if out, err := store.sessionSearch(ctx, "query", `"browser-secret"`); err != nil || out != "No matching conversation messages." {
		t.Fatalf("private event appears in transcript search: %q, %v", out, err)
	}

	embedding := make([]float32, dimensions)
	embedding[0] = 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Error(r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": embedding}}})
	}))
	defer server.Close()
	store.Embedder = &Embedder{BaseURL: server.URL + "/v1", Model: "fake-1024"}
	for range 2 {
		if err := store.embedOne(ctx); err != nil {
			t.Fatal(err)
		}
	}
	out, err = store.recall(ctx, "query", "unrelated vocabulary", false)
	if err != nil || !strings.Contains(out, note.Content) || strings.Contains(out, "Private") {
		t.Fatalf("owner-scoped semantic recall: %q, %v", out, err)
	}
	server.Close()
	out, err = store.recall(ctx, "query", "cloud forest", false)
	if err != nil || !strings.Contains(out, note.Content) {
		t.Fatalf("keyword fallback: %q, %v", out, err)
	}

	if deleted, err := store.Delete(ctx, 1, note.ID); !deleted || err != nil {
		t.Fatalf("owner deletion: %t, %v", deleted, err)
	}
	id, err := store.remember(ctx, "first", "first/tool/1/c1", Preference, note.Content)
	if id != note.ID || err != nil {
		t.Fatalf("retry receipt after deletion: %q, %v", id, err)
	}
	if entries, err := store.List(ctx, 1); len(entries) != 0 || err != nil {
		t.Fatalf("retry restored a deleted memory: %+v, %v", entries, err)
	}
	if err := store.summarizeOne(ctx, func(context.Context, string, string) (string, error) {
		t.Fatal("summarized without opt-in")
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSummaries(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE agent_runs SET created_at = clock_timestamp() WHERE id = 'first'`)
	if err := store.summarizeOne(ctx, func(_ context.Context, prompt, reply string) (string, error) {
		if prompt != "Compare cloud forests" || reply != "Fog supports epiphytes" {
			t.Fatalf("unexpected summary input: %q, %q", prompt, reply)
		}
		return "Evidence suggests fog supports epiphytes.", nil
	}); err != nil {
		t.Fatal(err)
	}
	entries, err = store.List(ctx, 1)
	if err != nil || len(entries) != 1 || entries[0].Kind != RunSummary || entries[0].Source != "first" {
		t.Fatalf("summary projection: %+v, %v", entries, err)
	}
	if _, err := store.Delete(ctx, 1, entries[0].ID); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE agent_runs SET memory_summary_next = now() WHERE id = 'first'`)
	if err := store.summarizeOne(ctx, func(context.Context, string, string) (string, error) {
		t.Fatal("regenerated a deleted summary")
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
}
