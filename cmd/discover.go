package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nip19"
	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash/listing"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/output"
)

// Offer listings (NIP-CASH §Listing Offers). Listings are ads: `list` shows
// what the relays carry, ranked by the signals of the user's curators; `show`
// is the tap, the only contact with the listed Hub (get_info, nothing else);
// `open` taps again and hands the listed string to `cash order place` or
// `join`, which repeat their own checks.

// discoverChains are the chains cashctl holds bills for.
var discoverChains = []string{"bitcoin", "flokicoin"}

// versionBookKey is the VersionBook's discover_state key.
const versionBookKey = "version_book"

func newCashDiscoverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Find order links and circles others list (ads, checked when you open one)",
		RunE:  groupRunE,
	}
	cmd.AddCommand(newDiscoverListCmd(), newDiscoverShowCmd(), newDiscoverOpenCmd(),
		newDiscoverRelaysCmd(), newDiscoverCuratorsCmd(), newDiscoverRecommendCmd(), newDiscoverReportCmd(),
		newDiscoverPublishDueCmd(), newDiscoverOperatorCmd())
	return cmd
}

// fetchEventsFn reads events matching filters from the relays. Swapped in tests.
var fetchEventsFn = func(ctx context.Context, relays []string, filters []nip01.SubscriptionFilter) ([]*nip01.Event, []string) {
	var out []*nip01.Event
	var failed []string
	group := nip01.NewSubscriptionFilterGroup()
	for i := range filters {
		f := filters[i]
		group.Add(&f)
	}
	for _, r := range relays {
		u, err := url.Parse(r)
		if err != nil {
			failed = append(failed, r)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		evs, err := relayclient.ReadEventsFromRelay(rctx, u, group)
		cancel()
		if err != nil {
			failed = append(failed, r)
			continue
		}
		out = append(out, evs...)
	}
	return out, failed
}

// discoverRelays is --relay and the stored discovery relays.
func discoverRelays(cmd *cobra.Command) ([]string, error) {
	extra, _ := cmd.Flags().GetStringSlice("relay")
	stored, err := config.DiscoverRelays()
	if err != nil {
		return nil, output.RuntimeError(cmd, err)
	}
	all := append(slices.Clone(extra), stored...)
	slices.Sort(all)
	all = slices.Compact(all)
	if len(all) == 0 {
		return nil, output.UsageError(cmd, errors.New("add a discovery relay: `cashctl cash discover relays add wss://…`"))
	}
	return all, nil
}

// offerHandle is how `list` names an offer: of- and 10 hex of its address.
func offerHandle(addr string) string {
	sum := sha256.Sum256([]byte(addr))
	return "of-" + hex.EncodeToString(sum[:])[:10]
}

// discovered is one listing with its signals and, when verified, its operator.
type discovered struct {
	L        *listing.Listing
	Signals  listing.Signals
	Operator string // the verified operator's pubkey, "" when unverified
	Names    map[string]string
}

// discoverWorld is everything the cache holds, read into its parts.
type discoverWorld struct {
	book      *listing.VersionBook
	recs      []*listing.Recommendation
	reps      []*listing.Report
	deletions []*nip01.Event
	operators map[string]*listing.OperatorOffers // newest 11191 per author
	names     map[string]string                  // kind-0 display names
	followsOf map[string][]string                // newest kind-3 p tags per author
}

// trustedSeeds is who the trusted requests ask for (NIP-CASH §Fetching
// Listings): node identities named by recommendations from people (follows and
// curators) and by verified operators' 11191, and the operators those nodes'
// listings name, plus self's own 11191. Nothing a stranger's listing or
// recommendation names gets in, so a flood can't steer the trusted fetch (D-1).
func (w *discoverWorld) trustedSeeds(people []string, self string, now int64) (nodes, ops []string) {
	trusted := map[string]bool{}
	for _, p := range people {
		trusted[p] = true
	}
	nodeSet, opSet := map[string]bool{}, map[string]bool{}
	for _, r := range w.recs {
		if trusted[r.Author] && r.Node != "" {
			nodeSet[r.Node] = true
		}
	}
	if self != "" {
		opSet[self] = true
	}
	for range 2 {
		for _, addr := range w.book.Addresses() {
			l, ok := w.book.Current(addr, now)
			if ok && nodeSet[l.NodeIdentity] && l.Operator != nil {
				opSet[l.Operator.Pubkey] = true
			}
		}
		for op := range opSet {
			offers := w.operators[op]
			if offers == nil {
				continue
			}
			for _, addr := range w.book.Addresses() {
				if l, ok := w.book.Current(addr, now); ok && listing.OperatorVerified(l, offers) {
					for _, n := range offers.Nodes {
						nodeSet[n.Pubkey] = true
					}
					break
				}
			}
		}
	}
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	for o := range opSet {
		ops = append(ops, o)
	}
	sort.Strings(nodes)
	sort.Strings(ops)
	return nodes, ops
}

// localIdentityHex is the local identity's pubkey without unlocking anything,
// "" when there is none.
func localIdentityHex() string {
	npub, _, err := currentIdentity()
	if err != nil || npub == "" {
		return ""
	}
	pub, err := nip19.DecodePublicKey(npub)
	if err != nil {
		return ""
	}
	return pub
}

// sinceKey names the since of one filter on one relay in discover_state.
func sinceKey(relay string, f nip01.SubscriptionFilter) string {
	f.Since = 0
	b, _ := json.Marshal(struct {
		F    nip01.SubscriptionFilter
		Tags map[string][]string
	}{f, f.Tags})
	sum := sha256.Sum256(append([]byte(relay+"|"), b...))
	return "since:" + hex.EncodeToString(sum[:])[:24]
}

// fetchIntoCache asks every relay for every filter, from the since it last
// answered, and caches each event whose id and signature check. It returns the
// relays that failed every request.
func fetchIntoCache(ctx context.Context, relays []string, filters []nip01.SubscriptionFilter, origin string) []string {
	var failed []string
	for _, r := range relays {
		answered := false
		for _, f := range filters {
			key := sinceKey(r, f)
			if b, _ := config.DiscoverState(key); len(b) > 0 {
				if n, err := strconv.ParseUint(string(b), 10, 64); err == nil {
					f.Since = n
				}
			}
			started := time.Now().Unix()
			evs, bad := fetchEventsFn(ctx, []string{r}, []nip01.SubscriptionFilter{f})
			if len(bad) > 0 {
				continue
			}
			answered = true
			for _, ev := range evs {
				if ev == nil || ev.Verify() != nil {
					continue
				}
				raw, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				_ = config.PutDiscoverEvent(ev.ID, ev.Kind, ev.PubKey, int64(ev.CreatedAt), string(raw), origin) //nolint:gosec // unix time
			}
			// A little before the request started, so an event a relay stored
			// meanwhile is asked for again.
			_ = config.PutDiscoverState(key, []byte(strconv.FormatInt(max(started-60, 0), 10)))
		}
		if !answered {
			failed = append(failed, r)
		}
	}
	_ = config.PruneDiscoverEvents(discoverCacheMax)
	return failed
}

// discoverCacheMax bounds discover_events.
const discoverCacheMax = 5000

// readWorld reads the cache: listings into the version book, and every signal.
func readWorld(chains []string, testNets bool, now int64) *discoverWorld {
	w := &discoverWorld{book: listing.NewVersionBook(), operators: map[string]*listing.OperatorOffers{},
		names: map[string]string{}, followsOf: map[string][]string{}}
	if snap, _ := config.DiscoverState(versionBookKey); snap != nil {
		_ = w.book.Restore(snap)
	}
	raws, _ := config.DiscoverEvents()
	var ops, kind0, kind3 []*nip01.Event
	for _, raw := range raws {
		var ev nip01.Event
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		e := ev
		switch ev.Kind {
		case listing.KindListing:
			if l, err := listing.ParseListing(&e, listing.CheckOptions{Chains: chains, TestNetworks: testNets}); err == nil {
				w.book.Offer(l, now)
			}
		case listing.KindRecommendation:
			if r, err := listing.ParseRecommendation(&e); err == nil {
				w.recs = append(w.recs, r)
			}
		case listing.KindReport:
			if r, err := listing.ParseReport(&e); err == nil {
				w.reps = append(w.reps, r)
			}
		case 5:
			w.deletions = append(w.deletions, &e)
		case listing.KindOperatorOffers:
			ops = append(ops, &e)
		case 0:
			kind0 = append(kind0, &e)
		case 3:
			kind3 = append(kind3, &e)
		}
	}
	for _, d := range w.deletions {
		w.book.ApplyDeletion(d)
	}
	w.recs = listing.NewestRecommendations(w.recs, w.deletions)
	authors := map[string]bool{}
	for _, ev := range ops {
		authors[ev.PubKey] = true
	}
	for a := range authors {
		if o := listing.NewestOperatorOffers(ops, a); o != nil {
			w.operators[a] = o
		}
	}
	for _, ev := range newestPerAuthor(kind0) {
		var meta struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
		}
		if json.Unmarshal([]byte(ev.Content), &meta) == nil {
			name := meta.DisplayName
			if name == "" {
				name = meta.Name
			}
			if name != "" {
				w.names[ev.PubKey] = untrusted(name, listing.MaxTitle)
			}
		}
	}
	for _, ev := range newestPerAuthor(kind3) {
		for _, t := range ev.Tags {
			if len(t) >= 2 && t[0] == "p" && isHex64(t[1]) {
				w.followsOf[ev.PubKey] = append(w.followsOf[ev.PubKey], t[1])
			}
		}
	}
	if snap, err := w.book.Snapshot(); err == nil {
		_ = config.PutDiscoverState(versionBookKey, snap)
	}
	return w
}

