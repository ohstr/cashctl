package cmd

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipCO"
	"github.com/ohstr/nmilat/nipcash/listing"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/testmint"
)

// signalFixture is a listing fixture whose relays also take what the user
// publishes, and answer filters by id, author and kind.
type signalFixture struct {
	*listingFixture
	published []*nip01.Event
	down      bool
	// onlyOn names, by event id, the one relay that serves an event; every
	// other event is served by every relay.
	onlyOn map[string]string
}

func newSignalFixture(t *testing.T, withIdentity bool) *signalFixture {
	t.Helper()
	f := &signalFixture{listingFixture: newListingFixture(t), onlyOn: map[string]string{}}
	if withIdentity {
		if _, err := identity.GenerateAndSaveLocal(); err != nil {
			t.Fatal(err)
		}
	}
	prevFetch, prevPub, prevDelay := fetchEventsFn, publishEventFn, randomDelay
	fetchEventsFn = func(_ context.Context, relays []string, filters []nip01.SubscriptionFilter) ([]*nip01.Event, []string) {
		if f.down {
			return nil, relays
		}
		var out []*nip01.Event
		for _, ev := range append(slices.Clone(f.events), f.published...) {
			if r, ok := f.onlyOn[ev.ID]; ok && !slices.Contains(relays, r) {
				continue
			}
			for _, flt := range filters {
				if (len(flt.IDs) == 0 || slices.Contains(flt.IDs, ev.ID)) &&
					(len(flt.Kinds) == 0 || slices.Contains(flt.Kinds, ev.Kind)) &&
					(len(flt.Authors) == 0 || slices.Contains(flt.Authors, ev.PubKey)) {
					out = append(out, ev)
					break
				}
			}
		}
		return out, nil
	}
	publishEventFn = func(_ context.Context, relays []string, ev *nip01.Event) ([]string, []string) {
		if f.down {
			return nil, relays
		}
		e := *ev
		f.published = append(f.published, &e)
		return relays, nil
	}
	randomDelay = func() time.Duration { return 2 * time.Hour }
	t.Cleanup(func() { fetchEventsFn, publishEventFn, randomDelay = prevFetch, prevPub, prevDelay })
	return f
}

func runSignal(t *testing.T, c *cobra.Command, args []string, flags map[string]string) (map[string]any, error) {
	t.Helper()
	c = discoverCmd(c, nil)
	c.SetArgs(args)
	for _, sub := range c.Commands() {
		discoverCmd(sub, nil)
	}
	for k, v := range flags {
		target := c
		if len(args) > 0 {
			if sub, _, err := c.Find(args); err == nil && sub != nil {
				target = sub
			}
		}
		if err := target.Flags().Set(k, v); err != nil {
			t.Fatalf("flag %s: %v", k, err)
		}
	}
	var err error
	stdout, stderr := captureStreams(t, func() { err = c.Execute() })
	var doc map[string]any
	_ = json.Unmarshal([]byte(stdout), &doc)
	if doc == nil && testing.Verbose() {
		t.Logf("stdout %q stderr %q", stdout, stderr)
	}
	return doc, err
}

