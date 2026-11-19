// Package tool is the tool runtime: registry, effect types, an idempotency
// ledger for side effects (pass runID:seq as the idempotency key to
// external systems — the journal only guarantees the step's recorded
// result is exactly-once, not the side effect itself), and sandboxing.
package tool

// TODO: Registry (name -> tool definition + handler), used by
// internal/executor to resolve the tool calls the reducer emits.
