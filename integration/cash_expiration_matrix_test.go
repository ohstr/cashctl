//go:build integration

// cash_expiration_matrix_test.go sweeps expiration handling across every
// money-moving/inspection command this audit angle covers — cash_transfer,
// cash_consolidate, decode --check, and cash list-recipients — none of
// which previously showed any awareness of a token's Hub-side expiry the
// way cash_redeem's own preview already did (cash_redeem.go's
// resolveAmountAndPreview/previewSuffix, exercised by
// cash_confirmation_test.go's TestCashRedeem_PreviewShowsExpiryWarning).
//
// Ground truth, confirmed by reading lokihub's own
// integration/expiration_test.go and nip47/permissions/permissions.go
// before writing any of this: a Cash Wallet's Hub-side expiry
// (nip47/permissions.HasPermission, keyed on AppPermission.ExpiresAt) gates
// EVERY scoped method on that wallet uniformly — cash_redeem, cash_transfer,
// cash_consolidate, list_recipients, get_balance all reject with
// ERROR_EXPIRED once it lapses; get_info/get_budget are always-granted and
// survive. That check is per-CONNECTION (the app actually dialed), not
// per-source: cash_consolidate_controller.go's resolveConsolidateSource
// never re-checks any OTHER source's own wallet expiry, only the calling
// app's. See docs/private/audit-round2-expiration-matrix.md for the full
// write-up; this file is the live evidence.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
)

// shortCashExpirySecs is short enough to expire mid-test (mint -> receive
// -> wait -> act), long enough that the mint+receive round trip against a
// real, possibly-loaded shared dev Hub can't itself race past it — mirrors
// lokihub's own integration/expiration_test.go shortLivedExpirySecs, with
// a bit more headroom since this suite's own receive step (an extra
// network round trip lokihub's own equivalent test doesn't have to make)
// eats into the same window.
const shortCashExpirySecs = 6

// waitPastCashExpiry sleeps well past a shortCashExpirySecs token's own
// expiry, well before the periodic 5-minute expiry-sweep
// (service/cash_cleanup_service.go) would ever delete it out from under
// this test — this is exercising the live permission check, not racing
// deletion.
func waitPastCashExpiry() { time.Sleep(9 * time.Second) }

// mintPubkeyTokenExpiry mints a pubkey-mode cash token with an explicit
// per-mint expiry (nipcash.MintCashParams.Expiry — 0 defers to the Hub's
// own ceiling, NIP-CASH §Minting Cash), unlike mintPubkeyTokenFromHub
// (cash_test.go), which never sets one. Needed to mint two tokens off the
// SAME hub with deliberately different expiries within one test.
func mintPubkeyTokenExpiry(t *testing.T, hub adminCreateAppResponse, pubkeyHex string, amountMillis uint64, expirySecs int) string {
	t.Helper()
	what := fmt.Sprintf("mint_cash (expiry=%ds)", expirySecs)
	return retryEphemeralNWC(t, what, func(ctx context.Context) (string, error) {
		cashClient := dialCash(t, ctx, hub.PairingUri)
		result, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
			Recipients: []nipcash.Allocation{nipcash.Send(nipcash.Pubkey(pubkeyHex), amountMillis)},
			Expiry:     time.Duration(expirySecs) * time.Second,
		})
		if err != nil {
			return "", err
		}
		return result.CashToken, nil
	})
}

