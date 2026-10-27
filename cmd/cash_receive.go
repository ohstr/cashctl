package cmd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/dial"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newCashReceiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "receive <token>",
		Short: `"Cash-in" a token: verify and add it to your wallet`,
		Long:  `Verifies a cash token against the Hub before saving it. A cash-mode token needs its secret embedded as "<token>#<secret>".`,
		Example: `  cashctl receive lokicash1...
  cashctl receive lokicash1...#deadbeef
  cashctl receive - < bill.txt    # read it from stdin, keeping the secret out of ps and shell history`,
		Args: output.ExactArgs(1),
		RunE: runCashReceive,
	}
	cmd.Flags().String("zap-event", "", "the Cash Zap (kind 9522) a zap bill came with: a file, - for stdin, or the JSON")
	cmd.Flags().Bool("find-zap", false, "find a zap bill's Cash Zap on --relay (never on the bill's own relay hints)")
	cmd.Flags().StringSlice("relay", nil, "a relay to look for the Cash Zap on (with --find-zap)")
	cmd.Flags().Bool("trust-minter", false, "check a zap whose minter you don't trust yet with its Hub (its network can't be verified)")
	return cmd
}

func runCashReceive(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	raw, err := argOrStdin(cmd, args[0])
	if err != nil {
		return err
	}
	// A Cash Zap given as the input, or a zap bill with its event.
	if looksLikeZapEvent(raw) {
		ev, evRaw, err := readZapEvent(cmd, raw)
		if err != nil {
			return err
		}
		return receiveZap(cmd, ev, evRaw)
	}
	if zapFlag, _ := cmd.Flags().GetString("zap-event"); zapFlag != "" {
		ev, evRaw, err := readZapEvent(cmd, zapFlag)
		if err != nil {
			return err
		}
		return receiveZap(cmd, ev, evRaw)
	}
	if find, _ := cmd.Flags().GetBool("find-zap"); find {
		tok, err := nipcash.Decode(strings.TrimSpace(raw))
		if err != nil || tok.Scheme != nipcash.SchemeZapV1 {
			return output.InvalidInputError(cmd, "", errors.New("--find-zap is for a zap bill (one signed for a Cash Zap)"))
		}
		ev, evRaw, err := findZapFor(cmd, tok)
		if err != nil {
			return err
		}
		return receiveZap(cmd, ev, evRaw)
	}
	// Trimmed: dial.Sniff trims for its own classification but the raw
	// string would otherwise still reach nipcash.Decode, which rejects a
	// stray leading/trailing space as invalid bech32.
	input, embeddedSecret := nipcash.SplitCashSliceString(strings.TrimSpace(raw))

	switch dial.Sniff(input) {
	case dial.KindCircleHub:
		return output.InvalidInputError(cmd, input, fmt.Errorf("that's a Circle Hub connection — use `cashctl join <connection>` to join it"))
	case dial.KindWitnessAddress:
		return output.InvalidInputError(cmd, input, fmt.Errorf("that's an on-chain address — pay a bill out to it with `cashctl cash withdraw quote <address>`"))
	case dial.KindCashOrder:
		return output.InvalidInputError(cmd, input, fmt.Errorf("that's an Order connection — buy cash over it with `cashctl cash order place <connection> <amount>`"))
	case dial.KindCashHub:
		return output.InvalidInputError(cmd, input, fmt.Errorf("that's a Cash Hub connection (for minting cash) — mint with `cashctl cash mint <amount> --hub -`"))
	case dial.KindNWCURI:
		return output.InvalidInputError(cmd, input, fmt.Errorf("that looks like a wallet connection, not a cash token — use `cashctl connect add <name> <uri>` instead"))
	case dial.KindNostrEntity:
		return output.InvalidInputError(cmd, input, errors.New(nostrEntityMessage(dial.NostrEntityHRP(input))))
	case dial.KindLightningInvoice:
		return output.InvalidInputError(cmd, input, errors.New(lightningMessage(input)))
	case dial.KindUnknown:
		return output.InvalidInputError(cmd, input, fmt.Errorf("doesn't look like a valid cash token"))
	}

	tok, err := nipcash.Decode(input)
	if err != nil {
		return output.InvalidInputError(cmd, input, err)
	}
	// The provenance gate first: a copy whose signature does not cover its own
	// relays is refused before any of them is named or dialled.
	if err := checkBillProvenance(tok); err != nil {
		if errors.Is(err, errNeedsZapEvent) {
			return output.InvalidInputError(cmd, input, errors.New(
				"this bill came from a Cash Zap and needs its Cash Zap event: pass --zap-event <file>, or --find-zap --relay <url>"))
		}
		return output.InvalidInputError(cmd, input, err)
	}
	// Before anything below dials the bill's relays.
	warnLocalRelays(jsonMode, tok.RelayURLs)

	// Local-only guess, used below just to decide whether to bail at Step
	// 0 and what to print pre-network. tok.IdentityRequired is a
	// best-effort hint that can go stale (NIP-CASH §Redemption Metadata)
	// — an embedded secret is checked too since it's unambiguous either
	// way. The save further down never trusts this guess: it uses
	// result.IsCash, CheckClaim's own live answer.
	isCash := (tok.IdentityRequired != nil && !*tok.IdentityRequired) || embeddedSecret != ""

	// Step 0: a cash-mode token with no embedded secret carries nothing this
	// command can act on — degrade to exactly decode --check's own
	// contract (read, optionally cross-check live, never save, never
	// propose securing) rather than erroring. This is an expected
	// outcome, not a usage mistake.
	if isCash && embeddedSecret == "" {
		// Before giving up on the strength of a local guess, ask the Hub.
		//
		// identity_required can go stale in the direction that STRANDS a bill. A
		// cash-mode bill reassigned to a pubkey identity by cash_transfer keeps its
		// token string — that is the documented in-place reassignment the ledger
		// check below relies on — so the token still says identity_required: false
		// while the Hub now requires identity. The guess then sends a bill its
		// rightful holder CAN claim down this dead end, which never saves anything,
		// and there is no other way in: every later operation needs a ledger entry.
		//
		// The live answer discriminates the two cases exactly, which the guess cannot:
		// a genuinely cash-mode bill has no credential to offer without its secret, so
		// the call is refused or omitted, while a reassigned one resolves against the
		// local identity.
		//
		// The cost is one bounded network attempt on a path that used to be able to
		// answer offline. Paid deliberately: the alternative is a bill that cannot be
		// received by anyone.
		if live, liveErr := checkClaimAsLocalIdentity(cmd, input, tok); liveErr == nil && !live.Row.IsCash() {
			isCash = false
			output.Notef(jsonMode, "This bill is identity-bound now, though its own metadata still says cash-mode — reassigned by a transfer. Continuing with your local identity.")
		}
	}

	if isCash && embeddedSecret == "" {
		printCashBill(jsonMode, tok, isCash)
		if shouldRunCheck(cmd, jsonMode, false, "Verify online?") {
			checkResult, checkErr := checkClaimOnce(cmd, input, embeddedSecret, tok)
			if !jsonMode {
				if checkErr == nil {
					output.Printf("check: matches (%s)\n", output.FormatAmount(int64(checkResult.Row.AmountMillis)))
				} else {
					explained, _ := explainCashSecretRefusal(checkErr)
					if errors.Is(checkErr, nipcash.ErrClaimNotFound) {
						explained = errNeedsCashSecret
					}
					output.Printf("check: %s\n", explained)
				}
			}
		}
		if jsonMode {
			output.PrintJSON(map[string]any{"received": false, "reason": "no_embedded_secret"})
			return nil
		}
		output.Println()
		output.Println(`No spending secret — ask for the full "token#secret" string, or run "cashctl decode".`)
		return nil
	}

	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	// Only a token still HELD blocks this — one that's since been
	// transferred/redeemed/consolidated away is allowed back in (ledger.Add
	// reactivates its existing row): a full-amount transfer reassigns a
	// wallet in place, so the very same token string legitimately returns
	// to a previous holder.
	if held, ok := l.FindByToken(input); ok && held.Status == ledger.StatusHeld {
		return output.ConflictError(cmd, input, ledger.ErrAlreadyHeld)
	}

	printCashBill(jsonMode, tok, isCash)

	result, err := checkClaimWithCashHub(cmd, input, embeddedSecret, tok)
	if err != nil {
		return err
	}
	if err := validateClaimedAmount(cmd, tok, result.Row.AmountMillis, minterPubkeyFromToken(tok)); err != nil {
		return err
	}

	entry := ledger.Entry{
		Token:        input,
		WalletPubkey: tok.WalletPubkey,
		Secret:       tok.Secret,
		CashSecret:   embeddedSecret,
		RelayURLs:    tok.RelayURLs,
		// result.IsCash, not tok.IdentityRequired or the guess above:
		// this drives every later operation on the entry, so it should
		// carry CheckClaim's live answer, not a pre-network guess.
		IdentityRequired: ptrTo(!result.Row.IsCash()),
		AmountMillis:     &result.Row.AmountMillis,
		Verified:         true,
		MinterPubkey:     minterPubkeyFromToken(tok),
		ExpiresAt:        expiryPtr(result.Row.ExpiresAt),
		RedeemFeeMillis:  &result.Row.RedeemFeeMillis,
	}
	if result.Row.IsCash() {
		// Shared, not yet protected: this is the secret exactly as it
		// arrived in the token/gift string, still spendable by anyone else
		// who was shown it too. protectCashReceipt (below, after this
		// entry is saved) flips it to CashProtected on a successful
		// re-key — see ledger.Entry.CashProtection's own doc comment.
		entry.CashProtection = ledger.CashShared
	}

	added, err := l.Add(entry)
	if err != nil {
		if errors.Is(err, ledger.ErrAlreadyHeld) {
			return output.ConflictError(cmd, input, err)
		}
		return output.RuntimeError(cmd, err)
	}
	l.AppendHistory("receive", fmt.Sprintf("received %s", output.FormatAmount(int64(result.Row.AmountMillis))))

	if err := l.Save(); err != nil {
		// A concurrent `cashctl receive` of this same token: both
		// processes' Add checks ran against their own pre-Save in-memory
		// snapshot and saw nothing, so only the DB's own token-uniqueness
		// constraint catches the loser here, as ledger.ErrAlreadyHeld
		// (see Save's own doc comment on this path) — same classification
		// and message as the ordinary already-held case above, not a raw
		// SQL error.
		if errors.Is(err, ledger.ErrAlreadyHeld) {
			return output.ConflictError(cmd, input, err)
		}
		return output.RuntimeError(cmd, err)
	}

	if !jsonMode {
		output.Printf("Received %s.\n", output.FormatAmount(int64(result.Row.AmountMillis)))
		// NIP-CASH §Removing a Recipient: an unclaimed slice can still be taken
		// back by its issuer; only redeeming or moving it makes it final.
		output.Println("Until you redeem or transfer it, the bill's issuer can still take it back.")
	}

	// result.IsCash, not the pre-check guess above.
	protected, finalEntry := protectCashReceipt(cmd, l, added, result.Row.IsCash())

	if jsonMode {
		// JSON key stays "secured" — an intentional, unchanged part of the
		// --json contract (only the human-mode wording/identifiers changed).
		output.PrintJSON(map[string]any{"entry": finalEntry, "secured": protected.json()})
	}
	return nil
}

