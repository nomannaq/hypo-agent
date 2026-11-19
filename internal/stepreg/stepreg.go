// Package stepreg is a process-wide registry of step functions, keyed by
// name. temporalkernel's generic "RunStep" activity looks functions up here
// because Temporal activities must be statically registered, unlike
// memkernel/pgkernel, which call the fn passed to kernel.Ctx.Step directly
// and never need this package.
package stepreg

import (
	"context"
	"fmt"
	"sync"
)

// Func is a step body: the same shape as the fn argument to kernel.Ctx.Step.
type Func func(context.Context, []byte) ([]byte, error)

var (
	mu  sync.RWMutex
	reg = map[string]Func{}
)

// Register adds fn under name at startup. It panics on a duplicate name —
// that's a programming error to catch at boot, not a runtime condition.
func Register(name string, fn Func) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := reg[name]; exists {
		panic(fmt.Sprintf("stepreg: %q already registered", name))
	}
	reg[name] = fn
}

// Lookup returns the function registered under name, or false if none.
func Lookup(name string) (Func, bool) {
	mu.RLock()
	defer mu.RUnlock()
	fn, ok := reg[name]
	return fn, ok
}
