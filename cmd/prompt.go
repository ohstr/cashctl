package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
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
	output.Fprintf(os.Stderr, "%s %s ", message, suffix)
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
	// The three writes below are the one place in cmd/ that deliberately emits
	// raw control characters — \r to redraw a frame in place and \033[K to erase
	// the line — so they cannot go through output's sanitizing wrappers, which
	// would replace exactly those with U+FFFD and leave a trail of broken frames
	// instead of an animation. cmd/auditD_cli_output_boundary_test.go allowlists
	// this function by name for that reason.
	//
	// The MESSAGE still has to be sanitized, and separately: a caller builds it
	// from a wallet name or a Hub's own label, so leaving it raw next to a
	// hand-written \r would be a hole in exactly the surface the rest of this
	// change closes. Sanitized once here rather than per frame.
	message = output.Sanitize(message)
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
	output.Fprint(os.Stderr, message)
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
	return readSecretFromTTY("Vault password: ")
}

// readSecretFromTTY reads a secret with echo disabled, prompting on stderr.
// It reads the controlling terminal (/dev/tty on Unix, CONIN$ on Windows —
// see openControllingTTY) rather than fd 0, so a value piped into stdin —
// e.g. the <token>#<secret> bill of `echo … | cashctl receive -` — does
// not collide with the password read and turn its termios ioctl into
// "inappropriate ioctl for device". This is what ssh-keygen and sudo do.
//
// If the controlling terminal can't be opened, it falls back to fd 0 only
// when fd 0 is itself a terminal; otherwise it returns a clear, actionable
// error instead of the cryptic ioctl failure reading a pipe would produce.
func readSecretFromTTY(prompt string) (string, error) {
	if tty, err := openControllingTTY(); err == nil {
		defer func() { _ = tty.Close() }()
		return readNoEcho(tty, prompt)
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return readNoEcho(os.Stdin, prompt)
	}
	return "", errors.New("vault password required but no terminal available: set NCLI_VAULT_PASSWORD")
}

// readNoEcho prints prompt to stderr, then reads one line from f with echo
// disabled, printing the trailing newline the suppressed Enter keystroke
// couldn't.
func readNoEcho(f *os.File, prompt string) (string, error) {
	output.Fprint(os.Stderr, prompt)
	pw, err := term.ReadPassword(int(f.Fd()))
	output.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(pw), nil
}
