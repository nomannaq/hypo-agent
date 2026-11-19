// Package executor is the agent loop: internal/agent's pure reducer plus
// policy (guards, effects, approvals), driven through a kernel.Ctx. It's
// the one package that imports both agent and kernel, and the only place
// internal/tool and internal/model get called from.
package executor

// TODO: Run(ctx kernel.Ctx, input []byte) ([]byte, error) — the function
// registered as the workflow body in every kernel implementation.
