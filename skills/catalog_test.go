package skills

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadValidatesFrontmatter(t *testing.T) {
	const front = "---\nname: a\ndescription: A small skill.\n---\n"
	for _, tt := range []struct {
		name string
		file string
	}{
		{"missing-frontmatter", "# Instructions"},
		{"name-mismatch", strings.Replace(front, "name: a", "name: b", 1) + "body"},
		{"unknown-key", strings.Replace(front, "name: a", "name: a\npermission: write", 1) + "body"},
		{"duplicate-key", strings.Replace(front, "name: a", "name: a\nname: a", 1) + "body"},
		{"blank-description", strings.Replace(front, "A small skill.", "", 1) + "body"},
		{"blank-instructions", front},
		{"oversized", front + strings.Repeat("a", maxFileSize)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(fstest.MapFS{"a/SKILL.md": {Data: []byte(tt.file)}}); err == nil {
				t.Fatal("invalid skill accepted")
			}
		})
	}

	catalog, err := Load(fstest.MapFS{
		"z/SKILL.md": {Data: []byte(strings.Replace(front, "name: a", "name: z", 1) + "body two")},
		"a/SKILL.md": {Data: []byte(front + "body one")},
	})
	if err != nil || catalog.Entries[0].Name != "a" || len(catalog.Digest) != 64 {
		t.Fatalf("catalog = %+v, %v", catalog, err)
	}
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	catalog.Entries[0].Body = "changed after admission"
	if err := catalog.Validate(); err == nil {
		t.Fatal("tampered snapshot passed digest verification")
	}
}

func TestReadSkillUsesAnExactCatalogName(t *testing.T) {
	catalog, err := Bundled()
	if err != nil {
		t.Fatal(err)
	}
	read := ReadTool()
	ctx := WithCatalog(t.Context(), catalog)
	for _, name := range []string{"./hypothesis-research", "../hypothesis-research", "hypothesis-research/SKILL.md", "other"} {
		args, _ := json.Marshal(map[string]string{"name": name})
		if out, err := read.Run(ctx, args); out != "" || err == nil {
			t.Errorf("name %q: returned %q, %v", name, out, err)
		}
	}
	args := json.RawMessage(`{"name":"hypothesis-research"}`)
	body, err := read.Run(ctx, args)
	if err != nil || !strings.Contains(body, "# Hypothesis research") {
		t.Fatalf("bundled skill body = %q, %v", body, err)
	}
	if prompt := catalog.Prompt(); !strings.Contains(prompt, catalog.Entries[0].Description) || strings.Contains(prompt, body) {
		t.Fatalf("index includes incomplete metadata or full instructions: %q", prompt)
	}
}
