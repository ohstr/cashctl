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
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newCashRedeemCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "redeem [wallet]",
		Short: "Redeem a held cash token into a Lightning wallet",
		Long: `cash_redeem always needs a destination invoice on the wire — cashctl
generates one for you: --token picks the source (auto-picked when you only
hold one), the destination wallet (positional, or --into) defaults to your
default wallet. --invoice bypasses both, redeeming straight into an invoice
from any other wallet app you already have — no cashctl-registered wallet
needed.`,
		Args: output.MaximumNArgs(1),
		RunE: runCashRedeem,
	}
	cmd.Flags().String("token", "", "which held token to redeem (auto-picked if you only hold one)")
	cmd.Flags().String("into", "", "which wallet to redeem into (defaults to your default wallet); same as the positional argument, kept for scripted/agentic use")
	cmd.Flags().String("invoice", "", "redeem straight into this external invoice")
	cmd.Flags().String("as", "", "override credential (pubkey:<priv> | connection-key:... | bearer:<secret>)")
	return cmd
}

func runCashRedeem(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	explicitInvoice, _ := cmd.Flags().GetString("invoice")
	intoFlagValue, _ := cmd.Flags().GetString("into")

	var positionalInto string
	if len(args) > 0 {
		positionalInto = args[0]
	}
	intoValue, err := resolvePositionalOrFlag(cmd, positionalInto, "into", intoFlagValue)
	if err != nil {
		return err
	}

	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	entry, err := resolveHeldToken(cmd, l)
	if err != nil {
		return err
	}
	cred, err := resolveCredential(cmd, entry)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sourceClient, err := nipcashclient.Connect(ctx, entry.Token)
	if err != nil {
		return output.NetworkError(cmd, err)
	}
	defer sourceClient.Close()

	var invoice string
	var destName string
	var message string
	if explicitInvoice != "" {
		invoice = explicitInvoice
		destName = "the invoice above"
		message = fmt.Sprintf("This will redeem this cash token into %s.", destName)
		if entry.AmountMillis != nil {
			message = fmt.Sprintf("This will redeem %d %s into %s.", *entry.AmountMillis, output.CurrencyUnit, destName)
		}
	} else {
		// Always a live CheckClaim, in every mode including --json/--yes:
		// the fee quote isn't confirmation-prompt decoration anymore, it
		// decides the destination invoice's amount below (see
		// redeemInvoiceAmount) — "skip it, nothing prints the preview" is
		// no longer a valid reason to bypass this.
		quote, err := resolveRedeemQuote(cmd, l, entry, sourceClient)
		if err != nil {
			return err
		}
		destWalletName, destValue, err := resolveDestWallet(cmd, intoValue)
		if err != nil {
			return err
		}
		destClient, err := DialGeneric(ctx, destValue)
		if err != nil {
			return output.NetworkError(cmd, err)
		}
		defer destClient.Close()
		tx, err := destClient.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(redeemInvoiceAmount(quote))})
		if err != nil {
			return classifyNWCErr(cmd, err)
		}
		invoice = tx.Invoice
		destName = destWalletName
		message = fmt.Sprintf("This will redeem %d %s into %s.", quote.AmountMillis, output.CurrencyUnit, destName)
		message += previewSuffix(quote)
	}
	message += " Continue?"
	// defaultYes=false: moves real money — never accept on a bare Enter.
	// --yes/--json skip this entirely, unaffected.
	if !Confirm(cmd, false, message) {
		fmt.Println("Cancelled.")
		return nil
	}

	result, err := sourceClient.CashRedeem(ctx, nipcash.CashRedeemParams{Invoice: invoice, Credential: cred})
	if err != nil {
		return classifyNWCErr(cmd, err)
	}

	_ = l.SetStatus(entry.ID, ledger.StatusRedeemed)
	if entry.AmountMillis != nil {
		l.AppendHistory("redeem", fmt.Sprintf("redeemed %d %s into %s", *entry.AmountMillis, output.CurrencyUnit, destName))
	} else {
		l.AppendHistory("redeem", fmt.Sprintf("redeemed into %s", destName))
	}
	_ = l.Save()

	if jsonMode {
		output.PrintJSON(map[string]any{
			"redeemed_token": entry.ID, "to_wallet": destName,
			"fee_mloki": result.FeesPaid, "preimage": result.Preimage,
		})
		return nil
	}
	fmt.Printf("Redeemed → %s.\n", destName)
	if result.FeesPaid > 0 {
		fmt.Printf("Fee: %d %s.\n", result.FeesPaid, output.CurrencyUnit)
	}
	return nil
}

