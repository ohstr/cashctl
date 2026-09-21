//go:build integration

package integration

import (
	"regexp"
	"strings"
	"testing"
)

var (
	nwcSecretRe = regexp.MustCompile(`secret=([0-9a-f]{64})`)
	hex64Re     = regexp.MustCompile(`[0-9a-f]{64}`)
)

// `redeem --into <raw nostr+walletconnect://...secret=...>` uses the value
// verbatim as the wallet's display name, so the destination wallet's secret
// ends up in the confirmation prompt, the --json `to_wallet`, and the stored
// history. A display name must never carry a connection secret.
func TestRedeem_IntoRawWalletURI_DoesNotEchoWalletSecret(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin) // its pairing URI is a wallet that can make invoices
	secretMatch := nwcSecretRe.FindStringSubmatch(hub.PairingUri)
	if secretMatch == nil {
		t.Fatalf("no secret= in the hub pairing URI, cannot run this check")
	}
	secret := secretMatch[1]

	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 30_000))

	res := f.run("redeem", "--into", hub.PairingUri, "--yes")
	if strings.Contains(res.Stdout, secret) || strings.Contains(res.Stderr, secret) {
		t.Errorf("redeem --into <raw URI> echoed the destination wallet's secret:\nstdout: %s\nstderr: %s", res.Stdout, res.Stderr)
	}
	if hist := f.run("wallet", "history"); strings.Contains(hist.Stdout, secret) {
		t.Errorf("wallet history persisted the destination wallet's secret: %s", hist.Stdout)
	}
}

// A wallet that only ever held bearer cash never ran `init` and never needs
// to for receive/transfer/redeem/history — but `wallet show`, the command
// that lists held token ids, refuses with "run `cashctl init` first".
func TestWalletShow_WorksForBearerOnlyWalletWithoutIdentity(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t) // no init
	f.mustJSON("receive", mintBearerGift(t, hub, 20_000), "--yes")

	res := f.run("wallet", "show")
	if res.ExitCode != 0 {
		t.Fatalf("wallet show on a bearer-only wallet: exit %d (%s) — the held token is otherwise fully usable without an identity\nstderr: %s",
			res.ExitCode, "wallet show requires `cashctl init`", res.Stderr)
	}
	if n := len(mustDecodeJSON(t, "wallet show", res.Stdout)["held_tokens"].([]any)); n != 1 {
		t.Errorf("wallet show lists %d held tokens, want 1", n)
	}
}

// `consolidate --to bearer-target` generates a fresh bearer secret. It must
// not be printed before the user has confirmed anything (transfer already
// suppresses it; consolidate's copy of the same line does not).
func TestConsolidate_ToBearerTarget_DoesNotPrintSecretBeforeConfirm(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	id1 := entryID(t, f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 30_000))["entry"])
	id2 := entryID(t, f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 30_000))["entry"])

	res := f.runInteractive("n\n", "consolidate", "--sources", id1+","+id2, "--to", "bearer-target")
	if hex64Re.MatchString(res.Stdout) || hex64Re.MatchString(res.Stderr) {
		t.Errorf("consolidate --to bearer-target printed the freshly generated bearer secret before the user confirmed (answered n):\n%s", res.Stdout)
	}
	if n := heldCount(t, f); n != 2 {
		t.Errorf("declining must leave both tokens held, got %d", n)
	}
}

// Amounts that overflow uint64 once converted to mloki must be rejected, not
// silently wrapped: `transfer <target> 18446744073709552` used to send 384
// mloki (0.384 loki) and report success, and neighbouring values become
// negative when cast to int64.
func TestAmounts_OverflowingLokiValuesAreRejected(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 30_000))

	for _, amt := range []string{"18446744073709552", "18446744073709551", "9223372036854776"} {
		res := f.run("transfer", fakeHex32(t), amt, "--yes")
		if res.ExitCode == 0 {
			t.Errorf("transfer of %s loki (overflows uint64 mloki) exited 0 — it wrapped to a small amount and moved real funds:\n%s", amt, res.Stdout)
			continue
		}
		if e := parseErrorReport(t, res); e.Code != "usage" && e.Code != "invalid_input" {
			t.Errorf("transfer of %s loki: code=%q, want usage/invalid_input\nstderr: %s", amt, e.Code, res.Stderr)
		}
	}
}

// Bech32 is case-insensitive, so the uppercase spelling of a held token is
// the same token. Receiving it again must be refused as already held — not
// saved as a second entry that doubles the displayed balance.
func TestReceive_UppercaseSpellingOfHeldToken_IsTheSameToken(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	token := mintPubkeyTokenFromHub(t, hub, pub, 20_000)
	f.mustJSON("receive", token)

	res := f.run("receive", strings.ToUpper(token))
	if res.ExitCode == 0 {
		t.Errorf("receiving the uppercase spelling of an already-held token succeeded: %s", res.Stdout)
	}
	if n := heldCount(t, f); n != 1 {
		t.Errorf("held tokens = %d after re-receiving the uppercase spelling, want 1 (one token counted twice)", n)
	}
}

// `join --json` used to embed the SDK's own raw CreateCircleWalletResponse
// (no JSON tags of its own): CamelCase keys, and — the actual secret leak
// — the new wallet's PairingURI, already saved locally under a name and
// never needed again from output alone.
func TestJoin_DoesNotPrintNewWalletSecret(t *testing.T) {
	admin := adminOrSkip(t)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	hub := setUpCircleHub(t, admin, pub)
	if hub.CircleHubToken == nil || *hub.CircleHubToken == "" {
		t.Fatalf("setUpCircleHub: no circleHubToken in response: %+v", hub)
	}
	secretMatch := nwcSecretRe.FindStringSubmatch(hub.PairingUri)

	res := f.run("join", "--hub", *hub.CircleHubToken, "--max-amount", "100", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("join: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stdout, "PairingURI") || strings.Contains(res.Stdout, "nostr+walletconnect://") {
		t.Errorf("join --json embedded the raw pairing URI: %s", res.Stdout)
	}
	if secretMatch != nil && strings.Contains(res.Stdout, secretMatch[1]) {
		t.Errorf("join --json leaked a secret: %s", res.Stdout)
	}
	out := mustDecodeJSON(t, "join", res.Stdout)
	resp, _ := out["response"].(map[string]any)
	if resp == nil || resp["wallet_pubkey"] == "" {
		t.Errorf("join --json response is missing the expected safe fields: %v", out)
	}
}
