package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var stdin = bufio.NewReader(os.Stdin)

// Confirm asks a yes/no question in human mode, returning defaultYes if
// the user just presses Enter. Under --json or --yes, this returns true
// immediately with no actual prompt: an agent/script has no terminal to
// answer from, and --yes is an explicit "don't ask" request — see
// cashctl-plan.md's "auto-selection is fine for reads, not for silently
// spending" principle: the *pick* can be automatic, but a destructive
// action still needs one confirmation, satisfied non-interactively by
// either flag.
func Confirm(cmd *cobra.Command, defaultYes bool, message string) bool {
	jsonMode, _ := cmd.Flags().GetBool("json")
	yesFlag, _ := cmd.Flags().GetBool("yes")
	if jsonMode || yesFlag {
		return true
	}
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	// stderr, like every prompt (ssh, sudo, git): stdout carries only what a
	// command produced, so `cashctl ... > out` never captures the question.
	fmt.Fprintf(os.Stderr, "%s %s ", message, suffix)
	line, _ := stdin.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return defaultYes
	}
	return line == "y" || line == "yes"
}

// spinnerFrames animates WithSpinner's "waiting on the network" indicator.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// WithSpinner runs fn while animating message on stderr — cashctl's shared
// "waiting on a network call" UX, so every command that blocks on one
// (`receive`'s mandatory Hub check, ...) looks the same rather than each
// command inventing its own. Progress narration goes to stderr always
// (AGENTS.md), and only animates when stderr is a real terminal: redirected
// to a file or a pipe, the \r-and-erase frames are just noise in a log. A
// no-op wrapper under --json, same as Notef: a JSON consumer only wants the
// final structured result. The spinner goroutine is fully stopped (not just
// signaled) before returning, so its own writes can never race with
// whatever the caller prints right after.
func WithSpinner(jsonMode bool, message string, fn func() error) error {
	return withSpinner(os.Stderr, !jsonMode && term.IsTerminal(int(os.Stderr.Fd())), message, fn)
}

// withSpinner is WithSpinner with its output and on/off decision injected,
// so the animation's own behavior (frames written, goroutine stopped before
// return) stays unit-testable without a real terminal.
func withSpinner(w io.Writer, animate bool, message string, fn func() error) error {
	if !animate {
		return fn()
	}
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		frame := 0
		_, _ = fmt.Fprintf(w, "\r%s %s", spinnerFrames[0], message)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				frame++
				_, _ = fmt.Fprintf(w, "\r%s %s", spinnerFrames[frame%len(spinnerFrames)], message)
			}
		}
	}()
	err := fn()
	close(stop)
	<-stopped
	_, _ = fmt.Fprint(w, "\r\033[K")
	return err
}

// PromptLine asks for a single line of free-text input.
func PromptLine(message string) (string, error) {
	fmt.Fprint(os.Stderr, message)
	line, err := stdin.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// ResolveVaultPassword returns the ncli vault password: NCLI_VAULT_PASSWORD
// if set (the non-interactive/agentic path, exactly as ncli's own
// keyresolve.ResolveVaultPassword honors it), otherwise a hidden
// interactive prompt. Errors under --json with no env var set, rather than
// blocking forever on a prompt no agent can answer.
func ResolveVaultPassword(jsonMode bool) (string, error) {
	if pw := os.Getenv("NCLI_VAULT_PASSWORD"); pw != "" {
		return pw, nil
	}
	if jsonMode {
		return "", fmt.Errorf("vault password required: set NCLI_VAULT_PASSWORD (no interactive prompt under --json)")
	}
	fmt.Fprint(os.Stderr, "Vault password: ")
	pw, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(pw), nil
}
