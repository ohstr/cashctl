//go:build integration

package integration

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	btcec "github.com/flokiorg/go-flokicoin/crypto"
	"github.com/ohstr/nmilat/nipIC"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/nipcw"
)

// These tests need no admin API and no network: they fabricate
// syntactically valid (but never-dialed) cash/circle/hub connection
// strings locally via nmilat's own Encode functions, then drive the
// compiled binary through error paths that fail before any network call
// is ever made (dial.Sniff-based mis-paste detection, and the
// no-wallet-configured check) — deterministic, always-runnable, no
// config.local.yaml required.

func fakeHex32(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func fakeCashHubConnection(t *testing.T) string {
	t.Helper()
	tok, err := nipcash.EncodeCashHubConnection(nipcash.CashHubConnection{
		WalletPubkey: fakeHex32(t),
		Secret:       fakeHex32(t),
		RelayURLs:    []string{"wss://fake.invalid"},
	})
	if err != nil {
		t.Fatalf("encode fake cash hub connection: %v", err)
	}
	return tok
}

func fakeCircleHubConnection(t *testing.T) string {
	t.Helper()
	tok, err := nipcw.EncodeCircleHubConnection(nipcw.CircleHubConnection{
		WalletPubkey: fakeHex32(t),
		Secret:       fakeHex32(t),
		RelayURLs:    []string{"wss://fake.invalid"},
	})
	if err != nil {
		t.Fatalf("encode fake circle hub connection: %v", err)
	}
	return tok
}

// fakeCashToken encodes a syntactically valid, never-dialed cash token —
// identityRequired nil/true/false lets a caller build the unspecified,
// pubkey-mode, or bearer-mode shape without touching any network.
func fakeCashToken(t *testing.T, identityRequired *bool) string {
	t.Helper()
	tok, err := nipcash.Encode(nipcash.Token{
		HRP:              "lokicash",
		WalletPubkey:     fakeHex32(t),
		Secret:           fakeHex32(t),
		RelayURLs:        []string{"wss://fake.invalid"},
		IdentityRequired: identityRequired,
	})
	if err != nil {
		t.Fatalf("encode fake cash token: %v", err)
	}
	return tok
}

// fakeSignedCashToken encodes a syntactically valid, never-dialed cash
// token carrying a genuine, self-signed mint-provenance pair — same
// technique as cmd/cash_receive_test.go's own signProvenance, replicated
// here since nipcash's own signing helper is unexported. Returns the
// token and the minter's recovered pubkey (hex), so a caller can assert
// decode's minter_pubkey matches without needing a live minting node.
func fakeSignedCashToken(t *testing.T, amountMillis uint64) (token, minterPubkeyHex string) {
	t.Helper()
	priv, pub := btcec.PrivKeyFromBytes([]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32,
	})
	walletPubkey := fakeHex32(t)
	payload := nipcash.MintPayload("lokicash", walletPubkey, amountMillis)
	first := sha256.Sum256([]byte(nipcash.LNSignedMessagePrefix + payload))
	second := sha256.Sum256(first[:])
	sig := ecdsa.SignCompact(priv, second[:], true)

	tok, err := nipcash.Encode(nipcash.Token{
		HRP:                  "lokicash",
		WalletPubkey:         walletPubkey,
		Secret:               fakeHex32(t),
		RelayURLs:            []string{"wss://fake.invalid"},
		MintSignature:        sig,
		AttestedAmountMillis: &amountMillis,
	})
	if err != nil {
		t.Fatalf("encode fake signed cash token: %v", err)
	}
	serialized := btcec.ToSerialized(pub)
	return tok, hex.EncodeToString(serialized[:])
}

