package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nip19"
	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/nipcw"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/dial"
	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/output"
)

// newDecodeCmd is the single, do-everything local inspector: paste in
// anything cashctl otherwise accepts as a connection/token — a cash-token-
// family bech32 string, a circlehub1... Circle Hub connection, or a plain
// nostr+walletconnect:// URI — and get its fields back, no network call and
// nothing dialed or held by default. Dispatch reuses dial.Sniff, the same
// classifier `receive`/`join --hub`/`connect add` already use to figure
// out what a pasted string is, so there's exactly one place that knows how
// to tell these formats apart. A pairing secret is never included in the
// output, in any mode or for any kind — this is inspection, not a way to
// extract a working credential.
//
// --check opts into a network round trip, for the two kinds where "is
// this real" is actually answerable without committing to anything:
//   - a cash token: cross-checked against the Hub exactly like `receive`'s
//     own mandatory check (list_recipients) — but read-only, nothing saved.
//   - a circlehub1... connection: checked for reachability and whether the
//     Hub advertises create_circle_wallet at all — NOT a guarantee this
//     specific identity is allowlisted (that's only known by actually
//     calling create_circle_wallet), just "is joining even possible here."
//
// An NWC URI has no such check to opt into (there's nothing more specific
// than "is it reachable," and `connect add` already answers that by
// dialing for real) — --check is a no-op for that kind.
//
// In text mode, a human doesn't have to already know --check exists: if it
// wasn't passed explicitly, decode asks interactively instead (default NO
// — see shouldRunCheck's own doc comment for why: this command's whole
// pitch is no network by default) — see shouldRunCheck. --json never
// prompts (an agent has no terminal to answer from): it only checks when
// --check is passed, exactly as before. A cash token needing an identity
// proof (not cash-mode) is
// never prompted for at all when no local identity is configured — the
// check would have nothing to match against — decode just says so instead
// of asking a question it already knows the answer to.
func newDecodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "decode <string>",
		Short: "Inspect any cash token, Cash Hub or Circle Hub connection, NWC URI or npub locally — no network call unless --check is passed",
		Example: `  cashctl decode lokicash1...
  cashctl decode lokicash1... --check
  cashctl decode cashhub1... --check
  cashctl decode circlehub1...
  cashctl decode npub1...`,
		Args: output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectConnectionFlag(cmd); err != nil {
				return err
			}
			jsonMode, _ := cmd.Flags().GetBool("json")
			check, _ := cmd.Flags().GetBool("check")
			// Split before sniffing: NIP-CASH's "<token>#<cash_secret>"
			// combined presentation (§Presenting a Cash-Mode Slice as One
			// String) wouldn't decode as bech32 at all otherwise. A string
			// with no "#" is unaffected.
			// Trimmed before splitting: dial.Sniff trims internally for its
			// own classification, but a stray leading/trailing space or
			// newline (routine after a terminal copy-paste) would otherwise
			// still reach the actual decoder below, which rejects it
			// outright as invalid bech32 — confusing given Sniff itself
			// would've happily recognized it.
			value, embeddedSecret := nipcash.SplitCashSliceString(strings.TrimSpace(args[0]))

			switch dial.Sniff(value) {
			case dial.KindCashToken:
				return decodeCashToken(cmd, value, embeddedSecret, jsonMode, check)
			case dial.KindCircleHub:
				return decodeCircleHub(cmd, value, jsonMode, check)
			case dial.KindNWCURI:
				return decodeNWCURI(cmd, value, jsonMode)
			case dial.KindCashHub:
				return decodeCashHub(cmd, value, jsonMode, check)
			case dial.KindLightningInvoice:
				return output.InvalidInputError(cmd, value, errors.New(lightningMessage(value)))
			case dial.KindNostrEntity:
				if dial.NostrEntityHRP(value) == "npub" {
					return decodeNpub(cmd, value, jsonMode)
				}
				return output.InvalidInputError(cmd, value, errors.New(nostrEntityMessage(dial.NostrEntityHRP(value))))
			default:
				return output.InvalidInputError(cmd, value, fmt.Errorf(
					"not a recognized cash token, Hub connection, NWC URI or npub; "+
						"decode takes a cash token (lokicash1..., or <token>#<secret>), "+
						"a cashhub1... or circlehub1... connection, a nostr+walletconnect:// URI, or an npub1... key"))
			}
		},
	}
	cmd.Flags().Bool("check", false, "also cross-check against the Hub (cash token: matching recipient; cashhub: the online check; circlehub: can we join) — a network call")
	return cmd
}

