// Package pgkernel is our own durable kernel.Ctx implementation, journaled
// in Postgres (see db/migrations for the runs/journal/timers/signals
// schema). This is the learning path: claim-by-lease with
// SELECT ... FOR UPDATE SKIP LOCKED, fencing tokens on every journal write
// so a worker that loses its lease mid-step can't corrupt state, heartbeats
// that renew the lease and cancel the run's ctx on failure, and
// park/wake/replay for Sleep and Await.
//
// TODO: runs.go (claim, lease, fencing), journal.go (step replay +
// nondeterminism check), timers.go, signals.go, chaos_test.go (kill-worker,
// stale-fencing, dual-worker, bad-replay).
package pgkernel