// printCashBill prints a token's details before receive commits to
// anything — the human gets to see what they're about to accept, in the
// same moment cashctl is about to go check it's real. Never prints Secret
// or the cash secret: this is display, not a way to extract a working
// credential (see decode.go's own "pairing secret is never included"
// rule).
func printCashBill(jsonMode bool, tok nipcash.Token, isCash bool) {
	output.Notef(jsonMode, "Cash bill:")
	output.Notef(jsonMode, "  wallet_pubkey: %s", tok.WalletPubkey)
	if len(tok.RelayURLs) > 0 {
		output.Notef(jsonMode, "  relays: %s", joinStrings(tok.RelayURLs))
	}
	switch {
	case isCash:
		output.Notef(jsonMode, "  identity: cash-mode")
	case tok.IdentityRequired != nil:
		output.Notef(jsonMode, "  identity: requires proof")
	default:
		output.Notef(jsonMode, "  identity: unspecified")
	}
	minterLine, amountLine := formatMinterStatus(tok)
	output.Notef(jsonMode, "  %s", minterLine)
	if amountLine != "" {
		output.Notef(jsonMode, "  %s", amountLine)
	}
}

// minterPubkeyFromToken resolves the Entry.MinterPubkey to persist at
// receive time: nil for a token with no mint-provenance pair, or one whose
// signature doesn't recover cleanly — a signature VerifyProvenance
// confirms as self-consistent gets its recovered pubkey recorded, NOT a
// vetted "trustworthy minter": anyone can mint-sign their own token with a
// disposable key (see ledger.Entry.MinterPubkey's own doc comment for why
// this only ever means "same signer," never "a mint you'd trust," and why
// that's still useful for cash selection).
func minterPubkeyFromToken(tok nipcash.Token) *string {
	if !tok.HasProvenance() {
		return nil
	}
	minter, valid := nipcash.VerifyProvenance(tok)
	if !valid {
		return nil
	}
	return &minter
}

