// Command worker-workflow hosts the deterministic Temporal workflow worker:
// the generic agent-run workflow (internal/kernel/temporalkernel),
// polling the "workflow" task queue. No I/O, no model calls — those run in
// cmd/worker-activity. Not wired up yet.
package main

func main() {
	panic("TODO: wire temporalkernel's workflow worker")
}
