package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/dial"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newCashRedeemCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "redeem [wallet]",
		Short: "Redeem a held cash token into a Lightning wallet",
		Long:  `Redeems a held cash token into a Lightning wallet, or straight into any invoice via --invoice.`,
		Example: `  cashctl redeem
  cashctl redeem savings --token tok-abc123
  cashctl redeem --invoice lnbc1...`,
		Args: output.MaximumNArgs(1),
		RunE: runCashRedeem,
	}
	cmd.Flags().String("token", "", "which held token to redeem (auto-picked if you only hold one)")
	cmd.Flags().String("into", "", "which wallet to redeem into (defaults to your default wallet)")
	cmd.Flags().String("invoice", "", "redeem straight into this external invoice")
	cmd.Flags().String("as", "", "override credential (pubkey:<priv> | connection-key:... | cash:<secret>)")
	return cmd
}

func runCashRedeem(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	explicitInvoice, _ := cmd.Flags().GetString("invoice")
	intoFlagValue, _ := cmd.Flags().GetString("into")
	if explicitInvoice != "" {
		if err := validateInvoiceShape(explicitInvoice); err != nil {
			return output.InvalidInputError(cmd, explicitInvoice, err)
		}
	}

	var positionalInto string
	if len(args) > 0 {
		positionalInto = args[0]
	}
	intoValue, err := resolvePositionalOrFlag(cmd, positionalInto, "into", intoFlagValue)
	if err != nil {
		return err
	}
	// -c/--connection is the global "use this wallet instead of the default"
	// flag, and redeem's destination IS "the default wallet" unless told
	// otherwise — so honor it as the destination. It used to be ignored, so
	// `redeem -c savings` quietly paid out to the default wallet instead.
	// Anything ambiguous is refused rather than guessed at: it's real money.
	if conn, _ := cmd.Flags().GetString("connection"); conn != "" {
		switch {
		case explicitInvoice != "":
			return output.InvocationError(cmd, fmt.Errorf("-c/--connection names the wallet to redeem into, but --invoice redeems straight into an invoice — pass one or the other"))
		case intoValue != "" && intoValue != conn:
			return output.InvocationError(cmd, fmt.Errorf("got both a destination (%q) and -c/--connection (%q) with different values — pass only one", output.Sanitize(intoValue), output.Sanitize(conn)))
		}
		intoValue = conn
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

	var sourceClient *nipcashclient.Client
	err = WithSpinner(jsonMode, "Connecting...", func() error {
		c, cErr := nipcashclient.Connect(ctx, entry.Token)
		if cErr != nil {
			return cErr
		}
		sourceClient = c
		return nil
	})
	if err != nil {
		return output.NetworkError(cmd, err)
	}
	defer sourceClient.Close()

	var invoice string
	var destName string
	var message string
	if explicitInvoice != "" {
		invoice = explicitInvoice
		// "the invoice above" used to name a line this command never
		// actually printed — nothing shows the invoice anywhere before
		// this prompt/result, in either mode. A bolt11 invoice carries no
		// secret (it's meant to be shared/paid by anyone), so — unlike
		// safeDestinationLabel's own wallet-connection case — there's
		// nothing sensitive about naming a piece of it directly.
		destName = fmt.Sprintf("invoice %s…", truncateInvoiceForDisplay(explicitInvoice))
		message = fmt.Sprintf("Redeem into %s?", destName)
		if entry.AmountMillis != nil {
			message = fmt.Sprintf("Redeem %s into %s?", output.FormatAmount(int64(*entry.AmountMillis)), destName)
		}
	} else {
		// Always a live CheckClaim, in every mode including --json/--yes:
		// the fee quote isn't confirmation-prompt decoration anymore, it
		// decides the destination invoice's amount below (see
		// redeemInvoiceAmount) — "skip it, nothing prints the preview" is
		// no longer a valid reason to bypass this.
		var quote redeemQuote
		err = WithSpinner(jsonMode, "Checking fee quote...", func() error {
			q, qErr := resolveRedeemQuote(cmd, l, entry, sourceClient)
			if qErr != nil {
				return qErr
			}
			quote = q
			return nil
		})
		if err != nil {
			return err
		}
		destWalletName, destValue, err := resolveDestWallet(cmd, intoValue)
		if err != nil {
			return err
		}
		var destClient *relayclient.NWCClient
		err = WithSpinner(jsonMode, "Preparing invoice...", func() error {
			c, cErr := DialGeneric(ctx, destValue)
			if cErr != nil {
				return cErr
			}
			destClient = c
			tx, cErr := c.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(redeemInvoiceAmount(quote))})
			if cErr != nil {
				return cErr
			}
			invoice = tx.Invoice
			return nil
		})
		if err != nil {
			return classifyNWCErr(cmd, err)
		}
		defer destClient.Close()
		destName = destWalletName
		message = fmt.Sprintf("Redeem %s into %s?", output.FormatAmount(int64(quote.AmountMillis)), destName)
		// Linef, not fmt.Println: these are human-only narration — under
		// --json they used to land on stdout ahead of the result object, so
		// a script's json.Unmarshal of stdout failed on the first line.
		if fee := previewSuffix(quote); fee != "" {
			output.Notef(jsonMode, "%s", fee)
		}
		if w := expiryWarningSuffix(quote.ExpiresAt, "redeem"); w != "" {
			output.Notef(jsonMode, "%s", w)
		}
	}
	// defaultYes=false: moves real money — never accept on a bare Enter.
	// --yes/--json skip this entirely, unaffected.
	if !Confirm(cmd, false, message) {
		fmt.Println("Cancelled.")
		return nil
	}

	var result *nipcash.CashRedeemResult
	err = WithSpinner(jsonMode, "Redeeming...", func() error {
		r, cErr := spendBearerEntry(entry, cred, func(c nipcash.Credential) (*nipcash.CashRedeemResult, error) {
			return sourceClient.CashRedeem(ctx, nipcash.CashRedeemParams{Invoice: invoice, Credential: c})
		})
		if cErr != nil {
			return cErr
		}
		result = r
		return nil
	})
	if err != nil {
		return classifyCashTokenNWCErr(cmd, err)
	}

	_ = l.SetStatus(entry.ID, ledger.StatusRedeemed)
	if entry.AmountMillis != nil {
		l.AppendHistory("redeem", fmt.Sprintf("redeemed %s into %s", output.FormatAmount(int64(*entry.AmountMillis)), destName))
	} else {
		l.AppendHistory("redeem", fmt.Sprintf("redeemed into %s", destName))
	}
	// Unlike consolidate/transfer, there's no new token to lose here — the
	// cash was paid out for real (result.Preimage proves it) and no longer
	// exists to recover. What a failed Save leaves wrong is purely local
	// bookkeeping: this entry stays "held" and gets offered again by a
	// later pick/redeem, so that mismatch has to be reported, not hidden
	// behind an exit-0 "Redeemed" that implies the ledger agrees.
	if err := l.Save(); err != nil {
		return reportUnsavedResult(cmd, err, "Redeem",
			fmt.Sprintf("The payment went through (preimage: %s) — this entry will incorrectly keep showing as held until you remove or reconcile it.", result.Preimage))
	}

	if jsonMode {
		payload := map[string]any{
			"redeemed_token": entry.ID,
			"fee_mloki":      result.FeesPaid, "preimage": result.Preimage,
		}
		// Distinct field, not a repurposed "to_wallet": destName here
		// names an invoice, not a registered wallet — and unlike
		// destName's own truncated display form, the raw invoice isn't
		// secret, so a --json consumer gets the whole thing, not 12 chars
		// of it.
		if explicitInvoice != "" {
			payload["to_invoice"] = explicitInvoice
		} else {
			payload["to_wallet"] = destName
		}
		output.PrintJSON(payload)
		return nil
	}
	fmt.Printf("Redeemed → %s.\n", destName)
	if result.FeesPaid > 0 {
		fmt.Printf("Fee: %s.\n", output.FormatAmount(int64(result.FeesPaid)))
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
		// This branch is only reached under --json/--yes (jsonMode ||
		// yesFlag below) — no terminal to interactively pick from — so the
		// hint points at --json too: wallet show's plain-text listing
		// deliberately never prints entry.ID (docs/private/
		// wallet-abstraction-plan.md), only --json does.
		return output.UsageError(cmd, fmt.Errorf("you hold %d cash tokens — specify which with --token <id> (see `cashctl wallet show --json`)", len(held)))
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	yesFlag, _ := cmd.Flags().GetBool("yes")
	if jsonMode || yesFlag {
		return nil, tooManyErr()
	}

	output.Notef(false, "You hold %d cash tokens:", len(held))
	for i, e := range held {
		amount := "unknown amount"
		if e.AmountMillis != nil {
			amount = output.FormatAmount(int64(*e.AmountMillis))
		}
		output.Notef(false, "  %d) %s   received %s", i+1, amount, formatReceivedDate(e.ReceivedAt))
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
		return nil, output.InvocationError(cmd, fmt.Errorf(
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
		// Not a registered name, so it has to be a raw connection string — and
		// if it isn't one of those either it can never work: the caller's own
		// mistake (not_found), not a network failure to retry.
		if err := validateConnectionValue(into); err != nil {
			return "", "", unusableConnectionError(cmd, into)
		}
		// into isn't a registered name — a raw connection string, passed
		// directly. destName (the first return value) reaches the confirm
		// prompt, --json's to_wallet, and wallet history — using the raw
		// string itself there used to mean printing and PERSISTING the
		// destination wallet's own spending secret every time someone
		// redeemed into an unregistered wallet. value (the actual dial
		// target) still needs the raw string; only the display half changes.
		return safeDestinationLabel(into), into, nil
	}
	c, ok := s.DefaultConnection()
	if !ok {
		return "", "", output.NotFoundError(cmd, "", errors.New(noWalletMessage()+"\n  Or redeem straight into an invoice from any other wallet app: cashctl redeem --invoice <bolt11>"))
	}
	return c.Name, c.Value, nil
}

// truncateInvoiceForDisplay renders enough of a bolt11 invoice string to
// be recognizable in a prompt/result line without dumping the whole
// (often 200+ char) thing — purely a readability trim, not a secrecy one
// (see its own call site's comment).
func truncateInvoiceForDisplay(invoice string) string {
	const n = 12
	if len(invoice) <= n {
		return invoice
	}
	return invoice[:n]
}

// safeDestinationLabel derives a display-safe name for an unregistered
// destination string passed directly to redeem's `[wallet]`/`--into` —
// never the raw string itself, which (whatever kind it is) always carries
// a spending secret: an NWC URI's own secret= query value, or the
// equivalent embedded in a bech32 hub string's TLV encoding (same
// sensitivity as config.Connection.Value — see its own doc comment).
// Only an NWC URI has a genuinely separable non-secret piece worth
// showing (its wallet pubkey, plain in the host); a bech32 hub string
// encodes everything as one blob with no substring safe to reveal, so
// those fall back to naming just the connection kind. A caller that never
// registers the destination finds out it doesn't dial from the failure
// that follows, not from what got printed here first.
func safeDestinationLabel(raw string) string {
	switch dial.Sniff(raw) {
	case dial.KindNWCURI:
		if info, err := nip47.ParsePairingURI(raw); err == nil && len(info.WalletPubkey) >= 8 {
			return "wallet " + info.WalletPubkey[:8] + "…"
		}
		return "a wallet connection"
	case dial.KindCashHub:
		return "a Cash Hub connection"
	case dial.KindCircleHub:
		return "a Circle Hub connection"
	default:
		return "the given connection"
	}
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
	// Bounded, unlike a bare context.Background() (a real hang bug this
	// specific call had: with no deadline at all, a relay that accepts the
	// connection but never answers left `cashctl redeem`/`transfer`/
	// `consolidate` — every caller of resolveAmount — stuck forever,
	// confirmed live at 400+s; every other network call in this package
	// already bounds itself the same way).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	myPubHex, _ := localPubKeyHex(cmd)
	result, err := sourceClient.CheckClaim(ctx, tok, myPubHex)
	if errors.Is(err, nipcash.ErrClaimNotFound) {
		return 0, output.NotFoundError(cmd, entry.ID, fmt.Errorf("couldn't determine this token's amount — check `cashctl wallet show`, or that it's still valid"))
	}
	if err != nil {
		return 0, classifyCashTokenNWCErr(cmd, err)
	}
	entry.AmountMillis = &result.AmountMillis
	entry.ExpiresAt = result.ExpiresAt
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
	// Bounded for the same reason resolveAmount's own identical call is —
	// see its doc comment.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	myPubHex, _ := localPubKeyHex(cmd)
	result, err := sourceClient.CheckClaim(ctx, tok, myPubHex)
	if errors.Is(err, nipcash.ErrClaimNotFound) {
		return redeemQuote{}, output.NotFoundError(cmd, entry.ID, fmt.Errorf("couldn't determine this token's amount — check `cashctl wallet show`, or that it's still valid"))
	}
	if err != nil {
		return redeemQuote{}, classifyCashTokenNWCErr(cmd, err)
	}
	entry.AmountMillis = &result.AmountMillis
	entry.ExpiresAt = result.ExpiresAt
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

// previewSuffix renders q's fee caveat as its own confirmation-prompt
// line — "" if there's nothing to show. Fee is only mentioned when
// non-zero (a same-node redeem is routinely free) — and, since
// redeemInvoiceAmount always requests the net amount in that case, the
// outcome is one of exactly two things: this exact fee is charged, or
// (only if this specific redemption turns out to resolve to a same-node
// payment instead) the Hub rejects it outright rather than charging
// anything — never a range in between, so this doesn't hedge with "up
// to"/"at least" language. Expiry is handled separately, by the shared
// expiryWarningSuffix (cash_transfer.go) — same wording `transfer`/
// `consolidate` already use, rather than a second copy of it here.
func previewSuffix(q redeemQuote) string {
	if q.RedeemFeeMillis == 0 {
		return ""
	}
	return fmt.Sprintf("Fee: %s (you receive %s). Rejected instead of waived on a same-node match — retry with --invoice if so.",
		output.FormatAmount(int64(q.RedeemFeeMillis)), output.FormatAmount(int64(q.NetRedeemableMillis)))
}