// newestPerAuthor keeps each author's newest replaceable event.
func newestPerAuthor(evs []*nip01.Event) []*nip01.Event {
	best := map[string]*nip01.Event{}
	for _, ev := range evs {
		if cur, ok := best[ev.PubKey]; !ok || nip01.Supersedes(ev, cur) {
			best[ev.PubKey] = ev
		}
	}
	out := make([]*nip01.Event, 0, len(best))
	for _, ev := range best {
		out = append(out, ev)
	}
	return out
}

// operatorOf is the verified operator of the listing at addr: the listing's p,
// confirmed by that account's newest 11191 naming the listing.
func (w *discoverWorld) operatorOf(now int64) func(string) (string, bool) {
	return func(addr string) (string, bool) {
		l, ok := w.book.Current(addr, now)
		if !ok || l.Operator == nil {
			return "", false
		}
		if listing.OperatorVerified(l, w.operators[l.Operator.Pubkey]) {
			return l.Operator.Pubkey, true
		}
		return "", false
	}
}

// discoverRun fetches (unless --offline), then reads the cache: listings
// versioned, ranked by the signals of the user's follows and curators. It
// reports the relays that failed.
func discoverRun(cmd *cobra.Command) ([]discovered, *listing.VersionBook, []string, error) {
	offline, _ := cmd.Flags().GetBool("offline")
	var relays []string
	if !offline {
		var err error
		if relays, err = discoverRelays(cmd); err != nil {
			return nil, nil, nil, err
		}
	}
	testNets, _ := cmd.Flags().GetBool("test-networks")
	if os.Getenv("CASHCTL_TEST_NETWORKS") == "1" {
		testNets = true
	}
	chains := discoverChains
	if c, _ := cmd.Flags().GetString("chain"); c != "" {
		chains = []string{c}
	}
	curators, _ := config.Curators()
	self := localIdentityHex()
	now := time.Now().Unix()
	var failed []string
	if !offline {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		failed = fetchIntoCache(ctx, relays, []nip01.SubscriptionFilter{listing.GlobalFilter(chains, 0)}, config.OriginGlobal)
		if self != "" {
			// The user's own follows and relays are trusted, never pruned.
			own := fetchIntoCache(ctx, relays, []nip01.SubscriptionFilter{{Kinds: []int{3, 10002}, Authors: []string{self}}}, config.OriginTrusted)
			failed = intersect(failed, own)
		}
		// Trusted requests, again while they name more (a recommendation names
		// a node, whose listing names an operator, whose 11191 names nodes).
		var asked []nip01.SubscriptionFilter
		for range 3 {
			w := readWorld(chains, testNets, now)
			people := append(slices.Clone(w.followsOf[self]), curators...)
			nodes, ops := w.trustedSeeds(people, self, now)
			trusted := listing.TrustedFilters(listing.Trusted{Follows: w.followsOf[self], Curators: curators, Nodes: nodes, Operators: ops})
			if len(trusted) == 0 || sameFilters(trusted, asked) {
				break
			}
			asked = trusted
			failed = intersect(failed, fetchIntoCache(ctx, relays, trusted, config.OriginTrusted))
		}
		if raws, _ := config.DiscoverEvents(); len(failed) == len(relays) && len(raws) == 0 {
			return nil, nil, failed, output.NetworkError(cmd, errors.New("no discovery relay answered and nothing is cached"))
		}
	}
	w := readWorld(chains, testNets, now)
	follows := w.followsOf[self]
	trust := listing.Trust{Curators: map[string]bool{}, Follows: map[string]bool{}}
	for _, c := range curators {
		trust.Curators[c] = true
	}
	for _, f := range follows {
		trust.Follows[f] = true
	}
	opOf := w.operatorOf(now)
	var out []discovered
	for _, addr := range w.book.Addresses() {
		l, ok := w.book.Current(addr, now)
		if !ok {
			continue
		}
		var followers []string
		op, verified := opOf(addr)
		if verified {
			for _, f := range follows {
				if slices.Contains(w.followsOf[f], op) {
					followers = append(followers, f)
				}
			}
		}
		out = append(out, discovered{L: l, Signals: listing.SignalsFor(l, trust, w.recs, w.reps, followers, opOf),
			Operator: op, Names: w.names})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return listing.RankLess(out[i].L, out[i].Signals, out[j].L, out[j].Signals, now)
	})
	return out, w.book, failed, nil
}

