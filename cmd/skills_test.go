package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/skills"
)

// install has no default dir: without --dir it is a usage error and
// writes nothing, not even under HOME.
func TestSkillsInstall_RequiresDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := newSkillsInstallCmd()
	err := c.RunE(c, nil)
	var ce *output.CLIError
	if !errors.As(err, &ce) || ce.Code != output.CodeUsage {
		t.Fatalf("RunE = %v, want a usage error", err)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("install without --dir wrote under HOME: %v", entries)
	}
}

// install copies every file of a skill and reports installed, then
// unchanged, then updated once any copied file drifts — restoring it.
func TestInstallSkill_Statuses(t *testing.T) {
	s, ok, err := skills.Get("cashctl-cash")
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	dir := t.TempDir()
	root := filepath.Join(dir, s.Name)

	for _, want := range []string{"installed", "unchanged"} {
		r, err := installSkill(dir, s)
		if err != nil || r.Status != want || r.Path != root {
			t.Fatalf("installSkill = %+v, %v; want status %q at %s", r, err, want, root)
		}
	}
	for _, rel := range s.Files {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s not installed: %v", rel, err)
		}
	}
	last := filepath.Join(root, s.Files[len(s.Files)-1])
	if err := os.WriteFile(last, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, err := installSkill(dir, s); err != nil || r.Status != "updated" {
		t.Fatalf("installSkill after edit = %+v, %v; want updated", r, err)
	}
	want, _ := s.ReadFile(s.Files[len(s.Files)-1])
	if got, _ := os.ReadFile(last); string(got) != string(want) {
		t.Fatal("edited file not restored")
	}
}
