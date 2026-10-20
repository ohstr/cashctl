//go:build integration

// attack_surface_test.go stages, against a live Hub, attacks whose outcome a
// cashctl user can observe: racing a shared secret, watching the shared NWC
// transport for bill traffic, a slice deleted or a tombstone withheld by the
// Hub owner, forging a follow set, and smuggling terminal escapes through a
// hub label. A replayed private request is not staged here: the relay drops
// an event it already has before the Hub sees it, and the Hub's own nonce
// check is lokihub's suite's to test.
package integration

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/nipcw"
	"github.com/ohstr/nmilat/utils"
)

// kindNWCRequest is NIP-47's request kind: the shared transport no bill
// method may travel over (NIP-CASH §Private Transport).
const kindNWCRequest = 23194

func decodeToken(t *testing.T, token string) nipcash.Token {
	t.Helper()
	bare, _ := nipcash.SplitCashSliceString(token)
	tok, err := nipcash.Decode(bare)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	return tok
}

// TestRace_SimultaneousReceiveOfOneGift: two people handed the same cash
// note receive it at the same moment from separate wallets. Receiving
// re-keys the bill, so exactly one of them can still spend it afterwards.
func TestRace_SimultaneousReceiveOfOneGift(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	a, b := newFixture(t), newFixture(t)
	a.mustJSON("wallet", "init")
	b.mustJSON("wallet", "init")
	gift := mintCashGift(t, hub, 40_000)

	runConcurrentAcrossFixtures(fixtureCall{f: a, args: []string{"receive", gift}}, fixtureCall{f: b, args: []string{"receive", gift}})

	spendable := 0
	for _, f := range []*fixture{a, b} {
		held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
		for _, h := range held {
			if f.run("cash", "status", "--token", entryID(t, h)).ExitCode == 0 {
				spendable++
			}
		}
	}
	if spendable != 1 {
		t.Errorf("%d wallets can still spend the shared note after receiving it at once, want exactly 1", spendable)
	}
}

