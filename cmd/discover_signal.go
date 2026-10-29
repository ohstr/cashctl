package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nip44"
	"github.com/ohstr/nmilat/nipcash/listing"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// The user's own public signals (NIP-CASH §Recommendations, §Reports, §The
// Operator's Offers, §Curators). Recommendations and reports are queued with a
// random delay and published by publish-due; a recommendation withdrawal, the
// operator's 11191 and the curator set go out at once, each after the user
// confirmed what it makes public. Nothing here creates a key.

// Reasons of a refused signal (--json "reason").
const (
	reasonRecommendNotAllowed = "recommend_not_allowed"
	reasonIdentityRequired    = "identity_required"
)

// processStart is this session's start: a recommendation is never offered for
// an order or join of the same session.
var processStart = time.Now().Unix()

// publishEventFn publishes ev to the relays, returning those that accepted
// it. Swapped in tests.
var publishEventFn = func(ctx context.Context, relays []string, ev *nip01.Event) (accepted, failed []string) {
	for _, r := range relays {
		u, err := url.Parse(r)
		if err != nil {
			failed = append(failed, r)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ok, err := relayclient.PublishEventToRelay(rctx, u, ev)
		cancel()
		if err != nil || ok == nil || !ok.Accepted {
			failed = append(failed, r)
			continue
		}
		accepted = append(accepted, r)
	}
	return accepted, failed
}

// randomDelay is the queue delay of a recommendation or report: uniform in
// [1 h, 6 h). Swapped in tests.
var randomDelay = func() time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(5*3600))
	if err != nil {
		return 3 * time.Hour
	}
	return time.Hour + time.Duration(n.Int64())*time.Second
}

// requireIdentity is the local identity's pubkey; without one the command is
// refused (usage, identity_required) and no key is created.
func requireIdentity(cmd *cobra.Command, what string) (string, error) {
	pub := localIdentityHex()
	if pub == "" {
		return "", output.WithReason(output.UsageError(cmd, fmt.Errorf(
			"%s needs your Nostr account: run `cashctl init` (it asks before creating one)", what)), reasonIdentityRequired)
	}
	return pub, nil
}

// confirmPublic asks before publishing something public; under --json only
// --yes counts.
func confirmPublic(cmd *cobra.Command, question string) (bool, error) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	yes, _ := cmd.Flags().GetBool("yes")
	if jsonMode && !yes {
		return false, output.UsageError(cmd, errors.New("this publishes under your account: pass --yes once the user agreed"))
	}
	return Confirm(cmd, false, question), nil
}

