//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// cashctlBinary builds cashctl's module-root main package once per test run
// (cached across every test in the package) and returns the path to the
// resulting binary — real black-box testing against the actual compiled
// artifact, not the source tree.
func cashctlBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cashctl-integration-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "cashctl")
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		cmd.Dir = ".." // module root, one level up from integration/
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build cashctl: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("building cashctl binary: %v", buildErr)
	}
	return binPath
}

// fixture is one isolated cashctl "user": its own --config-dir (cashctl's own
// local state) and its own empty XDG_CONFIG_HOME (so vault.Exists()
// reliably reports false, regardless of what's on the host running this
// suite — every fixture always gets cashctl's own freshly generated local
// identity, never an incidentally-present ncli vault).
type fixture struct {
	t         *testing.T
	bin       string
	configDir string
	xdgHome   string
}

// newFixture returns a fresh, fully isolated cashctl environment.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	xdgHome := filepath.Join(root, "xdg")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(xdgHome, 0700); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, bin: cashctlBinary(t), configDir: configDir, xdgHome: xdgHome}
}

// result is one cashctl invocation's outcome.
type result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Combined is stdout followed by stderr. Text-mode narration (prompts,
// previews, warnings, pick-lists) goes to stderr and results to stdout
// (AGENTS.md), so a test asserting on what a HUMAN sees — not on which
// stream carried it — reads both. Tests about the routing itself read
// Stdout/Stderr directly.
func (r result) Combined() string { return r.Stdout + r.Stderr }

// run executes the cashctl binary with args, always passing --config-dir and
// --json, isolated per-fixture. Never fails the test on a non-zero exit —
// callers assert on ExitCode themselves, since a classified failure
// (e.g. "no held tokens") is an expected, correct outcome for several tests.
func (f *fixture) run(args ...string) result {
	f.t.Helper()
	full := append([]string{"--config-dir", f.configDir, "--json"}, args...)
	cmd := exec.Command(f.bin, full...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+f.xdgHome)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			f.t.Fatalf("running cashctl %v: %v", args, err)
		}
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}
}

// runInteractive is run's counterpart for exercising a real interactive
// confirmation prompt: omits --json (which implies --yes, skipping every
// prompt) and pipes stdin. stdin == "" simulates a bare Enter.
func (f *fixture) runInteractive(stdin string, args ...string) result {
	f.t.Helper()
	full := append([]string{"--config-dir", f.configDir}, args...)
	cmd := exec.Command(f.bin, full...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+f.xdgHome)
	cmd.Stdin = strings.NewReader(stdin)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			f.t.Fatalf("running cashctl %v: %v", args, err)
		}
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}
}

// mustJSON runs args, requires exit code 0, and decodes stdout as JSON.
func (f *fixture) mustJSON(args ...string) map[string]any {
	f.t.Helper()
	res := f.run(args...)
	if res.ExitCode != 0 {
		f.t.Fatalf("cashctl %v: exit %d\nstdout: %s\nstderr: %s", args, res.ExitCode, res.Stdout, res.Stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		f.t.Fatalf("cashctl %v: decode JSON stdout: %v\nstdout: %s", args, err, res.Stdout)
	}
	return out
}
