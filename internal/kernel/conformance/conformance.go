// Package conformance is kernel/conformance: one test suite that exercises
// kernel.Ctx — Step replay, Sleep, Await, Spawn, Now/Rand determinism — run
// against every implementation (memkernel, pgkernel, temporalkernel). If
// the same suite passes against all three, they're interchangeable in fact,
// not just by the shape of the interface.
//
// TODO: Run(t *testing.T, newCtx func(t *testing.T) kernel.Ctx), called
// from each implementation's own _test.go.
package conformance