// signalText is --text, "-" for stdin, or (interactively) a prompt; never
// pre-filled, may be empty.
func signalText(cmd *cobra.Command) (string, error) {
	t, _ := cmd.Flags().GetString("text")
	switch {
	case t == stdinArg:
		b, err := io.ReadAll(stdin)
		return strings.TrimSpace(string(b)), err
	case t != "" || cmd.Flags().Changed("text"):
		return t, nil
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	yes, _ := cmd.Flags().GetBool("yes")
	if jsonMode || yes {
		return "", nil
	}
	output.Fprintf(os.Stderr, "Your text (optional, Enter for none): ")
	line, _ := stdin.ReadString('\n')
	return strings.TrimSpace(line), nil
}

// userRelays are where the user's own events go and are read from: the write
// relays of the user's newest cached kind 10002, the discovery relays and
// --relay.
func userRelays(cmd *cobra.Command, self string) []string {
	var out []string
	raws, _ := config.DiscoverEvents()
	var lists []*nip01.Event
	for _, raw := range raws {
		var ev nip01.Event
		if json.Unmarshal([]byte(raw), &ev) == nil && ev.Kind == 10002 && ev.PubKey == self {
			e := ev
			lists = append(lists, &e)
		}
	}
	for _, ev := range newestPerAuthor(lists) {
		for _, t := range ev.Tags {
			if len(t) >= 2 && t[0] == "r" && (len(t) == 2 || t[2] == "write") {
				out = append(out, t[1])
			}
		}
	}
	stored, _ := config.DiscoverRelays()
	extra, _ := cmd.Flags().GetStringSlice("relay")
	out = append(append(out, stored...), extra...)
	slices.Sort(out)
	return slices.Compact(out)
}

// publishNow signs draft with the local identity (created_at now), publishes it
// and reads it back by id from a relay that took it. It caches the event.
func publishNow(cmd *cobra.Command, relays []string, draft *nip01.Event) (*nip01.Event, []string, []string, error) {
	priv, err := localPrivKey(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	ev := *draft
	ev.CreatedAt = uint64(time.Now().Unix()) //nolint:gosec // unix time
	if err := ev.Sign(priv); err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	accepted, failed := publishEventFn(ctx, relays, &ev)
	if len(accepted) == 0 {
		return &ev, nil, failed, errors.New("no relay took it")
	}
	back, _ := fetchEventsFn(ctx, accepted[:1], []nip01.SubscriptionFilter{{IDs: []string{ev.ID}}})
	if !slices.ContainsFunc(back, func(b *nip01.Event) bool { return b != nil && b.ID == ev.ID }) {
		return &ev, nil, append(failed, accepted...), errors.New("no relay that took it returns it")
	}
	raw, _ := json.Marshal(ev)
	_ = config.PutDiscoverEvent(ev.ID, ev.Kind, ev.PubKey, int64(ev.CreatedAt), string(raw), config.OriginTrusted, accepted[0]) //nolint:gosec // unix time
	return &ev, accepted, failed, nil
}

// discoverPre runs publish-due first, best effort, for every discover command.
func discoverPre(cmd *cobra.Command) {
	if _, err := publishDue(cmd, false); err != nil {
		jsonMode, _ := cmd.Flags().GetBool("json")
		output.Notef(jsonMode, "Note: queued recommendations or reports were not published yet: %v", err)
	}
}

// publishDue signs and publishes every waiting row that is due. A row whose
// publish fails stays waiting.
func publishDue(cmd *cobra.Command, force bool) ([]config.OutboxItem, error) {
	items, err := config.Outbox()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var due []config.OutboxItem
	for _, o := range items {
		if o.State == config.OutboxWaiting && (force || o.PublishAfter <= now) {
			due = append(due, o)
		}
	}
	if len(due) == 0 {
		return nil, nil
	}
	self := localIdentityHex()
	if self == "" {
		return nil, errors.New("no local identity")
	}
	var published []config.OutboxItem
	for _, o := range due {
		var draft nip01.Event
		if err := json.Unmarshal([]byte(o.Draft), &draft); err != nil {
			continue
		}
		ev, accepted, _, err := publishNow(cmd, userRelays(cmd, self), &draft)
		if err != nil {
			if ev == nil {
				return published, err // the key could not be used: every row waits
			}
			continue
		}
		o.State, o.EventID, o.PublishedTo = config.OutboxPublished, ev.ID, strings.Join(accepted, " ")
		_ = config.PutOutbox(o)
		published = append(published, o)
	}
	return published, nil
}

func newDiscoverPublishDueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "publish-due",
		Short: "Publish the queued recommendations and reports that are due",
		Args:  output.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			if id, _ := cmd.Flags().GetString("cancel"); id != "" {
				items, _ := config.Outbox()
				for _, o := range items {
					if o.ID == id && o.State == config.OutboxWaiting {
						o.State = config.OutboxCancelled
						if err := config.PutOutbox(o); err != nil {
							return output.RuntimeError(cmd, err)
						}
						output.Printf("Cancelled %s.\n", id)
						return nil
					}
				}
				return output.NotFoundError(cmd, id, errors.New("no waiting row by that name"))
			}
			if list, _ := cmd.Flags().GetBool("list"); list {
				items, _ := config.Outbox()
				var waiting []config.OutboxItem
				for _, o := range items {
					if o.State == config.OutboxWaiting {
						waiting = append(waiting, o)
					}
				}
				if jsonMode {
					output.PrintJSON(map[string]any{"waiting": output.NonNil(waiting)})
					return nil
				}
				for _, o := range waiting {
					output.Printf("%s  kind %d  %s  due %s\n", o.ID, o.Kind, o.Offer, time.Unix(o.PublishAfter, 0).UTC().Format(time.RFC3339))
				}
				return nil
			}
			published, err := publishDue(cmd, false)
			if err != nil {
				output.Notef(jsonMode, "Note: %v; the rows stay queued.", err)
			}
			items, _ := config.Outbox()
			var waiting []config.OutboxItem
			for _, o := range items {
				if o.State == config.OutboxWaiting {
					waiting = append(waiting, o)
				}
			}
			if jsonMode {
				output.PrintJSON(map[string]any{"published": output.NonNil(published), "waiting": output.NonNil(waiting)})
				return nil
			}
			output.Printf("Published %d; %d waiting.\n", len(published), len(waiting))
			return nil
		},
	}
	cmd.Flags().Bool("list", false, "print the queue")
	cmd.Flags().String("cancel", "", "cancel a waiting row (rq-…)")
	cmd.Flags().StringSlice("relay", nil, "also publish to this relay")
	return cmd
}

