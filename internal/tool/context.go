package tool

import "context"

type callKey struct{}

type call struct{ runID, key string }

// WithCall is how the executor tells a tool which run and step it is serving:
// the run ID scopes per-run state (the browser session), and key (run ID +
// step) is the idempotency key for anything the call writes. The model never
// supplies these: a model-chosen run ID could reach another run's session.
func WithCall(ctx context.Context, runID, key string) context.Context {
	return context.WithValue(ctx, callKey{}, call{runID, key})
}

// RunID is the run the current tool call belongs to; "" outside a run.
func RunID(ctx context.Context) string {
	c, _ := ctx.Value(callKey{}).(call)
	return c.runID
}

// CallKey is the current call's idempotency key (run ID + "/" + step).
func CallKey(ctx context.Context) string {
	c, _ := ctx.Value(callKey{}).(call)
	return c.key
}
