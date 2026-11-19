// Package agent is the pure domain core: State × Event -> State. It has no
// dependency on internal/kernel, internal/tool, or internal/model, which is
// what makes it fast and deterministic to test (golden tests, no I/O).
// internal/executor is the only package that wires this reducer to a
// kernel.Ctx and a tool runtime.
package agent

// TODO: Reduce(state State, event Event) (State, error)
