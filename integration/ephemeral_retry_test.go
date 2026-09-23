//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// nwcCallTimeout is how long a single NWC round-trip gets before it is
// treated as dropped. Matches the per-call deadline these helpers used
// individually before the retry below existed.
const nwcCallTimeout = 30 * time.Second

// retryEphemeralNWC runs one NWC round-trip, and runs it a second time if
// the first produced no response at all.
//
// NIP-47 requests are kind 23194, inside Nostr's ephemeral range
// (20000-29999). A relay forwards an ephemeral event to whoever is
// subscribed at that instant and stores nothing, so there is no replay and
// no acknowledgement that anyone actually received it. The Hub subscribes
// once per app wallet, and starts each of those subscriptions in its own
// goroutine after the app exists (lokihub service/start.go). A request
// published into the window between an app being created and its
// subscription attaching is therefore delivered to nobody and is gone: the
// publish still reports success ("published to 1/1 relays"), and the caller
// simply waits out its deadline.
//
// Measured on the dev Hub: 8 identical mints, 7 handled — the eighth never
// reached the Hub at all and left no trace on the relay. It is rare (~1%)
// but the suite makes hundreds of these calls, so it surfaced on most full
// runs, always as a 30s "context deadline exceeded" on a different test.
//
// Retrying is the right response rather than a longer deadline: waiting
// longer cannot help an event that no longer exists anywhere, while a fresh
// publish goes out after the subscription has had time to attach. Only a
// timeout is retried — a real protocol error still fails immediately, so
// this cannot mask a genuine Hub rejection.
func retryEphemeralNWC[T any](t *testing.T, what string, call func(ctx context.Context) (T, error)) T {
	t.Helper()

	var last error
	for attempt := 1; attempt <= 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), nwcCallTimeout)
		v, err := call(ctx)
		cancel()

		if err == nil {
			return v
		}
		if !isDroppedEphemeralRequest(err) {
			t.Fatalf("%s: %v", what, err)
		}

		last = err
		if attempt == 1 {
			t.Logf("%s: no response within %s — the request was most likely dropped as an ephemeral event before the Hub's subscription attached; retrying once", what, nwcCallTimeout)
		}
	}

	t.Fatalf("%s: %v (no response on either attempt)", what, last)
	var zero T
	return zero
}

// isDroppedEphemeralRequest reports whether err is "nobody ever answered",
// as opposed to the Hub answering with a refusal. The string checks cover
// the NWC client's own wrapped phrasing, which does not always preserve
// context.DeadlineExceeded through the chain.
func isDroppedEphemeralRequest(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "timed out waiting for response")
}

// TestRetryEphemeralNWC_RetriesOnceOnTimeout exercises the retry path
// directly. A dropped ephemeral request is rare enough (~1% of calls) that
// a whole suite run can pass without hitting one, so the behaviour is
// pinned here rather than left to chance.
func TestRetryEphemeralNWC_RetriesOnceOnTimeout(t *testing.T) {
	attempts := 0
	got := retryEphemeralNWC(t, "fake_call", func(ctx context.Context) (string, error) {
		attempts++
		if attempts == 1 {
			return "", context.DeadlineExceeded
		}
		return "second-attempt-value", nil
	})

	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one drop, one retry)", attempts)
	}
	if got != "second-attempt-value" {
		t.Errorf("got %q, want the retry's value", got)
	}
}

// TestRetryEphemeralNWC_NoRetryWhenFirstAttemptSucceeds guards against the
// helper calling twice when it does not need to: these calls move real
// money, so a spurious second mint would be a genuine bug.
func TestRetryEphemeralNWC_NoRetryWhenFirstAttemptSucceeds(t *testing.T) {
	attempts := 0
	got := retryEphemeralNWC(t, "fake_call", func(ctx context.Context) (int, error) {
		attempts++
		return 42, nil
	})

	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

// TestIsDroppedEphemeralRequest separates "nobody answered" from "the Hub
// said no". Only the former may be retried — retrying a real refusal would
// reissue a call the Hub already rejected.
func TestIsDroppedEphemeralRequest(t *testing.T) {
	dropped := []error{
		context.DeadlineExceeded,
		fmt.Errorf("mint_cash: %w", context.DeadlineExceeded),
		errors.New("timed out waiting for response (published to 1/1 relays)"),
		errors.New("context deadline exceeded"),
	}
	for _, err := range dropped {
		if !isDroppedEphemeralRequest(err) {
			t.Errorf("isDroppedEphemeralRequest(%v) = false, want true", err)
		}
	}

	answered := []error{
		errors.New("UNAUTHORIZED: app does not have the cash_redeem scope"),
		errors.New("INTERNAL: no suitable channel"),
		errors.New("cash_consolidate does not accept a cash-mode source"),
	}
	for _, err := range answered {
		if isDroppedEphemeralRequest(err) {
			t.Errorf("isDroppedEphemeralRequest(%v) = true, want false - a real refusal must not be retried", err)
		}
	}
}
