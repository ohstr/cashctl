// Package dial sniffs what kind of string a user pasted into `receive`,
// `join --hub`, or `connect add` — a plain NWC URI, a cash token, or one
// of the two Hub bech32 formats — without a network call
// (cashctl-plan.md's "receive auto-detects what you pasted").
package dial

import (
	"encoding/json"
	"strings"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
)

// Kind identifies what Sniff found.
type Kind int

const (
	KindUnknown Kind = iota
	// KindNWCURI: a plain nostr+walletconnect:// URI.
	KindNWCURI
	// KindCashToken: a cash-token-family bech32 string (lokicash1...,
	// satscash1..., or any other HRP nipcash.Decode would accept) —
	// anything that isn't specifically a Hub connection.
	KindCashToken
	// KindCircleHub: a circlehub1... string — the one Hub kind cashctl
	// actually acts on (feeds `join --hub`).
	KindCircleHub
	// KindCashHub: a cashhub1... string — recognized so a mis-paste can
	// fail with a specific error, never acted on (cashctl has no mint
	// capability).
	KindCashHub
	// KindNostrEntity: a standard NIP-19 entity (npub/nsec/note/nprofile/
	// nevent/naddr) or this project's own nconnection1... — recognized so
	// a mis-paste gets a specific "that's a Nostr <hrp>, not a cash
	// token/..." error instead of falling through to nipcash.Decode,
	// which accepts any HRP (see its own doc comment) and so used to try
	// parsing these bytes as NIP-CASH TLV data, failing with a cryptic
	// "nipcash: truncated TLV entry at offset N" that named neither what
	// was pasted nor why it was wrong — worst on nsec1... (a Nostr
	// PRIVATE KEY): the actual bytes are already redacted from any error
	// by RedactSecretInput's secretLikePattern, but the message itself
	// gave no hint the user had just pasted a private key into the wrong
	// place. NostrEntityHRP recovers which one, for a caller that wants
	// to name it specifically.
	KindNostrEntity
	// KindLightningInvoice: a BOLT11 invoice (lnbc1..., lnfc1..., any HRP
	// starting "ln") or an LNURL — recognized so a mis-paste is named as
	// what it is instead of being decoded as a cash token and failing on
	// the bytes.
	KindLightningInvoice
	// KindCashOrder: a cashorder1... Order connection (NIP-CO), which
	// `cash order place` buys cash over.
	KindCashOrder
	// KindWitnessAddress: a segwit (bech32/bech32m) address of a chain cashctl
	// holds bills for — where `cash withdraw` pays out.
	KindWitnessAddress
	// KindWithdrawEvent: a JSON kind-23187 withdraw quote or kind-23188
	// withdraw receipt (NIP-CASH §Redeeming On-chain).
	KindWithdrawEvent
	// KindPayout: a JSON payout object as cash_withdraw and its lookups answer.
	KindPayout
)

// witnessHRPs are the witness HRPs of the chains cashctl holds bills for.
func witnessHRPs() map[string]bool {
	out := map[string]bool{}
	for _, chain := range []string{"bitcoin", "flokicoin"} {
		hrps, _ := nipcash.WithdrawHRPs(chain)
		for _, h := range hrps {
			out[h] = true
		}
	}
	return out
}

// SniffJSON classifies a JSON value: a withdraw quote or receipt event, a
// payout object, or KindUnknown.
func SniffJSON(s string) Kind {
	var v struct {
		Kind         *int            `json:"kind"`
		State        string          `json:"state"`
		PayoutMillis json.RawMessage `json:"payout_millis"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(s)), &v) != nil {
		return KindUnknown
	}
	switch {
	case v.Kind != nil && (*v.Kind == 23187 || *v.Kind == 23188):
		return KindWithdrawEvent
	case v.Kind == nil && v.State != "" && len(v.PayoutMillis) > 0:
		return KindPayout
	}
	return KindUnknown
}

// hub HRPs are matched exactly; anything else that decodes as valid
// bech32 is treated as an attempted cash token — the only other bech32
// family in this ecosystem (nipcash.Decode itself accepts any HRP, so
// cashctl doesn't need to enumerate lokicash/satscash/... here) — except
// nostrEntityHRPs, the one other family cashctl has any reason to name
// specifically (see KindNostrEntity's own doc comment).
const (
	hrpCircleHub = "circlehub"
	hrpCashHub   = "cashhub"
	hrpCashOrder = "cashorder"
)

// nostrEntityHRPs: the standard NIP-19 entity prefixes (npub, nsec, note,
// nprofile, nevent, naddr — nip19.go's own EncodeAny/DecodeAny switch)
// plus nconnection, this project's own non-standard extension
// (internal/credential's connection-key targets) — not itself NIP-19, but
// the same "bech32 identity/entity string, not a cash token" family a
// mis-paste needs the same specific error for.
var nostrEntityHRPs = map[string]bool{
	"npub": true, "nsec": true, "note": true,
	"nprofile": true, "nevent": true, "naddr": true,
	"nconnection": true,
}

// decodeHRP is Sniff's own bech32 decode, factored out so NostrEntityHRP
// can reuse it without re-deriving the same trim/decode logic.
func decodeHRP(s string) (hrp string, ok bool) {
	s = strings.TrimSpace(s)
	// DecodeNoLimit, not Decode: a cash token routinely exceeds BIP-173's
	// 90-character limit (NIP-CASH §Wire Format explicitly says
	// implementations MUST NOT enforce it) — Decode would misclassify a
	// long, perfectly valid token as KindUnknown.
	hrp, _, err := bech32.DecodeNoLimit(s)
	if err != nil {
		return "", false
	}
	return hrp, true
}

// Sniff classifies s without a network call: a scheme check for the
// plain-URI case, a bare bech32 decode (HRP only, no TLV parsing) for
// everything else.
func Sniff(s string) Kind {
	if strings.HasPrefix(strings.TrimSpace(s), "nostr+walletconnect://") {
		return KindNWCURI
	}
	hrp, ok := decodeHRP(s)
	if !ok {
		return KindUnknown
	}
	switch {
	case hrp == hrpCircleHub:
		return KindCircleHub
	case hrp == hrpCashHub:
		return KindCashHub
	case hrp == hrpCashOrder:
		return KindCashOrder
	case nostrEntityHRPs[hrp]:
		return KindNostrEntity
	case strings.HasPrefix(hrp, "ln"):
		return KindLightningInvoice
	case witnessHRPs()[hrp]:
		return KindWitnessAddress
	default:
		return KindCashToken
	}
}

// NostrEntityHRP returns s's own bech32 HRP when Sniff(s) == KindNostrEntity
// (empty otherwise) — for a caller that wants to name which kind of Nostr
// entity was pasted (an npub, an nsec, ...) rather than a generic message.
func NostrEntityHRP(s string) string {
	hrp, ok := decodeHRP(s)
	if !ok || !nostrEntityHRPs[hrp] {
		return ""
	}
	return hrp
}