func decodeCashToken(cmd *cobra.Command, value, embeddedCashSecret string, jsonMode, check bool) error {
	tok, err := nipcash.Decode(value)
	if err != nil {
		return output.InvalidInputError(cmd, value, err)
	}
	// See cash_receive.go: tok.IdentityRequired can go stale, an embedded
	// cash_secret overrides it.
	hasEmbeddedCashSecret := embeddedCashSecret != ""
	isCash := (tok.IdentityRequired != nil && !*tok.IdentityRequired) || hasEmbeddedCashSecret

	if !jsonMode {
		output.Println("type: cash_token")
		output.Printf("wallet_pubkey: %s\n", tok.WalletPubkey)
		output.Printf("relays: %s\n", joinStrings(tok.RelayURLs))
		if tok.IdentityRequired != nil {
			output.Printf("identity_required: %v\n", *tok.IdentityRequired)
		}
		printMintSignatureStatus(tok)
		if hub := hubGroupHex(tok); hub != "" {
			output.Printf("cash_hub: %s (only tokens from the same Cash Hub can be merged)\n", hub)
		}
		if hasEmbeddedCashSecret {
			output.Println("cash_secret: embedded — `cashctl receive` uses it automatically")
		}
	}

	// nipcash.Token has no JSON tags of its own (it's an internal SDK
	// type, not a wire DTO) — built explicitly here so decode's --json
	// output stays snake_case like every other cashctl command's, instead
	// of leaking Go field names.
	out := map[string]any{
		"type":              "cash_token",
		"hrp":               tok.HRP,
		"wallet_pubkey":     tok.WalletPubkey,
		"relays":            tok.RelayURLs,
		"identity_required": tok.IdentityRequired,
	}
	if hub := hubGroupHex(tok); hub != "" {
		out["cash_hub"] = hub
	}
	// Presence only, never the value itself — decode never echoes a
	// working spending credential (this file's own doc comment).
	if hasEmbeddedCashSecret {
		out["embedded_cash_secret_present"] = true
	}
	for k, v := range provenanceJSON(tok) {
		out[k] = v
	}

	if shouldCheckCashToken(cmd, jsonMode, check, isCash) {
		result := checkCashTokenAgainstHub(cmd, jsonMode, value, embeddedCashSecret, tok)
		if jsonMode {
			out["check"] = result
		} else {
			printCashCheck(result)
		}
	}
	if jsonMode {
		output.PrintJSON(out)
	}
	return nil
}

func decodeCircleHub(cmd *cobra.Command, value string, jsonMode, check bool) error {
	conn, err := nipcw.DecodeCircleHubConnection(value)
	if err != nil {
		return output.InvalidInputError(cmd, value, err)
	}

	if !jsonMode {
		output.Println("type: circlehub")
		output.Printf("wallet_pubkey: %s\n", conn.WalletPubkey)
		output.Printf("relays: %s\n", joinStrings(conn.RelayURLs))
		if conn.Label != "" {
			// Sanitized: an attacker-controlled Circle Hub connection's own
			// label has no character restrictions at decode time (see
			// nipcw.DecodeCircleHubConnection) — this is exactly the
			// inspect-before-you-trust output `decode` exists for.
			output.Printf("label: %s\n", output.Sanitize(conn.Label))
		}
	}

	out := map[string]any{
		"type":          "circlehub",
		"hrp":           nipcw.CircleHubConnectionHRP,
		"wallet_pubkey": conn.WalletPubkey,
		"relays":        conn.RelayURLs,
		"label":         conn.Label,
	}

	if shouldRunCheck(cmd, jsonMode, check, "Check if joining is possible?") {
		result := checkCircleHubJoinable(jsonMode, conn)
		if jsonMode {
			out["check"] = result
		} else {
			printCircleCheck(result)
		}
	}
	if jsonMode {
		output.PrintJSON(out)
	}
	return nil
}

