package temporalkernel

// TODO: a single generic Activity ("RunStep") registered on
// cmd/worker-activity. It takes (stepName string, payload []byte), looks
// stepName up in internal/stepreg, and runs it. Ctx.Step on this kernel
// calls workflow.ExecuteActivity against this one activity rather than the
// closure it was given — the closure only matters to memkernel/pgkernel.
