package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
// wasn't passed explicitly, decode asks interactively instead (default
// yes) — see shouldRunCheck. --json never prompts (an agent has no
// terminal to answer from): it only checks when --check is passed, exactly
// as before. A cash token needing an identity proof (not bearer-mode) is
// never prompted for at all when no local identity is configured — the
// check would have nothing to match against — decode just says so instead
// of asking a question it already knows the answer to.
func newDecodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "decode <string>",
		Short: "Inspect any cash token, Circle Hub connection, or NWC URI locally",
		Long:  `Inspects a cash token, Circle Hub connection, or NWC URI locally. --check adds a network round trip.`,
		Example: `  cashctl decode lokicash1...
  cashctl decode lokicash1... --check
  cashctl decode circlehub1...`,
		Args: output.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonMode, _ := cmd.Flags().GetBool("json")
			check, _ := cmd.Flags().GetBool("check")
			// Split before sniffing: NIP-CASH's "<token>#<bearer_secret>"
			// combined presentation (§Presenting a Bearer Slice as One
			// String) wouldn't decode as bech32 at all otherwise. A string
			// with no "#" is unaffected.
			// Trimmed before splitting: dial.Sniff trims internally for its
			// own classification, but a stray leading/trailing space or
			// newline (routine after a terminal copy-paste) would otherwise
			// still reach the actual decoder below, which rejects it
			// outright as invalid bech32 — confusing given Sniff itself
			// would've happily recognized it.
			value, embeddedSecret := nipcash.SplitBearerSliceString(strings.TrimSpace(args[0]))

			switch dial.Sniff(value) {
			case dial.KindCashToken:
				return decodeCashToken(cmd, value, jsonMode, check, embeddedSecret != "")
			case dial.KindCircleHub:
				return decodeCircleHub(cmd, value, jsonMode, check)
			case dial.KindNWCURI:
				return decodeNWCURI(cmd, value, jsonMode)
			case dial.KindCashHub:
				return output.InvalidInputError(cmd, value, fmt.Errorf(
					"that's a Cash Hub connection (for minting cash) — cashctl has no mint capability and can't decode this format"))
			default:
				return output.InvalidInputError(cmd, value, fmt.Errorf(
					"not a recognized cash token, Circle Hub connection, or NWC URI"))
			}
		},
	}
	cmd.Flags().Bool("check", false, "also cross-check against the Hub (cash token: matching recipient; circlehub: can we join) — a network call")
	return cmd
}

