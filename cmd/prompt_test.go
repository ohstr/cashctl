package cmd

import (
	"errors"
	"io"
	"os"
	"runtime"
	"testing"
	"time"
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
	withStdoutSuppressed(func() {
		_ = WithSpinner(false, "checking...", func() error {
			time.Sleep(250 * time.Millisecond)
			return nil
		})
	})
	// Give the runtime a moment to actually reclaim the stopped goroutine's
	// stack before recounting.
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before {
		t.Errorf("goroutine count grew from %d to %d — WithSpinner's animation goroutine may have leaked", before, after)
	}
}