// mintSignedPubkeyTokenExpiry is mintPubkeyTokenExpiry's mint-provenance
// variant, mirroring cash_selection_test.go's own
// mintSignedPubkeyTokenFromHub: cash_transfer's client-side cash selection
// (ledger.SelectForAmount) groups "same minter" candidates by their
// recovered mint-signature pubkey, not merely by having come from the same
// Cash Hub (every mint_cash call spins off a brand-new wallet_pubkey
// regardless), so exercising transferWithAutoConsolidate's own
// auto-consolidate path needs MintSignature: true on both mints.
func mintSignedPubkeyTokenExpiry(t *testing.T, f *fixture, hub adminCreateAppResponse, pubkeyHex string, amountMillis uint64, expirySecs int) (token, minterPubkeyHex string, ok bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cashClient := dialCash(t, ctx, hub.PairingUri)
	result, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
		Recipients:    []nipcash.Allocation{nipcash.Send(nipcash.Pubkey(pubkeyHex), amountMillis)},
		Expiry:        time.Duration(expirySecs) * time.Second,
		MintSignature: true,
	})
	if err != nil {
		t.Fatalf("mint_cash (expiry=%ds, MintSignature: true): %v", expirySecs, err)
	}
	decodeResp := f.mustJSON("decode", result.CashToken)
	minter, valid := decodeResp["minter_pubkey"].(string)
	return result.CashToken, minter, valid && minter != ""
}

// nwcCodeFromError parses stderr as cashctl's --json error shape and
// returns its nwc_code field ("" if absent).
func nwcCodeFromError(t *testing.T, stderr string) string {
	t.Helper()
	var body struct {
		NWCCode string `json:"nwc_code"`
	}
	if err := json.Unmarshal([]byte(stderr), &body); err != nil {
		t.Fatalf("decode stderr JSON: %v: %s", err, stderr)
	}
	return body.NWCCode
}

// TestCashTransfer_ExpiredWallet_ClassifiedAsAuth confirms cash_transfer
// needed no NEW error-classification fix: the generic EXPIRED->auth
// mapping (internal/output/nwc_errors.go) already applies uniformly
// through classifyNWCErr, regardless of which cash command triggered it.
func TestCashTransfer_ExpiredWallet_ClassifiedAsAuth(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHubOpts(t, admin, cashHubOpts{MaxExpSecs: shortCashExpirySecs})
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, 10_000)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	waitPastCashExpiry()

	res := f.run("transfer", fakeHex32(t), "--yes")
	if res.ExitCode != 7 {
		t.Fatalf("transfer on an expired wallet: exit = %d, want 7 (auth)\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	// --json's "error" field is now the wallet's own raw message
	// (CLIError.RawMessage, see internal/output/errors.go), not cashctl's
	// generic "This wallet has expired..." translation — lokihub phrases
	// this specific decline as a "redemption deadline ... has passed",
	// never the literal word "expired", so check for either the way
	// TestDecodeCheck_ExpiredCashToken_ReportsAccurateDeadlineMessage
	// (this same file) already does, rather than assuming one exact
	// phrasing.
	if !jsonErrorContainsAny(t, res.Stderr, "auth", "deadline", "expired") {
		t.Errorf("unexpected error body: %s", res.Stderr)
	}
	if got := nwcCodeFromError(t, res.Stderr); got != "EXPIRED" {
		t.Errorf("nwc_code = %q, want EXPIRED", got)
	}
	if n := heldCount(t, f); n != 1 {
		t.Errorf("a rejected transfer must leave the token held, got %d held", n)
	}
}

// TestWalletBalance_ExpiredHeldToken_ExcludedFromTotal is the live evidence
// for the fix to a token that stayed HELD after expiring: `balance` used to
// keep summing an expired held token's amount into the total forever
// (nothing cached its Hub-side deadline, so there was no local signal to
// exclude it, and re-checking every held token live on every `balance`
// call was never acceptable). `receive`'s own mandatory CheckClaim now
// caches that deadline (Entry.ExpiresAt), so `balance` can exclude it
// without a second network round trip.
func TestWalletBalance_ExpiredHeldToken_ExcludedFromTotal(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHubOpts(t, admin, cashHubOpts{MaxExpSecs: shortCashExpirySecs})
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, 10_000)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// Before expiry: received while still valid, so receive's own
	// CheckClaim cached a not-yet-passed ExpiresAt — the token counts
	// normally.
	before := f.mustJSON("wallet", "balance", "--breakdown")
	if got := int64(before["total_mloki"].(float64)); got != 10_000 {
		t.Fatalf("before expiry: total_mloki = %d, want 10000", got)
	}
	if got := int64(before["expired_held_mloki"].(float64)); got != 0 {
		t.Fatalf("before expiry: expired_held_mloki = %d, want 0", got)
	}

	waitPastCashExpiry()

	// After expiry: no new network call happened (balance never re-checks
	// a held token live) — the exclusion comes entirely from the
	// ExpiresAt cached at receive time.
	after := f.mustJSON("wallet", "balance", "--breakdown")
	if got := int64(after["total_mloki"].(float64)); got != 0 {
		t.Errorf("after expiry: total_mloki = %d, want 0 (expired held token must not count)", got)
	}
	if got := int64(after["expired_held_mloki"].(float64)); got != 10_000 {
		t.Errorf("after expiry: expired_held_mloki = %d, want 10000", got)
	}
	breakdown, _ := after["breakdown"].([]any)
	if len(breakdown) != 1 {
		t.Fatalf("after expiry: breakdown has %d lines, want 1 (still listed, not dropped)", len(breakdown))
	}
	line, _ := breakdown[0].(map[string]any)
	if expired, _ := line["expired"].(bool); !expired {
		t.Errorf("after expiry: breakdown line %v missing expired:true", line)
	}
	if amount := int64(line["amount_mloki"].(float64)); amount != 10_000 {
		t.Errorf("after expiry: breakdown line amount_mloki = %d, want 10000 (still shown, just not counted)", amount)
	}
	// The token itself is untouched — still HELD, just excluded from the
	// spendable total.
	if n := heldCount(t, f); n != 1 {
		t.Errorf("an expired held token must stay held (not disappear), got %d held", n)
	}
}