func (f *signalFixture) publishedOf(kind int) []*nip01.Event {
	var out []*nip01.Event
	for _, ev := range f.published {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// usedYesterday places an order from the listing and dates it before this
// session, as if it was placed and answered a day ago.
func (f *signalFixture) usedYesterday(t *testing.T) {
	t.Helper()
	if _, err := runSignal(t, newDiscoverOpenCmd(), []string{offerHandle(f.addr), "100"}, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	oc, _ := nipCO.DecodeOrderConnection(f.conn)
	orders, _ := ledger.Orders()
	o := orders[0]
	o.CreatedAt = time.Now().Unix() - 2*86400
	_ = ledger.PutOrder(o)
	l, _ := ledger.Load()
	c, _ := l.FindCall(o.CallID)
	c.Target = oc.Pubkey
	_ = l.Save()
	prev := processStart
	processStart = time.Now().Unix() - 3600
	t.Cleanup(func() { processStart = prev })
}

// Recommending needs an identity, then an offer used a day before and not in
// this session; it is queued with a delay and published by publish-due.
func TestDiscover_RecommendQueueAndPublish(t *testing.T) {
	f := newSignalFixture(t, false)
	if _, err := runSignal(t, newDiscoverRecommendCmd(), []string{offerHandle(f.addr)}, map[string]string{"yes": "true"}); output.AsCLIError(err) == nil || output.AsCLIError(err).Reason != reasonIdentityRequired {
		t.Fatalf("no identity: %v", err)
	}
	if _, err := identity.GenerateAndSaveLocal(); err != nil {
		t.Fatal(err)
	}
	_, err := runSignal(t, newDiscoverRecommendCmd(), []string{offerHandle(f.addr)}, map[string]string{"yes": "true"})
	if ce := output.AsCLIError(err); ce == nil || ce.Reason != reasonRecommendNotAllowed {
		t.Fatalf("never used: %v", err)
	}
	f.usedYesterday(t)
	if _, err := runSignal(t, newDiscoverRecommendCmd(), []string{offerHandle(f.addr)}, nil); output.ExitCode(err) != 2 {
		t.Fatalf("--json without --yes: %v", err)
	}
	doc, err := runSignal(t, newDiscoverRecommendCmd(), []string{offerHandle(f.addr)}, map[string]string{"yes": "true", "text": "good"})
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	after := int64(doc["publish_after"].(float64))
	if after < time.Now().Unix()+3000 || !strings.Contains(doc["audience"].(string), "public") {
		t.Errorf("queued %v", doc)
	}
	if len(f.publishedOf(listing.KindRecommendation)) != 0 {
		t.Fatal("published before its time")
	}
	items, _ := config.Outbox()
	items[0].PublishAfter = time.Now().Unix() - 1
	_ = config.PutOutbox(items[0])
	if _, err := runSignal(t, newDiscoverPublishDueCmd(), nil, nil); err != nil {
		t.Fatalf("publish-due: %v", err)
	}
	recs := f.publishedOf(listing.KindRecommendation)
	if len(recs) != 1 || recs[0].PubKey != localIdentityHex() || recs[0].Content != "good" {
		t.Fatalf("published %v", recs)
	}
	if r, err := listing.ParseRecommendation(recs[0]); err != nil || !slices.Contains(r.Offers, f.addr) {
		t.Errorf("recommendation %+v %v", r, err)
	}
	// p and a carry the relay the listing was read from.
	for _, tag := range recs[0].Tags {
		if (tag[0] == "p" || tag[0] == "a") && (len(tag) < 3 || tag[2] != "wss://discover.example") {
			t.Errorf("%s tag %v: want the relay hint wss://discover.example", tag[0], tag)
		}
	}
}

// A recommendation's hint is the relay that served the listing, not merely the
// first one the user reads.
func TestDiscover_RecommendHintIsTheListingsRelay(t *testing.T) {
	f := newSignalFixture(t, true)
	if err := config.AddDiscoverRelay("wss://zeta.example"); err != nil {
		t.Fatal(err)
	}
	f.onlyOn[f.ev.ID] = "wss://zeta.example"
	f.usedYesterday(t)
	if _, err := runSignal(t, newDiscoverRecommendCmd(), []string{offerHandle(f.addr)}, map[string]string{"yes": "true"}); err != nil {
		t.Fatalf("recommend: %v", err)
	}
	items, _ := config.Outbox()
	var draft nip01.Event
	if len(items) != 1 || json.Unmarshal([]byte(items[0].Draft), &draft) != nil {
		t.Fatalf("outbox %v", items)
	}
	for _, tag := range draft.Tags {
		if (tag[0] == "p" || tag[0] == "a") && (len(tag) < 3 || tag[2] != "wss://zeta.example") {
			t.Errorf("%s tag %v: want the hint wss://zeta.example", tag[0], tag)
		}
	}
}

// A report needs a reason, and is queued like a recommendation (no "used" rule).
func TestDiscover_Report(t *testing.T) {
	f := newSignalFixture(t, true)
	if _, err := runSignal(t, newDiscoverReportCmd(), []string{offerHandle(f.addr)}, map[string]string{"yes": "true"}); output.ExitCode(err) != 2 {
		t.Fatalf("no reason: %v", err)
	}
	doc, err := runSignal(t, newDiscoverReportCmd(), []string{offerHandle(f.addr)}, map[string]string{"yes": "true", "reason": "spam"})
	if err != nil || doc["kind"] != float64(listing.KindReport) {
		t.Fatalf("report: %v %v", err, doc)
	}
}

// The operator's own 11191: the first publish warns, an offer whose listing
// doesn't name the user is not verified; once a listing names the user, list
// shows the operator verified.
func TestDiscover_OperatorAndVerification(t *testing.T) {
	f := newSignalFixture(t, true)
	self := localIdentityHex()
	doc, err := runSignal(t, newDiscoverOperatorCmd(), []string{"add", offerHandle(f.addr)}, map[string]string{"yes": "true"})
	if err != nil {
		t.Fatalf("operator add: %v", err)
	}
	if doc["first_publish"] != true || len(doc["not_verified"].([]any)) != 1 || len(f.publishedOf(listing.KindOperatorOffers)) != 1 {
		t.Fatalf("operator add: %v", doc)
	}
	if doc, _ := runSignal(t, newDiscoverOperatorCmd(), []string{"add", offerHandle(f.addr)}, map[string]string{"yes": "true"}); len(f.publishedOf(listing.KindOperatorOffers)) != 1 {
		t.Errorf("nothing to change published again: %v", doc)
	}
	// A newer version of the listing names the user as its operator.
	oc, _ := nipCO.DecodeOrderConnection(f.conn)
	now := time.Now().Unix()
	ev, err := listing.NewListing(listing.ListingFields{Order: &oc, Network: "mainnet", Title: "Buy cash",
		Status: listing.StatusOpen, Expiration: now + 86400, Operator: &listing.Ref{Pubkey: self}}, now-5, now,
		func(ev *nip01.Event) error { return ev.Sign(f.orders.hubPriv) })
	if err != nil {
		t.Fatal(err)
	}
	f.events = append(f.events, ev)
	list, err := runSignal(t, newDiscoverListCmd(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	o := list["offers"].([]any)[0].(map[string]any)
	op, _ := o["operator"].(map[string]any)
	if op == nil || op["verified"] != true || op["pubkey"] != self {
		t.Errorf("operator %v", o["operator"])
	}
}

// --offline reads only the cache: no relay is needed or contacted.
func TestDiscover_Offline(t *testing.T) {
	f := newSignalFixture(t, false)
	if _, err := runSignal(t, newDiscoverListCmd(), nil, nil); err != nil {
		t.Fatal(err)
	}
	f.down = true
	if err := config.RemoveDiscoverRelay("wss://discover.example"); err != nil {
		t.Fatal(err)
	}
	doc, err := runSignal(t, newDiscoverListCmd(), nil, map[string]string{"offline": "true"})
	if err != nil || len(doc["offers"].([]any)) != 1 {
		t.Fatalf("offline: %v %v", err, doc)
	}
}

// curators publish keeps the names private; pull restores them.
func TestDiscover_CuratorsPublishPull(t *testing.T) {
	f := newSignalFixture(t, true)
	cur := strings.Repeat("c1", 32)
	_ = config.AddCurator(cur)
	if _, err := runSignal(t, newDiscoverCuratorsCmd(), []string{"publish"}, map[string]string{"yes": "true"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	sets := f.publishedOf(listing.CuratorSetKind)
	if len(sets) != 1 || strings.Contains(sets[0].Content, cur) || slices.ContainsFunc(sets[0].Tags, func(tg []string) bool { return tg[0] == "p" }) {
		t.Fatalf("the curator set is not private: %+v", sets)
	}
	_ = config.RemoveCurator(cur)
	doc, err := runSignal(t, newDiscoverCuratorsCmd(), []string{"pull"}, map[string]string{"yes": "true"})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got, _ := config.Curators(); !slices.Contains(got, cur) {
		t.Errorf("after pull: %v %v", got, doc)
	}
	_ = json.Marshal
}

// D-4: a recommendation the user withdrew with a kind 5 carries nothing over.
func TestOwnWithdrawn(t *testing.T) {
	newListingFixture(t)
	priv, self := testmint.Key(t)
	node := strings.Repeat("ab", 32)
	rec := listing.NewRecommendation(node, "", []listing.Ref{{Pubkey: "31190:" + node + ":x"}}, "")
	rec.CreatedAt = uint64(time.Now().Unix() - 10) //nolint:gosec // unix time
	if err := rec.Sign(priv); err != nil {
		t.Fatal(err)
	}
	if ownWithdrawn(self, rec) {
		t.Fatal("withdrawn with no deletion")
	}
	del := listing.NewRecommendationDeletion("31191:"+self+":"+node, time.Now().Unix())
	if err := del.Sign(priv); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(del)
	if err := config.PutDiscoverEvent(del.ID, 5, self, int64(del.CreatedAt), string(raw), config.OriginTrusted, ""); err != nil { //nolint:gosec // unix time
		t.Fatal(err)
	}
	if !ownWithdrawn(self, rec) {
		t.Error("a withdrawn recommendation still counts")
	}
}