// decodeNpub shows an npub's hex public key, the form `--to` and the Hub's
// own output use.
func decodeNpub(cmd *cobra.Command, value string, jsonMode bool) error {
	pub, err := nip19.DecodePublicKey(value)
	if err != nil {
		return output.InvalidInputError(cmd, value, err)
	}
	if jsonMode {
		output.PrintJSON(map[string]any{"type": "npub", "pubkey": pub})
		return nil
	}
	output.Println("type: npub")
	output.Printf("pubkey: %s\n", pub)
	return nil
}

func decodeNWCURI(cmd *cobra.Command, value string, jsonMode bool) error {
	pairing, err := nip47.ParsePairingURI(value)
	if err != nil {
		return output.InvalidInputError(cmd, value, err)
	}
	if jsonMode {
		output.PrintJSON(map[string]any{
			"type":          "nwc_uri",
			"wallet_pubkey": pairing.WalletPubkey,
			"relays":        pairing.RelayURLs,
		})
		return nil
	}
	output.Println("type: nwc_uri")
	output.Printf("wallet_pubkey: %s\n", pairing.WalletPubkey)
	output.Printf("relays: %s\n", joinStrings(pairing.RelayURLs))
	return nil
}

// provenance is what a bill's mint signature says about this copy, by its
// signature scheme (TLV 8): who signed it, and whether the signature covers this
// copy's own relays and secret (NIP-CASH §Mint Provenance).
type provenance struct {
	Scheme     string // mint_v1 | zap_v1 | malformed | none
	Verdict    string // valid | invalid | needs_zap_event | none
	RelayHash  string // schemes 1 and 2
	Minter     string // compressed hex, valid only
	ValidKnown bool   // false when the verdict needs the zap event
}

func provenanceOf(tok nipcash.Token) provenance {
	var p provenance
	switch tok.Scheme {
	case nipcash.SchemeMintV1:
		p.Scheme, p.ValidKnown = "mint_v1", true
		if minter, ok := nipcash.VerifyProvenance(tok); ok {
			p.Verdict, p.Minter = "valid", minter
		} else {
			p.Verdict = "invalid"
		}
	case nipcash.SchemeZapV1:
		p.Scheme, p.Verdict = "zap_v1", "needs_zap_event"
	case nipcash.SchemeNone:
		p.Scheme, p.Verdict = "none", "none"
	default:
		p.Scheme, p.Verdict, p.ValidKnown = "malformed", "invalid", true
	}
	if p.Scheme == "mint_v1" || p.Scheme == "zap_v1" {
		p.RelayHash, _ = nipcash.RelayHash(tok.RelayURLs, tok.Secret)
	}
	return p
}

// provenanceJSON is decode --json's provenance fields: signature_scheme,
// provenance, relay_hash (schemes 1 and 2), mint_signature_valid (null when
// only the zap event can tell) and, when valid, minter_pubkey.
func provenanceJSON(tok nipcash.Token) map[string]any {
	p := provenanceOf(tok)
	out := map[string]any{"signature_scheme": p.Scheme, "provenance": p.Verdict}
	if p.RelayHash != "" {
		out["relay_hash"] = p.RelayHash
	}
	if p.Scheme == "none" {
		return out
	}
	if len(tok.MintSignature) > 0 {
		out["mint_signature"] = fmt.Sprintf("%x", tok.MintSignature)
	}
	if tok.AttestedAmountMillis != nil {
		out["attested_amount_millis"] = *tok.AttestedAmountMillis
	}
	if p.ValidKnown {
		out["mint_signature_valid"] = p.Verdict == "valid"
	} else {
		out["mint_signature_valid"] = nil
	}
	if p.Minter != "" {
		out["minter_pubkey"] = p.Minter
	}
	return out
}

// formatMinterStatus is the "minter: ..." line, and the "attested_amount: ..."
// line to print after it ("" when the bill carries no amount), shared by decode
// and receive's report.
//
// A valid signature names WHICH key signed this copy and proves the copy's
// relays and secret are the ones it signed — never that the key is a mint
// anyone should trust: anyone can sign a bill with a throwaway key.
func formatMinterStatus(tok nipcash.Token) (minterLine, amountLine string) {
	p := provenanceOf(tok)
	amount := func(suffix string) string {
		if tok.AttestedAmountMillis == nil {
			return ""
		}
		return fmt.Sprintf("attested_amount: %s%s", output.FormatAmount(int64(*tok.AttestedAmountMillis)), suffix)
	}
	switch p.Verdict {
	case "valid":
		return fmt.Sprintf("minter: %s (signature covers this copy's relays; a valid signature ≠ a trusted mint — anyone can self-sign)", p.Minter), amount("")
	case "needs_zap_event":
		return "minter: unknown until checked with its zap event (kind 9522); cashctl won't contact its relays", amount(" (unverified)")
	case "invalid":
		return "minter: INVALID SIGNATURE — this copy's relays or secret differ from what its minter signed; cashctl won't contact them", amount(" (unverified)")
	default:
		return "minter: unknown — no mint signature, so cashctl won't contact its relays", ""
	}
}

