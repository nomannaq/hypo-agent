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
	base = append(base, "20261122014500_assistant_messages.sql", "20261122015500_research_lane.sql", "20261122015600_conversation_run_lanes.sql", "20261122015700_run_cancellation.sql")
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
	forged := second
	forged.Owner = 2
	if _, err := s.Admit(ctx, forged); !errors.Is(err, ErrMissingConversation) {
		t.Fatalf("cross-owner admission: %v", err)
	}
	admit(Request{ID: "third", Owner: 1, NewConversationID: "other", Input: executor.Input{Prompt: "other"}})
	admit(Request{ID: "fourth", Owner: 1, NewConversationID: "last", Input: executor.Input{Prompt: "last"}})

	// Concurrent dispatchers claim distinct runs.
	claims := make(chan string, 3)
	for range 3 {
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
	if !got["first"] || !got["third"] || !got["fourth"] {
		t.Fatalf("claims = %v", got)
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
	admit(second)
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
			t.Errorf("unexpected workflow check %q", id)
		}
		return true, nil
	}
	s.Start = func(context.Context, string, []byte) error {
		t.Error("dispatcher reran an accepted workflow")
		return nil
	}
	if err := s.checkDue(ctx); err != nil {
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
	if err := s.checkDue(ctx); err != nil {
		t.Fatal(err)
	}
	assertState("second", Failed)
}

