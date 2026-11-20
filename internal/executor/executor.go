// Package executor is the agent loop: internal/agent's pure reducer plus
// policy (guards, effects, approvals), driven through a kernel.Ctx. It's
// the one package that imports both agent and kernel, and the only place
// internal/tool and internal/model get called from.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"nomanqureshi.dev/hpyo-agent/internal/agent"
	"nomanqureshi.dev/hpyo-agent/internal/kernel"
	"nomanqureshi.dev/hpyo-agent/internal/model"
)

// Model is the part of model.Client the loop needs; tests pass a scripted fake.
type Model interface {
	Complete(ctx context.Context, req model.Request) (model.Response, error)
}

// Tools resolves tool calls. Step 2's tool.Registry implements it.
type Tools interface {
	Specs() []model.Tool
	Call(ctx context.Context, name string, args json.RawMessage) (string, error)
}

// Limits are the run's guards. Tripping one ends the run with a Failed event;
// it is never an error from Run.
type Limits struct {
	MaxTurns      int           // model calls per run
	MaxToolCalls  int           // tool calls in one model reply
	MaxTokens     int           // prompt+completion budget; 0 = unlimited
	MaxToolOutput int           // bytes of one tool result kept in context
	StepTimeout   time.Duration // per model or tool call
}

var DefaultLimits = Limits{
	MaxTurns:      8,
	MaxToolCalls:  8,
	MaxToolOutput: 16 << 10,
	StepTimeout:   2 * time.Minute,
}

// Runner holds both halves of an agent run: workflow-side config (System,
// Limits, the tool specs offered to the model) used by Workflow/Run, and
// activity-side dependencies (Model, Tools) used by Activities. On Temporal
// the two halves run in different places; the worker registers both.
type Runner struct {
	Model  Model
	Tools  Tools // nil: the model is offered no tools
	System string
	Limits Limits
	// Events persists a run's events (run_events) so the UI can stream them.
	// Activity side, like Model. nil: events aren't persisted (tests).
	Events func(ctx context.Context, runID string, seq int, e agent.Event) error
}

// WorkflowName is the name the agent workflow is registered and started
// under. The server starts it; the worker registers it.
const WorkflowName = "agent"

// SystemPrompt is the agent's system prompt.
const SystemPrompt = "You are Hypo, a helpful research assistant. Be concise and clear."

// The activities a run calls. Names are part of every journal: renaming one
// breaks replay of runs recorded under the old name.
var (
	modelActivity = kernel.Activity[model.Request, model.Response]{Name: "model.complete"}
	toolActivity  = kernel.Activity[toolInput, toolResult]{Name: "tool.call"}
	eventActivity = kernel.Activity[eventInput, struct{}]{Name: "events.append"}
)

type eventInput struct {
	RunID string      `json:"run_id"`
	Seq   int         `json:"seq"`
	Event agent.Event `json:"event"`
}

type toolInput struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

func (r *Runner) limits() Limits {
	if r.Limits == (Limits{}) {
		return DefaultLimits
	}
	return r.Limits
}

// Activities returns the activity bodies this runner's workflow calls, keyed
// by name. Register them with the kernel (or Temporal worker) that runs it.
func (r *Runner) Activities() map[string]kernel.ActivityFunc {
	lim := r.limits()
	return map[string]kernel.ActivityFunc{
		modelActivity.Name: modelActivity.Func(func(ctx context.Context, req model.Request) (model.Response, error) {
			ctx, cancel := context.WithTimeout(ctx, lim.StepTimeout)
			defer cancel()
			return r.Model.Complete(ctx, req)
		}),
		// Tool failures (handler error, timeout) are results the model gets to
		// see, so they return a nil error and get journaled like any output.
		toolActivity.Name: toolActivity.Func(func(ctx context.Context, in toolInput) (toolResult, error) {
			if r.Tools == nil {
				return toolResult{Output: fmt.Sprintf("unknown tool %q", in.Name), IsError: true}, nil
			}
			parent := ctx
			ctx, cancel := context.WithTimeout(ctx, lim.StepTimeout)
			defer cancel()
			out, err := r.Tools.Call(ctx, in.Name, in.Args)
			if err != nil {
				if parent.Err() != nil {
					return toolResult{}, parent.Err() // the worker is going away: not a tool failure
				}
				return toolResult{Output: err.Error(), IsError: true}, nil
			}
			if len(out) > lim.MaxToolOutput {
				out = out[:lim.MaxToolOutput] + "\n…[truncated]"
			}
			return toolResult{Output: out}, nil
		}),
		// A retried append writes the same (run, seq), so Events must ignore
		// duplicates: the activity can run again after its write landed.
		eventActivity.Name: eventActivity.Func(func(ctx context.Context, in eventInput) (struct{}, error) {
			if r.Events == nil {
				return struct{}{}, nil
			}
			return struct{}{}, r.Events(ctx, in.RunID, in.Seq, in.Event)
		}),
	}
}

type Input struct {
	History []agent.Message `json:"history,omitempty"`
	Prompt  string          `json:"prompt"`
}

// Workflow adapts Run to a kernel's registered-workflow shape: JSON Input in,
// JSON agent.State out.
func (r *Runner) Workflow(c kernel.Ctx, input []byte) ([]byte, error) {
	var in Input
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("executor: bad input: %w", err)
	}
	s, err := r.Run(c, in)
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