// TestNoBillTrafficOnTheSharedTransport watches the relay for any NIP-47
// request addressed to a held bill while every bill command runs, and
// checks a bill stripped of its mint provenance (so the Hub's private
// transport cannot be located) is refused rather than sent over it.
func TestNoBillTrafficOnTheSharedTransport(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f, pub := newMember(t)
	requireOK(t, "connect add", f.run("connect", "add", "dest", hub.PairingUri))

	tokens := []string{
		mintPubkeyTokenFromHub(t, hub, pub, 30_000),
		mintPubkeyTokenFromHub(t, hub, pub, 30_000),
		mintCashGift(t, hub, 30_000),
	}
	unsigned := decodeToken(t, mintPubkeyTokenFromHub(t, hub, pub, 30_000))
	var stops []func() int64
	for _, tok := range tokens {
		d := decodeToken(t, tok)
		stops = append(stops, countEvents(t, d.RelayURLs[0], kindNWCRequest, d.WalletPubkey))
	}
	stops = append(stops, countEvents(t, unsigned.RelayURLs[0], kindNWCRequest, unsigned.WalletPubkey))

	var ids []string
	for _, tok := range tokens {
		ids = append(ids, entryID(t, f.mustJSON("receive", tok)["entry"]))
	}
	f.mustJSON("cash", "status")
	_, other := newIdentity(t)
	requireOK(t, "transfer", f.run("transfer", "5", other, "--token", ids[0], "--yes"))
	requireOK(t, "consolidate", f.run("consolidate", "--yes"))
	requireOK(t, "redeem --all", f.run("redeem", "--all", "--into", "dest", "--yes"))

	stripped, err := nipcash.Encode(nipcash.Token{
		HRP: unsigned.HRP, WalletPubkey: unsigned.WalletPubkey, Secret: unsigned.Secret,
		RelayURLs: unsigned.RelayURLs, IdentityRequired: unsigned.IdentityRequired,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := f.run("receive", stripped); res.ExitCode == 0 {
		t.Errorf("receive of a bill with no mint provenance succeeded: %s", res.Stdout)
	}

	for i, stop := range stops {
		if n := stop(); n != 0 {
			t.Errorf("bill %d: %d NIP-47 request(s) addressed to it on the shared transport, want none", i, n)
		}
	}
}

// TestDeletedSlice_IsNotFound: the Hub owner removes this holder's slice of
// a bill. The holder is told it is gone, not that it was spent.
func TestDeletedSlice_IsNotFound(t *testing.T) {
	admin, hub := plainClientHub(t)
	f, pub := newMember(t)
	_, other := newIdentity(t)
	token := mintTwoRecipientBill(t, hub, pub, other, 3_000, 2_000)
	id := entryID(t, f.mustJSON("receive", token)["entry"])

	walletPub := decodeToken(t, token).WalletPubkey
	claims, err := admin.listCashWalletClaims(hub.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleted := false
	for _, c := range claims {
		if c.WalletPubkey == walletPub && c.IdentityValue == pub && !c.Archived {
			if err := admin.deleteCashClaim(hub.ID, c.WalletAppID, c.ID); err != nil {
				t.Fatalf("delete the holder's slice: %v", err)
			}
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("no live slice for %s on wallet %s", pub, walletPub)
	}

	requireFailure(t, "cash status of a deleted slice", f.run("cash", "status", "--token", id), 4, "not_found", "")
	requireFailure(t, "redeem of a deleted slice", f.run("redeem", "--token", id, "--invoice", makeHubInvoice(t, hub, 3_000), "--yes"), 4, "not_found", "")
}

// TestZeroRetention_SilenceIsNotSpent: with no tombstone kept, a destroyed
// bill gets no answer at all. cashctl must not read that silence as spent
// (AGENTS.md: spent is never inferred from silence), so the copy still holds it.
func TestZeroRetention_SilenceIsNotSpent(t *testing.T) {
	admin := adminOrSkip(t)
	zero := 0
	hub := setUpCashHubOpts(t, admin, cashHubOpts{SpentRetentionSecs: &zero})
	a, pub := newMember(t)
	const amount = uint64(30_000)
	token := mintPubkeyTokenFromHub(t, hub, pub, amount)
	a.mustJSON("receive", token)

	b := newFixture(t)
	if err := os.Remove(b.configDir); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(b.configDir, os.DirFS(a.configDir)); err != nil {
		t.Fatalf("copy wallet: %v", err)
	}
	requireOK(t, "redeem", a.run("redeem", "--invoice", makeHubInvoice(t, hub, amount), "--yes"))
	requireBillSpentAway(t, admin, hub.ID, token, "after redeem")

	held, _ := b.mustJSON("wallet", "show")["held_tokens"].([]any)
	if len(held) != 1 {
		t.Fatalf("the copied wallet holds %d bills, want 1", len(held))
	}
	res := b.run("cash", "status", "--token", entryID(t, held[0]))
	if res.ExitCode == 0 || res.ExitCode == 10 {
		t.Fatalf("cash status of a bill the Hub went silent on: exit %d, want a failure that is not spent (10)\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if still, _ := b.mustJSON("wallet", "show")["held_tokens"].([]any); len(still) != 1 {
		t.Errorf("silence marked the bill spent: held_tokens = %v", still)
	}
}

// TestForgedFollowSet_IsIgnored: a newer follow set with the same d, signed
// by someone other than the provider, never changes who may join.
func TestForgedFollowSet_IsIgnored(t *testing.T) {
	admin := adminOrSkip(t)
	info, err := admin.getInfo()
	if err != nil || info.GeneralRelay == "" {
		t.Skipf("skipping: no general relay (info=%+v err=%v)", info, err)
	}
	relayURL := strings.TrimSpace(strings.Split(info.GeneralRelay, ",")[0])
	member, memberPub := newMember(t)
	outsider, outsiderPub := newMember(t)
	providerPriv, forgerPriv := randomHex32(t), randomHex32(t)
	providerPub, err := utils.GetPublicKey(providerPriv)
	if err != nil {
		t.Fatal(err)
	}
	d := randomFollowSetD(t)
	opts := followSetHubOpts.withDefaults()
	req := circleHubRequest("forged follow_set circle_hub", "follow_set", followSetHubScopes, opts)
	req.ProviderPubkey, req.FollowSetD = providerPub, d
	hub := createCircleHubApp(t, admin, req, opts.FundLoki)

	now := uint64(time.Now().Unix())
	publishProviderFollowSet(t, relayURL, providerPriv, d, now, memberPub)
	refreshUntilResolved(t, admin, hub.ID)
	publishProviderFollowSet(t, relayURL, forgerPriv, d, now+60, outsiderPub)
	refreshUntilResolved(t, admin, hub.ID)

	requireFailure(t, "outsider named only by the forged set", joinFollowSet(outsider, hub), 7, "auth", "RESTRICTED")
	requireOK(t, "member named by the provider", joinFollowSet(member, hub))
}

// TestCircleHubLabel_ControlBytesNeverReachTheTerminal: a hub label is
// chosen by whoever made the connection string, so it may carry terminal
// escapes or bidirectional overrides meant to rewrite what the user sees.
func TestCircleHubLabel_ControlBytesNeverReachTheTerminal(t *testing.T) {
	conn, err := nipcw.EncodeCircleHubConnection(nipcw.CircleHubConnection{
		WalletPubkey: fakeHex32(t),
		Secret:       fakeHex32(t),
		RelayURLs:    []string{"wss://fake.invalid"},
		Label:        "Family\x1b[2J\x1b]0;pwned\x07 circle ‮gnp.exe",
	})
	if err != nil {
		t.Fatalf("encode circle hub connection: %v", err)
	}
	f := newFixture(t)
	f.mustJSON("wallet", "init")
	for name, res := range map[string]result{
		"decode --json": f.run("decode", conn),
		"decode":        f.runInteractive("", "decode", conn),
		"join --json":   f.run("join", conn, "10"),
		"join":          f.runInteractive("", "join", conn, "10"),
	} {
		out := res.Combined()
		for _, bad := range []string{"\x1b", "\x07", "‮"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: output carries the label's raw %q: %q", name, bad, out)
			}
		}
	}
}

// TestConfigDir_LooseExistingDirIsMadePrivate: a state directory created
// earlier as 0755 is tightened to 0700 the first time cashctl uses it.
func TestConfigDir_LooseExistingDirIsMadePrivate(t *testing.T) {
	f := newFixture(t)
	if err := os.Chmod(f.configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	res := f.run("wallet", "show")
	if res.ExitCode != 0 {
		t.Fatalf("wallet show: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	info, err := os.Stat(f.configDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("config dir mode = %v (err %v), want 0700", info.Mode().Perm(), err)
	}
	if !strings.Contains(res.Stderr, "private") {
		t.Errorf("no notice that the directory was made private: %q", res.Stderr)
	}
}