// resolveHeldToken picks which held token to act on: --token by ID, or
// auto-picked when exactly one is held — see cashctl-plan.md's "auto-picked
// when only one held token qualifies" rule. Every path re-resolves the
// chosen ID through l.Find before returning, rather than handing back a
// pointer into l.Held()/pickHeldToken's own slice — both of those return
// *copies* (Ledger.Held's own doc comment), so a pointer into them looks
// like a live *ledger.Entry but silently doesn't write through to
// l.Entries at all. resolveAmount relies on this: it discovers an
// uncached amount live and writes it straight onto the *ledger.Entry it
// was given (entry.AmountMillis = &result.AmountMillis) rather than going
// through a setter — before this fix, that write landed on a throwaway
// copy for the auto-pick/interactive-pick paths (silently lost, persisted
// as NULL forever) while appearing to work, since the accompanying
// l.SetVerified(entry.ID, true) call right next to it writes through by ID
// regardless. Found auditing internal/ledger's own copy-vs-pointer
// contract; see docs/private/audit-round2-race-adversarial.md's "Found but
// out of scope" section and docs/private/audit-round3-redeem-fee-boundaries.md.
func resolveHeldToken(cmd *cobra.Command, l *ledger.Ledger) (*ledger.Entry, error) {
	if id, _ := cmd.Flags().GetString("token"); id != "" {
		e, ok := l.Find(id)
		if !ok {
			return nil, output.NotFoundError(cmd, id, fmt.Errorf("no held token %q", id))
		}
		return e, nil
	}
	held := l.Held()
	if len(held) == 0 {
		return nil, output.NotFoundError(cmd, "", fmt.Errorf("you have no held cash tokens — receive one first with `cashctl receive <token>`"))
	}
	var id string
	if len(held) == 1 {
		id = held[0].ID
	} else {
		picked, err := pickHeldToken(cmd, held)
		if err != nil {
			return nil, err
		}
		id = picked.ID
	}
	// Re-resolve by ID against l itself (not the held/pickHeldToken copy)
	// so the returned pointer is the live entry — see this func's own doc
	// comment above.
	e, ok := l.Find(id)
	if !ok {
		return nil, output.NotFoundError(cmd, id, fmt.Errorf("no held token %q", id))
	}
	return e, nil
}

