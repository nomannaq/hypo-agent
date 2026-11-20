// Command worker runs agent runs: it registers the agent workflow and its
// activities (model and tool calls) with Temporal and executes them. Runs
// survive this process dying: another worker, or this one restarted, resumes
// them from their history without repeating completed calls.
package main

import (
	"cmp"
	"context"
	"errors"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/dotenv"
	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/temporalkernel"
	"nomanqureshi.dev/hpyo-agent/internal/model"
	"nomanqureshi.dev/hpyo-agent/internal/tool"
)

func main() {
	dotenv.Load(".env")

	c, err := client.Dial(client.Options{HostPort: cmp.Or(os.Getenv("TEMPORAL_ADDRESS"), "localhost:7233")})
	if err != nil {
		log.Fatalf("connecting to Temporal (is `make up` running?): %v", err)
	}
	defer c.Close()

	db, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	obscura, obscuraToken := cmp.Or(os.Getenv("OBSCURA_URL"), "http://localhost:9222"), os.Getenv("OBSCURA_CDP_TOKEN")
	if obscuraToken == "" {
		log.Print("warning: OBSCURA_CDP_TOKEN is not set; the browse tool will fail (401). Set it in .env and run `make up`.")
	}

	runner := &executor.Runner{
		Model:  model.NewClient(os.Getenv("NEBIUS_TOKEN_FACTORY"), os.Getenv("NEBIUS_BASE_URL"), os.Getenv("NEBIUS_MODEL")),
		System: executor.SystemPrompt,
		// Tools on the compose services (`make up`). submit_form is a Write
		// tool: each call waits for the user's approval.
		Tools: tool.Registry{
			tool.WebSearch(cmp.Or(os.Getenv("SEARXNG_URL"), "http://localhost:8888")),
			tool.Browse(obscura, obscuraToken),
			tool.InspectForms(obscura, obscuraToken),
			tool.SubmitForm(obscura, obscuraToken),
		},
		// The append can be retried after it landed; the (run_id, seq) key
		// makes the retry a no-op. The insert trigger wakes the server's streams.
		Events: func(ctx context.Context, runID string, seq int, e agent.Event) error {
			_, err := db.Exec(ctx, `INSERT INTO run_events (run_id, seq, event) VALUES ($1, $2, $3)
				ON CONFLICT DO NOTHING`, runID, seq, e)
			return err
		},
		Ledger: func(ctx context.Context, key string, run func() (string, error)) (string, error) {
			var out string
			err := db.QueryRow(ctx, `SELECT output FROM tool_ledger WHERE key = $1`, key).Scan(&out)
			if err == nil {
				return out, nil // this step's effect already happened
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return "", err
			}
			if out, err = run(); err != nil {
				return "", err
			}
			// Recorded even if the activity is being cancelled: the effect
			// happened, and a retry must find it. If the write fails, the
			// call still succeeds; only a retry could repeat it.
			runID, _, _ := strings.Cut(key, "/")
			if _, err := db.Exec(context.WithoutCancel(ctx), `INSERT INTO tool_ledger (key, run_id, output) VALUES ($1, $2, $3)
				ON CONFLICT DO NOTHING`, key, runID, out); err != nil {
				log.Printf("tool ledger: recording %s: %v", key, err)
			}
			return out, nil
		},
	}
	w := worker.New(c, temporalkernel.TaskQueue, worker.Options{})
	temporalkernel.RegisterWorkflow(w, executor.WorkflowName, runner.Workflow, temporalkernel.DefaultOptions)
	for name, fn := range runner.Activities() {
		temporalkernel.RegisterActivity(w, name, fn)
	}
	log.Printf("worker polling task queue %q", temporalkernel.TaskQueue)
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatal(err)
	}
}