func newOutboxID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "rq-" + hex.EncodeToString(b)
}

// usedOffer reports whether this device used the offer (an order answered at
// least once, or a join done, over its connection at its node, from a verified
// string), and when it last ordered from or joined that node.
func usedOffer(l *listing.Listing) (used bool, last int64) {
	orders, _ := ledger.Orders()
	ld, _ := ledger.Load()
	for _, o := range orders {
		if o.HubIdentity != l.NodeIdentity {
			continue
		}
		last = max(last, o.CreatedAt)
		if c, ok := ld.FindCall(o.CallID); ok && c.Target == l.D && o.ConnVerified && o.State != ledger.OrderPlacing {
			used = true
		}
	}
	joins, _ := config.Joins()
	for _, j := range joins {
		if j.HubIdentity != l.NodeIdentity {
			continue
		}
		last = max(last, j.CreatedAt)
		if j.HubPubkey == l.D && j.State == config.JoinDone && j.ConnVerified {
			used = true
		}
	}
	return used, last
}

// resolveOffers resolves every argument to a current listing, in one run.
func resolveOffers(cmd *cobra.Command, refs []string) ([]*listing.Listing, error) {
	var out []*listing.Listing
	for _, ref := range refs {
		l, _, err := resolveOffer(cmd, ref)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

func newDiscoverRecommendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recommend <offer>...",
		Short: "Recommend offers of one node you used (public, published after a random delay)",
		Args:  cobra.ArbitraryArgs,
		RunE:  runDiscoverRecommend,
	}
	cmd.Flags().StringSlice("drop", nil, "stop naming this offer in your recommendation of its node")
	cmd.Flags().String("text", "", "your text (- reads it from stdin); never pre-filled")
	cmd.Flags().String("withdraw", "", "withdraw your recommendation of this offer's node (published at once)")
	cmd.Flags().StringSlice("relay", nil, "also read and publish to this relay")
	cmd.Flags().Bool("offline", false, "")
	cmd.Flags().Bool("test-networks", false, "")
	cmd.Flags().String("chain", "", "")
	return cmd
}

