//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/ohstr/nmilat/nipIC"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
)

// liveCashHub mints on a live Hub over its cashhub1 connection. The Hub now
// takes only caller-made cash secrets and a written-ahead request id, so the
// suite keeps an in-memory journal and remembers each secret it generated.
type liveCashHub struct {
	t   *testing.T
	hub *nipcashclient.HubClient
}

// MintCash mints p as a test caller would, written ahead to a memory journal.
func (h *liveCashHub) MintCash(ctx context.Context, p nipcash.MintCashParams) (*nipcash.MintCashResult, error) {
	h.t.Helper()
	return h.hub.MintCash(ctx, p, &memJournal{}, "integration")
}

// memJournal is a Journal that keeps calls for the life of one test: the suite
// never recovers a call across processes.
type memJournal struct {
	mu      sync.Mutex
	calls   []nipcash.PendingCall
	derived map[string]bool
}

func (j *memJournal) WriteAhead(_ context.Context, calls []nipcash.PendingCall) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = append(j.calls, calls...)
	return nil
}

func (j *memJournal) RecordDerived(_ context.Context, id string, _ int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.derived == nil {
		j.derived = map[string]bool{}
	}
	j.derived[id] = true
	return nil
}

func (j *memJournal) DerivedPlaced(_ context.Context, id string) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.derived[id], nil
}

var (
	cashSecretsMu sync.Mutex
	cashSecrets   = map[string]string{} // commitment → secret
)

// newCashRecipient is a cash-mode mint recipient: a secret made here, whose
// commitment is all the Hub sees. secretFor finds it again from the result.
func newCashRecipient(t *testing.T) *nipcash.CashTarget {
	t.Helper()
	ct, err := nipcash.NewCashTarget()
	if err != nil {
		t.Fatal(err)
	}
	cashSecretsMu.Lock()
	cashSecrets[ct.Commitment()] = ct.Secret()
	cashSecretsMu.Unlock()
	return ct
}

// secretFor is the secret of a cash-mode recipient row newCashRecipient made,
// "" for any other row.
func secretFor(r nipcash.RecipientResult) string {
	cashSecretsMu.Lock()
	defer cashSecretsMu.Unlock()
	return cashSecrets[r.IdentityValue]
}

// mustConnKey is a canonical ConnectionKey, failing the test for input the
// canonical form refuses.
func mustConnKey(t *testing.T, platform nipIC.WebIdentity, user string) nipIC.ConnectionKey {
	t.Helper()
	k, err := nipIC.NewConnectionKey(platform, user)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
