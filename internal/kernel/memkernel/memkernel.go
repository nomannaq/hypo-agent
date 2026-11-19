// Package memkernel is the in-memory kernel.Ctx implementation: no
// persistence, no replay across process restarts. It exists to develop and
// unit-test internal/executor before any durable backend is wired up, and
// to run kernel/conformance against something trivially correct.
//
// TODO: implement kernel.Ctx (Step runs fn inline and records it in a
// per-run slice so a second Step with the same name returns the cached
// result within the same process run).
package memkernel