// sameFilters reports two filter lists as equal (as JSON).
func sameFilters(a, b []nip01.SubscriptionFilter) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// intersect is the relays in both lists: a relay failed overall only when it
// failed both passes.
func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		if slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// untrusted is a lister's text made safe for a terminal: one line, bounded,
// look-alike marks and controls removed.
func untrusted(s string, maxClusters int) string {
	return output.Sanitize(listing.SanitizeText(s, maxClusters))
}

// signalAuthor names a signal's author: their kind-0 name, else their npub.
func signalAuthor(r map[string]any) string {
	if n, _ := r["name"].(string); n != "" {
		return n
	}
	by, _ := r["by"].(string)
	return npubOrHex(by)
}

func npubOrHex(pub string) string {
	if n, err := nip19.EncodePublicKey(pub); err == nil {
		return n
	}
	return pub
}

// listingView is one listing for --json: sanitized strings only, marks as words.
func listingView(d discovered) map[string]any {
	l := d.L
	v := map[string]any{"offer": offerHandle(l.Address()), "address": l.Address(), "type": string(l.Type),
		"title": untrusted(l.Title, listing.MaxTitle), "content": untrusted(l.Content, listing.MaxContent),
		"node": l.NodeIdentity, "chain": l.Chain, "network": l.Network, "test_network": l.Network != "mainnet",
		"status": string(l.Status), "summary": output.NonNil(l.Summary), "listed_at": l.CreatedAt(),
		"operator": nil}
	if d.Operator != "" {
		op := map[string]any{"pubkey": d.Operator, "verified": true, "followed_by": output.NonNil(d.Signals.OperatorFollowedBy)}
		if n := d.Names[d.Operator]; n != "" {
			op["name"] = n
		}
		v["operator"] = op
	}
	if l.Image != "" {
		v["image"] = l.Image
	}
	var hints []map[string]any
	for _, h := range l.Hints {
		hints = append(hints, map[string]any{"payment": h.Payment, "min_millis": h.MinMillis, "max_millis": h.MaxMillis,
			"fee_base": h.FeeBase, "fee_ppm": h.FeePpm})
	}
	v["hints_as_listed"] = output.NonNil(hints)
	var recs, reps []map[string]any
	named := func(m map[string]any, who string) map[string]any {
		if n := d.Names[who]; n != "" {
			m["name"] = n
		}
		return m
	}
	for _, r := range d.Signals.Recommendations {
		recs = append(recs, named(map[string]any{"by": r.Author, "at": r.CreatedAt}, r.Author))
	}
	for _, r := range d.Signals.Reports {
		reps = append(reps, named(map[string]any{"by": r.Author, "reason": string(r.Reason), "other_offer": r.Addr != "" && r.Addr != l.Address()}, r.Author))
	}
	v["recommendations"], v["reports"] = output.NonNil(recs), output.NonNil(reps)
	section := "unreviewed"
	if len(d.Signals.Recommendations) > 0 || len(d.Signals.OperatorFollowedBy) > 0 {
		section = "reviewed"
	}
	v["section"] = section
	return v
}

func newDiscoverListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List offers the discovery relays carry (ads; nothing is checked until you show one)",
		Args:  output.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := rejectConnectionFlag(cmd); err != nil {
				return err
			}
			discoverPre(cmd)
			jsonMode, _ := cmd.Flags().GetBool("json")
			typ, _ := cmd.Flags().GetString("type")
			onlyUnreviewed, _ := cmd.Flags().GetBool("unreviewed")
			all, _, failed, err := discoverRun(cmd)
			if err != nil {
				return err
			}
			curators, _ := config.Curators()
			var views []map[string]any
			for _, d := range all {
				if typ != "" && !strings.HasPrefix(string(d.L.Type), map[string]string{"order": "cash_order", "circle": "circle_wallet"}[typ]) {
					continue
				}
				v := listingView(d)
				if onlyUnreviewed && v["section"] != "unreviewed" {
					continue
				}
				views = append(views, v)
			}
			if jsonMode {
				follows := 0
				if self := localIdentityHex(); self != "" {
					follows = len(readFollows(self))
				}
				output.PrintJSON(map[string]any{"offers": output.NonNil(views), "curators": len(curators),
					"ranking_from":  map[string]any{"follows": follows, "curators": output.NonNil(curators)},
					"relays_failed": output.NonNil(failed)})
				return nil
			}
			for _, r := range failed {
				output.Notef(false, "relay %s didn't answer", output.Sanitize(r))
			}
			if len(curators) == 0 {
				output.Println("Ranking from: nobody yet: everything below is unreviewed. Listings are ads; the Hub's terms are checked when you `show` one.")
			} else {
				output.Printf("Ranking from: %d curators — change them with `cash discover curators`\n", len(curators))
			}
			unreviewed := 0
			for _, v := range views {
				if v["section"] == "unreviewed" && !onlyUnreviewed {
					unreviewed++
					continue
				}
				printListingCard(v)
			}
			if unreviewed > 0 {
				output.Printf("Unreviewed offers (%d): `cash discover list --unreviewed`\n", unreviewed)
			}
			return nil
		},
	}
	cmd.Flags().String("type", "", "order or circle")
	cmd.Flags().Bool("unreviewed", false, "list the offers nobody you trust recommends")
	cmd.Flags().StringSlice("relay", nil, "also read this relay (this run only)")
	cmd.Flags().Bool("offline", false, "read only what is cached, contacting no relay")
	cmd.Flags().String("chain", "", "only this chain")
	cmd.Flags().Bool("test-networks", false, "also list offers on test networks")
	return cmd
}

