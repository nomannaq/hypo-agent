// Package skills loads repository-owned skill bundles for new runs. Each run
// receives its own snapshot, stored with the rest of its workflow input.
package skills

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

//go:embed */SKILL.md
var bundled embed.FS

const (
	maxSkills      = 8
	maxFileSize    = 8 << 10
	maxDescription = 240
)

var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

type Metadata struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
}

type Entry struct {
	Metadata
	Body string `json:"body"`
}

type Catalog struct {
	Digest  string  `json:"digest"`
	Entries []Entry `json:"entries"`
}

func Bundled() (*Catalog, error) { return Load(bundled) }

// Load reads only a SKILL.md at the root of each skill folder. fs.Glob
// returns paths in lexical order, which also fixes prompt order and digest.
func Load(files fs.FS) (*Catalog, error) {
	paths, err := fs.Glob(files, "*/SKILL.md")
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 || len(paths) > maxSkills {
		return nil, fmt.Errorf("skill catalog needs 1–%d skills", maxSkills)
	}
	var catalog Catalog
	for _, file := range paths {
		entry, err := readEntry(files, file)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		catalog.Entries = append(catalog.Entries, entry)
	}
	catalog.Digest = catalog.digest()
	return &catalog, nil
}

func readEntry(files fs.FS, file string) (Entry, error) {
	f, err := files.Open(file)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return Entry{}, err
	}
	if len(raw) > maxFileSize {
		return Entry{}, fmt.Errorf("skill file exceeds %d bytes", maxFileSize)
	}
	if !utf8.Valid(raw) {
		return Entry{}, errors.New("skill file must be UTF-8")
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	head, body, ok := strings.Cut(strings.TrimPrefix(text, "---\n"), "\n---\n")
	if !strings.HasPrefix(text, "---\n") || !ok {
		return Entry{}, errors.New("expected YAML frontmatter followed by Markdown")
	}
	var meta Metadata
	decoder := yaml.NewDecoder(bytes.NewBufferString(head))
	decoder.KnownFields(true)
	if err := decoder.Decode(&meta); err != nil {
		return Entry{}, err
	}
	meta.Description = strings.TrimSpace(meta.Description)
	entry := Entry{Metadata: meta, Body: strings.TrimSpace(body)}
	if !entry.valid() || meta.Name != path.Dir(file) {
		return Entry{}, errors.New("skill needs a name matching its folder, a short single-line description, and a Markdown body")
	}
	return entry, nil
}

func (e Entry) valid() bool {
	return len(e.Name) <= 64 && skillName.MatchString(e.Name) &&
		len(e.Description) > 0 && len(e.Description) <= maxDescription && !strings.ContainsAny(e.Description, "\r\n") &&
		len(e.Body) > 0 && len(e.Body) <= maxFileSize
}

func (c *Catalog) digest() string {
	data, _ := json.Marshal(c.Entries) // entries contain only strings
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *Catalog) Validate() error {
	if c == nil || len(c.Entries) == 0 || len(c.Entries) > maxSkills {
		return errors.New("skill catalog snapshot is missing or out of bounds")
	}
	previous := ""
	for _, e := range c.Entries {
		if !e.valid() || e.Name <= previous {
			return fmt.Errorf("invalid or duplicate skill %q in snapshot", e.Name)
		}
		previous = e.Name
	}
	if c.Digest != c.digest() {
		return errors.New("skill catalog digest does not match its contents")
	}
	return nil
}

// Prompt lists metadata only. The model gets bodies through read_skill.
func (c *Catalog) Prompt() string {
	var out strings.Builder
	out.WriteString("\nAvailable skills\nLoad a matching skill with read_skill before following its instructions. Skills use the tools already available to this run.\n")
	for _, e := range c.Entries {
		fmt.Fprintf(&out, "- %s — %s\n", e.Name, e.Description)
	}
	return out.String()
}