// printMintSignatureStatus is decode's own top-level, unindented rendering
// of formatMinterStatus's result.
func printMintSignatureStatus(tok nipcash.Token) {
	minterLine, amountLine := formatMinterStatus(tok)
	output.Println(minterLine)
	if amountLine != "" {
		output.Println(amountLine)
	}
}

// shouldRunCheck decides whether decode's --check network round trip
// actually runs. Under --json there's no terminal to prompt from, so it's
// flag-only: exactly checkFlag, same as before this existed. In text mode,
// an explicit --check/--check=false on the command line is honored as-is
// (no point re-asking a question the user already answered); otherwise
// decode asks interactively so a human doesn't need to already know the
// flag exists — default NO: decode's whole pitch (its own doc comment,
// above) is "no network call and nothing dialed by default," and a bare
// Enter or a closed/empty stdin (a script piping decode with neither
// --check nor --yes/--json) used to default to yes, silently going online
// anyway — for a token whose embedded relay URL fully controls where
// cashctl connects, that's a real fingerprint/deanonymization exposure
// the documented default-safe contract exists specifically to avoid. An
// explicit --yes still opts in (its own established "don't ask, I know
// what I'm doing" contract, same as every other confirmation), only the
// unattended-defaults case changed.
func shouldRunCheck(cmd *cobra.Command, jsonMode, checkFlag bool, prompt string) bool {
	// --yes is handled here rather than left to Confirm, which answers yes
	// to everything (cmd/prompt.go). This prompt is not a confirmation of
	// something already asked for — it is an opt-in to a network call the
	// caller did not request, deliberately defaulting to No. Letting --yes
	// flip it meant `decode <token> --yes` silently went online, against
	// AGENTS.md's "locally, no network call; --check opts into a read-only
	// Hub check". Skipping a default-No prompt takes the default; only
	// --check turns the call on.
	yesFlag, _ := cmd.Flags().GetBool("yes")
	if jsonMode || yesFlag || cmd.Flags().Changed("check") {
		return checkFlag
	}
	return Confirm(cmd, false, prompt)
}

// shouldCheckCashToken wraps shouldRunCheck with one more guard specific to
// cash tokens: a token requiring an identity proof (not cash-mode) can
// only ever match the Hub's list_recipients response against a local
// pubkey — if no local identity is configured at all, that match can never
// succeed, so asking "want to check?" would just be asking a question
// whose answer is already "it can't." Skipped silently under --json/--yes/
// an explicit --check, same as shouldRunCheck itself — this only changes
// the interactive-prompt path. isCash is the caller's own corrected
// determination (decodeCashToken's own isCash), not re-derived from the
// token here.
func shouldCheckCashToken(cmd *cobra.Command, jsonMode, checkFlag bool, isCash bool) bool {
	if !jsonMode && !cmd.Flags().Changed("check") {
		if !isCash {
			if exists, _ := identity.Exists(); !exists {
				output.Notef(false, "Skipping check — no local identity (run `cashctl init`).")
				return false
			}
		}
	}
	return shouldRunCheck(cmd, jsonMode, checkFlag, "Verify online?")
}

// cashCheckResult is decode --check's report for a cash token — the same
// verdict `receive`'s own mandatory Hub check produces, just never leading
// to anything being saved. ExpiresAt is the wallet's own Hub-side
// redemption deadline (identical on every recipient row, NIP-CASH
// §Listing Recipients) — this is the one live fact decode can surface
// about it that a purely local decode never can, since expiry isn't
// encoded in the token itself (see nipcash.Token's own fields).
type cashCheckResult struct {
	OK           bool    `json:"ok"`
	AmountMillis *uint64 `json:"amount_millis,omitempty"`
	ExpiresAt    *int64  `json:"expires_at,omitempty"`
	Error        string  `json:"error,omitempty"`
	// Spent is true only when the Hub answered with a tombstone, never on silence.
	Spent bool `json:"spent,omitempty"`
}