func TestCashReceive_MisPasteCashHub(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	res := f.run("receive", fakeCashHubConnection(t))
	if res.ExitCode != 3 {
		t.Fatalf("receive cashhub1...: exit = %d, want 3 (invalid_input)\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if !jsonErrorContains(t, res.Stderr, "invalid_input", "Cash Hub") {
		t.Errorf("unexpected error body: %s", res.Stderr)
	}
}

func TestCashReceive_MisPasteCircleHub(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	res := f.run("receive", fakeCircleHubConnection(t))
	if res.ExitCode != 3 {
		t.Fatalf("receive circlehub1...: exit = %d, want 3 (invalid_input)\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if !jsonErrorContains(t, res.Stderr, "invalid_input", "join") {
		t.Errorf("unexpected error body: %s", res.Stderr)
	}
}

func TestCircleJoin_MisPasteCashHub(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	res := f.run("join", "--hub", fakeCashHubConnection(t), "--max-amount", "100")
	if res.ExitCode != 3 {
		t.Fatalf("join --hub cashhub1...: exit = %d, want 3 (invalid_input)\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if !jsonErrorContains(t, res.Stderr, "invalid_input", "Cash Hub") {
		t.Errorf("unexpected error body: %s", res.Stderr)
	}
}

// TestCashReceive_BearerWithoutEmbeddedSecretDegradesToReadOnly is a
// regression test for receive's Step 0: --secret is gone (a bearer-mode
// token's spending secret must arrive embedded in the token itself,
// "<token>#<bearer_secret>"), so a bearer-mode token pasted bare
// (identity_required: false, no "#") is no longer a usage error at all —
// it degrades to a read-only report, same contract as `decode --check`.
// Nothing here is dialed (the relay URL is fake, and --json always skips
// the optional live check unless --check is explicit), and nothing gets
// saved to the ledger.
func TestCashReceive_BearerWithoutEmbeddedSecretDegradesToReadOnly(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	bearer := false
	resp := f.mustJSON("receive", fakeCashToken(t, &bearer))
	if resp["received"] != false {
		t.Errorf(`receive bearer token with no embedded secret: got "received" = %v, want false`, resp["received"])
	}
	if resp["reason"] != "no_embedded_secret" {
		t.Errorf(`receive bearer token with no embedded secret: got "reason" = %v, want "no_embedded_secret"`, resp["reason"])
	}

	showResp := f.mustJSON("wallet", "show")
	if held, _ := showResp["held_tokens"].([]any); len(held) != 0 {
		t.Errorf("a bearer token with no embedded secret must not be saved, but wallet show reports: %v", held)
	}
}

// TestDecode_NoCheckStaysLocal confirms decode's default path never gains
// a "check" field (and, implicitly, never hangs on the fake/unreachable
// relay URLs these fixtures use) unless --check is explicitly passed —
// the always-local promise decode.go's own doc comment makes.
func TestDecode_NoCheckStaysLocal(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	bearer := false
	cashResp := f.mustJSON("decode", fakeCashToken(t, &bearer))
	if _, present := cashResp["check"]; present {
		t.Errorf("decode (no --check) on a cash token must not include a check field: %v", cashResp)
	}

	circleResp := f.mustJSON("decode", fakeCircleHubConnection(t))
	if _, present := circleResp["check"]; present {
		t.Errorf("decode (no --check) on a circlehub connection must not include a check field: %v", circleResp)
	}
}

// TestDecode_MintSignatureVerification confirms decode's own mint-
// provenance reporting (folded in from the now-removed cash
// verify-provenance) locally, against a self-signed fake token — no live
// minting node needed, since VerifyProvenance only checks that the
// signature recovers cleanly against the token's own fields.
func TestDecode_MintSignatureVerification(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	const amountMillis = uint64(12345)
	signed, minterPubkeyHex := fakeSignedCashToken(t, amountMillis)

	resp := f.mustJSON("decode", signed)
	if valid, _ := resp["mint_signature_valid"].(bool); !valid {
		t.Fatalf("decode (signed token): mint_signature_valid = %v, want true: %v", resp["mint_signature_valid"], resp)
	}
	if got, _ := resp["minter_pubkey"].(string); got != minterPubkeyHex {
		t.Errorf("decode (signed token): minter_pubkey = %q, want %q", got, minterPubkeyHex)
	}
	if got, _ := resp["attested_amount_millis"].(float64); uint64(got) != amountMillis {
		t.Errorf("decode (signed token): attested_amount_millis = %v, want %d", resp["attested_amount_millis"], amountMillis)
	}

	bearer := false
	unsignedResp := f.mustJSON("decode", fakeCashToken(t, &bearer))
	if _, present := unsignedResp["mint_signature_valid"]; present {
		t.Errorf("decode (unsigned token): must not include mint_signature_valid at all: %v", unsignedResp)
	}
	if _, present := unsignedResp["minter_pubkey"]; present {
		t.Errorf("decode (unsigned token): must not include minter_pubkey at all: %v", unsignedResp)
	}
}

// TestDecode_MintSignatureVerification_TextModeDoesNotOverclaimTrust is the
// bug this exact fixture (fakeSignedCashToken) was already built to prove
// live: a signature that recovers cleanly is real cryptography, but it's
// signed by a throwaway key this test controls, not any mint cashctl or
// its user has ever heard of — "valid" here means self-consistent, not
// trustworthy. Text mode used to print a bare "minter: <pubkey>", which
// reads as cashctl vouching for it; it must now say so explicitly. --json
// stays untouched on purpose (TestDecode_MintSignatureVerification, above,
// already pins mint_signature_valid/minter_pubkey — an agent parsing JSON
// is expected to know VerifyProvenance's real semantics; a human skimming
// text output is the one who needs the caveat spelled out).
func TestDecode_MintSignatureVerification_TextModeDoesNotOverclaimTrust(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	const amountMillis = uint64(12345)
	signed, minterPubkeyHex := fakeSignedCashToken(t, amountMillis)

	// EOF stdin (no --check answer needed): decode's own network-check
	// prompt now defaults to no, so this returns immediately rather than
	// dialing the fake/unreachable relay URL fakeSignedCashToken sets.
	res := f.runInteractive("", "decode", signed)
	if res.ExitCode != 0 {
		t.Fatalf("decode (text mode, signed token): exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, minterPubkeyHex) {
		t.Errorf("decode (text mode) doesn't print the recovered minter pubkey: %s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "trusted mint") {
		t.Errorf("decode (text mode) minter line doesn't caveat that a valid signature isn't a trusted mint: %s", res.Stdout)
	}
}

// TestDecode_BearerGiftString confirms decode splits NIP-CASH's optional
// "<token>#<bearer_secret>" gift-string presentation (§Bearer Slices →
// Presenting a Bearer Slice as One String) before decoding — entirely
// local, a fake bearer token and a fake secret never touch a network —
// and reports only that a secret was present, never its value, in either
// output mode. A token with no "#" at all must decode exactly as before.
func TestDecode_BearerGiftString(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	bearer := false
	token := fakeCashToken(t, &bearer)
	secret := fakeHex32(t)
	combined := token + "#" + secret

	resp := f.mustJSON("decode", combined)
	if present, _ := resp["embedded_bearer_secret_present"].(bool); !present {
		t.Errorf("decode (gift string): embedded_bearer_secret_present = %v, want true: %v", resp["embedded_bearer_secret_present"], resp)
	}
	if wp, _ := resp["wallet_pubkey"].(string); wp == "" {
		t.Errorf("decode (gift string): empty wallet_pubkey, splitting broke the token half: %v", resp)
	}

	// The secret must never appear anywhere in the response, in any field
	// (fixture.run always passes --json, so this is the JSON response —
	// decodeCashToken's text-mode line is driven by the same boolean and
	// prints only "embedded", never the value).
	res := f.run("decode", combined)
	if strings.Contains(res.Stdout, secret) {
		t.Errorf("decode (gift string) leaked the bearer secret into stdout: %s", res.Stdout)
	}

	// No "#" at all: must decode exactly as before, no false positive.
	plainResp := f.mustJSON("decode", token)
	if _, present := plainResp["embedded_bearer_secret_present"]; present {
		t.Errorf("decode (plain token, no gift string): must not report embedded_bearer_secret_present at all: %v", plainResp)
	}
}

// TestCashVerifyProvenance_CommandRemoved confirms `cash verify-provenance`
// is gone (folded into decode, no alias — docs/ux-review.md's build order
// item 5). cash --help is a sufficient, deterministic proof of removal
// without relying on cobra's own particular behavior for an unmatched
// subcommand of a RunE-less parent (which falls through to printing the
// parent's help and exiting 0, not an error — a general cobra
// characteristic, not something specific to this removal).
func TestCashVerifyProvenance_CommandRemoved(t *testing.T) {
	f := newFixture(t)
	res := f.run("cash", "--help")
	if strings.Contains(res.Stdout, "verify-provenance") {
		t.Errorf("verify-provenance still listed under `cashctl cash --help`, want it removed: %s", res.Stdout)
	}
}

// TestRedeem_ToFlagRemoved confirms redeem's --to flag was actually
// renamed, not just documented as renamed: passing the old name must be
// rejected as an unknown flag, in cashctl's own classified --json error
// shape (cobra's flag-parsing errors are wrapped into it, confirmed
// before writing this assertion) — before RunE ever runs, so no wallet or
// held token is needed to exercise it.
func TestRedeem_ToFlagRemoved(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	res := f.run("redeem", "--to", "somewallet")
	if res.ExitCode != 2 {
		t.Fatalf("redeem --to (removed flag): exit = %d, want 2 (usage)\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if !jsonErrorContains(t, res.Stderr, "usage", "--to") {
		t.Errorf("unexpected error body: %s", res.Stderr)
	}
}

// TestCashTransfer_MalformedNpubTarget confirms an invalid npub target is
// rejected before anything else — resolveTarget runs before ledger.Load,
// so this needs no held token and no network call at all.
func TestCashTransfer_MalformedNpubTarget(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	res := f.run("transfer", "npub1thisisnotvalidbech32atall", "5000")
	if res.ExitCode != 3 {
		t.Fatalf("transfer (malformed npub target): exit = %d, want 3 (invalid_input)\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if !jsonErrorContains(t, res.Stderr, "invalid_input", "npub") {
		t.Errorf("unexpected error body: %s", res.Stderr)
	}
}

// TestCashTransfer_NConnectionWithoutIA_UsageError confirms an
// nconnection1... target with no IA specified is refused under --yes
// (no terminal to prompt from) with a usage error naming --ia — entirely
// local: a real nconnection1... is built via nipIC.EncodeNConnection
// (pure encoding, no network), and resolveTarget's NeedsIAError surfaces
// before ledger.Load is ever reached, so no held token is needed either.
func TestCashTransfer_NConnectionWithoutIA_UsageError(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	nconn, err := nipIC.EncodeNConnection(nipIC.NewConnectionKey("discord", "482910"), []string{"wss://fake.invalid"}, "discord")
	if err != nil {
		t.Fatalf("encode nconnection: %v", err)
	}

	res := f.run("transfer", nconn, "5000", "--yes")
	if res.ExitCode != 2 {
		t.Fatalf("transfer nconnection (no --ia, --yes): exit = %d, want 2 (usage)\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if !jsonErrorContains(t, res.Stderr, "usage", "--ia") {
		t.Errorf("unexpected error body: %s", res.Stderr)
	}
}

// TestCashTransfer_NConnectionWithIA_ResolvesPastTarget confirms --ia
// actually lets an nconnection1... target resolve successfully — proven
// by getting *past* target resolution to the next stage (no held tokens),
// rather than failing on the target itself. Still needs no live server:
// the failure this hits is purely local ("you have no held cash tokens").
func TestCashTransfer_NConnectionWithIA_ResolvesPastTarget(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	nconn, err := nipIC.EncodeNConnection(nipIC.NewConnectionKey("discord", "482910"), []string{"wss://fake.invalid"}, "discord")
	if err != nil {
		t.Fatalf("encode nconnection: %v", err)
	}

	res := f.run("transfer", nconn, "5000", "--ia", fakeHex32(t), "--yes")
	if res.ExitCode != 4 {
		t.Fatalf("transfer nconnection --ia <hex>: exit = %d, want 4 (not_found, i.e. past target resolution)\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if !jsonErrorContains(t, res.Stderr, "not_found", "no held cash tokens") {
		t.Errorf("unexpected error body (expected to fail on 'no held tokens', proving --ia resolved successfully): %s", res.Stderr)
	}
}

// TestCashTransfer_MalformedAmountBlamesTheAmountNotTheTarget is the live
// evidence for the disambiguateTransferArgs fix: a malformed amount
// alongside a genuinely valid target used to blame the target — "<hex
// pubkey> is not a valid amount in loki" — because the old position-only
// fallback (args[0] fails ParseAmount -> assume args[0] is the target)
// never checked whether the OTHER argument was a plausible target either.
// Needs no live server: this fails purely on local argument parsing,
// before any network call.
func TestCashTransfer_MalformedAmountBlamesTheAmountNotTheTarget(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	target := fakeHex32(t)
	for _, args := range [][]string{
		{"transfer", "1.5x", target, "--yes"},
		{"transfer", target, "1.5x", "--yes"},
	} {
		res := f.run(args...)
		if res.ExitCode != 3 {
			t.Errorf("cashctl %v: exit %d, want 3 (invalid_input)\nstderr: %s", args, res.ExitCode, res.Stderr)
			continue
		}
		if strings.Contains(res.Stderr, target) {
			t.Errorf("cashctl %v: error blames the target, not the malformed amount: %s", args, res.Stderr)
		}
		if !jsonErrorContains(t, res.Stderr, "invalid_input", "1.5x") {
			t.Errorf("cashctl %v: error doesn't name \"1.5x\" as the invalid amount: %s", args, res.Stderr)
		}
	}
}

// jsonErrorContains parses stderr as cashctl's --json error shape and checks
// its code and that its error message contains substr.
func jsonErrorContains(t *testing.T, stderr, wantCode, substr string) bool {
	t.Helper()
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal([]byte(stderr), &body); err != nil {
		t.Errorf("decode stderr JSON: %v: %s", err, stderr)
		return false
	}
	if body.Code != wantCode {
		t.Errorf("code = %q, want %q", body.Code, wantCode)
		return false
	}
	return strings.Contains(body.Error, substr)
}

// jsonErrorContainsAny is jsonErrorContains for a wallet-decline's own raw
// message (CLIError.RawMessage — see internal/output/errors.go): unlike a
// cashctl-generated message, its exact wording isn't this codebase's own
// to pin down, so callers checking for a *concept* (e.g. "this decline was
// about expiry") rather than an exact phrase should list every phrasing
// they've actually observed rather than assume one.
func jsonErrorContainsAny(t *testing.T, stderr, wantCode string, anySubstr ...string) bool {
	t.Helper()
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal([]byte(stderr), &body); err != nil {
		t.Errorf("decode stderr JSON: %v: %s", err, stderr)
		return false
	}
	if body.Code != wantCode {
		t.Errorf("code = %q, want %q", body.Code, wantCode)
		return false
	}
	for _, substr := range anySubstr {
		if strings.Contains(body.Error, substr) {
			return true
		}
	}
	return false
}
