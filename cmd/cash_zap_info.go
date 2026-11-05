package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipCZ"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/output"
)

// The user's own Cash Zap Info (NIP-CZ §Kind 10522): a replaceable kind-10522
// event listing the chains, trusted minters and read relays the user accepts, so
// senders' wallets know what to send. It is built from the local trust store
// (`cashctl cash minters`) and the user's NIP-65 relays; nothing here creates a
// key, and `publish` asks before it makes the list public.

// buildCashZapInfo builds the user's unsigned kind-10522 Cash Zap Info draft from
// the minters it trusts, any extra accepted chains, and the relays it reads Cash
// Zaps on. It is pure and deterministic: tags are sorted so the same inputs
// always produce the same event (content "", kind 10522).
//
//   - chain: one per accepted chain — the distinct chains of the trusted minters,
//     plus extraChains (a chain with no trusted minter is never auto-included).
//   - mint: ["mint", "02"+xonly, chain] per trusted minter. The store holds a
//     minter as its 64-hex x-only part, and NIP-CZ compares minters by that part
//     only (§Terminology), never the parity byte. So the x-only key is emitted as
//     the compressed form "02"+xonly, a valid 66-hex key that always matches; the
//     true parity is not recovered (there is no canonical helper for it).
//   - relay: one per read relay.
func buildCashZapInfo(minters []config.TrustedMinter, extraChains, relays []string) *nip01.Event {
	chainSet := map[string]bool{}
	for _, m := range minters {
		if m.Chain != "" {
			chainSet[m.Chain] = true
		}
	}
	for _, c := range extraChains {
		if c != "" {
			chainSet[c] = true
		}
	}
	chains := make([]string, 0, len(chainSet))
	for c := range chainSet {
		chains = append(chains, c)
	}
	slices.Sort(chains)

	type mint struct{ key, chain string }
	seen := map[mint]bool{}
	var mints []mint
	for _, m := range minters {
		x := strings.ToLower(strings.TrimSpace(m.MinterXOnly))
		if m.Chain == "" || !isHexKey(x) {
			continue
		}
		e := mint{key: "02" + x, chain: m.Chain}
		if !seen[e] {
			seen[e] = true
			mints = append(mints, e)
		}
	}
	slices.SortFunc(mints, func(a, b mint) int {
		if a.chain != b.chain {
			return strings.Compare(a.chain, b.chain)
		}
		return strings.Compare(a.key, b.key)
	})

	rs := slices.Clone(relays)
	slices.Sort(rs)
	rs = slices.Compact(rs)

	tags := make([][]string, 0, len(chains)+len(mints)+len(rs))
	for _, c := range chains {
		tags = append(tags, []string{"chain", c})
	}
	for _, m := range mints {
		tags = append(tags, []string{"mint", m.key, m.chain})
	}
	for _, r := range rs {
		tags = append(tags, []string{"relay", r})
	}
	return nip01.NewEvent(nipCZ.KindCashZapInfo, "", tags...)
}

// userReadRelays are where the user reads its Cash Zaps: the read relays of its
// newest cached NIP-65 (kind 10002) — a bare `r` entry counts as read, as in
// NIP-65 — plus --relay. These become the 10522 `relay` tags (NIP-CZ §Kind
// 10522), where recipients' wallets look for Cash Zaps.
func userReadRelays(cmd *cobra.Command, self string) []string {
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
			if len(t) >= 2 && t[0] == "r" && (len(t) == 2 || t[2] == "read") {
				out = append(out, t[1])
			}
		}
	}
	extra, _ := cmd.Flags().GetStringSlice("relay")
	out = append(out, extra...)
	slices.Sort(out)
	return slices.Compact(out)
}

// cashZapInfoDraft gathers the local trusted minters, any --chain additions and
// the user's read relays (--relay added), and builds the unsigned 10522. No
// network: it reads only the local store and cache.
func cashZapInfoDraft(cmd *cobra.Command, self string) (*nip01.Event, []config.TrustedMinter, error) {
	minters, err := config.TrustedMinters()
	if err != nil {
		return nil, nil, output.RuntimeError(cmd, err)
	}
	extraChains, _ := cmd.Flags().GetStringSlice("chain")
	draft := buildCashZapInfo(minters, extraChains, userReadRelays(cmd, self))
	return draft, minters, nil
}

// newCashZapInfoCmd publishes or previews the user's Cash Zap Info (kind 10522).
func newCashZapInfoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "zap-info",
		Short: "Publish the chains, minters and relays you accept for Cash Zaps (kind 10522)",
		RunE:  groupRunE,
	}
	cmd.AddCommand(newCashZapInfoShowCmd(), newCashZapInfoPublishCmd())
	return cmd
}

func newCashZapInfoShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the Cash Zap Info that `publish` would sign (a dry run; nothing is sent)",
		Long: "Print, without publishing, the kind-10522 Cash Zap Info built from the minters you trust\n" +
			"(`cashctl cash minters`), the distinct chains they cover, and the relays you read Cash Zaps\n" +
			"on (your NIP-65 read relays, plus --relay). A chain with no trusted minter is not included\n" +
			"unless you add it with --chain.",
		Args: output.NoArgs,
		RunE: runCashZapInfoShow,
	}
	zapInfoFlags(cmd)
	return cmd
}

func newCashZapInfoPublishCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Sign and publish your Cash Zap Info so senders know what you accept (public; asks first)",
		Long: "Build, sign and publish your replaceable kind-10522 Cash Zap Info so senders' wallets know\n" +
			"which chains, minters and relays you accept. This is public: it names the minters you trust\n" +
			"and the relays you read on. The event goes to your NIP-65 write relays (plus --relay); its\n" +
			"`relay` tags are the relays you read Cash Zaps on (your NIP-65 read relays, plus --relay).",
		Args: output.NoArgs,
		RunE: runCashZapInfoPublish,
	}
	zapInfoFlags(cmd)
	return cmd
}

// zapInfoFlags are shared by show and publish.
func zapInfoFlags(cmd *cobra.Command) {
	cmd.Flags().StringSlice("chain", nil, "also accept this chain, even with no trusted minter (repeatable)")
	cmd.Flags().StringSlice("relay", nil, "add this relay to the ones you read Cash Zaps on, and publish there")
}

func runCashZapInfoShow(cmd *cobra.Command, _ []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	self, err := requireIdentity(cmd, "showing your Cash Zap Info")
	if err != nil {
		return err
	}
	draft, minters, err := cashZapInfoDraft(cmd, self)
	if err != nil {
		return err
	}
	draft.PubKey = self // the event would be signed by the local identity
	if jsonMode {
		output.PrintJSON(map[string]any{"event": draft, "trusted_minters": len(minters),
			"chains": zapInfoTagValues(draft, "chain"), "mints": zapInfoTagValues(draft, "mint"), "relays": zapInfoTagValues(draft, "relay")})
		return nil
	}
	output.Printf("kind %d (Cash Zap Info), by %s — not published\n", draft.Kind, npubOrHex(self))
	for _, t := range draft.Tags {
		output.Println(strings.Join(t, " "))
	}
	if len(draft.Tags) == 0 {
		output.Println("(nothing to list yet: trust a minter with `cashctl cash minters add`, or add --chain/--relay)")
	}
	return nil
}

func runCashZapInfoPublish(cmd *cobra.Command, _ []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	self, err := requireIdentity(cmd, "publishing your Cash Zap Info")
	if err != nil {
		return err
	}
	draft, minters, err := cashZapInfoDraft(cmd, self)
	if err != nil {
		return err
	}
	targets := userRelays(cmd, self)
	if len(targets) == 0 {
		return output.UsageError(cmd, errors.New(
			"no relay to publish to: set your NIP-65 relays, or pass --relay wss://…"))
	}
	output.Notef(jsonMode, "This is public: it tells anyone the chains you accept, the %d minter(s) you trust and the relays you read Cash Zaps on. It goes to %s.",
		len(minters), strings.Join(targets, ", "))
	if ok, err := confirmPublic(cmd, "Publish your Cash Zap Info now?"); err != nil || !ok {
		if err == nil {
			output.Println("Cancelled.")
		}
		return err
	}
	ev, accepted, failed, err := publishNow(cmd, targets, draft)
	if err != nil {
		return output.NetworkError(cmd, fmt.Errorf("not published: %w", err))
	}
	if jsonMode {
		output.PrintJSON(map[string]any{"event_id": ev.ID, "published_to": accepted,
			"relays_failed": output.NonNil(failed), "trusted_minters": len(minters)})
		return nil
	}
	output.Printf("Published your Cash Zap Info (%s) to %s.\n", ev.ID, strings.Join(accepted, ", "))
	return nil
}

// zapInfoTagValues are the first values of every tag named name, for the
// --json summary (every chain, mint or relay, in event order).
func zapInfoTagValues(ev *nip01.Event, name string) []string {
	out := []string{}
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == name {
			out = append(out, t[1])
		}
	}
	return out
}
