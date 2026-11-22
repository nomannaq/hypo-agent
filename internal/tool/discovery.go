package tool

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode"

	"nomanqureshi.dev/hpyo-agent/internal/model"
)

const SearchToolsName = "search_tools"

// SearchTools is executed inside the workflow against its authorized catalog.
func SearchTools() Tool {
	return Tool{
		Name:        SearchToolsName,
		Description: "Find tools for one capability by tool name or task. The matching tools are available starting with your next reply.",
		Params:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Exact tool name or a short task description","maxLength":256}},"required":["query"]}`),
		Run:         func(context.Context, json.RawMessage) (string, error) { return "", errExecutorTool },
	}
}

// These lists and their order are part of agent-discovery-v1 replay.
var discoveryBase = []string{
	"web_search", "browse", AskUserName, "todo_write", "read_skill", SendMessageName, SearchToolsName,
}

var discoveryGroups = []struct {
	names   []string
	aliases string
}{
	{
		names: []string{"browser_open", "browser_snapshot", "browser_click", "browser_type", "browser_select",
			"browser_press", "browser_scroll", "browser_back", "browser_wait", "browser_screenshot"},
		aliases: "browser interact interactive page website open navigate snapshot click type fill form submit select dropdown press scroll back wait screenshot capture",
	},
	{names: []string{ResearchName}, aliases: "research parallel subagent subagents independent delegate investigate investigation"},
	{names: []string{"remember", "recall", "session_search"}, aliases: "memory memories preference preferences remember recall note notes past conversation conversations chat chats transcript"},
	{names: []string{"schedule"}, aliases: "schedule scheduling later future remind reminder recurring daily weekly tomorrow repeat"},
}

func IsBaseTool(name string) bool {
	return slices.Contains(discoveryBase, name)
}

// FindTools matches one capability within the caller's authorized catalog.
// Returned specs keep registry order.
func FindTools(query string, authorized []model.Tool) []model.Tool {
	query = strings.ToLower(strings.TrimSpace(query))
	tokens := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	slices.Sort(tokens)
	tokens = slices.Compact(tokens)
	for _, spec := range authorized {
		if spec.Function.Name == query && IsBaseTool(query) {
			return []model.Tool{spec}
		}
	}
	var best []model.Tool
	bestScore := 0
	for _, group := range discoveryGroups {
		var matches []model.Tool
		for _, spec := range authorized {
			if slices.Contains(group.names, spec.Function.Name) {
				matches = append(matches, spec)
			}
		}
		if len(matches) == 0 {
			continue
		}
		terms := strings.Fields(group.aliases)
		for _, spec := range matches {
			if spec.Function.Name == query {
				return matches
			}
			terms = append(terms, strings.Split(spec.Function.Name, "_")...)
		}
		score := 0
		for _, token := range tokens {
			if slices.Contains(terms, token) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = matches, score
		}
	}
	return best
}
