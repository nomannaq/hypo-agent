package tool

import (
	"slices"
	"testing"

	"nomanqureshi.dev/hpyo-agent/internal/model"
)

func TestFindTools(t *testing.T) {
	names := []string{"schedule", "browser_snapshot", "web_search", "research", "browser_open", "session_search", "recall"}
	var specs []model.Tool
	for _, name := range names {
		specs = append(specs, model.Tool{Function: model.ToolFunction{Name: name}})
	}
	tests := []struct {
		query string
		want  []string
	}{
		{"  BROWSER_OPEN ", []string{"browser_snapshot", "browser_open"}},
		{"fill in a form and take a screenshot", []string{"browser_snapshot", "browser_open"}},
		{"find my past conversations", []string{"session_search", "recall"}},
		{"remember a preference", []string{"session_search", "recall"}},
		{"remind me tomorrow on a weekly schedule", []string{"schedule"}},
		{"delegate parallel research", []string{"research"}},
		{"browser memory", []string{"browser_snapshot", "browser_open"}},
		{"memory memory memory browser form", []string{"browser_snapshot", "browser_open"}},
		{"web_search", []string{"web_search"}},
		{"unrecognized", nil},
		{"", nil},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			var got []string
			for _, spec := range FindTools(test.query, specs) {
				got = append(got, spec.Function.Name)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("matches = %v, want %v", got, test.want)
			}
		})
	}
}
