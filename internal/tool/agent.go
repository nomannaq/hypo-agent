package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Tools about the run itself rather than the web. ask_user and research are
// offered to the model like any tool, but the executor carries them out in
// workflow code (a durable wait for the user; child runs), so their Run is
// never called.

const (
	AskUserName  = "ask_user"
	ResearchName = "research"

	// MaxResearchTasks caps the subagents one research call starts.
	MaxResearchTasks = 4
)

var errExecutorTool = errors.New("this tool is carried out by the executor, not called directly")

// AskUser returns ask_user: the run pauses until the user answers in the chat
// (or the wait times out), and the answer is the tool's result.
func AskUser() Tool {
	return Tool{
		Name: AskUserName,
		Description: "Ask the user a question and wait for the answer, when you need a decision or information only they have " +
			"(which option to pick, a detail for a form, whether to continue). Don't ask what you can find out yourself. " +
			"Offer options when the choice is between a few known answers.",
		Params: json.RawMessage(`{"type":"object","properties":{` +
			`"question":{"type":"string"},` +
			`"options":{"type":"array","items":{"type":"string"},"maxItems":6,"description":"Suggested answers; the user can also write their own"}},` +
			`"required":["question"]}`),
		Run: func(context.Context, json.RawMessage) (string, error) { return "", errExecutorTool },
	}
}

// Research returns research: it starts up to MaxResearchTasks subagents in
// parallel, each a full agent run with the read-only tools, and returns their
// findings. For questions with several independent parts.
func Research() Tool {
	return Tool{
		Name: ResearchName,
		Description: fmt.Sprintf("Split deep research into up to %d independent tasks, run them in parallel by subagents "+
			"(each searches and reads the web on its own), and get back each one's findings with sources. "+
			"Use it for questions with several separate parts; do simple lookups yourself. "+
			"Each prompt must stand alone: the subagents don't see this conversation.", MaxResearchTasks),
		Params: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"tasks":{"type":"array","minItems":1,"maxItems":%d,"items":{"type":"object","properties":{`+
			`"title":{"type":"string","description":"A short name for the task"},`+
			`"prompt":{"type":"string","description":"Complete instructions for the subagent"}},"required":["title","prompt"]}}},"required":["tasks"]}`, MaxResearchTasks)),
		Run: func(context.Context, json.RawMessage) (string, error) { return "", errExecutorTool },
	}
}

// todoStatuses are the states a todo item can be in.
var todoStatuses = []string{"pending", "in_progress", "completed"}

// TodoWrite returns todo_write: the agent's plan for multi-step work. The
// list lives in the call's arguments (the chat renders the latest one), so the
// tool only checks it.
func TodoWrite() Tool {
	return Typed("todo_write",
		"Write your plan for multi-step work as a todo list the user sees, and update it as you go: one item per concrete step, "+
			"exactly one in_progress while work remains, each marked completed as soon as it is done. Send the whole list every time. Skip it for one-step tasks.",
		`{"type":"object","properties":{"todos":{"type":"array","maxItems":20,"items":{"type":"object","properties":{`+
			`"content":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed"]}},"required":["content","status"]}}},"required":["todos"]}`,
		func(_ context.Context, in struct {
			Todos []struct {
				Content string `json:"content"`
				Status  string `json:"status"`
			} `json:"todos"`
		}) (string, error) {
			if len(in.Todos) > 20 {
				return "", errors.New("keep the list to 20 items or fewer")
			}
			count := map[string]int{}
			for i, t := range in.Todos {
				if strings.TrimSpace(t.Content) == "" {
					return "", fmt.Errorf("todo %d has no content", i)
				}
				ok := false
				for _, s := range todoStatuses {
					ok = ok || t.Status == s
				}
				if !ok {
					return "", fmt.Errorf("todo %d: status must be one of %s, got %q", i, strings.Join(todoStatuses, ", "), t.Status)
				}
				count[t.Status]++
			}
			return fmt.Sprintf("Todo list updated: %d completed, %d in progress, %d pending.",
				count["completed"], count["in_progress"], count["pending"]), nil
		})
}
