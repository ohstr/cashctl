package cmd

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/nipcash/transport"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// tokenWithRelays builds a real, decodable cash token naming relays, so
// planRelays is exercised against the actual codec rather than a stub.
func tokenWithRelays(t *testing.T, relays ...string) string {
	t.Helper()
	tok, err := nipcash.Encode(nipcash.Token{
		HRP:              "lokicash",
		WalletPubkey:     strings.Repeat("aa", 32),
		Secret:           strings.Repeat("bb", 32),
		IdentityRequired: ptrTo(false),
		RelayURLs:        relays,
	})
	if err != nil {
		t.Fatalf("nipcash.Encode: %v", err)
	}
	return tok
}

func planFor(id, minter, token string) redeemPlan {
	e := &ledger.Entry{ID: id, Token: token, Status: ledger.StatusHeld}
	if minter != "" {
		e.MinterPubkey = ptrTo(minter)
	}
	return redeemPlan{Entry: e}
}

// TestGroupPlansByHub_SplitsByMinter is the load-bearing property of the private
// transport: an envelope's items must all bind to the SAME hub, so bills from
// different hubs can never share one. Getting this wrong is not a visible error —
// a hub omits items it cannot authorize, and omission is information-free by
// design, so the caller would learn nothing about why their bills vanished.
func TestGroupPlansByHub_SplitsByMinter(t *testing.T) {
	plans := []redeemPlan{
		planFor("tok-a", "hub-1", "t"),
		planFor("tok-b", "hub-2", "t"),
		planFor("tok-c", "hub-1", "t"),
	}
	groups, ungrouped := groupPlansByHub(plans)

	if len(ungrouped) != 0 {
		t.Errorf("ungrouped = %d, want 0 — every bill here has a minter", len(ungrouped))
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2 (one per hub)", len(groups))
	}
	// Hub order follows first appearance, so output is stable run to run.
	if groups[0].HubXOnly != "hub-1" || groups[1].HubXOnly != "hub-2" {
		t.Errorf("hub order = (%q, %q), want (hub-1, hub-2) by first appearance", groups[0].HubXOnly, groups[1].HubXOnly)
	}
	if len(groups[0].Plans) != 2 {
		t.Errorf("hub-1 got %d bills, want both of its own", len(groups[0].Plans))
	}
	if len(groups[1].Plans) != 1 {
		t.Errorf("hub-2 got %d bills, want 1", len(groups[1].Plans))
	}
	// The real failure this guards: a hub's group must contain only its own.
	for _, p := range groups[0].Plans {
		if *p.Entry.MinterPubkey != "hub-1" {
			t.Errorf("hub-1's group contains %s, which belongs to %s", p.Entry.ID, *p.Entry.MinterPubkey)
		}
	}
}

// TestGroupPlansByHub_NoMinterCannotBeBatched covers the bills that have no hub
// identity at all. Without a recovered minter there is nothing to verify an
// announcement's signature against, and verifying it against a key learned from
// that same announcement would simply believe an attacker — so these must fall
// back rather than be batched against a guessed identity.
func TestGroupPlansByHub_NoMinterCannotBeBatched(t *testing.T) {
	plans := []redeemPlan{
		planFor("tok-a", "hub-1", "t"),
		planFor("tok-unsigned", "", "t"),
		planFor("tok-empty", "", "t"),
	}
	plans[2].Entry.MinterPubkey = ptrTo("") // present but empty is just as unusable

	groups, ungrouped := groupPlansByHub(plans)
	if len(groups) != 1 || groups[0].HubXOnly != "hub-1" {
		t.Fatalf("groups = %+v, want just hub-1", groups)
	}
	got := []string{}
	for _, p := range ungrouped {
		got = append(got, p.Entry.ID)
	}
	if want := []string{"tok-unsigned", "tok-empty"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ungrouped = %v, want %v", got, want)
	}
}

// TestEntryRelays_UnionsEveryBillsHints matters because a bill's hints were fixed
// when it was minted: an older bill may name a relay the hub has since left, and
// a newer one may name where it went. Using only the first bill's hints would
// strand a group whose newer bills know exactly where to look.
func TestEntryRelays_UnionsEveryBillsHints(t *testing.T) {
	entries := []*ledger.Entry{
		planFor("tok-a", "hub-1", tokenWithRelays(t, "wss://old.example")).Entry,
		planFor("tok-b", "hub-1", tokenWithRelays(t, "wss://new.example", "wss://old.example")).Entry,
	}
	got := entryRelays(entries)
	if want := []string{"wss://old.example", "wss://new.example"}; !reflect.DeepEqual(got, want) {
		t.Errorf("entryRelays() = %v, want %v (union, in order, deduplicated)", got, want)
	}
}

