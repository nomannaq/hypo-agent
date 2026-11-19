// Command worker-activity hosts the Temporal activity worker: the generic
// RunStep activity (internal/kernel/temporalkernel), which dispatches to
// internal/stepreg by name. This is where model calls and tool execution
// actually happen — separate task queue, separate pool from the workflow
// worker. Not wired up yet.
package main

func main() {
	panic("TODO: wire temporalkernel's activity worker")
}
