//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
)

// requireBillSpentAway asserts that a bill whose value has moved away is gone
// from the Hub.
//
// These checks used to read the bill's own recipient list back as server-side
// ground truth that nothing unclaimed was left on it. That no longer works: a
// Hub deletes a bill once nothing is left, and then answers nothing at all
// about it, so the call waits out its whole timeout instead of returning an
// empty list. Silence is the stronger statement anyway -- "this bill no longer
// exists" rather than "this bill exists and holds nothing".
//
// Retried rather than asserted once, because the Hub deletes the bill AFTER
// answering the request that emptied it: for a moment afterwards it is still
// there and still replies.
func requireBillSpentAway(t *testing.T, token, what string) {
	t.Helper()

	const (
		silenceWindow = 3 * time.Second
		deleteWindow  = 20 * time.Second
	)

	deadline := time.Now().Add(deleteWindow)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), silenceWindow)
		err := func() error {
			c, dialErr := nipcashclient.Connect(ctx, token)
			if dialErr != nil {
				return dialErr
			}
			defer c.Close()
			_, listErr := c.ListRecipients(ctx)
			return listErr
		}()
		cancel()

		if errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the Hub still answers for this bill %s after its value moved away (last result: %v)",
				what, deleteWindow, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