func TestEntryRelays_UndecodableTokenIsSkippedNotFatal(t *testing.T) {
	entries := []*ledger.Entry{
		planFor("tok-bad", "hub-1", "not-a-token").Entry,
		planFor("tok-good", "hub-1", tokenWithRelays(t, "wss://live.example")).Entry,
	}
	if got := entryRelays(entries); !reflect.DeepEqual(got, []string{"wss://live.example"}) {
		t.Errorf("entryRelays() = %v, want the reachable hint only", got)
	}
}

// TestGroupEntriesByHub_MatchesGroupPlansByHub pins that the two splits agree.
// They must: one chooses the wire path before quoting and the other decides what
// to send, so a disagreement would quote a bill on one transport and spend it on
// another — or build an envelope whose items bind to different hubs, which the
// hub answers with silence.
func TestGroupEntriesByHub_MatchesGroupPlansByHub(t *testing.T) {
	plans := []redeemPlan{
		planFor("tok-a", "hub-1", "t"),
		planFor("tok-b", "hub-2", "t"),
		planFor("tok-c", "hub-1", "t"),
		planFor("tok-unsigned", "", "t"),
	}
	entries := make([]*ledger.Entry, len(plans))
	for i, p := range plans {
		entries[i] = p.Entry
	}

	planGroups, planUngrouped := groupPlansByHub(plans)
	entryGroups, entryUngrouped := groupEntriesByHub(entries)

	if len(planGroups) != len(entryGroups) {
		t.Fatalf("group counts differ: %d plan groups vs %d entry groups", len(planGroups), len(entryGroups))
	}
	for i := range planGroups {
		if planGroups[i].HubXOnly != entryGroups[i].HubXOnly {
			t.Errorf("group %d: plans say hub %q, entries say %q", i, planGroups[i].HubXOnly, entryGroups[i].HubXOnly)
		}
		if len(planGroups[i].Plans) != len(entryGroups[i].Entries) {
			t.Errorf("group %d (%s): %d plans vs %d entries", i, planGroups[i].HubXOnly, len(planGroups[i].Plans), len(entryGroups[i].Entries))
		}
	}
	if len(planUngrouped) != len(entryUngrouped) {
		t.Errorf("ungrouped differs: %d vs %d", len(planUngrouped), len(entryUngrouped))
	}
}

// TestEntriesForHub_SkipsCredentiallessBills covers a subtle one: a bill we
// cannot build a credential for must be dropped BEFORE the envelope, not sent and
// omitted. An omission is information-free, so a bill dropped for a reason we
// already knew locally would come back indistinguishable from one the hub refused.
func TestEntriesForHub_SkipsCredentiallessBills(t *testing.T) {
	entries := []*ledger.Entry{
		planFor("tok-a", "hub-1", "t").Entry,
		planFor("tok-b", "hub-1", "t").Entry,
		planFor("tok-other", "hub-2", "t").Entry,
	}
	creds := map[string]nipcash.Credential{"tok-a": nipcash.BySecret("s")}

	got := entriesForHub(entries, "hub-1", creds)
	if len(got) != 1 || got[0].ID != "tok-a" {
		ids := []string{}
		for _, e := range got {
			ids = append(ids, e.ID)
		}
		t.Errorf("entriesForHub = %v, want just tok-a (tok-b has no credential, tok-other another hub)", ids)
	}
}

func TestParseTransportMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want transportMode
		bad  bool
	}{
		{in: "", want: transportStandard},
		{in: "auto", want: transportAuto},
		{in: "private", want: transportPrivate},
		{in: "standard", want: transportStandard},
		{in: "batch", bad: true},
	} {
		cmd := &cobra.Command{}
		cmd.Flags().String("transport", tc.in, "")
		cmd.Flags().Bool("json", false, "")
		got, err := parseTransportMode(cmd)
		if tc.bad {
			if err == nil {
				t.Errorf("parseTransportMode(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTransportMode(%q) error = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseTransportMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- applyRedeemResults: the three states a batched bill can come back in. This is
// the mapping that decides whether the caller is told money moved, money did not
// move, or nobody knows — and two of the three are easy to get subtly wrong in a
// way no compiler catches.

func batchOutcome(id string, state nipcashclient.OutcomeState, result *nipcash.CashRedeemResult, resErr *transport.ResultError) nipcashclient.RedeemOutcome {
	return nipcashclient.RedeemOutcome{
		ItemOutcome: nipcashclient.ItemOutcome{ID: id, State: state, Error: resErr},
		Result:      result,
	}
}

func TestApplyRedeemResults_PaidBillIsRecordedAndReported(t *testing.T) {
	plans := []redeemPlan{planFor("tok-a", "hub-1", "t")}
	plans[0].Index = 0
	outcomes := []redeemOutcome{{EntryID: "tok-a"}}
	l := &ledger.Ledger{Entries: []ledger.Entry{{ID: "tok-a", Status: ledger.StatusHeld}}}

	applyRedeemResults(&cobra.Command{}, l, plans, []nipcashclient.RedeemOutcome{
		batchOutcome("tok-a", nipcashclient.OutcomeResult, &nipcash.CashRedeemResult{Preimage: "beef01"}, nil),
	}, outcomes, "savings")

	if outcomes[0].Result == nil || outcomes[0].Result.Preimage != "beef01" {
		t.Fatalf("outcome = %+v, want the payment's preimage", outcomes[0])
	}
	if l.Entries[0].Status != ledger.StatusRedeemed {
		t.Errorf("ledger status = %q, want redeemed — a paid bill left as held gets offered again", l.Entries[0].Status)
	}
}

// TestApplyRedeemResults_HubRefusalKeepsItsClassification pins that a batched
// refusal reads exactly like a single-bill one. transport.ResultError mirrors
// NIP-47's error shape precisely so it can, and a caller must not have to branch
// on which transport carried an error to understand it.
func TestApplyRedeemResults_HubRefusalKeepsItsClassification(t *testing.T) {
	plans := []redeemPlan{planFor("tok-a", "hub-1", "t")}
	outcomes := []redeemOutcome{{EntryID: "tok-a"}}
	l := &ledger.Ledger{Entries: []ledger.Entry{{ID: "tok-a", Status: ledger.StatusHeld}}}

	applyRedeemResults(&cobra.Command{}, l, plans, []nipcashclient.RedeemOutcome{
		batchOutcome("tok-a", nipcashclient.OutcomeError, nil, &transport.ResultError{Code: "EXPIRED", Message: "this bill's deadline has passed"}),
	}, outcomes, "savings")

	if outcomes[0].Err == nil {
		t.Fatal("a refused bill produced no error")
	}
	ce := output.AsCLIError(outcomes[0].Err)
	if ce.NWCCode != "EXPIRED" {
		t.Errorf("nwc_code = %q, want EXPIRED preserved verbatim for an agent that branches on it", ce.NWCCode)
	}
	if ce.RawMessage == "" {
		t.Error("the hub's own message was discarded; --json consumers read it")
	}
	// A refused bill did NOT pay out, so the ledger must still show it as held.
	if l.Entries[0].Status != ledger.StatusHeld {
		t.Errorf("ledger status = %q, want held — a refusal must not mark a bill spent", l.Entries[0].Status)
	}
}

// TestApplyRedeemResults_OmissionIsNeitherSuccessNorSilence is the important one.
// An omission is information-free by design — the same answer for a bill the hub
// does not hold, a proof that did not verify, and a method it will not serve — so
// it is indistinguishable from a redemption whose reply was lost. It must never
// read as success, must never be silently dropped, and must point at the check to
// run rather than invite a blind retry that could double-spend.
func TestApplyRedeemResults_OmissionIsNeitherSuccessNorSilence(t *testing.T) {
	plans := []redeemPlan{planFor("tok-a", "hub-1", "t")}
	outcomes := []redeemOutcome{{EntryID: "tok-a"}}
	l := &ledger.Ledger{Entries: []ledger.Entry{{ID: "tok-a", Status: ledger.StatusHeld}}}

	applyRedeemResults(&cobra.Command{}, l, plans, []nipcashclient.RedeemOutcome{
		batchOutcome("tok-a", nipcashclient.OutcomeNotServed, nil, nil),
	}, outcomes, "savings")

	if outcomes[0].Result != nil {
		t.Fatal("an omitted bill was reported as paid — the hub said nothing at all")
	}
	if outcomes[0].Err == nil {
		t.Fatal("an omitted bill was reported as nothing happening; it may or may not have been redeemed")
	}
	if !strings.Contains(outcomes[0].Err.Error(), "list-recipients") {
		t.Errorf("the error does not name the check to run before retrying: %v", outcomes[0].Err)
	}
	// The ledger must NOT be marked spent: we genuinely do not know.
	if l.Entries[0].Status != ledger.StatusHeld {
		t.Errorf("ledger status = %q, want held — an omission is not evidence of a spend", l.Entries[0].Status)
	}
}

// TestApplyRedeemResults_UnknownIDIsIgnored: a reply naming a bill we never sent
// must not be written anywhere. DecodeResponse already rejects unrequested ids, so
// this is belt-and-braces — but writing into a slot by a hub-supplied name is
// exactly the shape of bug worth making impossible.
func TestApplyRedeemResults_UnknownIDIsIgnored(t *testing.T) {
	plans := []redeemPlan{planFor("tok-a", "hub-1", "t")}
	outcomes := []redeemOutcome{{EntryID: "tok-a"}}
	l := &ledger.Ledger{Entries: []ledger.Entry{{ID: "tok-a", Status: ledger.StatusHeld}}}

	applyRedeemResults(&cobra.Command{}, l, plans, []nipcashclient.RedeemOutcome{
		batchOutcome("tok-never-sent", nipcashclient.OutcomeResult, &nipcash.CashRedeemResult{Preimage: "x"}, nil),
	}, outcomes, "savings")

	if outcomes[0].Result != nil || outcomes[0].Err != nil {
		t.Errorf("a result for an unsent bill landed on tok-a: %+v", outcomes[0])
	}
	if l.Entries[0].Status != ledger.StatusHeld {
		t.Errorf("ledger status = %q, want untouched", l.Entries[0].Status)
	}
}

// TestBatchableHub_ExcludesUnresolvedCashSecrets pins the rule that a bill whose
// spending credential can only be settled by a live per-bill decline must not be
// batched: an omission carries no such signal, so the fallback that resolves it
// cannot run (see spendCashEntry).
func TestBatchableHub_ExcludesUnresolvedCashSecrets(t *testing.T) {
	minter := "hub-1"
	cashMode := func(secret, pending string) *ledger.Entry {
		no := false
		return &ledger.Entry{
			ID: "tok", MinterPubkey: &minter, IdentityRequired: &no,
			CashSecret: secret, PendingCashSecret: pending,
		}
	}

	if got := batchableHub(cashMode("", "")); got != nil {
		t.Errorf("a cash-mode bill with no secret must not batch — it cannot even build an item; got %v", *got)
	}
	if got := batchableHub(cashMode("", "pending-one")); got != nil {
		t.Errorf("a cash-mode bill whose only secret is unconfirmed must not batch; got %v", *got)
	}
	if got := batchableHub(cashMode("live", "other")); got != nil {
		t.Errorf("an ambiguous pair of candidate secrets must not batch — only a decline can say which is live; got %v", *got)
	}
	// Settled cash-mode: batchable.
	if got := batchableHub(cashMode("live", "")); got == nil || *got != minter {
		t.Errorf("a cash-mode bill with a settled secret must batch, got %v", got)
	}
	if got := batchableHub(cashMode("live", "live")); got == nil {
		t.Error("a pending value equal to the live one is not ambiguous and must batch")
	}
	// Pubkey-mode is unaffected by any of this.
	yes := true
	if got := batchableHub(&ledger.Entry{ID: "tok", MinterPubkey: &minter, IdentityRequired: &yes}); got == nil {
		t.Error("a pubkey-mode bill must still batch")
	}
	// The case that actually escaped: an interrupted auto-secure leaves a bill that
	// is cash-mode ON THE HUB while cashctl's own entry does not say so. Gating on
	// IdentityRequired first let it through, cashctl signed a pubkey proof, and the
	// hub correctly refused it — as an omission, which carries no signal to retry on.
	// The pending secret is the reliable marker, whatever IdentityRequired says.
	if got := batchableHub(&ledger.Entry{
		ID: "tok", MinterPubkey: &minter, IdentityRequired: &yes,
		CashSecret: "live", PendingCashSecret: "other",
	}); got != nil {
		t.Errorf("a bill with an unreconciled pending secret must not batch even when it looks pubkey-mode; got %v", *got)
	}
	// No minter: nothing to verify an announcement against.
	if got := batchableHub(&ledger.Entry{ID: "tok"}); got != nil {
		t.Errorf("a bill with no recovered minter must not batch; got %v", *got)
	}
}
