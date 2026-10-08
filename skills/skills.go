// Package skills embeds the agent skills under this directory, so the
// guidance an agent reads always matches the binary it is driving.
package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed */SKILL.md
var files embed.FS

// Skill is one embedded SKILL.md and its frontmatter fields.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"-"`
}

// All returns every embedded skill, sorted by name.
func All() ([]Skill, error) {
	paths, err := fs.Glob(files, "*/SKILL.md")
	if err != nil {
		return nil, err
	}
	out := make([]Skill, 0, len(paths))
	for _, p := range paths {
		raw, err := files.ReadFile(p)
		if err != nil {
			return nil, err
		}
		s := Skill{Content: string(raw)}
		s.Name, s.Description = frontmatter(s.Content)
		if s.Name == "" {
			return nil, fmt.Errorf("%s: frontmatter has no name", p)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns the embedded skill called name; ok is false if there is none.
func Get(name string) (Skill, bool, error) {
	all, err := All()
	if err != nil {
		return Skill{}, false, err
	}
	for _, s := range all {
		if s.Name == name {
			return s, true, nil
		}
	}
	return Skill{}, false, nil
}

// frontmatter reads the single-line name and description keys from a
// leading "---" block — all these files use, so no YAML dependency.
func frontmatter(content string) (name, description string) {
	body, ok := strings.CutPrefix(content, "---\n")
	if !ok {
		return "", ""
	}
	block, _, ok := strings.Cut(body, "\n---")
	if !ok {
		return "", ""
	}
	for _, line := range strings.Split(block, "\n") {
		if v, ok := strings.CutPrefix(line, "name:"); ok {
			name = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "description:"); ok {
			description = strings.TrimSpace(v)
		}
	}
	return name, description
}