// pickHeldToken disambiguates which held token to act on when more than
// one qualifies and --token wasn't given: interactively, with the same
// numbered-list-and-prompt style setUpIdentity already uses for multiple
// ncli vault identities (cmd/wallet_init.go's own setUpIdentity) — falls
// back to today's hard error under --json/--yes, where there's no
// terminal to prompt from and no point asking a question a script can't
// answer.
func pickHeldToken(cmd *cobra.Command, held []ledger.Entry) (*ledger.Entry, error) {
	tooManyErr := func() error {
		return output.UsageError(cmd, fmt.Errorf("you hold %d cash tokens — specify which with --token <id> (see `cashctl wallet show`)", len(held)))
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	yesFlag, _ := cmd.Flags().GetBool("yes")
	if jsonMode || yesFlag {
		return nil, tooManyErr()
	}

	output.Linef(false, "You hold %d cash tokens:", len(held))
	for i, e := range held {
		amount := "unknown amount"
		if e.AmountMillis != nil {
			amount = fmt.Sprintf("%d %s", *e.AmountMillis, output.CurrencyUnit)
		}
		output.Linef(false, "  %d) %s   received %s", i+1, amount, formatReceivedDate(e.ReceivedAt))
	}
	choice, err := PromptLine(fmt.Sprintf("Which one? [1-%d] ", len(held)))
	if err != nil {
		return nil, output.RuntimeError(cmd, err)
	}
	idx := parseChoice(choice, len(held))
	if idx < 0 {
		return nil, tooManyErr()
	}
	return &held[idx], nil
}

// resolveCredential resolves the credential to redeem/transfer with:
// --as overrides everything; a bearer-mode token uses its stored
// BearerSecret — `cashctl receive` refuses to save a bearer-mode entry
// without one (see ledger.Entry.BearerSecret's own doc comment), so any
// entry reaching this point is guaranteed to have it; a connection-key-
// bound token currently requires an explicit --as (re-deriving a fresh
// live attestation automatically is a known gap — see ledger.Entry's own
// doc comment on why only a *reference* is stored); everything else
// defaults to the local identity.
func resolveCredential(cmd *cobra.Command, entry *ledger.Entry) (nipcash.Credential, error) {
	if as, _ := cmd.Flags().GetString("as"); as != "" {
		cred, err := credential.ParseCash(as)
		if err != nil {
			return nil, output.InvalidInputError(cmd, output.RedactSecretInput(as), err)
		}
		return cred, nil
	}
	if entry.IdentityRequired != nil && !*entry.IdentityRequired {
		return nipcash.BySecret(entry.BearerSecret), nil
	}
	if entry.ConnectionKeyPlatform != "" {
		return nil, output.UsageError(cmd, fmt.Errorf(
			"this token is connection-key-bound — pass --as connection-key:<privkey>,%s,%s,<attestation-file>",
			entry.ConnectionKeyPlatform, entry.ConnectionKeyExternalID))
	}
	cred, err := localCashCredential(cmd)
	if err != nil {
		return nil, output.RuntimeError(cmd, err)
	}
	return cred, nil
}

// resolveDestWallet resolves into (the merged positional-argument-or---into
// value — by store name, or a raw value), or the default wallet.
func resolveDestWallet(cmd *cobra.Command, into string) (name, value string, err error) {
	s, loadErr := config.Load()
	if loadErr != nil {
		return "", "", output.RuntimeError(cmd, loadErr)
	}
	if into != "" {
		if c, ok := s.Find(into); ok {
			return c.Name, c.Value, nil
		}
		return into, into, nil
	}
	c, ok := s.DefaultConnection()
	if !ok {
		return "", "", output.NotFoundError(cmd, "", fmt.Errorf(noWalletConfiguredMsg+"\n  Or redeem straight into an invoice from any other wallet app: cashctl redeem --invoice <bolt11>"))
	}
	return c.Name, c.Value, nil
}

// resolveAmount returns entry's known amount, or discovers it live via
// CheckClaim if it isn't cached yet — e.g. a cash_transfer split
// remainder, which isn't always known up front (see cash_transfer.go's own
// Add call). Every entry `cashctl receive` itself saves already has its
// amount cached, since receive's own mandatory Cash Hub check fills it in.
func resolveAmount(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, sourceClient *nipcashclient.Client) (uint64, error) {
	if entry.AmountMillis != nil {
		return *entry.AmountMillis, nil
	}
	tok, err := nipcash.Decode(entry.Token)
	if err != nil {
		return 0, output.RuntimeError(cmd, err)
	}
	// CheckClaim tries pubkey then bearer live; no need to pre-decide.
	myPubHex, _ := localPubKeyHex(cmd)
	result, err := sourceClient.CheckClaim(context.Background(), tok, myPubHex)
	if errors.Is(err, nipcash.ErrClaimNotFound) {
		return 0, output.NotFoundError(cmd, entry.ID, fmt.Errorf("couldn't determine this token's amount — check `cashctl wallet show`, or that it's still valid"))
	}
	if err != nil {
		return 0, classifyNWCErr(cmd, err)
	}
	entry.AmountMillis = &result.AmountMillis
	_ = l.SetVerified(entry.ID, true)
	return result.AmountMillis, nil
}

// redeemQuote is a live snapshot of everything redeem needs once it's
// decided to auto-generate a destination invoice: the token's own full
// face amount, plus its current redeem-fee quote — used both to pick the
// invoice amount (redeemInvoiceAmount) and to render the confirmation
// prompt (previewSuffix). RedeemFeeMillis/NetRedeemableMillis is a
// worst-case figure (NIP-CASH §The Redeem Fee — same-node redeems often
// pay out more, up to the full amount, never less than
// NetRedeemableMillis).
type redeemQuote struct {
	AmountMillis        uint64
	RedeemFeeMillis     uint64
	NetRedeemableMillis uint64
	ExpiresAt           *int64
}

// resolveRedeemQuote is redeem's one live CheckClaim call for the
// auto-invoice path — always made, never best-effort/skippable the way the
// old confirmation-only preview was: redeemInvoiceAmount below depends on
// RedeemFeeMillis/NetRedeemableMillis to pick a wire-correct amount, not
// merely to print something, so a failure here has to abort redeem rather
// than silently fall back to a possibly-wrong amount. Same failure
// contract resolveAmount has always had for an entry that didn't already
// have its amount cached — this just always pays that same cost, since the
// fee can change server-side at any time up to redeem, cached amount or
// not.
func resolveRedeemQuote(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, sourceClient *nipcashclient.Client) (redeemQuote, error) {
	tok, err := nipcash.Decode(entry.Token)
	if err != nil {
		return redeemQuote{}, output.RuntimeError(cmd, err)
	}
	myPubHex, _ := localPubKeyHex(cmd)
	result, err := sourceClient.CheckClaim(context.Background(), tok, myPubHex)
	if errors.Is(err, nipcash.ErrClaimNotFound) {
		return redeemQuote{}, output.NotFoundError(cmd, entry.ID, fmt.Errorf("couldn't determine this token's amount — check `cashctl wallet show`, or that it's still valid"))
	}
	if err != nil {
		return redeemQuote{}, classifyNWCErr(cmd, err)
	}
	entry.AmountMillis = &result.AmountMillis
	_ = l.SetVerified(entry.ID, true)
	return redeemQuote{
		AmountMillis:        result.AmountMillis,
		RedeemFeeMillis:     result.RedeemFeeMillis,
		NetRedeemableMillis: result.NetRedeemableMillis,
		ExpiresAt:           result.ExpiresAt,
	}, nil
}

// redeemInvoiceAmount is the amount to request from the destination
// wallet's make_invoice call. lokihub's cash_redeem enforces an EXACT
// match against this, no leeway either direction
// (cash_redeem_controller.go step 9) — between the resolved invoice amount
// and one of exactly two values only the Hub can tell apart: the slice's
// full amount, for a redemption that resolves to a same-node payment (fee
// waived), or the full amount minus this claim's own RedeemFeePpm cut, for
// a genuinely external one (transactions.IsSelfPayment). See
// nipcash.CashRedeemParams.Invoice's own doc comment — nmilat deliberately
// leaves this ambiguity for the caller, since only the Hub knows which one
// a given invoice will resolve to.
//
// Always the net amount. NetRedeemableMillis == AmountMillis whenever
// RedeemFeeMillis is zero (list_recipients computes it as a plain
// subtraction — lokihub's nip47/controllers/list_recipients_controller.go),
// so this is a no-op change for the zero-fee case: every existing test
// fixture, and — going by NIP-CASH's own "same-node redeems are routinely
// free" framing — presumably still the common real deployment too. When the
// fee is nonzero, net is what a genuinely external redemption needs —
// cashing a token out to a wallet the recipient actually controls
// elsewhere is the entire point of redeeming to Lightning, so that's the
// case this optimizes for rather than the coincidence of the destination
// happening to sit on the same Hub. The cost: a same-node redemption that
// happens to coincide with a fee-charging Hub now fails outright (a
// cleanly classified invalid_input) instead of silently succeeding
// fee-free — the Hub's own rejection message already names the fix
// ("present an invoice for the full amount instead"), recoverable today
// via --invoice with a manually built full-amount invoice. previewSuffix
// below states this as the deterministic either/or it now is, rather than
// hedging with "up to"/"at least" language.
//
// A same-node-sensing auto-retry (try net, fall back to full only on that
// specific rejection) was considered and rejected: nothing on the wire
// distinguishes "amount mismatch: this one resolves same-node" from any
// other BAD_REQUEST reason except lokihub's own free-form message text —
// pattern-matching another service's prose (lokihub is ground truth here,
// not something this repo pins a stable machine-readable code onto) to
// drive a silent second make_invoice/cash_redeem round trip is worse than
// one clear, immediately actionable failure.
func redeemInvoiceAmount(q redeemQuote) uint64 {
	return q.NetRedeemableMillis
}

// previewSuffix renders q as additional confirmation text — "" if there's
// nothing to show. Fee is only mentioned when non-zero (a same-node
// redeem is routinely free) — and, since redeemInvoiceAmount always
// requests the net amount in that case, the outcome is one of exactly two
// things: this exact fee is charged, or (only if this specific redemption
// turns out to resolve to a same-node payment instead) the Hub rejects it
// outright rather than charging anything — never a range in between, so
// this doesn't hedge with "up to"/"at least" language. Expiry warning only
// when it's actually close, so it doesn't become noise a user learns to
// ignore.
func previewSuffix(q redeemQuote) string {
	var b strings.Builder
	if q.RedeemFeeMillis > 0 {
		fmt.Fprintf(&b, " This Hub charges a redeem fee: you'll receive %d %s (a %d %s cut). If this happens to resolve to a same-node payment instead, it will be rejected rather than waived — retry with --invoice for the full amount if that happens.",
			q.NetRedeemableMillis, output.CurrencyUnit, q.RedeemFeeMillis, output.CurrencyUnit)
	}
	if q.ExpiresAt != nil {
		remaining := time.Until(time.Unix(*q.ExpiresAt, 0))
		const soon = 24 * time.Hour
		if remaining <= 0 {
			fmt.Fprintf(&b, " WARNING: this token's redemption deadline has already passed — this may fail.")
		} else if remaining < soon {
			fmt.Fprintf(&b, " WARNING: this token expires in %s — redeem it now or it may become unspendable.", remaining.Round(time.Minute))
		}
	}
	return b.String()
}
