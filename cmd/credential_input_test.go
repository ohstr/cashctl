package cmd

import (
	"errors"
	"testing"

	"github.com/ohstr/cashctl/internal/output"
)

func TestArgOrStdin_PassesAnOrdinaryArgThrough(t *testing.T) {
	stubStdin(t, "unread\n")
	got, err := argOrStdin(testCmdWithFlags(true, false), "lokicash1abc")
	if err != nil || got != "lokicash1abc" {
		t.Fatalf("argOrStdin = %q, %v; want the argument itself", got, err)
	}
	if line, _ := stdin.ReadString('\n'); line != "unread\n" {
		t.Errorf("stdin was consumed: next line = %q", line)
	}
}

// The value comes off the first line only, so a confirmation prompt later in
// the same command still reads its own answer.
func TestArgOrStdin_DashReadsOneLineAndLeavesTheRest(t *testing.T) {
	stubStdin(t, "  lokicash1abc#secret \ny\n")
	got, err := argOrStdin(testCmdWithFlags(false, false), "-")
	if err != nil || got != "lokicash1abc#secret" {
		t.Fatalf("argOrStdin = %q, %v; want the trimmed first line", got, err)
	}
	if !Confirm(testCmdWithFlags(false, false), false, "Continue?") {
		t.Error("the prompt after it did not get the next line's y")
	}
}

func TestArgOrStdin_LastLineWithoutNewline(t *testing.T) {
	stubStdin(t, "nostr+walletconnect://x")
	if got, err := argOrStdin(testCmdWithFlags(true, false), "-"); err != nil || got != "nostr+walletconnect://x" {
		t.Fatalf("argOrStdin = %q, %v", got, err)
	}
}

func TestArgOrStdin_EmptyStdinIsAUsageError(t *testing.T) {
	stubStdin(t, "")
	_, err := argOrStdin(testCmdWithFlags(true, false), "-")
	var ce *output.CLIError
	if !errors.As(err, &ce) || ce.Code != output.CodeUsage {
		t.Fatalf("err = %v, want a usage error", err)
	}
}
