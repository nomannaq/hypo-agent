// Package tool is the tool runtime: registry, effect types, an idempotency
// ledger for side effects (pass runID:seq as the idempotency key to
// external systems — the journal only guarantees the step's recorded
// result is exactly-once, not the side effect itself), and sandboxing.
//
// Today: the registry and read-only tools (web_search, browse). Effects, the
// ledger and sandboxing come with the first write tools (roadmap Step 2b).
package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"nomanqureshi.dev/hpyo-agent/internal/model"
)

// Tool is one tool the model can call: its spec (name, description, JSON
// Schema for the arguments) and the handler that runs it inside the tool
// activity. An error from Run goes back to the model as an error result.
type Tool struct {
	Name        string
	Description string
	Params      json.RawMessage
	Effect      Effect // Read unless set: Write calls need approval
	Run         func(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry is the set of tools a run offers; it implements executor.Tools.
// Order is the order the model sees them in. The executor reads Specs in
// workflow code, so a registry must not change while runs are live.
//
// ponytail: a slice with linear lookup; a map when there are dozens of tools
// (tool discovery, roadmap Step 6).
type Registry []Tool

func (r Registry) Specs() []model.Tool {
	specs := make([]model.Tool, len(r))
	for i, t := range r {
		specs[i] = model.Tool{Type: "function", Function: model.ToolFunction{
			Name: t.Name, Description: t.Description, Parameters: t.Params,
		}}
	}
	return specs
}

// Effect reports what calling name can do; unknown tools are Read (the
// executor never calls them).
func (r Registry) Effect(name string) Effect {
	for _, t := range r {
		if t.Name == name {
			return t.Effect
		}
	}
	return Read
}

func (r Registry) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	for _, t := range r {
		if t.Name == name {
			return t.Run(ctx, args)
		}
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// Typed builds a Tool whose handler takes its arguments decoded into A.
// Malformed arguments are an error the model sees, so it can retry.
func Typed[A any](name, description, schema string, fn func(context.Context, A) (string, error)) Tool {
	return Tool{
		Name:        name,
		Description: description,
		Params:      json.RawMessage(schema),
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a A
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", fmt.Errorf("invalid arguments for %s: %v", name, err)
			}
			return fn(ctx, a)
		},
	}
}