// TestCashConsolidate_AllSourcesExpired_ClassifiedAsAuth confirms the new
// retry-a-different-source logic (doCashConsolidate) doesn't paper over a
// genuinely hopeless case: when every source is expired, consolidate must
// still fail cleanly as auth/EXPIRED, not loop, hang, or misclassify.
func TestCashConsolidate_AllSourcesExpired_ClassifiedAsAuth(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHubOpts(t, admin, cashHubOpts{MaxExpSecs: shortCashExpirySecs})
	tokenA := mintPubkeyTokenFromHub(t, hub, myPubHex, 4_000)
	tokenB := mintPubkeyTokenFromHub(t, hub, myPubHex, 5_000)
	// Plain (unsigned) mints carry no minter pubkey, so a no-arg
	// `consolidate` (which auto-groups by minter) would find nothing to
	// merge — name both sources explicitly instead.
	receiveA := f.mustJSON("receive", tokenA)
	receiveB := f.mustJSON("receive", tokenB)
	idA, _ := receiveA["entry"].(map[string]any)["id"].(string)
	idB, _ := receiveB["entry"].(map[string]any)["id"].(string)
	if idA == "" || idB == "" {
		t.Fatalf("receive: missing entry id(s): %v / %v", receiveA, receiveB)
	}

	waitPastCashExpiry()

	res := f.run("consolidate", "--sources", idA+","+idB, "--yes")
	if res.ExitCode != 7 {
		t.Fatalf("consolidate with every source expired: exit = %d, want 7 (auth)\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if got := nwcCodeFromError(t, res.Stderr); got != "EXPIRED" {
		t.Errorf("nwc_code = %q, want EXPIRED", got)
	}
	if n := heldCount(t, f); n != 2 {
		t.Errorf("a rejected consolidate must leave both tokens held, got %d held", n)
	}
}

// TestCashConsolidate_RescuesExpiredSourceViaHealthySibling is the
// headline fix this audit angle found, and the important nuance it also
// found while confirming it live: doCashConsolidate used to always dial
// through localIDs[0] to place the cash_consolidate call. lokihub's
// generic permission-expiry gate checks only the CALLING connection, not
// any other source in the batch (confirmed by reading
// cash_consolidate_controller.go's resolveConsolidateSource, which never
// re-checks a non-dialed source's own wallet expiry) — so a batch
// consolidating one already-expired token alongside a healthy one used to
// fail to even PLACE outright whenever the expired one happened to be
// dialed first, an arbitrary implementation detail. This mints the doomed
// token first (so it lands at ledger index 0 and is named first in
// --sources, the order consolidate dials through), waits for it alone to expire
// while the healthy sibling is still good for another hour, and confirms
// the merge now succeeds regardless.
//
// It is NOT a full rescue, and the second half of this test is the live
// proof: NIP-CASH's own merge rule inherits the EARLIEST expiry across
// every source (§Consolidating Tokens) — confirmed here by immediately
// trying to list-recipients the freshly merged token and finding it's
// ALSO already expired, dragging what would otherwise be the healthy
// sibling's own good-for-another-hour balance down with it. The real,
// narrower value: the call places at all (funds end up in one traceable
// wallet an operator can act on) instead of failing on dial-order chance;
// see cmd/cash_consolidate.go's own doCashConsolidate doc comment and
// runCashConsolidate's pre-confirm contamination warning for the fix that
// followed from this finding.
func TestCashConsolidate_RescuesExpiredSourceViaHealthySibling(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin) // default 1h ceiling — plenty for the healthy sibling
	doomed := mintPubkeyTokenExpiry(t, hub, myPubHex, 4_000, shortCashExpirySecs)
	healthy := mintPubkeyTokenExpiry(t, hub, myPubHex, 6_000, 0) // 0 -> Hub's own ceiling (1h)

	// Received in this order so `doomed` lands first — the exact "index 0
	// is the one that expired" case that used to fail the whole batch.
	receiveDoomed := f.mustJSON("receive", doomed)
	receiveHealthy := f.mustJSON("receive", healthy)
	doomedID, _ := receiveDoomed["entry"].(map[string]any)["id"].(string)
	healthyID, _ := receiveHealthy["entry"].(map[string]any)["id"].(string)
	if doomedID == "" || healthyID == "" {
		t.Fatalf("receive: missing entry id(s): %v / %v", receiveDoomed, receiveHealthy)
	}

	waitPastCashExpiry()

	// Plain (unsigned) mints carry no minter pubkey, so a no-arg
	// `consolidate` (which auto-groups by minter) would find nothing to
	// merge — name both sources explicitly, doomed first (dial order).
	resp := f.mustJSON("consolidate", "--sources", doomedID+","+healthyID, "--yes")
	newEntry, _ := resp["new_entry"].(map[string]any)
	if newEntry == nil {
		t.Fatalf("consolidate: no new_entry in response: %v", resp)
	}
	if got, _ := newEntry["amount_millis"].(float64); uint64(got) != 10_000 {
		t.Errorf("consolidated amount_millis = %v, want 10000 (the expired source's balance must still be merged in, not dropped)", newEntry["amount_millis"])
	}
	if n := heldCount(t, f); n != 1 {
		t.Fatalf("expected exactly the one merged token held afterward, got %d", n)
	}

	// The contamination half: the merged wallet inherited doomed's
	// already-past deadline, so it's immediately just as unusable via NWC
	// as doomed always was — even though healthy alone still had ~1h left
	// before this merge.
	id, _ := newEntry["id"].(string)
	checkRes := f.run("cash", "list-recipients", "--token", id)
	if checkRes.ExitCode != 7 {
		t.Fatalf("list-recipients on the freshly merged (but expiry-contaminated) token: exit = %d, want 7 (auth) — expected this to already be dead too\nstdout: %s\nstderr: %s", checkRes.ExitCode, checkRes.Stdout, checkRes.Stderr)
	}
	if got := nwcCodeFromError(t, checkRes.Stderr); got != "EXPIRED" {
		t.Errorf("nwc_code = %q, want EXPIRED", got)
	}
}