func TestTurnsInAChatRunConcurrently(t *testing.T) {
	ctx := t.Context()
	pool := runtimeTestDB(t, "20261122015800_concurrent_chat_turns.sql")
	if _, err := pool.Exec(ctx, "INSERT INTO users (id,name,email,password_hash) VALUES (1,'One','orchestrator@test','-')"); err != nil {
		t.Fatal(err)
	}
	s := Scheduler{DB: pool}
	admit := func(id, prompt, workflow, conversation, newConversation string) {
		t.Helper()
		_, err := s.Admit(ctx, Request{
			ID: id, Owner: 1, ConversationID: conversation, NewConversationID: newConversation,
			Input: executor.Input{Prompt: prompt, Workflow: workflow},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	claim := func(want string) executor.Input {
		t.Helper()
		id, raw, err := s.claim(ctx)
		var input executor.Input
		if err != nil || id != want || json.Unmarshal(raw, &input) != nil {
			t.Fatalf("claim = %q, %v; want %q", id, err, want)
		}
		return input
	}
	noClaim := func(why string) {
		t.Helper()
		if _, _, err := s.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s: %v", why, err)
		}
	}

	// The orchestrator answers a follow-up while the first turn is still busy,
	// and the follow-up knows that turn is pending.
	admit("first", "Collect evidence", executor.ResponsiveWorkflowName, "", "chat")
	admit("follow-up", "Explain the terms", executor.ResponsiveWorkflowName, "chat", "")
	claim("first")
	for seq, e := range []agent.Event{
		{Kind: agent.UserMessage, Text: "Collect evidence"},
		{Kind: agent.ModelReply, Text: "Looking into it.", ToolCalls: []agent.ToolCall{
			{ID: "s", Name: "web_search", Args: json.RawMessage(`{"query":"q"}`)},
			{ID: "b", Name: "browse", Args: json.RawMessage(`{"url":"https://example.test/a"}`)},
		}},
	} {
		if err := s.AppendEvent(ctx, "first", seq, e); err != nil {
			t.Fatal(err)
		}
	}
	input := claim("follow-up")
	if len(input.ActiveResearch) != 1 || input.ActiveResearch[0] != "Collect evidence" {
		t.Fatalf("follow-up input = %+v", input)
	}
	// The orchestrator supervises the first turn from real progress.
	for _, want := range []string{`"Collect evidence": starting`, "Web searches: 1.", "https://example.test/a", `Last message to the user: "Looking into it."`} {
		if !strings.Contains(input.Progress, want) {
			t.Fatalf("progress lacks %q:\n%s", want, input.Progress)
		}
	}
	for range 2 {
		if yielded, err := s.YieldResearch(ctx, "first"); err != nil || !yielded {
			t.Fatalf("research yield = %v, %v", yielded, err)
		}
	}
	// A second investigation in the same chat also goes to the background.
	if yielded, err := s.YieldResearch(ctx, "follow-up"); err != nil || !yielded {
		t.Fatalf("same-chat yield = %v, %v", yielded, err)
	}
	admit("deep", "Deep review", executor.DeepResearchWorkflowV3, "chat", "")
	claim("deep") // third background slot, same chat
	admit("deep-2", "Wait for research capacity", executor.DeepResearchWorkflowV3, "chat", "")
	noClaim("background cap applies")

	// Foreground capacity is separate from background research.
	for _, id := range []string{"a", "b", "c", "d"} {
		admit(id, "question "+id, executor.ResponsiveWorkflowName, "chat", "")
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		claim(id)
	}
	admit("e", "question e", executor.ResponsiveWorkflowName, "chat", "")
	noClaim("foreground cap applies")

	admit("status", "/status", executor.ResponsiveWorkflowName, "chat", "")
	var reply string
	if err := pool.QueryRow(ctx, "SELECT body FROM run_assistant_messages('status')").Scan(&reply); err != nil ||
		!strings.Contains(reply, "Chat turns: 4 active, 1 queued") ||
		!strings.Contains(reply, "Background research: 3 active, 1 queued") {
		t.Fatalf("status must be published with all slots occupied: %q, %v", reply, err)
	}
	if err := s.AppendEvent(ctx, "first", 0, agent.Event{Kind: agent.ModelReply, Text: "Research complete."}); err != nil {
		t.Fatal(err)
	}
	claim("deep-2")
	if err := s.AppendEvent(ctx, "a", 0, agent.Event{Kind: agent.ModelReply, Text: "Done."}); err != nil {
		t.Fatal(err)
	}
	claim("e")
}

func TestCancellationPersistsAndReleasesSlots(t *testing.T) {
	ctx := t.Context()
	pool := runtimeTestDB(t)
	if _, err := pool.Exec(ctx, "INSERT INTO users (id,name,email,password_hash) VALUES (1,'One','cancel@test','-'), (2,'Two','other@test','-')"); err != nil {
		t.Fatal(err)
	}
	s := Scheduler{DB: pool}
	for _, id := range []string{"running", "other-running", "filler", "queued", "next"} {
		if _, err := s.Admit(ctx, Request{ID: id, Owner: 1, Input: executor.Input{Prompt: id, Workflow: executor.DeepResearchWorkflowV3}}); err != nil {
			t.Fatal(err)
		}
	}
	if id, _, err := s.claim(ctx); err != nil || id != "running" {
		t.Fatalf("claim = %s, %v", id, err)
	}
	for _, want := range []string{"other-running", "filler"} {
		if id, _, err := s.claim(ctx); err != nil || id != want {
			t.Fatalf("claim = %s, %v; want %s", id, err, want)
		}
	}
	for _, id := range []string{"queued", "running"} {
		if found, err := s.RequestCancellation(ctx, id, 2); err != nil || found {
			t.Fatalf("foreign stop = %t, %v", found, err)
		}
		for range 2 {
			if found, err := s.RequestCancellation(ctx, id, 1); err != nil || !found {
				t.Fatalf("owner stop = %t, %v", found, err)
			}
		}
	}
	if _, _, err := s.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cancelling run should retain its slot: %v", err)
	}
	restarted := Scheduler{
		DB:      pool,
		Inspect: func(context.Context, string) (bool, error) { return true, nil },
		CancelWorkflow: func(_ context.Context, id string) error {
			if id != "running" {
				t.Errorf("cancelled unexpected workflow %s", id)
			}
			return nil
		},
	}
	if err := restarted.checkDue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE agent_runs SET next_check = now() WHERE id = 'running'"); err != nil {
		t.Fatal(err)
	}
	restarted.Inspect = func(context.Context, string) (bool, error) { return false, nil }
	if err := restarted.checkDue(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"running", "queued"} {
		var status, failure string
		if err := pool.QueryRow(ctx, "SELECT status, failure FROM agent_runs WHERE id = $1", id).Scan(&status, &failure); err != nil || status != Failed.String() || failure != cancellationReason {
			t.Fatalf("state %s = %s (%s), %v", id, status, failure, err)
		}
	}
	if id, _, err := restarted.claim(ctx); err != nil || id != "next" {
		t.Fatalf("claim after cancellation = %s, %v", id, err)
	}
}
