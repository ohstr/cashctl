//go:build integration

package integration

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
)

// TestHubGroupTamper_WrongFingerprintIsStillRefusedByTheHub is the safety claim behind
// a 4-byte, unauthenticated, truncated grouping hint.
//
// The hub-group fingerprint is NOT covered by the mint signature — that commits only to
// `hrp:wallet_pubkey:amount_millis` — so anyone holding a token can re-encode it with a
// different fingerprint and its provenance still verifies. This test does exactly that,
// and the whole argument for accepting such a field rests on what happens next:
//
//   - the client groups WRONGLY, because it believes the fingerprint;
//   - the HUB refuses, because it checks the real parent hub itself;
//   - no value moves.
//
// So the worst a tampered fingerprint achieves is a failed consolidation and a retry
// with explicit sources. That is why the field is allowed to be short and unverifiable,
// and why signing it was judged not worth changing the payload format for.
//
// It also exercises crossHubGroupError, which per-hub grouping turned from the common
// path into a defence — this is the shape that still reaches it.
func TestHubGroupTamper_WrongFingerprintIsStillRefusedByTheHub(t *testing.T) {
	admin, hubA := plainClientHub(t)
	hubB := setUpCashHub(t, admin)

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	const amountA1, amountA2, amountB = uint64(15_000), uint64(25_000), uint64(35_000)

	// Two bills from hub A, so A alone has a mergeable pair.
	tokenA1 := mintPubkeyTokenFromHub(t, hubA, myPubHex, amountA1)
	tokenA2 := mintPubkeyTokenFromHub(t, hubA, myPubHex, amountA2)
	// One from hub B, which we will disguise as A's.
	tokenB := mintPubkeyTokenFromHub(t, hubB, myPubHex, amountB)

	a1, err := nipcash.Decode(tokenA1)
	if err != nil {
		t.Fatalf("decode A1: %v", err)
	}
	b, err := nipcash.Decode(tokenB)
	if err != nil {
		t.Fatalf("decode B: %v", err)
	}
	if len(a1.HubGroup) == 0 || len(b.HubGroup) == 0 {
		t.Skip("this hub does not stamp hub-group fingerprints, so there is nothing to tamper with")
	}
	if hex.EncodeToString(a1.HubGroup) == hex.EncodeToString(b.HubGroup) {
		t.Fatal("test premise broken: two different hubs produced the same fingerprint")
	}

	// The tamper: hub B's bill, wearing hub A's fingerprint. Everything else is
	// untouched — same wallet pubkey, same secret, same relays, same mint signature,
	// which still verifies because it never covered the fingerprint.
	disguised, err := nipcash.Encode(nipcash.Token{
		HRP:                  b.HRP,
		WalletPubkey:         b.WalletPubkey,
		Secret:               b.Secret,
		RelayURLs:            b.RelayURLs,
		IdentityRequired:     b.IdentityRequired,
		MintSignature:        b.MintSignature,
		AttestedAmountMillis: b.AttestedAmountMillis,
		HubGroup:             a1.HubGroup,
	})
	if err != nil {
		t.Fatalf("re-encode B with A's fingerprint: %v", err)
	}

	ids := map[string]string{}
	receive := func(label, token string) {
		t.Helper()
		resp := f.mustJSON("receive", token)
		entry, _ := resp["entry"].(map[string]any)
		id, _ := entry["id"].(string)
		if id == "" {
			t.Fatalf("receive(%s) produced no entry id: %v", label, resp)
		}
		ids[label] = id
	}
	receive("a1", tokenA1)
	receive("a2", tokenA2)
	// The disguised bill is accepted: its provenance verifies, so nothing about it
	// looks wrong locally. That is the premise, not a flaw — the fingerprint is a
	// hint, and a hint cannot be authenticated without signing it.
	receive("disguised", disguised)

	// cashctl now believes all three share one hub, so it offers ONE group of three.
	res := f.run("consolidate", "--json", "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("a group containing a foreign bill must be refused by the hub, got success:\n%s", res.Stdout)
	}

	// The refusal must be the HUB's same-hub rule, reported legibly — not a crash, and
	// not a generic failure.
	combined := res.Stdout + res.Stderr
	if !strings.Contains(combined, "same Cash Hub") && !strings.Contains(combined, "same Lightning node") {
		t.Errorf("want the hub's same-Cash-Hub refusal, got:\nstdout: %s\nstderr: %s", res.Stdout, res.Stderr)
	}
	// Classified, not retryable: retrying the identical selection fails identically.
	var errDoc map[string]any
	if err := json.Unmarshal([]byte(res.Stderr), &errDoc); err == nil {
		if retryable, _ := errDoc["retryable"].(bool); retryable {
			t.Error("a cross-hub refusal must not be marked retryable")
		}
	}

	// THE assertion: nothing moved. A tampered hint costs a failed command, never
	// value — which is the entire justification for the field's shape.
	show := f.mustJSON("wallet", "show")
	held := map[string]uint64{}
	for _, raw := range func() []any { v, _ := show["held_tokens"].([]any); return v }() {
		e, _ := raw.(map[string]any)
		id, _ := e["id"].(string)
		amt, _ := e["amount_millis"].(float64)
		held[id] = uint64(amt)
	}
	for label, want := range map[string]uint64{"a1": amountA1, "a2": amountA2, "disguised": amountB} {
		got, ok := held[ids[label]]
		if !ok {
			t.Errorf("bill %s (%s) is no longer held after a refused consolidation: %v", label, ids[label], held)
			continue
		}
		if got != want {
			t.Errorf("bill %s = %d after a refused consolidation, want %d untouched", label, got, want)
		}
	}
}