// checkClaimWithCashHub is receive's mandatory "is this bill real" step —
// dials the token's own Cash Hub connection and confirms a matching,
// unclaimed recipient actually exists there via nipcash/client's shared
// CheckClaim, animated with WithSpinner since it's a network round trip.
// Any failure here — the Cash Hub is unreachable, or reachable but has no
// recipient matching this token's identity — refuses the token outright:
// nothing gets saved, ever, on the strength of the token string alone.
// Always passes its own local pubkey (best-effort): CheckClaim tries
// both pubkey and cash mode live, so the caller doesn't need to guess.
// validateClaimedAmount is the second half of "refuses anything that
// doesn't check out" (this file's own package doc comment): checkClaimWithCashHub
// only proves a matching, unclaimed recipient exists — it never checks
// that the AMOUNT the Hub reports for it is sane, or (when the token
// carries one) consistent with its own signed provenance. Both gaps are
// exploitable by nothing more than an attacker-controlled relay (the
// token's own RelayURLs, dialed automatically): a fake Hub can make
// receive display and PERSIST an arbitrary "verified" balance, and an
// absurd reported amount (anything beyond what a real int64 mloki
// quantity can hold — every downstream consumer, from FormatAmount's own
// signature to ledger.Entry.AmountMillis's later int64 casts, eventually
// assumes one) becomes negative the moment such a cast touches it,
// exactly the value ParseAmount's own boundary already enforces on the
// input side (internal/output.ParseAmount) — this is that same boundary
// on the network-input side.
//
// tok.HasProvenance() being true only means a signature and an attested
// amount are BOTH present, not that the signature verified — minter is
// recovered from it (nipcash.VerifyProvenance), so a tampered signature paired with
// a forged attested amount can't pass.
func validateClaimedAmount(cmd *cobra.Command, tok nipcash.Token, amountMillis uint64, minter *string) error {
	if amountMillis > maxSaneAmountMillis {
		return output.InvalidInputError(cmd, "", fmt.Errorf(
			"the Hub reports an amount (%d mloki) too large to be real — refusing to trust it", amountMillis))
	}
	// The signature attests the whole bill, and the Hub reports only this
	// holder's slice of it, which on a multi-recipient bill is legitimately
	// smaller. Only more than the whole bill is impossible.
	if minter != nil && tok.AttestedAmountMillis != nil && amountMillis > *tok.AttestedAmountMillis {
		return output.InvalidInputError(cmd, "", fmt.Errorf(
			"the Hub reports %s for your share, but this bill's signed provenance attests only %s for the whole bill — refusing to trust it",
			output.FormatAmount(int64(amountMillis)), output.FormatAmount(int64(*tok.AttestedAmountMillis))))
	}
	return nil
}

