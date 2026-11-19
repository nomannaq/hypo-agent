package temporalkernel

// TODO: implement kernel.ContinueAsNewer. Temporal's workflow history has a
// size/length limit; the executor calls ContinueAsNew periodically on long
// sessions (every N steps), and this wraps workflow.NewContinueAsNewError.
// memkernel and pgkernel have no such limit and don't implement this
// interface, so the executor's type assertion is simply skipped for them.
