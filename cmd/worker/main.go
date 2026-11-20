// Command worker runs agent runs: it registers the agent workflow and its
// activities (model and tool calls) with Temporal and executes them. Runs
// survive this process dying: another worker, or this one restarted, resumes
// them from their history without repeating completed calls.
package main

import (
	"cmp"
	"context"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/dotenv"
	"nomanqureshi.dev/hpyo-agent/internal/executor"
	"nomanqureshi.dev/hpyo-agent/internal/kernel/temporalkernel"
	"nomanqureshi.dev/hpyo-agent/internal/model"
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

	runner := &executor.Runner{
		Model:  model.NewClient(os.Getenv("NEBIUS_TOKEN_FACTORY"), os.Getenv("NEBIUS_BASE_URL"), os.Getenv("NEBIUS_MODEL")),
		System: executor.SystemPrompt,
		// The append can be retried after it landed; the (run_id, seq) key
		// makes the retry a no-op. The insert trigger wakes the server's streams.
		Events: func(ctx context.Context, runID string, seq int, e agent.Event) error {
			_, err := db.Exec(ctx, `INSERT INTO run_events (run_id, seq, event) VALUES ($1, $2, $3)
				ON CONFLICT DO NOTHING`, runID, seq, e)
			return err
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
