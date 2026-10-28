//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipIC"
	"github.com/ohstr/nmilat/nipcash"
)

// cliMinter mints on a live Hub the way a user would: `cashctl cash mint`
// over the Hub's cashhub1 connection (kept out of argv in CASHCTL_CASH_HUB),
// from a fixture of its own so the minting wallet never mixes with the one
// under test. cashctl writes each mint ahead and checks the bill it gets back.
type cliMinter struct {
	t   *testing.T
	f   *fixture
	hub string
}

// newMinter is a minter for the Hub hub created (its cashHubToken).
func newMinter(t *testing.T, hub adminCreateAppResponse) *cliMinter {
	t.Helper()
	if hub.CashHubToken == nil || *hub.CashHubToken == "" {
		t.Fatalf("the cash_hub app %d has no cashhub1 connection", hub.ID)
	}
	return &cliMinter{t: t, f: newFixture(t), hub: *hub.CashHubToken}
}

// mint runs `cash mint` and returns its --json answer.
func (m *cliMinter) mint(millis uint64, to string, expiry time.Duration) (map[string]any, error) {
	m.t.Helper()
	args := []string{"cash", "mint", lokiArg(int64(millis)), "--to", to, "--json"} //nolint:gosec // test amounts fit
	if expiry > 0 {
		args = append(args, "--expiry", expiry.String())
	}
	res := m.f.runWith([]string{"CASHCTL_CASH_HUB=" + m.hub}, "", args...)
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("cashctl cash mint: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		return nil, fmt.Errorf("cashctl cash mint: %v: %s", err, res.Stdout)
	}
	return out, nil
}

// Cash mints a cash note and returns its token and secret.
func (m *cliMinter) Cash(millis uint64) (token, secret string, err error) {
	m.t.Helper()
	out, err := m.mint(millis, "cash", 0)
	if err != nil {
		return "", "", err
	}
	handoff, _ := out["handoff"].(string)
	token, secret, ok := strings.Cut(handoff, "#")
	if !ok || secret == "" {
		return "", "", fmt.Errorf("cashctl cash mint: no cash note in %v", out)
	}
	return token, secret, nil
}

// MustCash is Cash, failing the test.
func (m *cliMinter) MustCash(millis uint64) (token, secret string) {
	m.t.Helper()
	token, secret, err := m.Cash(millis)
	if err != nil {
		m.t.Fatalf("mint a cash note: %v", err)
	}
	return token, secret
}

// To mints a bill for pubkey (hex) and returns its token; expiry 0 is the Hub's.
func (m *cliMinter) To(pubkey string, millis uint64, expiry time.Duration) (string, error) {
	m.t.Helper()
	out, err := m.mint(millis, pubkey, expiry)
	if err != nil {
		return "", err
	}
	token, _ := out["cash_token"].(string)
	if token == "" {
		return "", fmt.Errorf("cashctl cash mint: no cash_token in %v", out)
	}
	return token, nil
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

// newCashRecipient is a cash-mode target made here, for the SDK-level
// transfers a few tests drive by hand.
func newCashRecipient(t *testing.T) *nipcash.CashTarget {
	t.Helper()
	ct, err := nipcash.NewCashTarget()
	if err != nil {
		t.Fatal(err)
	}
	return ct
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
