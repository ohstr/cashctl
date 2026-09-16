package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	btcec "github.com/flokiorg/go-flokicoin/crypto"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"
)

// The former TestValidateBearerSecret lived here — validateBearerSecret
// and the --secret flag it guarded are both gone now: a bearer-mode
// receive requires the secret to be embedded in the token itself
// (<token>#<bearer_secret>) or it degrades to a read-only check instead
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
	encoded, err := nipcash.Encode(nipcash.Token{
		HRP:              "lokicash",
		WalletPubkey:     walletPubkey,
		Secret:           secret,
		IdentityRequired: ptrTo(false),
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

// signProvenance replicates nipcash's own provenance_test.go signing
// helper (unexported there) so this test can produce a genuinely
// verifiable mint signature, rather than a mock, for the happy-path case.
func signProvenance(priv *btcec.PrivateKey, hrp, walletPubkeyHex string, amountMillis uint64) []byte {
	payload := nipcash.MintPayload(hrp, walletPubkeyHex, amountMillis)
	first := sha256.Sum256([]byte(nipcash.LNSignedMessagePrefix + payload))
	second := sha256.Sum256(first[:])
	return ecdsa.SignCompact(priv, second[:], true)
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

	t.Run("structurally malformed signature stays nil", func(t *testing.T) {
		// VerifyProvenance's own doc: recovery mathematically succeeds (with
		// *some* pubkey, not necessarily the true signer's) for any
		// well-formed-length signature, even a forged or tampered one —
		// deciding whether the recovered pubkey is trustworthy is the
		// caller's job, not VerifyProvenance's. `ok=false` only happens for
		// a structurally malformed (wrong-length) signature, so that's the
		// only reliable way to exercise this function's nil branch.
		tok := nipcash.Token{HRP: "lokicash", WalletPubkey: walletPubkey, MintSignature: []byte{1, 2, 3}, AttestedAmountMillis: &amount}
		if got := minterPubkeyFromToken(tok); got != nil {
			t.Fatalf("minterPubkeyFromToken(malformed) = %v, want nil", *got)
		}
	})

	t.Run("valid signature is recorded", func(t *testing.T) {
		priv, pub := btcec.PrivKeyFromBytes([]byte{
			1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
			17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32,
		})
		sig := signProvenance(priv, "lokicash", walletPubkey, amount)
		tok := nipcash.Token{HRP: "lokicash", WalletPubkey: walletPubkey, MintSignature: sig, AttestedAmountMillis: &amount}

		got := minterPubkeyFromToken(tok)
		if got == nil {
			t.Fatal("minterPubkeyFromToken(validly-signed) = nil, want the minter pubkey")
		}
		serialized := btcec.ToSerialized(pub)
		want := hex.EncodeToString(serialized[:])
		if *got != want {
			t.Fatalf("minterPubkeyFromToken(validly-signed) = %s, want %s", *got, want)
		}
	})
}

// The former TestMatchRecipient lived here — matchRecipient itself moved
// to nmilat as nipcash.MatchClaim, with equivalent (and, for the
// already-claimed-recipient case, corrected) coverage in that repo's own
// nipcash/claim_test.go.
