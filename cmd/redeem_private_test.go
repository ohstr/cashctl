package cmd

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
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
		{in: "", want: transportAuto},
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