// printListingCard prints a card: cashctl's own marks first, in their own
// column, then the lister's text as plain, sanitized text.
func printListingCard(v map[string]any) {
	marks := ""
	if v["status"] == string(listing.StatusPaused) {
		marks += "[paused] "
	}
	if v["test_network"] == true {
		marks += "[test network] "
	}
	if v["section"] == "unreviewed" {
		marks += "[unreviewed] "
	}
	if reps, _ := v["reports"].([]map[string]any); len(reps) > 0 {
		marks += "! "
	}
	title, _ := v["title"].(string)
	if title == "" {
		title = "(no title)"
	}
	node, _ := v["node"].(string)
	operator := "operator unknown"
	if op, ok := v["operator"].(map[string]any); ok {
		name, _ := op["name"].(string)
		if name == "" {
			name = npubOrHex(op["pubkey"].(string))
		}
		operator = "run by " + name + " ✓"
		if fb, _ := op["followed_by"].([]string); len(fb) > 0 {
			operator += fmt.Sprintf(", followed by %d you follow", len(fb))
		}
	}
	output.Printf("%v  %s%s  — node %s, %s\n", v["offer"], marks, title, shortFingerprint(node), operator)
	if summary, _ := v["summary"].([]string); len(summary) > 0 {
		output.Printf("    as listed: %s\n", strings.Join(summary, " · "))
	}
	// Each signal by its author, a recommendation with its date (D-3).
	recs, _ := v["recommendations"].([]map[string]any)
	for _, r := range recs {
		at, _ := r["at"].(int64)
		output.Printf("    recommended by %s on %s\n", signalAuthor(r), time.Unix(at, 0).UTC().Format("2006-01-02"))
	}
	reps, _ := v["reports"].([]map[string]any)
	for _, r := range reps {
		about := ""
		if r["other_offer"] == true {
			about = ", about another offer of this node"
		}
		output.Printf("    reported by %s (%v)%s\n", signalAuthor(r), r["reason"], about)
	}
}

