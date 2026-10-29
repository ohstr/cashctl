package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipCO"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/nipcash/listing"
	"github.com/ohstr/nmilat/utils"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/testmint"
)

// listingFixture is an order link listed by its Hub, the relays' events, and
// what the listed connection answers to get_info.
type listingFixture struct {
	orders  *fakeOrderHub
	conn    string
	ev      *nip01.Event
	events  []*nip01.Event
	info    *nip47.GetInfoResult
	addr    string
	fetched [][]nip01.SubscriptionFilter
}

func newListingFixture(t *testing.T) *listingFixture {
	t.Helper()
	orders, conn := setUpOrderHub(t)
	oc, err := nipCO.DecodeOrderConnection(conn)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	ev, err := listing.NewListing(listing.ListingFields{Order: &oc, Network: "mainnet", Summary: []string{listing.SummaryLightning},
		Title: "Buy cash \u202e here", Status: listing.StatusOpen, Expiration: now + 86400}, now-10, now,
		func(ev *nip01.Event) error { return ev.Sign(orders.hubPriv) })
	if err != nil {
		t.Fatal(err)
	}
	f := &listingFixture{orders: orders, conn: conn, ev: ev, events: []*nip01.Event{ev},
		addr: listing.Address(orders.hubXOnly, oc.Pubkey)}
	terms, _ := json.Marshal(map[string]any{"lightning": map[string]any{"min_millis": 1000, "max_millis": 1_000_000_000, "fee_base": 1000, "fee_ppm": 0, "window": 600},
		"bill": map[string]any{"redeem_fee_base": 0, "redeem_fee_ppm": 0, "min_transfer_millis": 0}})
	f.info = &nip47.GetInfoResult{Pubkey: orders.hubXOnly, Network: "mainnet", Chain: "flokicoin"}
	prevFetch, prevTap := fetchEventsFn, tapInfoFn
	fetchEventsFn = func(_ context.Context, _ []string, filters []nip01.SubscriptionFilter) ([]*nip01.Event, []string) {
		f.fetched = append(f.fetched, filters)
		var out []*nip01.Event
		for _, ev := range f.events {
			for _, flt := range filters {
				if containsInt(flt.Kinds, ev.Kind) {
					out = append(out, ev)
					break
				}
			}
		}
		return out, nil
	}
	tapInfoFn = func(context.Context, *listing.Listing) (*nip47.GetInfoResult, json.RawMessage, error) {
		raw, _ := json.Marshal(map[string]any{"pubkey": f.info.Pubkey, "network": f.info.Network, "chain": f.info.Chain, "cash_order": json.RawMessage(terms)})
		return f.info, raw, nil
	}
	t.Cleanup(func() { fetchEventsFn, tapInfoFn = prevFetch, prevTap })
	if err := config.AddDiscoverRelay("wss://discover.example"); err != nil {
		t.Fatal(err)
	}
	return f
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func discoverCmd(c *cobra.Command, flags map[string]string) *cobra.Command {
	c.Flags().Bool("json", true, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().String("connection", "", "")
	for k, v := range flags {
		if err := c.Flags().Set(k, v); err != nil {
			panic(err)
		}
	}
	return c
}

func runDiscover(t *testing.T, c *cobra.Command, args []string) (map[string]any, error) {
	t.Helper()
	c.SetArgs(args)
	var err error
	stdout, _ := captureStreams(t, func() { err = c.Execute() })
	var doc map[string]any
	_ = json.Unmarshal([]byte(stdout), &doc)
	return doc, err
}

// Without a discovery relay there is nothing to read: usage.
func TestDiscover_NeedsARelay(t *testing.T) {
	setUpOrderHub(t)
	if _, err := runDiscover(t, discoverCmd(newDiscoverListCmd(), nil), nil); output.ExitCode(err) != 2 {
		t.Errorf("err %v, want usage", err)
	}
}

// list shows a listing with its text sanitized, unreviewed without a curator
// and reviewed once a curator recommends it.
func TestDiscover_ListAndCurators(t *testing.T) {
	f := newListingFixture(t)
	doc, err := runDiscover(t, discoverCmd(newDiscoverListCmd(), nil), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	offers, _ := doc["offers"].([]any)
	if len(offers) != 1 {
		t.Fatalf("offers %v", doc)
	}
	o := offers[0].(map[string]any)
	if strings.Contains(o["title"].(string), "\u202e") || o["section"] != "unreviewed" || o["offer"] != offerHandle(f.addr) {
		t.Errorf("offer %v", o)
	}
	curPriv, _ := testmint.Key(t)
	curPub, _ := utils.GetPublicKey(curPriv)
	rec := listing.NewRecommendation(f.orders.hubXOnly, "", []listing.Ref{{Pubkey: f.addr}}, "")
	rec.CreatedAt = uint64(time.Now().Unix()) //nolint:gosec // unix time
	if err := rec.Sign(curPriv); err != nil {
		t.Fatal(err)
	}
	f.events = append(f.events, rec)
	if _, err := runDiscover(t, discoverCmd(newDiscoverCuratorsCmd(), nil), []string{"add", curPub}); err != nil {
		t.Fatal(err)
	}
	doc, _ = runDiscover(t, discoverCmd(newDiscoverListCmd(), nil), nil)
	o = doc["offers"].([]any)[0].(map[string]any)
	if o["section"] != "reviewed" || len(o["recommendations"].([]any)) != 1 {
		t.Errorf("after the curator's recommendation: %v", o)
	}
}

// show taps the listed connection: the Hub's terms when it answers as listed,
// hidden for good when it answers as another node.
func TestDiscover_ShowTaps(t *testing.T) {
	f := newListingFixture(t)
	doc, err := runDiscover(t, discoverCmd(newDiscoverShowCmd(), nil), []string{offerHandle(f.addr)})
	if err != nil || doc["tap"] != "show" || doc["terms"] == nil {
		t.Fatalf("show: %v %v", err, doc)
	}
	_, other := testmint.Key(t)
	f.info.Pubkey = other
	doc, _ = runDiscover(t, discoverCmd(newDiscoverShowCmd(), nil), []string{offerHandle(f.addr)})
	if doc["tap"] != "hide_for_good" {
		t.Errorf("another node: %v", doc)
	}
}

// open taps again and hands the listed connection to `cash order place`.
func TestDiscover_OpenPlacesTheOrder(t *testing.T) {
	f := newListingFixture(t)
	doc, err := runDiscover(t, discoverCmd(newDiscoverOpenCmd(), nil), []string{offerHandle(f.addr), "100"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if doc["state"] != "open" || len(f.orders.placed) != 1 {
		t.Errorf("open: %v, placed %d", doc, len(f.orders.placed))
	}
	if orders, _ := ledger.Orders(); len(orders) != 1 {
		t.Errorf("orders %v", orders)
	}
	f.info.Network = "regtest"
	if _, err := runDiscover(t, discoverCmd(newDiscoverOpenCmd(), nil), []string{offerHandle(f.addr), "100"}); output.ExitCode(err) != 7 {
		t.Errorf("a Hub answering on another network: %v", err)
	}
}

// D-1: a global listing's node and the operator it names are never asked for
// as trusted: only recommendations by follows and curators (and verified
// operators) seed the trusted requests.
func TestDiscover_GlobalListingSeedsNothingTrusted(t *testing.T) {
	f := newListingFixture(t)
	_, opPub := testmint.Key(t)
	oc, _ := nipCO.DecodeOrderConnection(f.conn)
	now := time.Now().Unix()
	ev, err := listing.NewListing(listing.ListingFields{Order: &oc, Network: "mainnet", Title: "flood", Status: listing.StatusOpen,
		Expiration: now + 86400, Operator: &listing.Ref{Pubkey: opPub}}, now-5, now,
		func(ev *nip01.Event) error { return ev.Sign(f.orders.hubPriv) })
	if err != nil {
		t.Fatal(err)
	}
	f.events = append(f.events, ev)
	for range 2 {
		if _, err := runDiscover(t, discoverCmd(newDiscoverListCmd(), nil), nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, filters := range f.fetched {
		for _, flt := range filters {
			for _, a := range flt.Authors {
				if a == f.orders.hubXOnly || a == opPub {
					t.Errorf("asked as trusted for %s (kinds %v), named only by a global listing", a, flt.Kinds)
				}
			}
		}
	}
}

// D-3: text output names each recommendation's author and date.
func TestDiscover_TextNamesEachSignal(t *testing.T) {
	f := newListingFixture(t)
	curPriv, curPub := testmint.Key(t)
	rec := listing.NewRecommendation(f.orders.hubXOnly, "", []listing.Ref{{Pubkey: f.addr}}, "")
	rec.CreatedAt = uint64(time.Now().Unix()) //nolint:gosec // unix time
	if err := rec.Sign(curPriv); err != nil {
		t.Fatal(err)
	}
	f.events = append(f.events, rec)
	if _, err := runDiscover(t, discoverCmd(newDiscoverCuratorsCmd(), nil), []string{"add", curPub}); err != nil {
		t.Fatal(err)
	}
	c := discoverCmd(newDiscoverListCmd(), map[string]string{"json": "false"})
	var err error
	stdout, _ := captureStreams(t, func() { err = c.Execute() })
	if err != nil {
		t.Fatal(err)
	}
	if want := "recommended by " + npubOrHex(curPub) + " on " + time.Now().UTC().Format("2006-01-02"); !strings.Contains(stdout, want) {
		t.Errorf("stdout %q lacks %q", stdout, want)
	}
}

// NIP-CASH §Wallet Rules, Signals: listings of different pubkeys are never
// merged; when several list one d, only a listing whose signer passed the
// check at tap is shown, or none. Each can still be tapped by its address.
func TestDiscover_SeveralPubkeysListingOneD(t *testing.T) {
	copycat := func(t *testing.T, f *listingFixture) *nip01.Event {
		t.Helper()
		cpPriv, cpX := testmint.Key(t)
		oc, err := nipCO.DecodeOrderConnection(f.conn)
		if err != nil {
			t.Fatal(err)
		}
		oc.HubIdentity, oc.Signature = cpX, nil
		signer, err := nipcash.LocalChainSigner(cpPriv, "flokicoin")
		if err != nil {
			t.Fatal(err)
		}
		signed, err := nipCO.SignOrderConnection(oc, signer, cpX)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().Unix()
		ev, err := listing.NewListing(listing.ListingFields{Order: &signed, Network: "mainnet", Summary: []string{listing.SummaryLightning},
			Title: "Buy cash", Status: listing.StatusOpen, Expiration: now + 86400}, now-5, now,
			func(ev *nip01.Event) error { return ev.Sign(cpPriv) })
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	list := func(t *testing.T) (offers []string, contested float64) {
		t.Helper()
		doc, err := runDiscover(t, discoverCmd(newDiscoverListCmd(), nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range doc["offers"].([]any) {
			offers = append(offers, o.(map[string]any)["address"].(string))
		}
		return offers, doc["contested"].(float64)
	}
	show := func(t *testing.T, addr string) string {
		t.Helper()
		doc, _ := runDiscover(t, discoverCmd(newDiscoverShowCmd(), nil), []string{offerHandle(addr)})
		s, _ := doc["tap"].(string)
		return s
	}

	t.Run("before any tap neither is shown", func(t *testing.T) {
		f := newListingFixture(t)
		cp := copycat(t, f)
		f.events = append(f.events, cp)
		if offers, contested := list(t); len(offers) != 0 || contested != 2 {
			t.Errorf("offers %v contested %v, want none shown and 2 contested", offers, contested)
		}
	})
	t.Run("the real one passes at tap and is then the one shown", func(t *testing.T) {
		f := newListingFixture(t)
		cp := copycat(t, f)
		f.events = append(f.events, cp)
		if got := show(t, f.addr); got != "show" {
			t.Fatalf("tap of the real listing: %q", got)
		}
		if offers, contested := list(t); len(offers) != 1 || offers[0] != f.addr || contested != 1 {
			t.Errorf("offers %v contested %v, want only the real listing", offers, contested)
		}
	})
	t.Run("the copy fails at tap and is hidden for good", func(t *testing.T) {
		f := newListingFixture(t)
		cp := copycat(t, f)
		f.events = append(f.events, cp)
		cpAddr := listing.Address(cp.PubKey, f.addr[strings.LastIndex(f.addr, ":")+1:])
		if got := show(t, cpAddr); got != "hide_for_good" {
			t.Fatalf("tap of the copy: %q", got)
		}
		if offers, contested := list(t); len(offers) != 1 || offers[0] != f.addr || contested != 0 {
			t.Errorf("offers %v contested %v, want the real listing alone", offers, contested)
		}
	})
	t.Run("listings of other d by other pubkeys are all shown", func(t *testing.T) {
		f := newListingFixture(t)
		otherPriv, _ := testmint.Key(t)
		oc, _ := nipCO.DecodeOrderConnection(testmint.CashOrder(t, otherPriv))
		now := time.Now().Unix()
		ev, err := listing.NewListing(listing.ListingFields{Order: &oc, Network: "mainnet", Summary: []string{listing.SummaryLightning},
			Title: "Other", Status: listing.StatusOpen, Expiration: now + 86400}, now-5, now,
			func(ev *nip01.Event) error { return ev.Sign(otherPriv) })
		if err != nil {
			t.Fatal(err)
		}
		f.events = append(f.events, ev)
		if offers, contested := list(t); len(offers) != 2 || contested != 0 {
			t.Errorf("offers %v contested %v, want both", offers, contested)
		}
	})
}