// errCheckedBillSpent is decode --check's word for a tombstone. The bill string
// being checked is often one this wallet already took: `receive` re-keys a cash
// note into a bill only its holder can spend, which spends the original, so "the
// bill is gone" read as lost money when it had only moved.
var errCheckedBillSpent = errors.New("the Hub says this bill is spent: its value has moved to another bill. " +
	"If you took it with `cashctl receive`, that bill is the re-keyed one you now hold (see `cashctl wallet show`)")

// hubGroupHex is a token's Cash Hub fingerprint, as `wallet show` prints it, or "".
func hubGroupHex(tok nipcash.Token) string {
	if len(tok.HubGroup) == 0 {
		return ""
	}
	return fmt.Sprintf("%x", tok.HubGroup)
}

func checkCashTokenAgainstHub(cmd *cobra.Command, jsonMode bool, value, embeddedCashSecret string, tok nipcash.Token) cashCheckResult {
	var amount uint64
	var expiresAt *int64
	var spent bool
	err := WithSpinner(jsonMode, "Checking...", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		client, err := dialBill(ctx, value)
		if err != nil {
			return err
		}
		defer client.Close()

		// CheckClaim tries pubkey then cash mode live; no need to pre-decide.
		cred, credErr := credentialForToken(cmd, embeddedCashSecret)
		if credErr != nil {
			return credErr
		}
		result, err := client.CheckClaim(ctx, cred, tok)
		var gone *nipcash.BillGone
		if errors.As(err, &gone) {
			spent = true
			return errCheckedBillSpent
		}
		if errors.Is(err, nipcash.ErrClaimNotFound) || (embeddedCashSecret != "" && isWrongSecretDecline(err)) {
			if isBareCashModeBill(tok, embeddedCashSecret) {
				return errNeedsCashSecret
			}
			if embeddedCashSecret != "" {
				// A cash note's Hub answers only to its current secret: this one was
				// re-keyed (protected) or spent by whoever took the note.
				return fmt.Errorf("this cash note no longer answers to that secret: whoever took it has re-keyed or spent it")
			}
			return fmt.Errorf("no matching recipient found on the Hub")
		}
		if err != nil {
			if explained, ok := explainNoAnswer(err); ok {
				return explained
			}
			if explained, ok := explainCashSecretRefusal(err); ok {
				return explained
			}
			return err
		}
		if !result.Claimable {
			return fmt.Errorf("this bill's slice is %s: it can no longer be spent", result.Reason)
		}
		amount = result.Row.AmountMillis
		expiresAt = expiryPtr(result.Row.ExpiresAt)
		return nil
	})
	if err != nil {
		// Sanitized: a raw Hub error, not cashctl's own text — this soft
		// check bypasses NWCError's own sanitizing.
		//
		// An expired bill still surfaces its own accurate deadline message,
		// because cash_status is gated by the same generic Hub-side
		// permission-expiry check as every other cash_wallet method (lokihub's
		// nip47/permissions.HasPermission, AppKindCashWallet branch) — but ONLY
		// for a caller who holds a slice of it. A caller who does not is omitted
		// during authorization, before permissions are ever consulted, so no
		// reason of any kind comes back. explainNoAnswer above is what that case
		// gets; it deliberately does not claim expiry, since the Hub did not say
		// so.
		return cashCheckResult{OK: false, Spent: spent, Error: output.Sanitize(err.Error())}
	}
	return cashCheckResult{OK: true, AmountMillis: &amount, ExpiresAt: expiresAt}
}

func printCashCheck(r cashCheckResult) {
	if !r.OK {
		output.Printf("check: %s\n", r.Error)
		return
	}
	output.Printf("check: matches (%s)\n", output.FormatAmount(int64(*r.AmountMillis)))
	if r.ExpiresAt != nil {
		output.Println(formatExpiry(*r.ExpiresAt))
	}
}