func runDiscoverRecommend(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	self, err := requireIdentity(cmd, "recommending")
	if err != nil {
		return err
	}
	discoverPre(cmd)
	npub := npubOrHex(self)
	if w, _ := cmd.Flags().GetString("withdraw"); w != "" {
		ls, err := resolveOffers(cmd, []string{w})
		if err != nil {
			return err
		}
		node := ls[0].NodeIdentity
		relays := userRelays(cmd, self)
		output.Notef(jsonMode, "This is public: it withdraws your (%s) recommendation of the Hub %s, on %s.", npub, shortFingerprint(node), strings.Join(relays, ", "))
		if ok, err := confirmPublic(cmd, "Withdraw it now?"); err != nil || !ok {
			if err == nil {
				output.Println("Cancelled.")
			}
			return err
		}
		del := listing.NewRecommendationDeletion(fmt.Sprintf("%d:%s:%s", listing.KindRecommendation, self, node), time.Now().Unix())
		ev, accepted, failed, err := publishNow(cmd, relays, del)
		if err != nil {
			return output.NetworkError(cmd, fmt.Errorf("not withdrawn: %w", err))
		}
		if jsonMode {
			output.PrintJSON(map[string]any{"event_id": ev.ID, "published_to": accepted, "relays_failed": output.NonNil(failed)})
			return nil
		}
		output.Printf("Withdrawn (%s).\n", ev.ID)
		return nil
	}
	if len(args) == 0 {
		return output.InvocationError(cmd, errors.New("name one or more offers of one node (or --withdraw <offer>)"))
	}
	ls, err := resolveOffers(cmd, args)
	if err != nil {
		return err
	}
	node := ls[0].NodeIdentity
	for _, l := range ls[1:] {
		if l.NodeIdentity != node {
			return output.UsageError(cmd, errors.New("a recommendation names offers of one node: recommend each node separately"))
		}
	}
	now := time.Now().Unix()
	for _, l := range ls {
		used, last := usedOffer(l)
		if !listing.RecommendAllowed(used, last, processStart, now) {
			msg := "only an offer you used (an order answered, or a join made, over a confirmed string) can be recommended"
			if used {
				msg = "a recommendation is allowed from " + time.Unix(max(last+86400, processStart), 0).UTC().Format(time.RFC3339) +
					", a day after your last order or join there and not in the same session"
			}
			return output.WithReason(output.InvalidInputError(cmd, offerHandle(l.Address()), errors.New(msg)), reasonRecommendNotAllowed)
		}
	}
	// One recommendation per user per node: start from the newest (a waiting
	// row, else the user's published one).
	relays := userRelays(cmd, self)
	var prevOffers []listing.Ref
	prevDate := int64(0)
	items, _ := config.Outbox()
	var waiting *config.OutboxItem
	for i := range items {
		if items[i].State == config.OutboxWaiting && items[i].Kind == listing.KindRecommendation && items[i].Node == node {
			waiting = &items[i]
		}
	}
	if waiting != nil {
		var d nip01.Event
		if json.Unmarshal([]byte(waiting.Draft), &d) == nil {
			prevOffers, prevDate = refsOf(&d), waiting.CreatedAt
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		failed := fetchIntoCache(ctx, relays, []nip01.SubscriptionFilter{
			{Kinds: []int{listing.KindRecommendation}, Authors: []string{self}, Tags: map[string][]string{"d": {node}}},
			{Kinds: []int{5}, Authors: []string{self}, Tags: map[string][]string{"k": {strconv.Itoa(listing.KindRecommendation)}}},
		}, config.OriginTrusted)
		cancel()
		if len(relays) > 0 && len(failed) == len(relays) {
			return output.NetworkError(cmd, errors.New("couldn't read your current recommendation of this node; nothing was queued"))
		}
		// A recommendation the user withdrew (their kind 5) names nothing to
		// carry over (D-4).
		if prev := ownNewest(self, listing.KindRecommendation, node); prev != nil && !ownWithdrawn(self, prev) {
			prevOffers, prevDate = refsOf(prev), int64(prev.CreatedAt) //nolint:gosec // unix time
		}
	}
	drop, _ := cmd.Flags().GetStringSlice("drop")
	// Every p and a carries a relay hint (NIP-CASH §Recommendations): where the
	// listing was read.
	nodeHint := listingRelayHint(ls[0], relays)
	var offers []listing.Ref
	for _, r := range prevOffers {
		if !slices.Contains(drop, r.Pubkey) && !slices.Contains(drop, offerHandle(r.Pubkey)) {
			if r.Relay == "" {
				r.Relay = nodeHint
			}
			offers = append(offers, r)
		}
	}
	for _, l := range ls {
		if !slices.ContainsFunc(offers, func(r listing.Ref) bool { return r.Pubkey == l.Address() }) {
			offers = append(offers, listing.Ref{Pubkey: l.Address(), Relay: listingRelayHint(l, relays)})
		}
	}
	text, err := signalText(cmd)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	audience := fmt.Sprintf("This is public. People who follow you, and anyone reading %s, will see that you (%s) use the Hub %s",
		strings.Join(relays, ", "), npub, shortFingerprint(node))
	if ls[0].Circle != nil {
		audience += fmt.Sprintf(", and that you are a member of %q", untrusted(ls[0].Title, listing.MaxTitle))
	}
	output.Notef(jsonMode, "%s.", audience)
	if prevDate > 0 {
		output.Notef(jsonMode, "This replaces your recommendation of %s, which named %d offer(s).", time.Unix(prevDate, 0).UTC().Format("2006-01-02"), len(prevOffers))
	}
	if ok, err := confirmPublic(cmd, "Queue this recommendation?"); err != nil || !ok {
		if err == nil {
			output.Println("Cancelled.")
		}
		return err
	}
	draft := listing.NewRecommendation(node, nodeHint, offers, text)
	return queueSignal(cmd, waiting, draft, node, offerHandle(ls[0].Address()), relays, audience)
}

// listingRelayHint is a relay l can be read from: the one that first served
// it, else the first discovery relay, else the first of relays.
func listingRelayHint(l *listing.Listing, relays []string) string {
	if l.Event != nil {
		if r := config.DiscoverEventRelay(l.Event.ID); r != "" {
			return r
		}
	}
	if stored, _ := config.DiscoverRelays(); len(stored) > 0 {
		return stored[0]
	}
	if len(relays) > 0 {
		return relays[0]
	}
	return ""
}

// refsOf is a recommendation draft's a tags.
func refsOf(ev *nip01.Event) []listing.Ref {
	var out []listing.Ref
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == "a" {
			r := listing.Ref{Pubkey: t[1]}
			if len(t) > 2 {
				r.Relay = t[2]
			}
			out = append(out, r)
		}
	}
	return out
}

