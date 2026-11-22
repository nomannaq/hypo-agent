package skills

import (
	"context"
	"fmt"

	"nomanqureshi.dev/hpyo-agent/internal/tool"
)

const ReadToolName = "read_skill"

type catalogKey struct{}

// WithCatalog carries the run's persisted bundle into the tool handler.
// Only the executor sets it. The model supplies an exact skill name.
func WithCatalog(ctx context.Context, catalog *Catalog) context.Context {
	return context.WithValue(ctx, catalogKey{}, catalog)
}

func ReadTool() tool.Tool {
	return tool.Typed(ReadToolName,
		"Load the instructions for an available skill by its exact catalog name.",
		`{"type":"object","properties":{"name":{"type":"string","maxLength":64}},"required":["name"]}`,
		func(ctx context.Context, args struct {
			Name string `json:"name"`
		}) (string, error) {
			catalog, _ := ctx.Value(catalogKey{}).(*Catalog)
			if catalog != nil {
				for _, e := range catalog.Entries {
					if e.Name == args.Name {
						return e.Body, nil
					}
				}
			}
			return "", fmt.Errorf("unknown skill %q; use a name from the available skills list", args.Name)
		})
}
