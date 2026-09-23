//go:build integration

package integration

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nipIC "github.com/ohstr/nmilat/nipIC"
	"github.com/ohstr/nmilat/utils"
)

// A local Identity Authority, for testing connection-key targets end to end.
//
// Nothing here talks to a relay or a real IA service. An nconnection target
// resolves purely locally — ParseTarget turns nconnection1... plus --ia into
// connection:<platform>:<external-id>:<ia-pubkey> with no lookup at all — and
// the claiming half reads its attestation from a FILE
// (credential.loadAttestation). So an IA is just a keypair that signs a
// Kind 35522 event, which nmilat already builds for us via
// nipIC.NewAttestation.
//
// Generated per test rather than checked into testdata on purpose: an
// attestation binds a ConnectionKey to one specific user pubkey, and every
// fixture here generates fresh identities, so a committed file would pin a
// key that nothing else in the run uses.

type localIA struct {
	privHex string
	PubHex  string
}

func newLocalIA(t *testing.T) *localIA {
	t.Helper()
	priv := randomHex32(t)
	pub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatalf("deriving IA pubkey: %v", err)
	}
	return &localIA{privHex: priv, PubHex: pub}
}

// Attest writes a signed attestation binding (platform, externalID) to
// userPubkey and returns the file path, in the form
// `--as connection-key:<privkey>,<platform>,<external-id>,<file>` expects.
// expireInDays of 0 means no expiry.
func (ia *localIA) Attest(t *testing.T, platform nipIC.WebIdentity, externalID, userPubkey string, expireInDays int) string {
	t.Helper()
	ev, err := nipIC.NewAttestation(nipIC.AttestationParams{
		PrivateKey:    ia.privHex,
		ConnectionKey: nipIC.NewConnectionKey(platform, externalID),
		UserPubkey:    userPubkey,
		Platform:      platform,
		Evidence: nipIC.Evidence{
			Platform:   platform,
			UserID:     externalID,
			Username:   "cashctl-integration",
			VerifiedAt: time.Now().Unix(),
		},
		ExpirationDays: expireInDays,
	})
	if err != nil {
		t.Fatalf("NewAttestation: %v", err)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal attestation: %v", err)
	}
	file := filepath.Join(t.TempDir(), "attestation.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatalf("write attestation: %v", err)
	}
	return file
}

func randomHex32(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

// TestConnectionTarget_TransferReachesTheHubAndIsRefusedForAnUntrustedIA
// is as far as an nconnection transfer can be driven against a hub that has
// not registered our IA — and it is considerably further than the two tests
// in cash_errors_test.go reach, which stop at "no held cash tokens" before
// any Hub call happens.
//
// Everything on the sending side is local: ParseTarget turns nconnection1...
// plus --ia into connection:<platform>:<external-id>:<ia-pubkey> with no
// lookup. So reaching a Hub-side BAD_REQUEST about the IA specifically
// proves the whole chain worked — decode, target construction, credential
// selection and the cash_transfer call — and that the only thing left is a
// trust decision lokihub makes from its own configured IA list.
//
// Registering a trusted IA is not exposed by the admin API this suite uses
// (see adminCreateAppRequest — no IA fields), so a delivery that actually
// lands still needs hub-side configuration. When that exists, this fixture
// already supplies everything else: newLocalIA(t).Attest(...) produces the
// attestation file the recipient claims with.
func TestConnectionTarget_TransferReachesTheHubAndIsRefusedForAnUntrustedIA(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}

	ia := newLocalIA(t)
	const platform, externalID = nipIC.WebIdentity("discord"), "482910"
	nconn, err := nipIC.EncodeNConnection(nipIC.NewConnectionKey(platform, externalID), []string{"wss://relay.invalid"}, platform)
	if err != nil {
		t.Fatalf("encode nconnection: %v", err)
	}

	entry := f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 40_000))["entry"].(map[string]any)
	id := entryID(t, entry)

	res := f.run("transfer", "10", nconn, "--ia", ia.PubHex, "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("this hub accepted an unregistered IA — the fixture can now drive a real delivery, see this test's doc comment: %s", res.Stdout)
	}
	// Not retryable, and blamed on the IA rather than reported as a bad
	// token or a network problem: an agent has to be able to tell "pick a
	// different Identity Authority" apart from "back off and retry".
	assertTerminalFailure(t, res, "transfer to a connection target with an untrusted IA", "invalid_input")
	if !strings.Contains(res.Stderr, "Identity Authority") {
		t.Errorf("error should name the Identity Authority as the problem, got: %s", res.Stderr)
	}

	// And the refusal must be clean: the money stays exactly where it was.
	var stillHeld bool
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	for _, h := range held {
		if e, _ := h.(map[string]any); e["id"] == id {
			stillHeld = true
		}
	}
	if !stillHeld {
		t.Errorf("token %s is no longer held after a refused transfer — the ledger moved money the Hub never accepted", id)
	}
}

// TestLocalIA_AttestationIsAcceptedAsACredential proves the fixture itself is
// sound independently of any hub: a locally-minted attestation parses as a
// connection-key credential through the real compiled binary. Without this,
// a failure in the test above could not be told apart from a malformed
// fixture.
func TestLocalIA_AttestationIsAcceptedAsACredential(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	userPriv := randomHex32(t)
	userPub, err := utils.GetPublicKey(userPriv)
	if err != nil {
		t.Fatalf("deriving user pubkey: %v", err)
	}
	ia := newLocalIA(t)
	file := ia.Attest(t, "discord", "482910", userPub, 90)

	// No held tokens, so this stops at not_found — AFTER the credential has
	// been parsed and the attestation file read. A malformed attestation
	// fails earlier, as invalid_input.
	res := f.run("transfer", "5", fakeHex32(t), "--as", "connection-key:"+userPriv+",discord,482910,"+file, "--yes")
	if res.ExitCode == 3 {
		t.Fatalf("the locally-minted attestation was rejected as malformed: %s", res.Stderr)
	}
	if !jsonErrorContains(t, res.Stderr, "not_found", "no held cash tokens") {
		t.Errorf("expected to get past credential parsing to 'no held tokens', got: %s", res.Stderr)
	}
}