// maxSaneAmountMillis is the ceiling every Hub-supplied amount is held to before it
// can reach the ledger or an invoice: what a real int64 mloki quantity can hold.
// Anything past it goes NEGATIVE at the first int64 cast — output.FormatAmount's own
// signature, ledger.Entry.AmountMillis's later casts, summarizeHeldTokens' running
// total — so one poisoned figure makes `wallet balance` report a negative total for
// the WHOLE wallet, not only the bill it arrived on.
const maxSaneAmountMillis = uint64(math.MaxInt64)

// validateQuotedAmounts bounds the three amounts a redeem quote takes from the Hub,
// before any of them is persisted, rendered, or turned into an invoice.
//
// A second validator rather than reusing validateClaimedAmount: both hold a figure
// for THIS holder's slice to the mint signature's whole-bill amount as an upper
// bound (a slice of a multi-recipient bill is legitimately smaller, never larger),
// but a quote carries a fee and a net amount as well.
//
// verifiedMinter is the caller's live proof that the provenance signature actually
// verified (ledger.Entry.MinterPubkey, or CheckClaimResult.MinterPubkey — both set
// only after nipcash.VerifyProvenance confirmed it), for the same reason
// validateClaimedAmount takes it: tok.HasProvenance() only means a signature and an
// attested amount are both PRESENT, so trusting it would let a tampered signature
// paired with a forged attested amount raise the ceiling at will.
func validateQuotedAmounts(cmd *cobra.Command, tok nipcash.Token, verifiedMinter *string, q redeemQuote) error {
	// Bound each figure first, so the sum below cannot itself overflow.
	for _, f := range []struct {
		name  string
		value uint64
	}{
		{"amount", q.AmountMillis},
		{"redeem fee", q.RedeemFeeMillis},
		{"net redeemable amount", q.NetRedeemableMillis},
	} {
		if f.value > maxSaneAmountMillis {
			return output.InvalidInputError(cmd, "", fmt.Errorf(
				"the Hub quotes a %s (%d mloki) too large to be real — refusing to trust it", f.name, f.value))
		}
	}

	// net == amount - fee is the protocol's own definition, a plain subtraction at
	// lokihub's single construction site (nip47/controllers/cash_status_controller.go,
	// which carries a //nolint:gosec asserting fee <= amount by construction). Worth
	// asserting rather than assuming, because redeemInvoiceAmount returns net and
	// lokihub's cash_redeem enforces an EXACT match against either the full amount or
	// amount-minus-fee: a quote where these three don't add up cannot produce an
	// invoice that matches either value, so such a redeem was going to fail anyway.
	// Failing here instead makes it a legible refusal rather than a puzzling
	// BAD_REQUEST after an invoice has already been minted on the destination wallet.
	if q.NetRedeemableMillis+q.RedeemFeeMillis != q.AmountMillis {
		return output.InvalidInputError(cmd, "", fmt.Errorf(
			"the Hub's quote doesn't add up: %d mloki net + %d mloki fee != %d mloki amount — refusing to trust it",
			q.NetRedeemableMillis, q.RedeemFeeMillis, q.AmountMillis))
	}

	if verifiedMinter != nil && tok.AttestedAmountMillis != nil && q.AmountMillis > *tok.AttestedAmountMillis {
		return output.InvalidInputError(cmd, "", fmt.Errorf(
			"the Hub quotes %s for this bill, but its own signed provenance attests the whole bill is only %s — refusing to trust it",
			output.FormatAmount(int64(q.AmountMillis)), output.FormatAmount(int64(*tok.AttestedAmountMillis))))
	}
	return nil
}

