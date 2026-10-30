//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/utils"

	"github.com/ohstr/cashctl/internal/testmint"
)

// forgeCashHubIdentity re-signs the real Cash Hub connection `real` so its
// TLV 4 identity is attackerPriv's x-only key, while TLV 0/1/2 (the Hub's own
// pubkey, relays and secret) still point at the REAL Hub, and TLV 6 (chain)
// is left exactly as issued. The forged string therefore Verify()s on its own
// — its connection signature recovers cleanly to the forged (attacker)
// identity — but when a client dials it, the REAL Hub's get_info answers as a
// different identity, which is the online check's identity-mismatch case
// (NIP-CASH §Connection Signatures, the online check).
//
// Mirrors nmilat nipcash/client/hub_online_check_test.go's resignedConnection,
// but keeps the real relays/pubkey/secret (and the real chain) so the string
// still reaches the Hub and only TLV 4 is a lie — isolating the identity
// mismatch from the chain mismatch (the chain check runs only after the
// identity check passes).
func forgeCashHubIdentity(t *testing.T, real, attackerPriv string) string {
	t.Helper()
	c, err := nipcash.DecodeCashHubConnection(real)
	if err != nil {
		t.Fatalf("decode real cashhub1: %v", err)
	}
	attacker, err := utils.GetPublicKey(attackerPriv)
	if err != nil {
		t.Fatalf("attacker pubkey: %v", err)
	}
	c.HubIdentity = attacker // TLV 4 now names the attacker; TLV 6 (chain) stays real
	signer, err := nipcash.LocalChainSigner(attackerPriv, c.Chain)
	if err != nil {
		t.Fatalf("attacker chain signer: %v", err)
	}
	signed, err := nipcash.SignCashHubConnection(c, signer, attacker)
	if err != nil {
		t.Fatalf("re-sign forged connection: %v", err)
	}
	if err := signed.Verify(); err != nil {
		t.Fatalf("the forged string must verify under the attacker key: %v", err)
	}
	out, err := nipcash.EncodeCashHubConnection(signed)
	if err != nil {
		t.Fatalf("encode forged cashhub1: %v", err)
	}
	return out
}

// TestMint_RefusedWhenGetInfoNamesAnotherIdentity is the live evidence for
// NIP-CASH §Connection Signatures' online check: `cash mint` over a cashhub1
// whose TLV 4 identity names an ATTACKER, but whose relays still reach the
// REAL Hub, is refused as a connection/identity mismatch and nothing is
// written ahead.
//
// Enforcement path, end to end: nmilat nipcash/online.go OnlineCheck +
// nipcash/client/hub.go Info() (which runs the check against get_info on
// every call) -> cashctl cmd/cash_mint.go dials and calls client.Info(ctx)
// BEFORE ledger.Load / MintCash -> cmd/connstr.go isConnectionMismatch /
// connectionMismatchErr turn nipcash.ErrHubIdentityMismatch into an auth
// error with reason connection_mismatch (exit 7 — the exit code every cashctl
// mismatch uses; cmd/cash_mint_test.go's own TestCashMint_RefusesBeforeWriting
// pins the same 7/connection_mismatch at the unit level).
//
// This is the TLV-4 identity case. The sibling TLV-6 CHAIN-mismatch case
// (keep TLV 4 = the real identity, change only the chain) can only be re-signed
// with the Hub's OWN node key — which this black-box harness deliberately never
// holds — so it stays a unit test: nmilat nipcash/client/hub_online_check_test.go
// TestHubClient_MintRefusedWhenGetInfoDisagrees, case "another chain, signed by
// the Hub" -> nipcash.ErrChainMismatch.
func TestMint_RefusedWhenGetInfoNamesAnotherIdentity(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	if hub.CashHubToken == nil || *hub.CashHubToken == "" {
		t.Fatalf("ephemeral cash_hub has no cashhub1 connection: %+v", hub)
	}

	f := newFixture(t)
	f.mustJSON("wallet", "init")

	attackerPriv, _ := testmint.Key(t)
	forged := forgeCashHubIdentity(t, *hub.CashHubToken, attackerPriv)
	// A would-be recipient pubkey: ParseTarget resolves it before the dial,
	// so the mint reaches the online check rather than failing on the target.
	_, target := testmint.Key(t)

	res := f.run("cash", "mint", lokiArg(5_000), "--hub", forged, "--to", target, "--yes")
	e := requireFailure(t, "mint over a forged-identity Hub", res, 7, "auth", "")
	if e.Reason != "connection_mismatch" {
		t.Errorf("reason = %q, want connection_mismatch\nstderr: %s", e.Reason, res.Stderr)
	}
	if !strings.Contains(e.Error, "answered as another") {
		t.Errorf("error doesn't read as an identity mismatch: %s", res.Stderr)
	}

	// Nothing may be written ahead AND no value may be minted: the online check
	// fails inside client.Info — which cash_mint.go calls BEFORE MintCash — so
	// mint_cash is never sent and the journal never runs (nmilat's own unit test
	// asserts the same server-free invariant with len(j.all()) == 0). Assert all
	// three client-observable traces of a mint are absent, not just open calls:
	// a regression that sent mint_cash and only THEN reported the mismatch would
	// still leave no open call, so open_calls alone does not prove nothing minted.
	show := f.mustJSON("wallet", "show")
	if calls, _ := show["open_calls"].([]any); len(calls) != 0 {
		t.Errorf("a refused mint wrote calls ahead: %v", calls)
	}
	if held, _ := show["held_tokens"].([]any); len(held) != 0 {
		t.Errorf("a refused mint kept %d held token(s); nothing may be minted on a mismatch: %v", len(held), held)
	}
	// Balance comes from `wallet balance` (`wallet show` carries no total): it must
	// be zero, since a mint that reached the Hub and minted value would show here.
	if total := walletTotalMloki(t, f); total != 0 {
		t.Errorf("wallet balance = %d mloki after a refused mint, want 0 — no value may be minted on a mismatch", total)
	}
}