// ownNewest is the user's newest cached event of kind with d.
func ownNewest(self string, kind int, d string) *nip01.Event {
	raws, _ := config.DiscoverEvents()
	var best *nip01.Event
	for _, raw := range raws {
		var ev nip01.Event
		if json.Unmarshal([]byte(raw), &ev) != nil || ev.Kind != kind || ev.PubKey != self {
			continue
		}
		if d != "" && !slices.ContainsFunc(ev.Tags, func(t []string) bool { return len(t) >= 2 && t[0] == "d" && t[1] == d }) {
			continue
		}
		e := ev
		if best == nil || nip01.Supersedes(&e, best) {
			best = &e
		}
	}
	return best
}

// ownWithdrawn reports a recommendation of self's that a cached kind 5 of
// self's withdrew.
func ownWithdrawn(self string, ev *nip01.Event) bool {
	r, err := listing.ParseRecommendation(ev)
	if err != nil {
		return false
	}
	raws, _ := config.DiscoverEvents()
	var dels []*nip01.Event
	for _, raw := range raws {
		var d nip01.Event
		if json.Unmarshal([]byte(raw), &d) == nil && d.Kind == 5 && d.PubKey == self {
			dels = append(dels, &d)
		}
	}
	return len(listing.NewestRecommendations([]*listing.Recommendation{r}, dels)) == 0
}

