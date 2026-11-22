package runtime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"nomanqureshi.dev/hpyo-agent/db"
	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/skills"
)

func runtimeTestDB(t *testing.T, migrations ...string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("HYPO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set HYPO_TEST_DATABASE_URL to run the Postgres scheduler test")
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
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	schema := pgx.Identifier{"test_" + strings.ToLower(rand.Text())}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ", public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	base := []string{"20261119191023_create_users.sql", "20261120223657_create_run_events.sql", "20261122004308_durable_conversations.sql"}
	base = append(base, "20261122014500_assistant_messages.sql", "20261122015500_research_lane.sql")
	for _, name := range append(base, migrations...) {
		sql, err := db.Migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(sql), "-- +goose Down")
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	return pool
}

func TestSchedulerPostgres(t *testing.T) {
	ctx := t.Context()
	pool := runtimeTestDB(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO users (id, name, email, password_hash) VALUES (1, 'One', 'one@test', '-'), (2, 'Two', 'two@test', '-')")

	s := Scheduler{DB: pool}
	admit := func(req Request) Receipt {
		t.Helper()
		res, err := s.Admit(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	first := Request{ID: "first", Owner: 1, NewConversationID: "chat", RequestID: "client-first", Input: executor.Input{Prompt: "first prompt"}}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			res, err := s.Admit(ctx, first)
			if err != nil || res.ID != "first" || res.ConversationID != "chat" {
				t.Errorf("duplicate receipt: %+v, %v", res, err)
			}
		})
	}
	wg.Wait()
	deepRetry := first
	deepRetry.Input.Workflow = executor.DeepResearchWorkflowName
	if _, err := s.Admit(ctx, deepRetry); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("retry changed research mode: %v", err)
	}
	catalog, err := skills.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	second := Request{ID: "second", Owner: 1, ConversationID: "chat", RequestID: "client-second", Input: executor.Input{Prompt: "follow up", Workflow: executor.SkillsWorkflowName, Skills: catalog}}
	admit(second)
	forged := second
	forged.Owner = 2
	if _, err := s.Admit(ctx, forged); !errors.Is(err, ErrMissingConversation) {
		t.Fatalf("cross-owner admission: %v", err)
	}
	admit(Request{ID: "third", Owner: 1, NewConversationID: "other", Input: executor.Input{Prompt: "other"}})
	admit(Request{ID: "fourth", Owner: 1, NewConversationID: "last", Input: executor.Input{Prompt: "last"}})

	// One dispatcher claims the first turn, another claims a different
	// conversation. The next turn and the account's third slot stay queued.
	claims := make(chan string, 2)
	for range 2 {
		wg.Go(func() {
			id, _, err := s.claim(ctx)
			if err != nil {
				t.Error(err)
			}
			claims <- id
		})
	}
	wg.Wait()
	close(claims)
	got := map[string]bool{}
	for id := range claims {
		got[id] = true
	}
	if !got["first"] || !got["third"] {
		t.Fatalf("claims = %v", got)
	}
	if _, _, err := s.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim beyond account cap: %v", err)
	}

	// A terminal append can win against the start acknowledgement. Its
	// output becomes the follow-up's history exactly once.
	events := []agent.Event{
		{Kind: agent.UserMessage, Text: "first prompt"},
		{Kind: agent.ModelReply, Text: "opening update", ToolCalls: []agent.ToolCall{{ID: "same", Name: "send_message"}, {ID: "bad", Name: "send_message"}}},
		{Kind: agent.ToolResult, CallID: "same", Text: "**Standalone update**"},
		{Kind: agent.ToolResult, CallID: "bad", Text: "validation error", IsError: true},
		{Kind: agent.ModelReply, Text: "\n\t", ToolCalls: []agent.ToolCall{{ID: "same", Name: "search_tools"}}},
		{Kind: agent.ToolResult, CallID: "same", Text: "internal tool discovery results"},
		{Kind: agent.ModelReply, ToolCalls: []agent.ToolCall{{ID: "ambiguous", Name: "browse"}, {ID: "ambiguous", Name: "send_message"}}},
		{Kind: agent.ToolResult, CallID: "ambiguous", Text: "ambiguous result"},
		{Kind: agent.ModelReply, Text: "first reply"},
	}
	for range 2 {
		for seq, event := range events {
			if err := s.AppendEvent(ctx, "first", seq, event); err != nil {
				t.Fatal(err)
			}
		}
	}
	var resumed []string
	if err := pool.QueryRow(ctx, `SELECT ARRAY(SELECT body FROM run_assistant_messages('first', 2) ORDER BY seq)`).Scan(&resumed); err != nil ||
		len(resumed) != 2 || resumed[0] != "**Standalone update**" || resumed[1] != "first reply" {
		t.Fatalf("resumed messages = %q, %v", resumed, err)
	}
	if err := s.advance(ctx, "first", Starting, WorkflowStarted, ""); err != nil {
		t.Fatal(err)
	}
	id, input, err := s.claim(ctx)
	if err != nil || id != "second" {
		t.Fatalf("follow-up claim = %q, %v", id, err)
	}
	var follow executor.Input
	if err := json.Unmarshal(input, &follow); err != nil || len(follow.History) != 4 ||
		follow.History[0].Text != "first prompt" || follow.History[1].Text != "opening update" ||
		follow.History[2].Text != "**Standalone update**" || follow.History[3].Text != "first reply" ||
		follow.Prompt != "follow up" || follow.Workflow != executor.SkillsWorkflowName || follow.Skills == nil || follow.Skills.Digest != catalog.Digest {
		t.Fatalf("follow-up input = %+v, %v", follow, err)
	}

	// Simulate a lost start response after Temporal accepted the workflow.
	exec("UPDATE agent_runs SET next_check = now() WHERE id = 'second'")
	s.Inspect = func(_ context.Context, id string) (bool, error) {
		if id != "second" {
			t.Fatalf("unexpected workflow check %q", id)
		}
		return true, nil
	}
	s.Start = func(context.Context, string, []byte) error {
		t.Fatal("dispatcher reran an accepted workflow")
		return nil
	}
	if err := s.checkOne(ctx); err != nil {
		t.Fatal(err)
	}
	assertState := func(id string, want Status) {
		t.Helper()
		var state string
		if err := pool.QueryRow(ctx, "SELECT status FROM agent_runs WHERE id = $1", id).Scan(&state); err != nil || state != want.String() {
			t.Fatalf("state %s = %s, %v; want %s", id, state, err, want)
		}
	}
	assertState("first", Succeeded)
	assertState("second", Running)
	exec("UPDATE agent_runs SET next_check = now() WHERE id = 'second'")
	s.Inspect = func(context.Context, string) (bool, error) { return false, nil }
	if err := s.checkOne(ctx); err != nil {
		t.Fatal(err)
	}
	assertState("second", Failed)
	if id, _, err := s.claim(ctx); err != nil || id != "fourth" {
		t.Fatalf("slot released after workflow closed: %s, %v", id, err)
	}
}
