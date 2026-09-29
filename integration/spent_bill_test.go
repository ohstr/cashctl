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
// A destroyed bill has TWO acceptable answers, and both mean gone:
//
//	tombstone  cash_status returns error="spent" with a retained_until — the Hub
//	           saying definitively "this bill existed, was spent, and is destroyed"
//	silence    the call times out — the Hub past that bill's retention window,
//	           back to saying nothing at all
//
// Accepting only silence, as this used to, is now wrong. The tombstone exists
// precisely because silence cannot be told apart from a Hub that is merely slow or
// unreachable, which forces every client to pick a wrong answer: treat a timeout as
// retryable and a genuinely spent bill retries forever, or treat it as gone and an
// outage tells someone their funds are lost. So the Hub answers definitively while
// it still can, and falls back to silence only once the bill is old enough that the
// tombstone is no longer retained (NIP-CASH §Answering About a Destroyed Bill).
//
// What must still fail is a real roster: a bill that answers with recipients is a
// bill that still exists and still holds value, which is the actual leak these
// checks guard against.
//
// Retried rather than asserted once, because the Hub deletes the bill AFTER
// answering the request that emptied it: for a moment afterwards it is still
// there and still replies with its live roster.
func requireBillSpentAway(t *testing.T, token, what string) {
	t.Helper()

	const (
		silenceWindow = 3 * time.Second
		deleteWindow  = 20 * time.Second
	)

	deadline := time.Now().Add(deleteWindow)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), silenceWindow)
		var spent bool
		var recipients int
		err := func() error {
			c, dialErr := nipcashclient.Connect(ctx, token)
			if dialErr != nil {
				return dialErr
			}
			defer c.Close()
			res, listErr := c.ListRecipients(ctx)
			if res != nil {
				spent = res.IsSpent()
				recipients = len(res.Recipients)
			}
			return listErr
		}()
		cancel()

		// Silence: past the retention window, the Hub says nothing at all.
		if errors.Is(err, context.DeadlineExceeded) {
			return
		}
		// Tombstone: the definitive answer, and the one a live Hub gives.
		if err == nil && spent {
			return
		}
		if time.Now().After(deadline) {
			if err == nil && recipients > 0 {
				t.Fatalf("%s: the Hub still returns a live roster of %d recipient(s) %s after this bill's value moved away — the bill was not destroyed",
					what, recipients, deleteWindow)
			}
			t.Fatalf("%s: %s after its value moved away the Hub gave neither a tombstone nor silence (err: %v, spent: %v, recipients: %d)",
				what, deleteWindow, err, spent, recipients)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
