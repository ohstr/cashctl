package cmd

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// withStdoutSuppressed redirects os.Stdout to a pipe for the duration of fn
// (draining it in the background so a write never blocks), then restores
// it — WithSpinner's animation writes straight to os.Stdout, and this
// suite has no interest in that output cluttering `go test`'s own.
func withStdoutSuppressed(fn func()) {
	real := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		fn()
		return
	}
	os.Stdout = w
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, r)
		close(done)
	}()
	fn()
	os.Stdout = real
	_ = w.Close()
	<-done
}

func TestWithSpinner_JSONModeRunsFnWithNoAnimation(t *testing.T) {
	called := false
	err := WithSpinner(true, "checking...", func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithSpinner returned %v, want nil", err)
	}
	if !called {
		t.Fatal("WithSpinner(jsonMode=true, ...) never called fn")
	}
}

func TestWithSpinner_PropagatesFnError(t *testing.T) {
	want := errors.New("boom")
	var got error
	withStdoutSuppressed(func() {
		got = WithSpinner(false, "checking...", func() error { return want })
	})
	if !errors.Is(got, want) {
		t.Fatalf("WithSpinner returned %v, want %v", got, want)
	}
}

func TestWithSpinner_StopsItsGoroutineBeforeReturning(t *testing.T) {
	// A regression guard for the exact race WithSpinner's own doc comment
	// calls out: if the spinner goroutine were still writing when
	// WithSpinner returns, whatever the caller prints right after could
	// interleave with it. Running long enough to cross several ticks (the
	// animation ticks every 100ms) and checking goroutine count stays flat
	// is the closest a unit test gets to proving that without a race
	// detector run finding an actual data race in the output stream.
	before := runtime.NumGoroutine()
	// withSpinner, not WithSpinner: the latter only animates on a real
	// terminal, which a `go test` run never has.
	_ = withSpinner(io.Discard, true, "checking...", func() error {
		time.Sleep(250 * time.Millisecond)
		return nil
	})
	// Give the runtime a moment to actually reclaim the stopped goroutine's
	// stack before recounting.
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before {
		t.Errorf("goroutine count grew from %d to %d — WithSpinner's animation goroutine may have leaked", before, after)
	}
}

func TestWithSpinner_AnimatesToTheWriterAndErasesItselfAfterwards(t *testing.T) {
	var buf bytes.Buffer
	_ = withSpinner(&buf, true, "checking...", func() error {
		time.Sleep(250 * time.Millisecond)
		return nil
	})
	got := buf.String()
	if !strings.Contains(got, "checking...") {
		t.Errorf("spinner wrote %q, want frames carrying its message", got)
	}
	if !strings.HasSuffix(got, "\r\033[K") {
		t.Errorf("spinner output %q must end by erasing its own line", got)
	}
}

func TestWithSpinner_NotAnimatingWritesNothingButStillRunsFn(t *testing.T) {
	var buf bytes.Buffer
	ran := false
	if err := withSpinner(&buf, false, "checking...", func() error { ran = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("fn never ran")
	}
	if buf.Len() != 0 {
		t.Errorf("a non-animated spinner wrote %q, want nothing", buf.String())
	}
}

// captureStreams runs fn with os.Stdout and os.Stderr each redirected to a
// pipe, returning what was written to each.
func captureStreams(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	realOut, realErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = realOut, realErr }()
	drain := func() (*os.File, chan string) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan string)
		go func() {
			b, _ := io.ReadAll(r)
			done <- string(b)
		}()
		return w, done
	}
	wOut, outDone := drain()
	wErr, errDone := drain()
	os.Stdout, os.Stderr = wOut, wErr
	fn()
	_ = wOut.Close()
	_ = wErr.Close()
	return <-outDone, <-errDone
}

// AGENTS.md: narration goes to stderr always, so a script capturing stdout
// never gets a prompt in it.
func TestConfirm_PromptGoesToStderrNotStdout(t *testing.T) {
	realStdin := stdin
	defer func() { stdin = realStdin }()
	stdin = bufio.NewReader(strings.NewReader("y\n"))

	c := &cobra.Command{}
	c.Flags().Bool("json", false, "")
	c.Flags().Bool("yes", false, "")
	var answer bool
	out, errOut := captureStreams(t, func() { answer = Confirm(c, false, "Move the money?") })

	if !answer {
		t.Error("Confirm() = false, want true for an explicit y")
	}
	if out != "" {
		t.Errorf("Confirm wrote %q to stdout, want nothing", out)
	}
	if !strings.Contains(errOut, "Move the money? [y/N]") {
		t.Errorf("stderr = %q, want the prompt there", errOut)
	}
}

func TestPromptLine_PromptGoesToStderrNotStdout(t *testing.T) {
	realStdin := stdin
	defer func() { stdin = realStdin }()
	stdin = bufio.NewReader(strings.NewReader("hello\n"))

	var got string
	out, errOut := captureStreams(t, func() { got, _ = PromptLine("Say something: ") })

	if got != "hello" {
		t.Errorf("PromptLine() = %q, want hello", got)
	}
	if out != "" || errOut != "Say something: " {
		t.Errorf("stdout=%q stderr=%q, want the prompt on stderr only", out, errOut)
	}
}