// Merging a healthy token with one that has ALREADY expired strands the
// healthy part inside a merged token that is born dead (it inherits the
// earliest deadline). consolidate is supposed to warn before that happens —
// but an expired wallet rejects the very CheckClaim call used to look its
// deadline up, so the lookup failed, read as "no deadline known", and the
// warning never fired: the user confirmed, and only found out afterwards.
func TestCashConsolidate_WarnsWhenAMemberHasAlreadyExpired(t *testing.T) {
	admin := adminOrSkip(t)
	f := newFixture(t)
	myPubHex, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	hub := setUpCashHub(t, admin)
	doomed := mintPubkeyTokenExpiry(t, hub, myPubHex, 4_000, shortCashExpirySecs)
	healthy := mintPubkeyTokenExpiry(t, hub, myPubHex, 6_000, 0)
	doomedID, _ := f.mustJSON("receive", doomed)["entry"].(map[string]any)["id"].(string)
	healthyID, _ := f.mustJSON("receive", healthy)["entry"].(map[string]any)["id"].(string)

	waitPastCashExpiry()

	res := f.runInteractive("n\n", "consolidate", doomedID, healthyID)
	if !strings.Contains(res.Combined(), "already expired") {
		t.Errorf("consolidate of an already-expired token with a healthy one showed no expiry warning before the prompt:\nstdout: %s\nstderr: %s", res.Stdout, res.Stderr)
	}
	if n := heldCount(t, f); n != 2 {
		t.Errorf("answered n, but %d tokens are held afterward, want both untouched", n)
	}
}