// resolveOffer finds a listing by handle, address or naddr among the current
// versions.
func resolveOffer(cmd *cobra.Command, ref string) (*listing.Listing, *listing.VersionBook, error) {
	all, book, _, err := discoverRun(cmd)
	if err != nil {
		return nil, nil, err
	}
	addr := ref
	if strings.HasPrefix(ref, "naddr1") {
		if pointer, err := nip19.DecodeAddr(ref); err == nil {
			addr = listing.Address(pointer.PublicKey, pointer.Identifier)
		}
	}
	for _, d := range all {
		if d.L.Address() == addr || offerHandle(d.L.Address()) == ref {
			return d.L, book, nil
		}
	}
	return nil, nil, output.NotFoundError(cmd, ref, errors.New("no such offer on the discovery relays (or it is hidden)"))
}

// tapInfoFn reads get_info over a listing's own connection, decoded and raw.
// Swapped in tests.
var tapInfoFn = func(ctx context.Context, l *listing.Listing) (*nip47.GetInfoResult, json.RawMessage, error) {
	var p nip47.PairingInfo
	switch {
	case l.Order != nil:
		p = nip47.PairingInfo{WalletPubkey: l.Order.Pubkey, RelayURLs: l.Order.Relays, Secret: l.Order.Secret}
	case l.Circle != nil:
		p = nip47.PairingInfo{WalletPubkey: l.Circle.Pubkey, RelayURLs: l.Circle.Relays, Secret: l.Circle.Secret}
	default:
		return nil, nil, errors.New("no listed connection")
	}
	nwc, err := relayclient.NewNWCClient(ctx, &p, nip47.EncryptionNIP44V2)
	if err != nil {
		return nil, nil, err
	}
	defer nwc.Close()
	resp, err := nwc.Call(ctx, nip47.MethodGetInfo, nil)
	if err != nil {
		return nil, nil, err
	}
	if resp.Error != nil {
		return nil, nil, &relayclient.WalletError{Method: nip47.MethodGetInfo, Code: resp.Error.Code, Message: resp.Error.Message}
	}
	var info nip47.GetInfoResult
	if err := json.Unmarshal(resp.Result, &info); err != nil {
		return nil, nil, err
	}
	return &info, resp.Result, nil
}

var tapNoticeShown bool

// tap runs the tap: get_info on the listed connection only, and CheckTap.
func tap(cmd *cobra.Command, l *listing.Listing, book *listing.VersionBook) listing.TapResult {
	jsonMode, _ := cmd.Flags().GetBool("json")
	if !tapNoticeShown {
		output.Notef(jsonMode, "Checking with the Hub… its relay sees your IP address and the time, not who you are.")
		tapNoticeShown = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, raw, err := tapInfoFn(ctx, l)
	r := listing.CheckTap(l, info, raw, err)
	book.Taps().Record(l, r)
	if snap, err := book.Snapshot(); err == nil {
		_ = config.PutDiscoverState(versionBookKey, snap)
	}
	return r
}

func newDiscoverShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <offer>",
		Short: "Check an offer with its Hub (get_info only) and show its real terms",
		Args:  output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectConnectionFlag(cmd); err != nil {
				return err
			}
			discoverPre(cmd)
			jsonMode, _ := cmd.Flags().GetBool("json")
			l, book, err := resolveOffer(cmd, args[0])
			if err != nil {
				return err
			}
			r := tap(cmd, l, book)
			v := listingView(discovered{L: l})
			v["tap"], v["tap_reason"] = r.Outcome.String(), r.Reason
			switch r.Outcome {
			case listing.TapShow:
				if r.Terms != nil {
					v["terms"] = r.Terms
				}
				if r.Circle != nil {
					v["circle"] = map[string]any{"fees_ppm": r.Circle.FeesPpm, "circle_policy": r.Circle.CirclePolicy,
						"available_millis": r.Circle.AvailableMillis, "rotation": r.Circle.RotationDelaySecs != nil}
				}
				v["hint_differs"] = r.HintDiffers
				v["next_step"] = "cashctl cash discover open " + offerHandle(l.Address()) + " <amount>"
			case listing.TapHideForGood:
				v["next_step"] = "you can report it: `cashctl cash discover report " + offerHandle(l.Address()) + " --reason impersonation`"
			}
			if jsonMode {
				output.PrintJSON(v)
				return nil
			}
			printListingCard(v)
			switch r.Outcome {
			case listing.TapShow:
				if r.HintDiffers {
					output.Println("    the listing's numbers differ from the Hub's; the Hub's are below")
				}
				if r.Terms != nil {
					if r.Terms.Lightning != nil {
						k := r.Terms.Lightning
						output.Printf("    Lightning: %s–%s, fee %s + %d ppm\n", output.FormatAmount(int64(k.MinMillis)), output.FormatAmount(int64(k.MaxMillis)), output.FormatAmount(int64(k.FeeBase)), k.FeePpm) //nolint:gosec // amounts fit
					}
					if r.Terms.Onchain != nil {
						k := r.Terms.Onchain
						output.Printf("    On-chain: %s–%s, fee %s + %d ppm\n", output.FormatAmount(int64(k.MinMillis)), output.FormatAmount(int64(k.MaxMillis)), output.FormatAmount(int64(k.FeeBase)), k.FeePpm) //nolint:gosec // amounts fit
					}
				}
				if r.Circle != nil {
					output.Printf("    circle: in-circle fee %d ppm, %s, up to %s available\n", r.Circle.FeesPpm, circlePolicyPhrase(r.Circle.CirclePolicy), output.FormatAmount(r.Circle.AvailableMillis))
				}
				output.Printf("Next: %v\n", v["next_step"])
			case listing.TapHideForGood:
				output.Println("This offer's Hub answered as a different node, or on another network: hidden for good.")
			case listing.TapHideUntilNewer:
				output.Printf("Closed for now: %s.\n", output.Sanitize(r.Reason))
			default:
				output.Printf("Unavailable for now: %s; try `show` again later.\n", output.Sanitize(r.Reason))
			}
			return nil
		},
	}
	cmd.Flags().StringSlice("relay", nil, "also read this relay (this run only)")
	cmd.Flags().Bool("offline", false, "read only what is cached, contacting no relay")
	cmd.Flags().Bool("test-networks", false, "also find offers on test networks")
	cmd.Flags().String("chain", "", "only this chain")
	return cmd
}

func newDiscoverOpenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "open <offer> <amount>",
		Short: "Check an offer again, then order from it or join it",
		Long: `Taps the offer again (never a stored answer) and, when the Hub answers as listed,
hands its connection to ` + "`cash order place`" + ` (an order link) or ` + "`join`" + ` (a circle),
which run their own checks and confirmations.`,
		Args: output.ExactArgs(2),
		RunE: runDiscoverOpen,
	}
	cmd.Flags().StringSlice("relay", nil, "also read this relay (this run only)")
	cmd.Flags().Bool("offline", false, "read only what is cached, contacting no relay")
	cmd.Flags().Bool("test-networks", false, "also find offers on test networks")
	cmd.Flags().String("chain", "", "only this chain")
	// The flags of `cash order place` and `join` it passes on.
	cmd.Flags().Bool("onchain", false, "order: pay on-chain")
	cmd.Flags().String("to", "cash", "order: who the bill is for")
	cmd.Flags().Bool("wait", false, "order: watch the order until it is finished")
	cmd.Flags().String("max-receive", "", "join: requested receive cap")
	cmd.Flags().Duration("expiry", 0, "join: requested expiry")
	cmd.Flags().String("budget-renewal", "", "join: budget renewal")
	cmd.Flags().String("backup-file", "", "join: where to back up the wallet's connection")
	cmd.Flags().String("as", "", "join: override credential")
	cmd.Flags().Bool("allow-unverified", false, "use a string whose signature can't be confirmed")
	return cmd
}

func runDiscoverOpen(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	discoverPre(cmd)
	l, book, err := resolveOffer(cmd, args[0])
	if err != nil {
		return err
	}
	r := tap(cmd, l, book)
	switch r.Outcome {
	case listing.TapShow:
	case listing.TapHideForGood:
		return output.WithReason(output.AuthError(cmd, errors.New("this offer's Hub answered as a different node, or on another network; nothing was sent")), reasonConnectionMismatch)
	case listing.TapHideUntilNewer:
		reason := reasonNotSelling
		if l.Circle != nil {
			reason = reasonNotAccepting
		}
		return output.WithReason(output.AuthError(cmd, fmt.Errorf("closed for now: %s", output.Sanitize(r.Reason))), reason)
	default:
		return output.NetworkError(cmd, fmt.Errorf("unavailable for now: %s", output.Sanitize(r.Reason)))
	}
	var target *cobra.Command
	var run func(*cobra.Command, []string) error
	var pass []string
	if l.Order != nil {
		target, run, pass = newCashOrderPlaceCmd(), runCashOrderPlace, []string{"onchain", "to", "wait", "allow-unverified"}
	} else {
		target, run, pass = newCircleJoinCmd(), runCircleJoin, []string{"max-receive", "expiry", "budget-renewal", "backup-file", "as", "allow-unverified"}
	}
	for _, name := range []string{"json", "yes", "connection", "config-dir"} {
		if f := cmd.Flags().Lookup(name); f != nil {
			target.Flags().AddFlag(f)
		}
	}
	for _, name := range pass {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			if err := target.Flags().Set(name, f.Value.String()); err != nil {
				return output.InvocationError(cmd, err)
			}
		}
	}
	return run(target, []string{l.Connection, args[1]})
}

func newDiscoverRelaysCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "relays", Short: "The discovery relays `list` reads (none are shipped)", RunE: groupRunE}
	cmd.AddCommand(&cobra.Command{Use: "list", Args: output.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		relays, err := config.DiscoverRelays()
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
		return printList(cmd, "relays", relays)
	}}, &cobra.Command{Use: "add <wss://…>...", Args: output.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		for _, r := range args {
			if !strings.HasPrefix(r, "wss://") && !strings.HasPrefix(r, "ws://") {
				return output.InvalidInputError(cmd, r, errors.New("a relay is a wss:// URL"))
			}
			if err := config.AddDiscoverRelay(r); err != nil {
				return output.RuntimeError(cmd, err)
			}
		}
		relays, _ := config.DiscoverRelays()
		return printList(cmd, "relays", relays)
	}}, &cobra.Command{Use: "rm <url>...", Args: output.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		for _, r := range args {
			if err := config.RemoveDiscoverRelay(r); err != nil {
				return output.RuntimeError(cmd, err)
			}
		}
		relays, _ := config.DiscoverRelays()
		return printList(cmd, "relays", relays)
	}})
	return cmd
}

func newDiscoverCuratorsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "curators", Short: "The accounts whose recommendations and reports rank offers (none are shipped)", RunE: groupRunE}
	parse := func(cmd *cobra.Command, v string) (string, error) {
		if pub, err := nip19.DecodePublicKey(v); err == nil {
			return pub, nil
		}
		if isHex64(v) {
			return strings.ToLower(v), nil
		}
		return "", output.InvalidInputError(cmd, v, errors.New("a curator is an npub or a hex pubkey"))
	}
	cmd.AddCommand(&cobra.Command{Use: "list", Args: output.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := config.Curators()
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
		return printList(cmd, "curators", c)
	}}, &cobra.Command{Use: "add <npub|hex>...", Args: output.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		for _, a := range args {
			pub, err := parse(cmd, a)
			if err != nil {
				return err
			}
			if err := config.AddCurator(pub); err != nil {
				return output.RuntimeError(cmd, err)
			}
		}
		c, _ := config.Curators()
		return printList(cmd, "curators", c)
	}}, &cobra.Command{Use: "rm <npub|hex>...", Args: output.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		for _, a := range args {
			pub, err := parse(cmd, a)
			if err != nil {
				return err
			}
			if err := config.RemoveCurator(pub); err != nil {
				return output.RuntimeError(cmd, err)
			}
		}
		c, _ := config.Curators()
		return printList(cmd, "curators", c)
	}})
	publish := &cobra.Command{Use: "publish", Short: "Publish your curator set (names private, NIP-44 to yourself)", Args: output.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return curatorsPublish(cmd) }}
	pull := &cobra.Command{Use: "pull", Short: "Replace your local curators with your newest published set", Args: output.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return curatorsPull(cmd) }}
	for _, c := range []*cobra.Command{publish, pull} {
		c.Flags().StringSlice("relay", nil, "also read and publish to this relay")
	}
	cmd.AddCommand(publish, pull)
	return cmd
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func printList(cmd *cobra.Command, key string, items []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	if jsonMode {
		output.PrintJSON(map[string]any{key: output.NonNil(items)})
		return nil
	}
	if len(items) == 0 {
		output.Printf("No %s.\n", key)
	}
	for _, i := range items {
		if key == "curators" {
			i = npubOrHex(i)
		}
		output.Println(i)
	}
	return nil
}

// readFollows is the identity's follows from its newest cached kind 3.
func readFollows(self string) []string {
	return readWorld(discoverChains, true, time.Now().Unix()).followsOf[self]
}
