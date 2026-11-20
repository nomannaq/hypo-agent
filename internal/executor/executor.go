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

type Runner struct {
	Model  Model
	Tools  Tools // nil: the model is offered no tools
	System string
	Limits Limits
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
// kernel Step, so a resumed run replays them instead of calling again.
//
// The returned error is only for the kernel's own failures (cancellation,
// i.e. a crash or shutdown, and nondeterminism), which mean "resume or fix the
// code", not "the agent failed". Model errors and guards end in State.Status ==
// agent.Stopped.
func (r *Runner) Run(c kernel.Ctx, in Input) (agent.State, error) {
	lim := r.Limits
	if lim == (Limits{}) {
		lim = DefaultLimits
	}
	var specs []model.Tool
	if r.Tools != nil {
		specs = r.Tools.Specs()
	}

	s, err := agent.Reduce(agent.Start(in.History), agent.Event{Kind: agent.UserMessage, Text: in.Prompt})
	if err != nil {
		return s, err
	}
	fail := func(reason string) error {
		s, err = agent.Reduce(s, agent.Event{Kind: agent.Failed, Err: reason})
		return err
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
			resp, err := kernel.StepT(c, fmt.Sprintf("model/%d", s.Turns), func(ctx context.Context) (model.Response, error) {
				ctx, cancel := context.WithTimeout(ctx, lim.StepTimeout)
				defer cancel()
				return r.Model.Complete(ctx, req)
			})
			if err != nil {
				if fatal(c, err) {
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
			if s, err = agent.Reduce(s, agent.Event{
				Kind:      agent.ModelReply,
				Text:      resp.Content,
				ToolCalls: fromModel(resp.ToolCalls),
				Usage:     agent.Usage{PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens},
			}); err != nil {
				return s, err
			}

		case agent.Acting:
			call := s.Pending[0]
			res, err := r.callTool(c, s.Turns, call, lim)
			if err != nil {
				return s, err
			}
			if s, err = agent.Reduce(s, agent.Event{Kind: agent.ToolResult, CallID: call.ID, Text: res.Output, IsError: res.IsError}); err != nil {
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

// callTool runs one tool call as a Step. Tool failures (unknown tool, handler
// error, timeout) are results the model gets to see, not run failures.
func (r *Runner) callTool(c kernel.Ctx, turn int, call agent.ToolCall, lim Limits) (toolResult, error) {
	if r.Tools == nil || !offered(r.Tools.Specs(), call.Name) {
		return toolResult{Output: fmt.Sprintf("unknown tool %q", call.Name), IsError: true}, nil
	}
	// Model call IDs can repeat across turns, so the turn is part of the name.
	res, err := kernel.StepT(c, fmt.Sprintf("tool/%d/%s", turn, call.ID), func(ctx context.Context) (toolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, lim.StepTimeout)
		defer cancel()
		out, err := r.Tools.Call(ctx, call.Name, call.Args)
		if err != nil {
			if c.Err() != nil {
				return toolResult{}, c.Err() // crash/shutdown: don't journal
			}
			return toolResult{Output: err.Error(), IsError: true}, nil
		}
		if len(out) > lim.MaxToolOutput {
			out = out[:lim.MaxToolOutput] + "\n…[truncated]"
		}
		return toolResult{Output: out}, nil
	})
	return res, err
}

// fatal reports errors that must stop the loop instead of becoming a Failed
// event: the run was cancelled (crash, shutdown) or replay diverged.
func fatal(c kernel.Ctx, err error) bool {
	return c.Err() != nil || errors.Is(err, kernel.ErrNondeterminism)
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
