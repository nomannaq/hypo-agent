// Package runtime holds the orchestration services that sit above a single
// run: admission control, supervision, and the event bus. None of these
// depend on a specific kernel implementation, only on kernel.Ctx /
// kernel.Future and the runs/signals it exposes.
package runtime

// TODO: Scheduler — admission control, per-user caps, per-run token/dollar
// budgets checked before every model Step.