// Run drives one turn: model -> tool calls -> results -> model ... until the
// model answers without tools or a guard trips. Every model and tool call is a
// kernel activity call, so a resumed run replays them instead of calling again.
//
// The returned error is only for the kernel's own failures (cancellation,
// i.e. a crash or shutdown, and nondeterminism), which mean "resume or fix the
// code", not "the agent failed". Model errors and guards end in State.Status ==
// agent.Stopped.
func (r *Runner) Run(c kernel.Ctx, in Input) (agent.State, error) {
	lim := r.limits()
	var specs []model.Tool
	if r.Tools != nil {
		specs = r.Tools.Specs()
	}

	s := agent.Start(in.History)
	// apply folds e into s and, if events are persisted, appends it as the
	// run's next event. The seq is the event's index in this run, so it is
	// the same on every replay.
	seq := 0
	apply := func(e agent.Event) error {
		next, err := agent.Reduce(s, e)
		if err != nil {
			return err
		}
		s = next
		if r.Events == nil {
			return nil
		}
		// ponytail: a failed append (Postgres down past the retries) fails the
		// workflow; the stream reports it as a run that ended without a reply.
		_, err = kernel.Call(c, fmt.Sprintf("event/%d", seq), eventActivity, eventInput{RunID: c.RunID(), Seq: seq, Event: e})
		seq++
		return err
	}
	if err := apply(agent.Event{Kind: agent.UserMessage, Text: in.Prompt}); err != nil {
		return s, err
	}
	fail := func(reason string) error {
		return apply(agent.Event{Kind: agent.Failed, Err: reason})
	}

	for {
		switch s.Status {
		case agent.Done, agent.Stopped:
			return s, nil

		case agent.Thinking:
			if s.Turns >= lim.MaxTurns {
				if err := fail(fmt.Sprintf("turn limit (%d) reached", lim.MaxTurns)); err != nil {
					return s, err
				}
				continue
			}
			if lim.MaxTokens > 0 && s.Usage.Total() >= lim.MaxTokens {
				if err := fail(fmt.Sprintf("token budget (%d) exhausted", lim.MaxTokens)); err != nil {
					return s, err
				}
				continue
			}
			req := model.Request{Messages: toModel(r.System, s.Messages), Tools: specs}
			resp, err := kernel.Call(c, fmt.Sprintf("model/%d", s.Turns), modelActivity, req)
			if err != nil {
				if fatal(err) {
					return s, err
				}
				if err := fail("model: " + err.Error()); err != nil {
					return s, err
				}
				continue
			}
			if len(resp.ToolCalls) > lim.MaxToolCalls {
				if err := fail(fmt.Sprintf("model asked for %d tool calls; the limit is %d", len(resp.ToolCalls), lim.MaxToolCalls)); err != nil {
					return s, err
				}
				continue
			}
			if err := apply(agent.Event{
				Kind:      agent.ModelReply,
				Text:      resp.Content,
				ToolCalls: fromModel(resp.ToolCalls),
				Usage:     agent.Usage{PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens},
			}); err != nil {
				return s, err
			}

		case agent.Acting:
			call := s.Pending[0]
			res, err := r.callTool(c, s.Turns, call, specs)
			if err != nil {
				return s, err
			}
			if err := apply(agent.Event{Kind: agent.ToolResult, CallID: call.ID, Text: res.Output, IsError: res.IsError}); err != nil {
				return s, err
			}

		default:
			return s, fmt.Errorf("executor: unexpected status %q", s.Status)
		}
	}
}

type toolResult struct {
	Output  string `json:"output"`
	IsError bool   `json:"is_error"`
}

// callTool runs one tool call as an activity. An unknown tool is answered
// here, without a call: the model sees the error and can recover.
func (r *Runner) callTool(c kernel.Ctx, turn int, call agent.ToolCall, specs []model.Tool) (toolResult, error) {
	if !offered(specs, call.Name) {
		return toolResult{Output: fmt.Sprintf("unknown tool %q", call.Name), IsError: true}, nil
	}
	// Model call IDs can repeat across turns, so the turn is part of the step.
	return kernel.Call(c, fmt.Sprintf("tool/%d/%s", turn, call.ID), toolActivity, toolInput{Name: call.Name, Args: call.Args})
}

// fatal reports errors that must stop the loop instead of becoming a Failed
// event: the run was cancelled (crash, shutdown) or replay diverged.
func fatal(err error) bool {
	return errors.Is(err, kernel.ErrCanceled) || errors.Is(err, kernel.ErrNondeterminism)
}

func offered(specs []model.Tool, name string) bool {
	for _, t := range specs {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

func toModel(system string, msgs []agent.Message) []model.Message {
	out := make([]model.Message, 0, len(msgs)+1)
	if system != "" {
		out = append(out, model.Message{Role: "system", Content: system})
	}
	for _, m := range msgs {
		mm := model.Message{Role: string(m.Role), Content: m.Text, ToolCallID: m.CallID}
		if m.IsError {
			mm.Content = "Error: " + m.Text
		}
		for _, tc := range m.ToolCalls {
			mm.ToolCalls = append(mm.ToolCalls, model.ToolCall{
				ID: tc.ID, Type: "function",
				Function: model.FunctionCall{Name: tc.Name, Arguments: string(tc.Args)},
			})
		}
		out = append(out, mm)
	}
	return out
}

func fromModel(calls []model.ToolCall) []agent.ToolCall {
	out := make([]agent.ToolCall, 0, len(calls))
	for _, tc := range calls {
		args := json.RawMessage(tc.Function.Arguments)
		if !json.Valid(args) {
			// Keep malformed arguments as a JSON string so the state stays
			// serializable; the tool will reject them and the model sees why.
			args, _ = json.Marshal(tc.Function.Arguments)
		}
		out = append(out, agent.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args})
	}
	return out
}
