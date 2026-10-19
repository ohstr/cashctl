package skills

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every skill directory on disk, and every file in it, must be embedded, with
// a name matching its directory — a new skill that fails either would silently go missing
// from `cashctl skills`.
func TestAll_EmbedsEverySkillDirectory(t *testing.T) {
	onDisk, err := filepath.Glob("*/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 || len(all) != len(onDisk) {
		t.Fatalf("embedded %d skills, %d on disk — is a new skill directory outside the cashctl-* embed pattern?", len(all), len(onDisk))
	}
	for _, s := range all {
		raw, err := os.ReadFile(filepath.Join(s.Name, "SKILL.md"))
		if err != nil {
			t.Errorf("%s: name doesn't match a directory: %v", s.Name, err)
			continue
		}
		if string(raw) != s.Content {
			t.Errorf("%s: embedded content differs from disk", s.Name)
		}
		var onDiskFiles []string
		_ = filepath.WalkDir(s.Name, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(s.Name, p)
				onDiskFiles = append(onDiskFiles, filepath.ToSlash(rel))
			}
			return err
		})
		if !slices.Equal(s.Files, onDiskFiles) {
			t.Errorf("%s: embedded files %v, on disk %v", s.Name, s.Files, onDiskFiles)
		}
		if s.Description == "" {
			t.Errorf("%s: empty description", s.Name)
		}
	}
}

func TestGet_UnknownName(t *testing.T) {
	if _, ok, err := Get("no-such-skill"); ok || err != nil {
		t.Fatalf("Get(unknown) = ok %v, err %v; want false, nil", ok, err)
	}
}

func TestFrontmatter(t *testing.T) {
	name, desc := frontmatter("---\nname: x\ndescription: does y\nlicense: z\n---\nbody\n")
	if name != "x" || desc != "does y" {
		t.Fatalf("got %q, %q", name, desc)
	}
	if name, _ := frontmatter("no frontmatter\n"); name != "" {
		t.Fatalf("got name %q from a file without frontmatter", name)
	}
}

// An installed skill can't point at AGENTS.md: it doesn't ship with the binary.
func TestAll_NoSkillRefersToAgentsMD(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		for _, rel := range s.Files {
			raw, err := s.ReadFile(rel)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "AGENTS.md") {
				t.Errorf("%s/%s refers to AGENTS.md, which an installed binary doesn't have", s.Name, rel)
			}
		}
	}
}