// formatExpiry renders a Hub-side expires_at timestamp for display —
// shared by decode --check's cash-token report and `cash status`
// (cash_inspect.go), the two read-only inspection commands that surface
// this live fact.
func formatExpiry(expiresAt int64) string {
	when := time.Unix(expiresAt, 0)
	remaining := time.Until(when)
	if remaining <= 0 {
		return fmt.Sprintf("expires: %s (already passed)", when.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("expires: %s (in %s)", when.UTC().Format(time.RFC3339), remaining.Round(time.Minute))
}

// circleCheckResult is decode --check's report for a circlehub1...
// connection: only ever "is joining possible here at all" (reachable, and
// the Hub advertises create_circle_wallet) — never a check of whether any
// particular identity is allowlisted, since nothing short of actually
// calling create_circle_wallet answers that. It ALSO can't see the Hub's
// own expiry: get_info is always-granted and bypasses the generic per-app
// expiry check entirely (confirmed live — an already-expired circle_hub's
// connection still reports OK here, even though `join` against the same
// connection is correctly rejected EXPIRED/auth; there's no read-only
// NIP-47 call that could see this ahead of time — see
// docs/private/audit-round2-expiration-matrix.md). Note carries that
// caveat into --json too, not just the text-mode rendering below, so a
// script/agent doesn't read OK:true as a stronger guarantee than it is.
type circleCheckResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Note  string `json:"note,omitempty"`
	// PrivateJoin is whether get_info names the node identity that NIP-CW
	// Private Join needs; nil when the check didn't get that far. Offered is
	// not verified: join still checks the Hub's announcement.
	PrivateJoin *bool `json:"private_join,omitempty"`
	// CirclePolicy is get_info's circle_policy as sent.
	CirclePolicy string `json:"circle_policy,omitempty"`
}

// circleJoinableCaveat is circleCheckResult's own Note/text-mode wording
// when OK — see the type's own doc comment for exactly what this can and
// can't prove.
const circleJoinableCaveat = "reachable and supports circle wallets — doesn't confirm the Hub hasn't expired or that you're allowlisted; an actual `join` can still fail"

func checkCircleHubJoinable(jsonMode bool, conn nipcw.CircleHubConnection) circleCheckResult {
	var cw *nip47.CircleWalletInfo
	err := WithSpinner(jsonMode, "Checking...", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		pairing := &nip47.PairingInfo{WalletPubkey: conn.WalletPubkey, RelayURLs: conn.RelayURLs, Secret: conn.Secret}
		client, err := relayclient.NewNWCClient(ctx, pairing, nip47.EncryptionNIP44V2)
		if err != nil {
			return err
		}
		defer client.Close()

		info, err := client.GetInfo(ctx)
		if err != nil {
			return err
		}
		for _, m := range info.Methods {
			if m == nipcw.MethodCreateCircleWallet {
				cw = info.CircleWallet
				return nil
			}
		}
		return fmt.Errorf("reachable, but this Hub doesn't support create_circle_wallet")
	})
	if err != nil {
		// Sanitized: same reasoning as checkCashTokenAgainstHub above.
		return circleCheckResult{OK: false, Error: output.Sanitize(err.Error())}
	}
	return circleJoinableResult(cw)
}

// circleJoinableResult is a passed check's report, with what get_info's
// circle_wallet says about private join and membership.
func circleJoinableResult(cw *nip47.CircleWalletInfo) circleCheckResult {
	offered := cw != nil && cw.Identity != ""
	r := circleCheckResult{OK: true, Note: circleJoinableCaveat, PrivateJoin: &offered}
	if cw != nil {
		r.CirclePolicy = output.Sanitize(cw.CirclePolicy)
	}
	return r
}

func printCircleCheck(r circleCheckResult) {
	if !r.OK {
		output.Printf("check: %s\n", r.Error)
		return
	}
	output.Printf("check: appears joinable (%s)\n", r.Note)
	if r.PrivateJoin != nil {
		output.Printf("private join: %s\n", offeredText(*r.PrivateJoin))
	}
	if r.CirclePolicy != "" {
		output.Printf("circle policy: %s\n", circlePolicyPhrase(r.CirclePolicy))
	}
}

// offeredText is how decode and get-info say whether a Hub offers private join.
func offeredText(offered bool) string {
	if offered {
		return "offered"
	}
	return "not offered"
}

// circlePolicyPhrase names get_info's circle_policy in words; an unknown
// value is shown as sent.
func circlePolicyPhrase(policy string) string {
	switch policy {
	case nip47.CirclePolicyFollowSet:
		return "follow set"
	case nip47.CirclePolicyAllowlist:
		return "allowlist"
	default:
		return output.Sanitize(policy)
	}
}