// queueSignal queues an unsigned draft with a random delay, replacing replace.
func queueSignal(cmd *cobra.Command, replace *config.OutboxItem, draft *nip01.Event, node, offer string, relays []string, audience string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	raw, err := json.Marshal(draft)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	o := config.OutboxItem{ID: newOutboxID(), Kind: draft.Kind, Node: node, Offer: offer, Draft: string(raw),
		PublishAfter: time.Now().Add(randomDelay()).Unix(), State: config.OutboxWaiting}
	if replace != nil {
		replace.State = config.OutboxCancelled
		_ = config.PutOutbox(*replace)
	}
	if err := config.PutOutbox(o); err != nil {
		return output.RuntimeError(cmd, err)
	}
	if jsonMode {
		output.PrintJSON(map[string]any{"outbox": o.ID, "kind": o.Kind, "publish_after": o.PublishAfter, "relays": output.NonNil(relays), "audience": audience})
		return nil
	}
	output.Printf("Queued as %s; published after %s by `cashctl cash discover publish-due` (any `cash discover` command runs it).\n",
		o.ID, time.Unix(o.PublishAfter, 0).UTC().Format(time.RFC3339))
	return nil
}

func newDiscoverReportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report <offer>",
		Short: "Report an offer's node (public, published after a random delay)",
		Args:  output.ExactArgs(1),
		RunE:  runDiscoverReport,
	}
	cmd.Flags().String("reason", "", "no_payout | impersonation | spam | terms_mismatch (required)")
	cmd.Flags().String("text", "", "your text (- reads it from stdin); never pre-filled")
	cmd.Flags().StringSlice("relay", nil, "also read and publish to this relay")
	cmd.Flags().Bool("offline", false, "")
	cmd.Flags().Bool("test-networks", false, "")
	cmd.Flags().String("chain", "", "")
	return cmd
}

func runDiscoverReport(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	reason, _ := cmd.Flags().GetString("reason")
	switch listing.ReportReason(reason) {
	case listing.ReasonNoPayout, listing.ReasonImpersonation, listing.ReasonSpam, listing.ReasonTermsMismatch:
	default:
		return output.InvocationError(cmd, errors.New("--reason is required: no_payout, impersonation, spam or terms_mismatch"))
	}
	self, err := requireIdentity(cmd, "reporting")
	if err != nil {
		return err
	}
	discoverPre(cmd)
	ls, err := resolveOffers(cmd, args)
	if err != nil {
		return err
	}
	l := ls[0]
	text, err := signalText(cmd)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	relays := userRelays(cmd, self)
	audience := fmt.Sprintf("This is public: it names you, %s, as the reporter, and the node %s; anyone reading %s sees it",
		npubOrHex(self), shortFingerprint(l.NodeIdentity), strings.Join(relays, ", "))
	output.Notef(jsonMode, "%s.", audience)
	if ok, err := confirmPublic(cmd, "Queue this report?"); err != nil || !ok {
		if err == nil {
			output.Println("Cancelled.")
		}
		return err
	}
	draft := listing.NewReport(l.NodeIdentity, l.Address(), l.Event.ID, listing.ReportReason(reason), text)
	return queueSignal(cmd, nil, draft, l.NodeIdentity, offerHandle(l.Address()), relays, audience)
}

// operatorPublishedKey marks, in discover_state, that this database published
// the user's 11191 once.
const operatorPublishedKey = "operator_published"

func newDiscoverOperatorCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "operator", Short: "Your own list of the offers you run on someone's node (kind 11191, public)", RunE: groupRunE}
	for _, add := range []bool{true, false} {
		add := add
		use := "add <offer>..."
		if !add {
			use = "remove <offer>..."
		}
		c := &cobra.Command{Use: use, Args: output.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			return runDiscoverOperator(cmd, args, add)
		}}
		c.Flags().StringSlice("relay", nil, "also read and publish to this relay")
		c.Flags().Bool("offline", false, "")
		c.Flags().Bool("test-networks", false, "")
		c.Flags().String("chain", "", "")
		cmd.AddCommand(c)
	}
	return cmd
}