// TestCashTransfer_AutoConsolidate_ExpiredSourceFailsButFundsAreRecorded is
// cash selection's auto-consolidate-then-transfer path
// (transferWithAutoConsolidate) under the same scenario as
// TestCashConsolidate_RescuesExpiredSourceViaHealthySibling above, and
// confirms it behaves differently, for a reason specific to this path: a
// transfer needs a SECOND live call (the actual send) through the
// just-merged wallet, and that wallet is born already expired whenever any
// of its sources was — there is no sibling connection to retry that
// specific leg through, unlike the interim consolidate leg itself (which
// this path's own retry logic, mirroring doCashConsolidate's, does still
// place successfully). So the overall transfer fails — but by design,
// this is exactly the existing PartialProgressError handling already
// covers (see transferWithAutoConsolidate's own doc comment): the merged
// funds are NOT lost, they land in a new held ledger entry (equally
// expiry-contaminated as the consolidate-only case above, and equally
// recoverable only by contacting the Hub operator either way) instead of
// silently vanishing or crashing.
func TestCashTransfer_AutoConsolidate_ExpiredSourceFailsButFundsAreRecorded(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin)
	doomed, minter1, ok1 := mintSignedPubkeyTokenExpiry(t, f, hub, myPubHex, 3_000, shortCashExpirySecs)
	if !ok1 {
		t.Skip("skipping: this Hub did not attach a mint signature (best-effort — see NIP-CASH §Mint Provenance) — same-minter grouping can't be exercised without one")
	}
	healthy, minter2, ok2 := mintSignedPubkeyTokenExpiry(t, f, hub, myPubHex, 5_000, 0)
	if !ok2 {
		t.Skip("skipping: this Hub did not attach a mint signature on the second token")
	}
	if minter1 != minter2 {
		t.Skipf("skipping: the two tokens' recovered minter pubkeys differ (%s vs %s) — can't exercise same-minter grouping", minter1, minter2)
	}
	if res := f.run("receive", doomed); res.ExitCode != 0 {
		t.Fatalf("receive (doomed): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := f.run("receive", healthy); res.ExitCode != 0 {
		t.Fatalf("receive (healthy): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	waitPastCashExpiry()

	// Neither single token covers 6000 (3000 and 5000 each fall short) but
	// their sum (8000) does — forces ledger.SelectForAmount's
	// ConsolidateFirst path, exercising transferWithAutoConsolidate's own
	// retry-on-EXPIRED loop and PartialProgressError handling, not
	// doCashConsolidate's.
	res := f.run("transfer", fakeHex32(t), lokiArg(6_000), "--yes")
	if res.ExitCode != 7 {
		t.Fatalf("transfer (auto-consolidate, one source already expired): exit = %d, want 7 (auth) — the interim merge inherits the expired source's deadline, so the transfer leg can never go through\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if got := nwcCodeFromError(t, res.Stderr); got != "EXPIRED" {
		t.Errorf("nwc_code = %q, want EXPIRED", got)
	}
	// The interim consolidate still landed for real (8000 total) — the
	// PartialProgressError branch must have recorded it as a new held
	// token, not dropped it, even though the transfer itself failed.
	if n := heldCount(t, f); n != 1 {
		t.Fatalf("expected the interim-consolidated 8000-mloki token to still be recorded as held despite the failed transfer, got %d held", n)
	}
	showResp := f.mustJSON("wallet", "show")
	held, _ := showResp["held_tokens"].([]any)
	if len(held) != 1 {
		t.Fatalf("wallet show: held_tokens = %v, want exactly 1", held)
	}
	entry, _ := held[0].(map[string]any)
	if got, _ := entry["amount_millis"].(float64); uint64(got) != 8_000 {
		t.Errorf("recorded interim entry amount_millis = %v, want 8000 (3000+5000, nothing lost)", entry["amount_millis"])
	}
}

// TestCashTransfer_PreviewShowsExpiryWarning is cash_confirmation_test.go's
// TestCashRedeem_PreviewShowsExpiryWarning, extended to transfer: the
// interactive confirmation must warn about a soon expiry BEFORE sending,
// the same as redeem already does — the underlying Hub-side deadline is
// identical either way (NIP-CASH's wallet-level expires_at, not something
// specific to redemption).
func TestCashTransfer_PreviewShowsExpiryWarning(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	// setUpCashHub's default CashMaxExpSecs (1h) is within previewSuffix's
	// own "soon" (24h) threshold — same fixture cash_confirmation_test.go's
	// redeem test relies on.
	hub := setUpCashHub(t, admin)
	const amountMillis = uint64(12_000)
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, amountMillis)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// Decline (bare Enter) — only the prompt's own text matters here.
	res := f.runInteractive("\n", "transfer", fakeHex32(t))
	if res.ExitCode != 0 {
		t.Fatalf("transfer (preview check): unexpected exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Combined(), "Expires in") && !strings.Contains(res.Combined(), "Deadline passed") {
		t.Errorf("transfer confirmation: expected an expiry warning (this hub's own CashMaxExpSecs is 1h), got stdout: %q", res.Combined())
	}
	if n := heldCount(t, f); n != 1 {
		t.Fatalf("a declined transfer must leave the token held, got %d held", n)
	}
}

// TestCashConsolidate_PreviewShowsExpiryWarning is
// TestCashTransfer_PreviewShowsExpiryWarning's consolidate mirror.
func TestCashConsolidate_PreviewShowsExpiryWarning(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin)
	tokenA := mintPubkeyTokenFromHub(t, hub, myPubHex, 3_000)
	tokenB := mintPubkeyTokenFromHub(t, hub, myPubHex, 4_000)
	// Plain (unsigned) mints carry no minter pubkey, so a no-arg
	// `consolidate` (which auto-groups by minter) would find nothing to
	// merge — name both sources explicitly instead.
	receiveA := f.mustJSON("receive", tokenA)
	receiveB := f.mustJSON("receive", tokenB)
	idA, _ := receiveA["entry"].(map[string]any)["id"].(string)
	idB, _ := receiveB["entry"].(map[string]any)["id"].(string)
	if idA == "" || idB == "" {
		t.Fatalf("receive: missing entry id(s): %v / %v", receiveA, receiveB)
	}

	res := f.runInteractive("\n", "consolidate", idA, idB)
	if res.ExitCode != 0 {
		t.Fatalf("consolidate (preview check): unexpected exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Combined(), "Expires in") && !strings.Contains(res.Combined(), "Deadline passed") {
		t.Errorf("consolidate confirmation: expected an expiry warning (this hub's own CashMaxExpSecs is 1h), got stdout: %q", res.Combined())
	}
	if n := heldCount(t, f); n != 2 {
		t.Fatalf("a declined consolidate must leave both tokens held, got %d held", n)
	}
}

// TestDecodeCheck_CashToken_ShowsExpiresAt confirms decode --check's own
// live round trip (checkCashTokenAgainstHub -> CheckClaim) now surfaces
// expires_at too, not just amount_millis — it was already fetching this
// field and discarding it.
func TestDecodeCheck_CashToken_ShowsExpiresAt(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	token := mintPubkeyToken(t, admin, myPubHex, 20_000)

	decodeResp := f.mustJSON("decode", token, "--check")
	check, _ := decodeResp["check"].(map[string]any)
	if check == nil {
		t.Fatalf("decode --check: no check field in response: %v", decodeResp)
	}
	if ok, _ := check["ok"].(bool); !ok {
		t.Fatalf("decode --check on a real, matching token: ok = %v (check: %v)", check["ok"], check)
	}
	expiresAt, hasExpiry := check["expires_at"].(float64)
	if !hasExpiry {
		t.Fatalf("decode --check: no expires_at in check response: %v", check)
	}
	remaining := time.Until(time.Unix(int64(expiresAt), 0))
	if remaining <= 0 || remaining > 2*time.Hour {
		t.Errorf("decode --check: expires_at = %v (in %s), want roughly 1h out (setUpCashHub's default CashMaxExpSecs)", expiresAt, remaining)
	}
}

// TestDecodeCheck_ExpiredCashToken_ReportsAccurateDeadlineMessage confirms
// decode --check on an already-expired token doesn't misreport it as
// "no matching recipient" (a different, misleading failure mode) — the
// Hub's own accurate deadline message must come through, and --check
// itself must still be soft (exit 0, nothing saved, no crash).
func TestDecodeCheck_ExpiredCashToken_ReportsAccurateDeadlineMessage(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	f.mustJSON("wallet", "init")

	hub := setUpCashHubOpts(t, admin, cashHubOpts{MaxExpSecs: shortCashExpirySecs})
	// Doesn't matter who it's addressed to — decode --check never needs a
	// local identity match to report the Hub's own decline.
	token := mintPubkeyTokenFromHub(t, hub, fakeHex32(t), 5_000)

	waitPastCashExpiry()

	decodeResp := f.mustJSON("decode", token, "--check")
	check, _ := decodeResp["check"].(map[string]any)
	if check == nil {
		t.Fatalf("decode --check: no check field in response: %v", decodeResp)
	}
	if ok, _ := check["ok"].(bool); ok {
		t.Fatalf("decode --check on an expired token: ok = true, want false: %v", check)
	}
	errText, _ := check["error"].(string)
	if !strings.Contains(errText, "deadline") && !strings.Contains(errText, "expired") {
		t.Errorf("decode --check error = %q, want it to name the actual expiry (lokihub's own AppKindCashWallet deadline message), not a generic decline", errText)
	}
}

// TestCashListRecipients_ShowsExpiry confirms `cash list-recipients`'s
// text-mode output surfaces the wallet's own expires_at (already present
// on every RecipientStatus row, just previously never printed) — the same
// live fact decode --check and cash_redeem's own preview both show.
func TestCashListRecipients_ShowsExpiry(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	token := mintPubkeyToken(t, admin, myPubHex, 9_000)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// Text mode (runInteractive omits --json); list-recipients never
	// prompts, so an empty stdin is a no-op.
	res := f.runInteractive("", "cash", "list-recipients")
	if res.ExitCode != 0 {
		t.Fatalf("cash list-recipients: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "expires:") {
		t.Errorf("cash list-recipients: expected an \"expires:\" line, got stdout: %q", res.Stdout)
	}
}

// TestDecodeCheck_ExpiredCircleHub_StillReportsJoinable documents a
// protocol-level gap this audit found but ruled out fixing in cashctl:
// decode --check's circlehub path (checkCircleHubJoinable) can only call
// get_info, which is always-granted (permissions.GetAlwaysGrantedMethods)
// and bypasses the generic per-app expiry check entirely — its "methods"
// list is built purely from granted scopes (GetPermittedMethods), with no
// expiry check of its own, and the response carries no expires_at for a
// hub app at all. So an already-expired Circle Hub's own connection still
// reports "joining is possible" here, even though an actual `cashctl join`
// against the same connection is correctly rejected (EXPIRED/auth) —
// confirmed by both halves of this test. There is no read-only NIP-47 call
// cashctl could make instead to detect this ahead of time without
// mutating anything (create_circle_wallet itself isn't read-only, and no
// other circle_hub-scoped method exists) — see
// docs/private/audit-round2-expiration-matrix.md for the full reasoning.
//
// What DID get fixed (resumed session): the detection gap is unfixable,
// but the wording used to overclaim it — a bare "ok":true/"joinable" read
// as a guarantee. `check.note` (--json) and the text-mode "appears
// joinable (...)" line now say plainly that this only proves reachability
// and create_circle_wallet support, not that a real join will succeed —
// this test's own second half is the live proof that gap is real.
func TestDecodeCheck_ExpiredCircleHub_StillReportsJoinable(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	pastExpiry := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	hubResp, err := admin.createApp(adminCreateAppRequest{
		Name:                    ephemeralFixtureNamePrefix + " expired circle_hub",
		Kind:                    "circle_hub",
		ExpiresAt:               pastExpiry,
		Scopes:                  []string{"circle_wallet"},
		CircleIdentityName:      ephemeralFixtureNamePrefix + " expired circle identity",
		CirclePolicy:            "allowlist",
		CircleMaxExpSecs:        86400,
		CirclePerWalletMaxMloki: 1_000_000,
	})
	if err != nil {
		t.Fatalf("create ephemeral (already-expired) circle_hub: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.deleteApp(hubResp.ID); err != nil {
			t.Logf("cleanup: delete ephemeral circle_hub app_id=%d: %v", hubResp.ID, err)
		}
	})
	if hubResp.CircleHubToken == nil || *hubResp.CircleHubToken == "" {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", hubResp)
	}

	f := newFixture(t)
	f.mustJSON("wallet", "init")

	// Half 1: decode --check reports joining as possible.
	decodeResp := f.mustJSON("decode", *hubResp.CircleHubToken, "--check")
	check, _ := decodeResp["check"].(map[string]any)
	if check == nil {
		t.Fatalf("decode --check: no check field in response: %v", decodeResp)
	}
	if ok, _ := check["ok"].(bool); !ok {
		t.Fatalf("decode --check on an already-expired circle_hub: ok = %v, want true (this is the documented gap — get_info can't see hub-level expiry at all): %v", check["ok"], check)
	}
	// The gap itself can't be closed (see above), but ok:true must not be
	// left looking like an unqualified guarantee — note should say so.
	if note, _ := check["note"].(string); !strings.Contains(note, "expired") && !strings.Contains(note, "allowlisted") {
		t.Errorf(`decode --check "note" = %q, want it to caveat that this doesn't confirm expiry/allowlist status`, note)
	}

	// Half 2: an actual join is correctly rejected anyway — cashctl's own
	// error classification is fine here; it's only the pre-flight --check
	// that can't see this coming.
	res := f.run("join", "--hub", *hubResp.CircleHubToken, "--max-amount", "100", "--yes")
	if res.ExitCode != 7 {
		t.Fatalf("join against an already-expired circle_hub: exit = %d, want 7 (auth)\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if got := nwcCodeFromError(t, res.Stderr); got != "EXPIRED" {
		t.Errorf("nwc_code = %q, want EXPIRED", got)
	}
}