func checkClaimWithCashHub(cmd *cobra.Command, input, embeddedSecret string, tok nipcash.Token) (*nipcash.CheckClaimResult, error) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	var result *nipcash.CheckClaimResult
	noMatch := false
	err := WithSpinner(jsonMode, "Checking...", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		client, err := dialBill(ctx, input)
		if err != nil {
			return err
		}
		defer client.Close()

		cred, credErr := credentialForToken(cmd, embeddedSecret)
		if credErr != nil {
			return credErr
		}
		r, err := client.CheckClaim(ctx, cred, tok)
		var gone *nipcash.BillGone
		if errors.As(err, &gone) {
			return err
		}
		if errors.Is(err, nipcash.ErrClaimNotFound) || (embeddedSecret != "" && isWrongSecretDecline(err)) {
			noMatch = true
			if embeddedSecret != "" {
				// The Hub answers a cash-mode bill only to its own secret, so a miss
				// with one given means that secret is wrong (often truncated).
				return fmt.Errorf("the cash secret after '#' does not match this bill (is it complete?) — refusing to receive it")
			}
			return fmt.Errorf("the Cash Hub has no matching recipient for this token — refusing to receive it")
		}
		if err != nil {
			return err
		}
		if !r.Claimable {
			// Claimed already, ended by a withdrawal, or expired: the slice
			// exists, and can no longer be spent by anyone.
			noMatch = true
			return fmt.Errorf("this bill's slice is %s, so it can no longer be spent — refusing to receive it", r.Reason)
		}
		result = r
		return nil
	})
	if err == nil {
		return result, nil
	}
	if noMatch {
		return nil, output.InvalidInputError(cmd, input, err)
	}
	var gone *nipcash.BillGone
	if errors.As(err, &gone) {
		return nil, output.SpentError(cmd, input, errBillSpent)
	}
	// classifyCashTokenNWCErr, not a blanket NetworkError: a Hub that
	// ANSWERS with a decline (an expired token's EXPIRED above all) is not
	// a network failure, and reporting it as retryable told a script to
	// keep retrying a token whose deadline had already passed. Dial
	// failures and timeouts are still `network`.
	return nil, classifyCashTokenNWCErr(cmd, err)
}

