package temporalkernel

// TODO: Ctx.Call -> workflow.ExecuteActivity by registered name, with
// per-activity timeouts and a retry policy (MaximumAttempts 3; non-retryable
// application errors for 4xx/bad input). Activities are registered on the
// worker from executor.Runner.Activities(); there is no closure dispatch.