// operatorRef resolves an offer to its address and relay hint; an address that
// no listing carries yet is still accepted.
func operatorRef(cmd *cobra.Command, ref string) (listing.Ref, *listing.Listing, error) {
	if k, _, _, ok := listing.ParseAddress(ref); ok && k == listing.KindListing {
		if l, _, err := resolveOffer(cmd, ref); err == nil {
			return listing.Ref{Pubkey: l.Address()}, l, nil
		}
		return listing.Ref{Pubkey: ref}, nil, nil
	}
	l, _, err := resolveOffer(cmd, ref)
	if err != nil {
		return listing.Ref{}, nil, err
	}
	return listing.Ref{Pubkey: l.Address()}, l, nil
}

func runDiscoverOperator(cmd *cobra.Command, args []string, add bool) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	self, err := requireIdentity(cmd, "an operator's list")
	if err != nil {
		return err
	}
	discoverPre(cmd)
	var refs []listing.Ref
	var notVerified []string
	for _, a := range args {
		r, l, err := operatorRef(cmd, a)
		if err != nil {
			return err
		}
		if l == nil {
			output.Notef(jsonMode, "No listing found for %s yet.", output.Sanitize(r.Pubkey))
		} else if add && (l.Operator == nil || l.Operator.Pubkey != self) {
			notVerified = append(notVerified, r.Pubkey)
		}
		refs = append(refs, r)
	}
	relays := userRelays(cmd, self)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	failed := fetchIntoCache(ctx, relays, []nip01.SubscriptionFilter{{Kinds: []int{listing.KindOperatorOffers}, Authors: []string{self}}}, config.OriginTrusted)
	cancel()
	if len(relays) == 0 || len(failed) == len(relays) {
		return output.NetworkError(cmd, errors.New("couldn't read your current list; nothing was published (a list built from nothing could drop offers you named elsewhere)"))
	}
	var evs []*nip01.Event
	if ev := ownNewest(self, listing.KindOperatorOffers, ""); ev != nil {
		evs = append(evs, ev)
	}
	newest := listing.NewestOperatorOffers(evs, self)
	var addRefs, removeRefs []listing.Ref
	if add {
		addRefs = refs
	} else {
		removeRefs = refs
	}
	merged := listing.MergeOperatorOffers(newest, addRefs, removeRefs)
	before := []string{}
	if newest != nil {
		for _, r := range newest.Offers {
			before = append(before, r.Pubkey)
		}
	}
	after := refsOf(merged)
	var afterAddrs, added, removed []string
	for _, r := range after {
		afterAddrs = append(afterAddrs, r.Pubkey)
		if !slices.Contains(before, r.Pubkey) {
			added = append(added, r.Pubkey)
		}
	}
	for _, b := range before {
		if !slices.Contains(afterAddrs, b) {
			removed = append(removed, b)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		word := "already named"
		if !add {
			word = "not named"
		}
		output.Printf("Nothing to change: %s.\n", word)
		return nil
	}
	published, _ := config.DiscoverState(operatorPublishedKey)
	first := newest == nil && len(published) == 0
	var warnings []string
	if first {
		w := fmt.Sprintf("This list is public and links your Nostr account (%s) to the nodes of these offers: anyone can see that you run them", npubOrHex(self))
		warnings = append(warnings, w)
		output.Notef(jsonMode, "%s.", w)
	}
	for _, nv := range notVerified {
		w := "not verified until the listing names you: ask the node's operator to set you as the listing's operator (" + nv + ")"
		warnings = append(warnings, w)
		output.Notef(jsonMode, "%s.", w)
	}
	output.Notef(jsonMode, "It goes to %s.", strings.Join(relays, ", "))
	if ok, err := confirmPublic(cmd, "Publish your list now?"); err != nil || !ok {
		if err == nil {
			output.Println("Cancelled.")
		}
		return err
	}
	ev, accepted, fails, err := publishNow(cmd, relays, merged)
	if err != nil {
		return output.NetworkError(cmd, fmt.Errorf("not published: %w", err))
	}
	_ = config.PutDiscoverState(operatorPublishedKey, []byte(ev.ID))
	if jsonMode {
		output.PrintJSON(map[string]any{"event_id": ev.ID, "offers": afterAddrs, "added": output.NonNil(added), "removed": output.NonNil(removed),
			"first_publish": first, "published_to": accepted, "relays_failed": output.NonNil(fails), "not_verified": output.NonNil(notVerified),
			"warnings": output.NonNil(warnings)})
		return nil
	}
	output.Printf("Published your list (%d offers) to %s.\n", len(afterAddrs), strings.Join(accepted, ", "))
	return nil
}