// checkClaimOnce is Step 0's optional, non-fatal check for a cash-mode token
// with no embedded secret — same underlying CheckClaim call as
// checkClaimWithCashHub, but never classified into a hard CLIError:
// there's nothing to save regardless of what this reports, so the
// caller just prints whatever comes back (a match, a miss, or a dial
// failure) and moves on.
// checkClaimAsLocalIdentity asks the Hub whether this bill resolves against the
// LOCAL identity, which is the question identity_required's stale hint cannot answer.
//
// checkClaimOnce's sibling: the Hub matches the caller's row by the credential,
// so this one asks with the local identity's.
func checkClaimAsLocalIdentity(cmd *cobra.Command, input string, tok nipcash.Token) (*nipcash.CheckClaimResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := dialBill(ctx, input)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	cred, err := localCashCredential(cmd)
	if err != nil {
		return nil, err
	}
	return client.CheckClaim(ctx, cred, tok)
}

func checkClaimOnce(cmd *cobra.Command, input, embeddedSecret string, tok nipcash.Token) (*nipcash.CheckClaimResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := dialBill(ctx, input)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	cred, err := credentialForToken(cmd, embeddedSecret)
	if err != nil {
		return nil, err
	}
	return client.CheckClaim(ctx, cred, tok)
}

// warnLocalRelays says so when a bill's relay hints point at this machine or
// its local network. receive dials them as soon as the bill is pasted, and a
// bill from someone else has no reason to name such a relay: it can be a way
// to make cashctl connect to a local service. A warning, not a refusal, since
// a local test Hub legitimately lives there.
func warnLocalRelays(jsonMode bool, relays []string) {
	for _, r := range relays {
		u, err := url.Parse(r)
		if err != nil {
			continue
		}
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
			(ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())) {
			output.Notef(jsonMode, "Note: this bill names a relay on your own machine or network (%s); a bill from someone else has no reason to.", output.Sanitize(u.Host))
		}
	}
}
