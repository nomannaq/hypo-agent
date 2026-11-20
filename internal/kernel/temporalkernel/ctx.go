// Package temporalkernel adapts kernel.Ctx onto the Temporal Go SDK.
// kernel.Ctx.Call maps to a registered Temporal activity, called by name with
// explicit input (see .junie/plans/durability.md).
//
// TODO: Ctx wraps workflow.Context. Call -> workflow.ExecuteActivity,
// Now() -> workflow.Now, Rand() ->
// workflow.SideEffect-backed source, Sleep() -> workflow.NewTimer,
// Await() -> workflow.GetSignalChannel + a timer via workflow.Selector,
// Spawn() -> workflow.ExecuteChildWorkflow of the same generic workflow.
package temporalkernel
