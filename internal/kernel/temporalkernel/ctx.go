// Package temporalkernel adapts kernel.Ctx onto the Temporal Go SDK. A
// Temporal workflow is a registered, deterministic function — it can't run
// an arbitrary closure the way memkernel/pgkernel's Step does — so Step
// bodies here are dispatched through internal/stepreg by name and run as
// Activities on a separate worker (cmd/worker-activity), not inline.
//
// TODO: Ctx wraps workflow.Context. Now() -> workflow.Now, Rand() ->
// workflow.SideEffect-backed source, Sleep() -> workflow.NewTimer,
// Await() -> workflow.GetSignalChannel + a timer via workflow.Selector,
// Spawn() -> workflow.ExecuteChildWorkflow of the same generic workflow.
package temporalkernel
