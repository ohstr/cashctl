package appdir

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDir_DefaultsUnderUserConfigDir(t *testing.T) {
	SetOverride("")
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error = %v", err)
	}
	want := filepath.Join(tmp, "cashctl")
	if dir != want {
		t.Errorf("Dir() = %q, want %q", dir, want)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("Dir() did not create the directory: %v", err)
	}
}

func TestDir_OverrideWins(t *testing.T) {
	tmp := t.TempDir()
	override := filepath.Join(tmp, "custom")
	SetOverride(override)
	t.Cleanup(func() { SetOverride("") })

	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error = %v", err)
	}
	if dir != override {
		t.Errorf("Dir() = %q, want override %q", dir, override)
	}
}

// An existing directory open to group or others is made private, once, and
// said so; an already-private one is left alone silently.
func TestDir_TightensALooseExistingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes on Windows")
	}
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	SetOverride(dir)
	t.Cleanup(func() { SetOverride("") })
	var out bytes.Buffer
	notices = &out
	t.Cleanup(func() { notices = os.Stderr })

	for i := 0; i < 2; i++ {
		if _, err := Dir(); err != nil {
			t.Fatalf("Dir() error = %v", err)
		}
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}
	if n := strings.Count(out.String(), "Made "); n != 1 {
		t.Errorf("notices = %q, want exactly one", out.String())
	}
}
