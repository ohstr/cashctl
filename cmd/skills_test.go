package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ohstr/cashctl/skills"
)

// install reports installed, then unchanged, then updated once a copy
// drifts — and the drifted copy is brought back in line.
func TestInstallSkill_Statuses(t *testing.T) {
	dir := t.TempDir()
	s := skills.Skill{Name: "demo", Content: "---\nname: demo\n---\n"}
	path := filepath.Join(dir, "demo", "SKILL.md")

	for _, want := range []string{"installed", "unchanged"} {
		r, err := installSkill(dir, s)
		if err != nil || r.Status != want || r.Path != path {
			t.Fatalf("installSkill = %+v, %v; want status %q at %s", r, err, want, path)
		}
	}
	if err := os.WriteFile(path, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, err := installSkill(dir, s); err != nil || r.Status != "updated" {
		t.Fatalf("installSkill after edit = %+v, %v; want updated", r, err)
	}
	if got, _ := os.ReadFile(path); string(got) != s.Content {
		t.Fatalf("file = %q, want embedded content", got)
	}
}