// curatorsPublish publishes the local curator set, its names private (NIP-44
// to self), with no title.
func curatorsPublish(cmd *cobra.Command) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	self, err := requireIdentity(cmd, "publishing your curators")
	if err != nil {
		return err
	}
	curators, err := config.Curators()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	relays := userRelays(cmd, self)
	output.Notef(jsonMode, "Your curator set goes to %s. A public curator set would tell everyone you browse cash offers; this one keeps the names private.", strings.Join(relays, ", "))
	if ok, err := confirmPublic(cmd, "Publish it?"); err != nil || !ok {
		if err == nil {
			output.Println("Cancelled.")
		}
		return err
	}
	priv, err := localPrivKey(cmd)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	draft, err := listing.NewCuratorSet(listing.CuratorSet{Curators: curators, Private: true}, func(s string) (string, error) {
		return nip44.EncryptHex(s, priv, self)
	})
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	ev, accepted, fails, err := publishNow(cmd, relays, draft)
	if err != nil {
		return output.NetworkError(cmd, fmt.Errorf("not published: %w", err))
	}
	if jsonMode {
		output.PrintJSON(map[string]any{"event_id": ev.ID, "curators": len(curators), "published_to": accepted, "relays_failed": output.NonNil(fails)})
		return nil
	}
	output.Printf("Published your curator set (%d) to %s.\n", len(curators), strings.Join(accepted, ", "))
	return nil
}

// curatorsPull reads the identity's newest curator set and, after a
// confirmation, makes it the local list.
func curatorsPull(cmd *cobra.Command) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	self, err := requireIdentity(cmd, "pulling your curators")
	if err != nil {
		return err
	}
	relays := userRelays(cmd, self)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	failed := fetchIntoCache(ctx, relays, []nip01.SubscriptionFilter{{Kinds: []int{listing.CuratorSetKind}, Authors: []string{self},
		Tags: map[string][]string{"d": {listing.CuratorSetD}}}}, config.OriginTrusted)
	cancel()
	if len(relays) == 0 || len(failed) == len(relays) {
		return output.NetworkError(cmd, errors.New("no relay answered"))
	}
	ev := ownNewest(self, listing.CuratorSetKind, listing.CuratorSetD)
	if ev == nil {
		return output.NotFoundError(cmd, "", errors.New("no curator set of yours was found"))
	}
	priv, err := localPrivKey(cmd)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	set, err := listing.ParseCuratorSet(ev, func(s string) (string, error) { return nip44.DecryptHex(s, priv, self) })
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	local, _ := config.Curators()
	var adds, drops []string
	for _, c := range set.Curators {
		if !slices.Contains(local, c) {
			adds = append(adds, c)
		}
	}
	for _, c := range local {
		if !slices.Contains(set.Curators, c) {
			drops = append(drops, c)
		}
	}
	output.Notef(jsonMode, "The published set adds %d and drops %d of your local curators.", len(adds), len(drops))
	if ok, err := confirmPublic(cmd, "Replace your local list with it?"); err != nil || !ok {
		if err == nil {
			output.Println("Cancelled.")
		}
		return err
	}
	for _, c := range drops {
		_ = config.RemoveCurator(c)
	}
	for _, c := range adds {
		_ = config.AddCurator(c)
	}
	now, _ := config.Curators()
	if jsonMode {
		output.PrintJSON(map[string]any{"curators": output.NonNil(now), "added": output.NonNil(adds), "removed": output.NonNil(drops)})
		return nil
	}
	return printList(cmd, "curators", now)
}
