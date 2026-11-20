// Command worker runs agent runs: it registers the agent workflow and its
// activities (model and tool calls) with Temporal and executes them. Runs
// survive this process dying: another worker, or this one restarted, resumes
// them from their history without repeating completed calls.
package main

import (
	"cmp"
	"log"
	"os"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

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

	runner := &executor.Runner{
		Model:  model.NewClient(os.Getenv("NEBIUS_TOKEN_FACTORY"), os.Getenv("NEBIUS_BASE_URL"), os.Getenv("NEBIUS_MODEL")),
		System: executor.SystemPrompt,
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
