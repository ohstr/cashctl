package cmd

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	btcec "github.com/flokiorg/go-flokicoin/crypto"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/testmint"
)

// The former TestValidateCashSecret lived here — validateCashSecret
// and the --secret flag it guarded are both gone now: a cash-mode
// receive requires the secret to be embedded in the token itself
// (<token>#<cash_secret>) or it degrades to a read-only check instead
// of erroring (cash_receive.go's own Step 0).

// TestRunCashReceive_NoEmbeddedSecret_JSONMode covers Step 0's
// no-embedded-secret path under --json: shouldRunCheck short-circuits to
// false before any dial happens (jsonMode always wins over the live-check
// prompt unless --check was explicitly passed), and the function returns
// before ledger.Load() is ever called — so, unlike the rest of receive,
// this exact branch is genuinely network-free and local, and belongs in
// this package's unit suite rather than integration's.
func TestRunCashReceive_NoEmbeddedSecret_JSONMode(t *testing.T) {
	walletPubkey := strings.Repeat("aa", 32)
	secret := strings.Repeat("bb", 32)
	encoded, err := testmint.Encode(t, nipcash.Token{
		HRP:              "lokicash",
		WalletPubkey:     walletPubkey,
		Secret:           secret,
		IdentityRequired: ptrTo(false),
		// Required by nipcash.Encode. It does not make this test dial anything:
		// shouldRunCheck short-circuits under --json before any connection.
		RelayURLs: []string{"wss://relay.example"},
	})
	if err != nil {
		t.Fatalf("nipcash.Encode: %v", err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", true, "")

	var runErr error
	printed := withCapturedStdout(func() {
		runErr = runCashReceive(cmd, []string{encoded})
	})
	if runErr != nil {
		t.Fatalf("runCashReceive() error = %v", runErr)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(printed), &got); err != nil {
		t.Fatalf("output isn't valid JSON: %v\noutput: %s", err, printed)
	}
	if got["received"] != false {
		t.Fatalf(`got "received" = %v, want false`, got["received"])
	}
	if got["reason"] != "no_embedded_secret" {
		t.Fatalf(`got "reason" = %v, want "no_embedded_secret"`, got["reason"])
	}
}

func TestMinterPubkeyFromToken(t *testing.T) {
	walletPubkey := "aaaa111122223333444455556666777788889999aaaabbbbccccddddeeee00"
	amount := uint64(5000)

	t.Run("no provenance stays nil", func(t *testing.T) {
		tok := nipcash.Token{HRP: "lokicash", WalletPubkey: walletPubkey}
		if got := minterPubkeyFromToken(tok); got != nil {
			t.Fatalf("minterPubkeyFromToken(unsigned) = %v, want nil", *got)
		}
	})

	t.Run("malformed provenance stays nil", func(t *testing.T) {
		tok := nipcash.Token{HRP: "lokicash", WalletPubkey: walletPubkey, MintSignature: []byte{1, 2, 3}, AttestedAmountMillis: &amount, Scheme: nipcash.SchemeMalformed}
		if got := minterPubkeyFromToken(tok); got != nil {
			t.Fatalf("minterPubkeyFromToken(malformed) = %v, want nil", *got)
		}
	})

	t.Run("valid signature is recorded", func(t *testing.T) {
		priv, _ := testmint.Key(t)
		tok := testmint.Token(t, nipcash.Token{HRP: "lokicash", WalletPubkey: walletPubkey, AttestedAmountMillis: &amount}, priv)
		got := minterPubkeyFromToken(tok)
		if got == nil {
			t.Fatal("minterPubkeyFromToken(validly-signed) = nil, want the minter pubkey")
		}
		privBytes, _ := hex.DecodeString(priv)
		_, pub := btcec.PrivKeyFromBytes(privBytes)
		if want := hex.EncodeToString(pub.SerializeCompressed()); *got != want {
			t.Fatalf("minterPubkeyFromToken(validly-signed) = %s, want %s", *got, want)
		}
	})
}

// The former TestMatchRecipient lived here — matchRecipient itself moved
// to nmilat as nipcash.MatchClaim, with equivalent (and, for the
// already-claimed-recipient case, corrected) coverage in that repo's own
// nipcash/claim_test.go.

// --- validateClaimedAmount: checkClaimWithCashHub proves an unclaimed
// recipient exists, but never that the AMOUNT reported for it is sane or
// consistent with the token's own signed provenance — both gaps a
// hostile relay (the token's own RelayURLs, dialed automatically) can
// exploit on nothing more than an attacker-controlled Hub reply.

func TestValidateClaimedAmount_MatchingProvenanceIsAccepted(t *testing.T) {
	c := testCmdWithFlags(true, false)
	amount := uint64(5000)
	minter := "minterpubkey"
	tok := nipcash.Token{AttestedAmountMillis: &amount}
	if err := validateClaimedAmount(c, tok, amount, &minter); err != nil {
		t.Errorf("validateClaimedAmount() error = %v, want nil (amounts agree)", err)
	}
}

// A multi-recipient bill's signature attests the whole bill; the holder's
// own slice of it is smaller, and that is no reason to refuse it.
func TestValidateClaimedAmount_SliceSmallerThanTheWholeBillIsAccepted(t *testing.T) {
	c := testCmdWithFlags(true, false)
	whole, slice := uint64(5000), uint64(3000)
	minter := "minterpubkey"
	tok := nipcash.Token{AttestedAmountMillis: &whole}
	if err := validateClaimedAmount(c, tok, slice, &minter); err != nil {
		t.Errorf("validateClaimedAmount() error = %v, want a slice under the attested whole accepted", err)
	}
}

func TestValidateClaimedAmount_RejectsMismatchAgainstVerifiedProvenance(t *testing.T) {
	// The regression test for R2-10: a forged/misbehaving Hub reporting an
	// amount that disagrees with the token's own cryptographically
	// verified attestation (MinterPubkey set means nipcashclient.CheckClaim
	// already confirmed the signature itself, not just that it's present)
	// must be refused outright, not silently trusted and persisted.
	c := testCmdWithFlags(true, false)
	attested := uint64(1000)             // what the token's own signature commits to
	hubReported := uint64(1_000_000_000) // what a lying Hub claims instead
	minter := "minterpubkey"
	tok := nipcash.Token{AttestedAmountMillis: &attested}
	err := validateClaimedAmount(c, tok, hubReported, &minter)
	if err == nil {
		t.Fatal("validateClaimedAmount() = nil error, want a mismatch rejected")
	}
	if !strings.Contains(err.Error(), "1 loki") || !strings.Contains(err.Error(), "1000000 loki") {
		t.Errorf("validateClaimedAmount() error = %v, want both figures named", err)
	}
}

func TestValidateClaimedAmount_UnverifiedProvenanceIsNotCompared(t *testing.T) {
	// MinterPubkey nil means CheckClaim itself couldn't verify the
	// signature (or none was ever attached) — comparing against
	// AttestedAmountMillis in that case would be comparing the Hub's real
	// answer against a number nothing actually vouches for. A tampered
	// token's own decode-time display already warns separately (E2-07);
	// this function's job is narrower: never trust a MISMATCH as evidence
	// of anything when the "attestation" itself was never confirmed.
	c := testCmdWithFlags(true, false)
	attested := uint64(1000)
	tok := nipcash.Token{AttestedAmountMillis: &attested}
	if err := validateClaimedAmount(c, tok, 999_999, nil); err != nil {
		t.Errorf("validateClaimedAmount() error = %v, want nil (nothing verified to compare against)", err)
	}
}

func TestValidateClaimedAmount_RejectsAbsurdlyLargeAmount(t *testing.T) {
	// The same overflow boundary internal/output.ParseAmount enforces on
	// typed input (math.MaxInt64 mloki) — here on the network-input side:
	// an amount beyond it goes negative the moment any downstream
	// int64(...) cast touches it (FormatAmount's own signature,
	// ledger.Entry's later use), exactly the "-9223372036854775.-808 loki"
	// bug a fake Hub could otherwise trigger.
	c := testCmdWithFlags(true, false)
	tok := nipcash.Token{}
	err := validateClaimedAmount(c, tok, 1<<64-1, nil)
	if err == nil {
		t.Fatal("validateClaimedAmount() = nil error, want an absurd amount rejected")
	}
}

func TestWarnLocalRelays(t *testing.T) {
	for _, tc := range []struct {
		relay string
		warn  bool
	}{
		{"ws://127.0.0.1:7777", true},
		{"ws://localhost:7777", true},
		{"wss://10.1.2.3", true},
		{"wss://[::1]:7777", true},
		{"wss://relay.example.com", false},
		{"wss://8.8.8.8", false},
	} {
		_, errOut := captureStreams(t, func() { warnLocalRelays(false, []string{tc.relay}) })
		if got := strings.Contains(errOut, "your own machine or network"); got != tc.warn {
			t.Errorf("%s: warned = %v, want %v (%q)", tc.relay, got, tc.warn, errOut)
		}
	}
	if _, errOut := captureStreams(t, func() { warnLocalRelays(true, []string{"ws://127.0.0.1"}) }); errOut != "" {
		t.Errorf("--json mode printed a note: %q", errOut)
	}
}