func decodeCashToken(cmd *cobra.Command, value string, jsonMode, check, hasEmbeddedBearerSecret bool) error {
	tok, err := nipcash.Decode(value)
	if err != nil {
		return output.InvalidInputError(cmd, value, err)
	}
	// See cash_receive.go: tok.IdentityRequired can go stale, an embedded
	// bearer_secret overrides it.
	isBearer := (tok.IdentityRequired != nil && !*tok.IdentityRequired) || hasEmbeddedBearerSecret

	if !jsonMode {
		fmt.Println("type: cash_token")
		fmt.Printf("wallet_pubkey: %s\n", tok.WalletPubkey)
		fmt.Printf("relays: %s\n", joinStrings(tok.RelayURLs))
		if tok.IdentityRequired != nil {
			fmt.Printf("identity_required: %v\n", *tok.IdentityRequired)
		}
		printMintSignatureStatus(tok)
		if hasEmbeddedBearerSecret {
			fmt.Println("bearer_secret: embedded — `cashctl receive` uses it automatically")
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
	// Presence only, never the value itself — decode never echoes a
	// working spending credential (this file's own doc comment).
	if hasEmbeddedBearerSecret {
		out["embedded_bearer_secret_present"] = true
	}
	if tok.HasProvenance() {
		out["mint_signature"] = fmt.Sprintf("%x", tok.MintSignature)
		out["attested_amount_millis"] = *tok.AttestedAmountMillis
		minter, valid := nipcash.VerifyProvenance(tok)
		out["mint_signature_valid"] = valid
		if valid {
			out["minter_pubkey"] = minter
		}
	}

	if shouldCheckCashToken(cmd, jsonMode, check, isBearer) {
		result := checkCashTokenAgainstHub(cmd, jsonMode, value, tok)
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
		fmt.Println("type: circlehub")
		fmt.Printf("wallet_pubkey: %s\n", conn.WalletPubkey)
		fmt.Printf("relays: %s\n", joinStrings(conn.RelayURLs))
		if conn.Label != "" {
			// Sanitized: an attacker-controlled Circle Hub connection's own
			// label has no character restrictions at decode time (see
			// nipcw.DecodeCircleHubConnection) — this is exactly the
			// inspect-before-you-trust output `decode` exists for.
			fmt.Printf("label: %s\n", output.Sanitize(conn.Label))
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
	fmt.Println("type: nwc_uri")
	fmt.Printf("wallet_pubkey: %s\n", pairing.WalletPubkey)
	fmt.Printf("relays: %s\n", joinStrings(pairing.RelayURLs))
	return nil
}

// formatMinterStatus verifies (not just checks presence of) tok's
// mint-provenance, so a forged/corrupt signature is never reported as
// trustworthy just because the fields are present. Returns the "minter: ..."
// line and, whenever there's a signature to speak of at all (valid or
// not), the "attested_amount: ..." line to print right after it — ""
// when there's no provenance, so the caller can skip that line entirely.
// Shared by decode's own printMintSignatureStatus and cash_receive.go's
// printCashBill, which need the exact same 3-state check.
func formatMinterStatus(tok nipcash.Token) (minterLine, amountLine string) {
	if !tok.HasProvenance() {
		return "minter: unknown", ""
	}
	minter, valid := nipcash.VerifyProvenance(tok)
	if !valid {
		return "minter: INVALID SIGNATURE — don't trust attested_amount",
			fmt.Sprintf("attested_amount: %s (unverified)", output.FormatAmount(int64(*tok.AttestedAmountMillis)))
	}
	return fmt.Sprintf("minter: %s", minter),
		fmt.Sprintf("attested_amount: %s", output.FormatAmount(int64(*tok.AttestedAmountMillis)))
}

// printMintSignatureStatus is decode's own top-level, unindented rendering
// of formatMinterStatus's result.
func printMintSignatureStatus(tok nipcash.Token) {
	minterLine, amountLine := formatMinterStatus(tok)
	fmt.Println(minterLine)
	if amountLine != "" {
		fmt.Println(amountLine)
	}
}

// shouldRunCheck decides whether decode's --check network round trip
// actually runs. Under --json there's no terminal to prompt from, so it's
// flag-only: exactly checkFlag, same as before this existed. In text mode,
// an explicit --check/--check=false on the command line is honored as-is
// (no point re-asking a question the user already answered); otherwise
// decode asks interactively, default yes, so a human doesn't need to
// already know the flag exists.
func shouldRunCheck(cmd *cobra.Command, jsonMode, checkFlag bool, prompt string) bool {
	if jsonMode || cmd.Flags().Changed("check") {
		return checkFlag
	}
	return Confirm(cmd, true, prompt)
}

// shouldCheckCashToken wraps shouldRunCheck with one more guard specific to
// cash tokens: a token requiring an identity proof (not bearer-mode) can
// only ever match the Hub's list_recipients response against a local
// pubkey — if no local identity is configured at all, that match can never
// succeed, so asking "want to check?" would just be asking a question
// whose answer is already "it can't." Skipped silently under --json/--yes/
// an explicit --check, same as shouldRunCheck itself — this only changes
// the interactive-prompt path. isBearer is the caller's own corrected
// determination (decodeCashToken's own isBearer), not re-derived from the
// token here.
func shouldCheckCashToken(cmd *cobra.Command, jsonMode, checkFlag bool, isBearer bool) bool {
	if !jsonMode && !cmd.Flags().Changed("check") {
		if !isBearer {
			if exists, _ := identity.Exists(); !exists {
				fmt.Println("Skipping check — no local identity (run `cashctl init`).")
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
}

func checkCashTokenAgainstHub(cmd *cobra.Command, jsonMode bool, value string, tok nipcash.Token) cashCheckResult {
	var amount uint64
	var expiresAt *int64
	err := WithSpinner(jsonMode, "Checking...", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		client, err := nipcashclient.Connect(ctx, value)
		if err != nil {
			return err
		}
		defer client.Close()

		// CheckClaim tries pubkey then bearer live; no need to pre-decide.
		myPubHex, _ := localPubKeyHex(cmd)
		result, err := client.CheckClaim(ctx, tok, myPubHex)
		if errors.Is(err, nipcash.ErrClaimNotFound) {
			return fmt.Errorf("no matching recipient found on the Hub")
		}
		if err != nil {
			return err
		}
		amount = result.AmountMillis
		expiresAt = result.ExpiresAt
		return nil
	})
	if err != nil {
		// Sanitized: a raw Hub error, not cashctl's own text — this soft
		// check bypasses NWCError's own sanitizing. Note this already
		// covers the token's own wallet having expired: CheckClaim's
		// underlying list_recipients call is gated by the exact same
		// generic Hub-side permission-expiry check as every other
		// cash_wallet method, so an expired token surfaces here as this
		// wallet's own accurate deadline message (lokihub's
		// nip47/permissions.HasPermission, AppKindCashWallet branch), not
		// a generic "no matching recipient."
		return cashCheckResult{OK: false, Error: output.Sanitize(err.Error())}
	}
	return cashCheckResult{OK: true, AmountMillis: &amount, ExpiresAt: expiresAt}
}

func printCashCheck(r cashCheckResult) {
	if !r.OK {
		fmt.Printf("check: %s\n", r.Error)
		return
	}
	fmt.Printf("check: matches (%s)\n", output.FormatAmount(int64(*r.AmountMillis)))
	if r.ExpiresAt != nil {
		fmt.Println(formatExpiry(*r.ExpiresAt))
	}
}

// formatExpiry renders a Hub-side expires_at timestamp for display —
// shared by decode --check's cash-token report and `cash list-recipients`
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
// calling create_circle_wallet answers that.
type circleCheckResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func checkCircleHubJoinable(jsonMode bool, conn nipcw.CircleHubConnection) circleCheckResult {
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
				return nil
			}
		}
		return fmt.Errorf("reachable, but this Hub doesn't support create_circle_wallet")
	})
	if err != nil {
		// Sanitized: same reasoning as checkCashTokenAgainstHub above.
		return circleCheckResult{OK: false, Error: output.Sanitize(err.Error())}
	}
	return circleCheckResult{OK: true}
}

func printCircleCheck(r circleCheckResult) {
	if r.OK {
		fmt.Println("check: joinable")
		return
	}
	fmt.Printf("check: %s\n", r.Error)
}