// cashHubKeepPrivate is said about every cashhub1 string: it carries a secret
// that mints bills, and one ever published must be revoked (NIP-CASH §Hub Rules).
const cashHubKeepPrivate = "this string can mint bills: keep it private"

// decodeCashHub shows a cashhub1 string's public fields and whether its
// signature confirms the Hub it names; never its secret. --check runs the
// online check (get_info's identity and chain against TLV 4 and 6); a failed
// check is a result, not a refusal.
func decodeCashHub(cmd *cobra.Command, value string, jsonMode, check bool) error {
	c, err := readConnection(cmd, value, "cashhub")
	if err != nil {
		return err
	}
	h := c.CashHub
	warnings := []string{cashHubKeepPrivate}
	if !c.Verified {
		warnings = append(warnings, "Hub not confirmed: "+unverifiedReasonText(c))
	}
	out := map[string]any{
		"type":              "cashhub",
		"connection_pubkey": h.Pubkey,
		"relays":            h.Relays,
		"hub_identity":      h.HubIdentity,
		"chain":             h.Chain,
		"signature":         signatureLabel(c),
		"warnings":          warnings,
	}
	if h.HasLabel {
		out["label"] = h.Label
	}
	if !c.Verified {
		out["signature_reason"] = c.Reason
	}
	if !jsonMode {
		output.Println("type: cashhub")
		if h.HasLabel {
			output.Printf("label: %s (not confirmed)\n", output.Sanitize(h.Label))
		}
		output.Printf("claimed Hub: %s\n", shortFingerprint(h.HubIdentity))
		output.Printf("chain: %s\n", output.Sanitize(h.Chain))
		output.Printf("relays: %s\n", joinStrings(h.Relays))
		if c.Verified {
			output.Println("signature: verified (the string was issued by the Hub it names)")
		} else {
			output.Printf("signature: unverified — %s\n", unverifiedReasonText(c))
		}
		output.Notef(false, "Note: %s.", cashHubKeepPrivate)
	}
	if shouldRunCheck(cmd, jsonMode, check, "Check the Hub online?") {
		result := checkCashHubOnline(cmd, jsonMode, c)
		if jsonMode {
			for k, v := range result {
				out[k] = v
			}
		} else {
			printOnlineCheck(result)
		}
	}
	if jsonMode {
		output.PrintJSON(out)
	}
	return nil
}

// checkCashHubOnline runs the online check against a cashhub1's Hub.
func checkCashHubOnline(cmd *cobra.Command, jsonMode bool, c *conn) map[string]any {
	out := map[string]any{}
	_ = WithSpinner(jsonMode, "Checking the Hub...", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		hub, err := nipcashclient.ConnectCashHub(ctx, c.Raw)
		if err != nil {
			out["online_check"], out["error"] = "unreachable", output.Sanitize(err.Error())
			return nil
		}
		defer hub.Close()
		info, err := hub.Info(ctx)
		switch {
		case errors.Is(err, nipcash.ErrHubIdentityMismatch):
			out["online_check"] = "identity_mismatch"
		case errors.Is(err, nipcash.ErrChainMismatch):
			out["online_check"] = "chain_mismatch"
		case err != nil:
			out["online_check"], out["error"] = "unreachable", output.Sanitize(err.Error())
		default:
			out["online_check"] = "pass"
			out["network"], out["chain"] = info.Network, info.Chain
			out["test_network"] = info.TestNetwork()
		}
		return nil
	})
	return out
}

// printOnlineCheck is checkCashHubOnline's result in text.
func printOnlineCheck(r map[string]any) {
	switch r["online_check"] {
	case "pass":
		output.Printf("online check: pass (the Hub answers as the identity and chain the string names)\n")
		output.Printf("network: %s\n", output.Sanitize(fmt.Sprint(r["network"])))
		if r["test_network"] == true {
			output.Println("This Hub runs on a test network: its bills are not real money.")
		}
	case "identity_mismatch":
		output.Println("online check: FAILED — the Hub answers as another identity than the string names")
	case "chain_mismatch":
		output.Println("online check: FAILED — the Hub names another chain than the string, or none")
	default:
		output.Printf("online check: couldn't reach the Hub (%v)\n", r["error"])
	}
}
