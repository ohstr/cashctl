//go:build crashpoints

package crashpoint

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
)

// The tagged build kills itself at the named point, and only there. Runs the
// test binary again as the process to kill.
func TestHit_KillsAtTheNamedPoint(t *testing.T) {
	if os.Getenv("CRASHPOINT_CHILD") == "1" {
		Hit("other")
		Hit("x")
		os.Exit(0) // reached only if Hit("x") did not kill
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHit_KillsAtTheNamedPoint$")
	cmd.Env = append(os.Environ(), "CRASHPOINT_CHILD=1", EnvVar+"=x")
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child exited %v; want it killed at the crash point", err)
	}
	if runtime.GOOS != "windows" {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Errorf("child ended %v; want SIGKILL", exitErr)
		}
	}
}
