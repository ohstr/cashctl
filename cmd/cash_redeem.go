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
		Short: "Redeem held cash token(s) into a Lightning wallet",
		Long: `Redeems one or more held cash tokens into a Lightning wallet, or a single
token straight into any invoice via --invoice.

Several tokens can be redeemed in one command: name them with a repeatable
(or comma-separated) --token, or pass --all for every held token. Each
token is paid out into its own invoice, because a slice pays out exactly
once and an invoice is payable once. Every selected token is attempted and
reported even if another fails — each payout is separate and irreversible,
so a failure part-way through must not hide which ones already paid.`,
		Example: `  cashctl redeem
  cashctl redeem savings --token tok-abc123
  cashctl redeem --token tok-abc123,tok-def456
  cashctl redeem --all
  cashctl redeem --amount 300
  cashctl redeem --invoice lnbc1...`,
		Args: output.MaximumNArgs(1),
		RunE: runCashRedeem,
	}
	// StringSlice, not String: redeeming several bills in one command is the
	// point of the batch work, and repeating/comma-separating --token is how a
	// script names them. A single `--token <id>` parses identically to before,
	// so nothing that already worked changes.
	cmd.Flags().StringSlice("token", nil, "which held token(s) to redeem — repeatable, or comma-separated (auto-picked if you only hold one)")
	cmd.Flags().Bool("all", false, "redeem every held token")
	// String, not a positional: redeem's own positional slot already means the
	// destination wallet, so an amount has nowhere to go but a flag. Net, not
	// gross — see selectHeldTokensForRedeemAmount's own doc comment for why,
	// and for the one case it's deliberately NOT built to handle yet.
	cmd.Flags().String("amount", "", fmt.Sprintf("redeem this much, in %s, landing at the destination after any fee — selects which held token(s) cover it exactly (mutually exclusive with --token/--all)", output.CurrencyUnit))
	cmd.Flags().String("into", "", "which wallet to redeem into (defaults to your default wallet)")
	cmd.Flags().String("invoice", "", "redeem straight into this external invoice")
	cmd.Flags().String("as", "", "override credential (pubkey:<priv> | connection-key:... | cash:<secret>)")
	return cmd
}

func runCashRedeem(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	explicitInvoice, _ := cmd.Flags().GetString("invoice")
	intoFlagValue, _ := cmd.Flags().GetString("into")
	var invoiceOverrideAmount *uint64
	if explicitInvoice != "" {
		if err := validateInvoiceShape(explicitInvoice); err != nil {
			return output.InvalidInputError(cmd, explicitInvoice, err)
		}
		amt, err := invoiceAmountOverride(cmd, explicitInvoice)
		if err != nil {
			return err
		}
		invoiceOverrideAmount = amt
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
	entries, err := resolveHeldTokensForRedeem(cmd, l)
	if err != nil {
		return err
	}
	if err := refuseInvoiceForManyBills(cmd, explicitInvoice, len(entries)); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), redeemTimeout(len(entries)))
	defer cancel()

	// Prepared per bill — credential, connection, live quote — and a bill that
	// cannot be prepared becomes its own failed outcome rather than aborting the
	// run. No money has moved for any of them at this point.
	//
	// Deliberately before the destination is resolved, preserving the order a
	// single-bill redeem has always had: resolveCredential's own usage error for
	// a cash-mode token with no stored secret comes first, and
	// TestRedeem_NoWalletConfigured's reasoning depends on exactly that
	// precedence.
	plans, outcomes := prepareRedeems(ctx, cmd, l, entries, explicitInvoice, invoiceOverrideAmount, jsonMode)
	defer func() {
		for _, p := range plans {
			// Nil for a bill quoted in a batch: nothing about it was ever
			// opened outside the shared envelope.
			if p.Client != nil {
				p.Client.Close()
			}
		}
	}()
	if len(plans) == 0 {
		// Nothing left to pay into, so the destination is never resolved or
		// dialled — there would be nothing to ask it for.
		if worthReportingRedeemRun(outcomes) {
			printRedeemOutcomes(jsonMode, outcomes, "", explicitInvoice)
		}
		return firstRedeemError(outcomes)
	}

	// The destination is resolved and dialled ONCE: every invoice comes from it,
	// and with several bills there is no "the bill" whose quote could size a
	// single shared invoice anyway.
	//
	// --invoice deliberately skips all of it — see
	// TestRedeem_InvoiceFlagBypassesNoWalletCheck, which exists to prove the
	// no-wallet-configured check really is bypassed rather than merely
	// preempted by some other error.
	destName := ""
	if explicitInvoice != "" {
		// "the invoice above" used to name a line this command never actually
		// printed — nothing shows the invoice anywhere before this
		// prompt/result, in either mode. A bolt11 invoice carries no secret
		// (it's meant to be shared/paid by anyone), so — unlike
		// safeDestinationLabel's own wallet-connection case — there's nothing
		// sensitive about naming a piece of it directly.
		destName = fmt.Sprintf("invoice %s…", truncateInvoiceForDisplay(explicitInvoice))
	} else {
		destWalletName, destValue, dErr := resolveDestWallet(cmd, intoValue)
		if dErr != nil {
			return dErr
		}
		var destClient *relayclient.NWCClient
		err = WithSpinner(jsonMode, "Preparing invoice...", func() error {
			c, cErr := DialGeneric(ctx, destValue)
			if cErr != nil {
				return cErr
			}
			destClient = c
			return nil
		})
		if err != nil {
			return classifyNWCErr(cmd, err)
		}
		defer destClient.Close()
		destName = destWalletName

		plans = makeRedeemInvoices(ctx, cmd, destClient, plans, outcomes, jsonMode)
		if len(plans) == 0 {
			if worthReportingRedeemRun(outcomes) {
				printRedeemOutcomes(jsonMode, outcomes, destName, explicitInvoice)
			}
			return firstRedeemError(outcomes)
		}
	}

	// Narration for the single-bill case, unchanged: the fee caveat and the
	// expiry warning that the confirmation prompt has always been preceded by.
	// Not printed for a multi-bill run, where one line per bill about a fee that
	// may or may not apply would bury the total the person actually has to
	// check — the per-bill detail is in the result instead.
	if len(plans) == 1 && explicitInvoice == "" {
		// Linef, not fmt.Println: these are human-only narration — under --json
		// they used to land on stdout ahead of the result object, so a script's
		// json.Unmarshal of stdout failed on the first line.
		if fee := previewSuffix(plans[0].Quote); fee != "" {
			output.Notef(jsonMode, "%s", fee)
		}
		if w := expiryWarningSuffix(plans[0].Quote.ExpiresAt, "redeem"); w != "" {
			output.Notef(jsonMode, "%s", w)
		}
	}

	// defaultYes=false: moves real money — never accept on a bare Enter.
	// --yes/--json skip this entirely, unaffected.
	if !Confirm(cmd, false, redeemConfirmMessage(plans, destName)) {
		output.Println("Cancelled.")
		return nil
	}

	executeRedeems(ctx, cmd, l, plans, outcomes, destName, jsonMode)

	// Unlike consolidate/transfer, there's no new token to lose here — the cash
	// was paid out for real (each result's Preimage proves it) and no longer
	// exists to recover. What a failed Save leaves wrong is purely local
	// bookkeeping: these entries stay "held" and get offered again by a later
	// pick/redeem, so that mismatch has to be reported, not hidden behind an
	// exit-0 "Redeemed" that implies the ledger agrees.
	if err := l.Save(); err != nil {
		// Reported BEFORE returning the save failure, so the preimages of
		// payments that really happened are on stdout where they can be
		// recovered from.
		printRedeemOutcomes(jsonMode, outcomes, destName, explicitInvoice)
		return reportUnsavedResult(cmd, err, "Redeem", redeemRecoveryHint(outcomes))
	}

	printRedeemOutcomes(jsonMode, outcomes, destName, explicitInvoice)
	return firstRedeemError(outcomes)
}

// refuseInvoiceForManyBills rejects --invoice when several bills were selected.
//
// One invoice can only be paid once and a slice pays out exactly once, so N bills
// need N invoices. Refused rather than silently redeeming only the first: a caller
// asking for several bills into one invoice has a wrong model of what redeeming
// does, and quietly doing part of it would leave them believing the rest had been
// paid too.
//
// Necessarily checked after selection, since the bills have to be resolved before
// they can be counted — which is why it is a separate function: the path through
// runCashRedeem that reaches it needs a populated ledger, so the rule itself is
// tested here instead.
func refuseInvoiceForManyBills(cmd *cobra.Command, explicitInvoice string, bills int) error {
	if explicitInvoice == "" || bills <= 1 {
		return nil
	}
	return output.InvocationError(cmd, fmt.Errorf(
		"--invoice redeems into one invoice, but %d tokens were selected — an invoice is payable once, so each token needs its own; drop --invoice to have them paid into a wallet, or redeem them one at a time", bills))
}

// redeemTimeout scales the overall budget with how many bills are being
// redeemed, since each one is its own sequence of round trips (quote, invoice,
// redeem) against a possibly different Hub. A fixed 30s was right for one bill
// and would abort a large batch partway through — which for a money path is the
// worst outcome available, since some bills would have paid out and the rest
// would be unknown.
func redeemTimeout(bills int) time.Duration {
	if bills < 1 {
		bills = 1
	}
	const perBill = 30 * time.Second
	if d := time.Duration(bills) * perBill; d < 5*time.Minute {
		return d
	}
	return 5 * time.Minute
}

// redeemPlan is one bill prepared for redemption: its own source connection, its
// own live quote, and its own invoice.
//
// Per-bill rather than shared because all three genuinely are: bills can sit on
// different Hubs, each Hub quotes its own fee, and an invoice is payable once.
type redeemPlan struct {
	Index   int
	Entry   *ledger.Entry
	Cred    nipcash.Credential
	Client  *nipcashclient.Client
	Quote   redeemQuote
	Invoice string
	// InvoiceAmount overrides Invoice's own encoded amount — set only for an
	// explicit, amountless --invoice paired with --amount, mirroring
	// nip47.PayInvoiceParams' own override. nil for everything else: a
	// fixed-amount invoice's own encoding is authoritative and --amount is
	// refused alongside one (invoiceAmountOverride), and the batched/no-
	// --invoice path has nothing to override at all.
	InvoiceAmount *uint64
}

// redeemOutcome is what happened to one selected bill. Three states, matching
// consolidateOutcome and the SDK's own ItemOutcome for the same reason: paid
// (Result set), not attempted or refused before the wire (Err set), or — for a
// run the person declined — neither.
type redeemOutcome struct {
	EntryID      string
	AmountMillis *uint64
	Result       *nipcash.CashRedeemResult
	Err          error
}

// prepareRedeems dials, quotes and builds an invoice for each selected bill.
//
// Returns the bills that are ready plus an outcome slot for every selected bill,
// so a bill that failed preparation is reported rather than silently dropped. No
// money has moved at this point for any of them.
func prepareRedeems(
	ctx context.Context,
	cmd *cobra.Command,
	l *ledger.Ledger,
	entries []*ledger.Entry,
	explicitInvoice string,
	invoiceOverrideAmount *uint64,
	jsonMode bool,
) ([]redeemPlan, []redeemOutcome) {
	outcomes := make([]redeemOutcome, len(entries))
	plans := make([]redeemPlan, 0, len(entries))

	// Credentials first, for every bill, because the batched quote below needs
	// them all before it can send anything. A credential failure is the caller's
	// own and is recorded per bill rather than aborting the run.
	creds := make(map[string]nipcash.Credential, len(entries))
	for i, entry := range entries {
		outcomes[i] = redeemOutcome{EntryID: entry.ID, AmountMillis: entry.AmountMillis}
		cred, err := resolveCredential(cmd, entry)
		if err != nil {
			outcomes[i].Err = err
			continue
		}
		creds[entry.ID] = cred
	}

	// One cash_status request per HUB rather than per bill. Not only fewer round
	// trips: each request is tagged with its own bill, so quoting forty bills one at
	// a time republishes precisely the linkable set that batching the spend hides.
	quotes := map[string]redeemQuote{}
	quoteErrs := map[string]error{}
	if explicitInvoice == "" {
		groups, ungrouped := groupEntriesByHub(entries)
		for _, g := range groups {
			gotQuotes, gotErrs := quoteHubGroup(ctx, cmd, l, g, creds, jsonMode)
			for id, q := range gotQuotes {
				quotes[id] = q
			}
			for id, e := range gotErrs {
				quoteErrs[id] = e
			}
		}
		// A bill with no recoverable minter cannot be quoted at all: there is no hub
		// identity to verify an announcement against, and no other transport serves
		// bill methods. Recorded per bill rather than aborting the run.
		for _, e := range ungrouped {
			quoteErrs[e.ID] = output.InvalidInputError(cmd, e.ID,
				fmt.Errorf("this token has no verifiable mint signature, so its hub cannot be identified and it cannot be spent"))
		}
	}

	for i, entry := range entries {
		if outcomes[i].Err != nil {
			continue // credential failure, already recorded
		}
		cred := creds[entry.ID]
		plan := redeemPlan{Index: i, Entry: entry, Cred: cred}

		if explicitInvoice != "" {
			// Guarded above: --invoice only ever reaches here with one bill, so
			// it is always the standard path and always needs a connection.
			if !connectPlan(ctx, cmd, &plan, outcomes, i, len(entries), jsonMode) {
				continue
			}
			plan.Invoice = explicitInvoice
			if entry.AmountMillis != nil {
				plan.Quote = redeemQuote{AmountMillis: *entry.AmountMillis}
			}
			if invoiceOverrideAmount != nil {
				// A real quote, not the bare AmountMillis placeholder two
				// lines up: bounding a Hub-supplied ceiling against a
				// locally-specified amount needs the actual
				// NetRedeemableMillis, the same invariant this package's
				// audit guard (validateQuotedAmounts, referenced from every
				// other site that persists a Hub-supplied amount) already
				// enforces everywhere else.
				q, qErr := resolveRedeemQuote(cmd, l, entry, plan.Client)
				if qErr != nil {
					outcomes[i] = redeemOutcome{EntryID: entry.ID, Err: qErr}
					continue
				}
				plan.Quote = q
				if *invoiceOverrideAmount > q.NetRedeemableMillis {
					outcomes[i] = redeemOutcome{EntryID: entry.ID, Err: output.InvalidInputError(cmd, "",
						fmt.Errorf("--amount %s exceeds this bill's own net redeemable value of %s",
							output.FormatAmount(int64(*invoiceOverrideAmount)), output.FormatAmount(int64(q.NetRedeemableMillis))))}
					continue
				}
				plan.InvoiceAmount = invoiceOverrideAmount
			}
			plans = append(plans, plan)
			continue
		}

		// Quoted in the batch: no per-bill connection is opened at all, which
		// is the whole point — nothing about this bill appears on the relay
		// outside the shared envelope.
		if q, ok := quotes[entry.ID]; ok {
			plan.Quote = q
			outcomes[i].AmountMillis = entry.AmountMillis
			plans = append(plans, plan)
			continue
		}
		if err, ok := quoteErrs[entry.ID]; ok {
			outcomes[i].Err = err
			continue
		}

		// Standard path: this bill's hub offers no batch inbox, or it has no
		// recoverable minter to address one with.
		if !connectPlan(ctx, cmd, &plan, outcomes, i, len(entries), jsonMode) {
			continue
		}

		// Always a live CheckClaim, in every mode including --json/--yes: the
		// fee quote isn't confirmation-prompt decoration anymore, it decides
		// the destination invoice's amount below (see redeemInvoiceAmount) —
		// "skip it, nothing prints the preview" is no longer a valid reason to
		// bypass this.
		err := WithSpinner(jsonMode, redeemStep(len(entries), i, "Checking fee quote..."), func() error {
			q, qErr := resolveRedeemQuote(cmd, l, entry, plan.Client)
			if qErr != nil {
				return qErr
			}
			plan.Quote = q
			return nil
		})
		if err != nil {
			plan.Client.Close()
			outcomes[i].Err = err
			continue
		}
		outcomes[i].AmountMillis = entry.AmountMillis
		plans = append(plans, plan)
	}
	return plans, outcomes
}

// connectPlan opens the standard-transport connection a plan needs, recording a
// failure as that bill's own outcome. Reports whether the plan is usable.
func connectPlan(ctx context.Context, cmd *cobra.Command, plan *redeemPlan, outcomes []redeemOutcome, i, total int, jsonMode bool) bool {
	if plan.Client != nil {
		return true
	}
	var client *nipcashclient.Client
	err := WithSpinner(jsonMode, redeemStep(total, i, "Connecting..."), func() error {
		c, cErr := nipcashclient.Connect(ctx, plan.Entry.Token)
		if cErr != nil {
			return cErr
		}
		client = c
		return nil
	})
	if err != nil {
		outcomes[i].Err = output.NetworkError(cmd, err)
		return false
	}
	plan.Client = client
	return true
}

// makeRedeemInvoices asks the destination wallet for one invoice per prepared
// bill, each sized to that bill's own quote.
//
// Separated from prepareRedeems so the destination is contacted once per bill
// only for bills that actually got a quote — and so a destination that starts
// refusing partway through marks exactly the remaining bills, not the ones
// already invoiced.
func makeRedeemInvoices(
	ctx context.Context,
	cmd *cobra.Command,
	destClient *relayclient.NWCClient,
	plans []redeemPlan,
	outcomes []redeemOutcome,
	jsonMode bool,
) []redeemPlan {
	ready := make([]redeemPlan, 0, len(plans))
	for i, p := range plans {
		if p.Invoice != "" {
			ready = append(ready, p)
			continue
		}
		var invoice string
		err := WithSpinner(jsonMode, redeemStep(len(plans), i, "Preparing invoice..."), func() error {
			tx, cErr := destClient.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(redeemInvoiceAmount(p.Quote))})
			if cErr != nil {
				return cErr
			}
			invoice = tx.Invoice
			return nil
		})
		if err != nil {
			p.Client.Close()
			outcomes[p.Index].Err = classifyNWCErr(cmd, err)
			continue
		}
		p.Invoice = invoice
		ready = append(ready, p)
	}
	return ready
}

// executeRedeems places the cash_redeem calls and records what happened, writing
// outcomes in place.
//
// One request per HUB, carrying every one of that hub's bills. The split is per hub
// because an envelope's items must all bind to one hub, and bills from two hubs cannot
// share a request however few they are.
//
// Never stops early. Each hub's request is a separate, irreversible payout, so
// abandoning the rest on one failure would leave a caller unable to tell which bills
// paid out — and unlike consolidate there is nothing to re-merge: the cash is gone or
// it is not.
func executeRedeems(
	ctx context.Context,
	cmd *cobra.Command,
	l *ledger.Ledger,
	plans []redeemPlan,
	outcomes []redeemOutcome,
	destName string,
	jsonMode bool,
) {
	groups, ungrouped := groupPlansByHub(plans)
	for _, g := range groups {
		redeemHubGroup(ctx, cmd, l, g, outcomes, destName, jsonMode)
	}
	// Unreachable in practice: a bill with no recoverable minter fails at the quote
	// stage and never reaches here. Recorded rather than dropped, because silently
	// losing a prepared bill is the one outcome a money path must not produce.
	for _, p := range ungrouped {
		outcomes[p.Index].Err = output.InvalidInputError(cmd, p.Entry.ID,
			fmt.Errorf("this token has no verifiable mint signature, so its hub cannot be identified and it cannot be spent"))
	}
}

// redeemStep labels a spinner with which bill of how many it is working on, so a
// multi-bill run does not sit on an unchanging "Redeeming..." with no sense of
// progress. Single-bill runs keep the original bare label.
func redeemStep(total, index int, label string) string {
	if total <= 1 {
		return label
	}
	return fmt.Sprintf("[%d/%d] %s", index+1, total, label)
}

// redeemConfirmMessage is the one prompt covering the whole run.
//
// One prompt rather than one per bill: a person who asked to redeem forty bills
// has already made the decision, and asking forty times trains them to answer
// without reading. The total is what they need to check.
func redeemConfirmMessage(plans []redeemPlan, destName string) string {
	if len(plans) == 1 {
		q := plans[0].Quote
		if q.AmountMillis > 0 {
			return fmt.Sprintf("Redeem %s into %s?", output.FormatAmount(int64(q.AmountMillis)), destName)
		}
		return fmt.Sprintf("Redeem into %s?", destName)
	}
	var total uint64
	for _, p := range plans {
		total += p.Quote.AmountMillis
	}
	if total > 0 {
		return fmt.Sprintf("Redeem %d tokens (%s) into %s?", len(plans), output.FormatAmount(int64(total)), destName)
	}
	return fmt.Sprintf("Redeem %d tokens into %s?", len(plans), destName)
}

// printRedeemOutcomes renders a whole run, paid and failed together.
//
// The single-successful-bill --json shape is deliberately unchanged, because
// that is what every existing consumer parses. The per-bill array is used only
// when more than one bill was selected or one of them did not succeed — states
// in which this command previously could not run at all, so nothing can depend
// on the old shape there.
func printRedeemOutcomes(jsonMode bool, outcomes []redeemOutcome, destName, explicitInvoice string) {
	destKey, destVal := "to_wallet", destName
	if explicitInvoice != "" {
		// Distinct field, not a repurposed "to_wallet": destName here names an
		// invoice, not a registered wallet — and unlike destName's own
		// truncated display form, the raw invoice isn't secret, so a --json
		// consumer gets the whole thing, not 12 chars of it.
		destKey, destVal = "to_invoice", explicitInvoice
	}

	paid := 0
	for _, o := range outcomes {
		if o.Result != nil {
			paid++
		}
	}

	if jsonMode {
		if len(outcomes) == 1 && paid == 1 {
			o := outcomes[0]
			output.PrintJSON(map[string]any{
				"redeemed_token": o.EntryID,
				"fee_mloki":      o.Result.FeesPaid, "preimage": o.Result.Preimage,
				destKey: destVal,
			})
			return
		}
		rows := make([]map[string]any, len(outcomes))
		for i, o := range outcomes {
			row := map[string]any{"token": o.EntryID}
			if o.AmountMillis != nil {
				row["amount_millis"] = *o.AmountMillis
			}
			switch {
			case o.Result != nil:
				row["status"] = "ok"
				row["fee_mloki"] = o.Result.FeesPaid
				row["preimage"] = o.Result.Preimage
			case o.Err != nil:
				ce := output.AsCLIError(o.Err)
				row["status"] = "failed"
				row["error"] = ce.Err.Error()
				if ce.RawMessage != "" {
					row["error"] = ce.RawMessage
				}
				row["code"] = string(ce.Code)
				if ce.NWCCode != "" {
					row["nwc_code"] = ce.NWCCode
				}
			default:
				// Neither paid nor failed: the run was declined before this
				// bill was ever attempted. Never reported as a failure — no
				// wire call was made, so there is nothing to retry or
				// reconcile.
				row["status"] = "not_attempted"
			}
			rows[i] = row
		}
		output.PrintJSON(map[string]any{"redeemed": rows, destKey: destVal})
		return
	}

	for _, o := range outcomes {
		switch {
		case o.Result != nil:
			amount := ""
			if o.AmountMillis != nil {
				amount = " " + output.FormatAmount(int64(*o.AmountMillis))
			}
			output.Printf("Redeemed%s → %s.\n", amount, destName)
			if o.Result.FeesPaid > 0 {
				output.Printf("Fee: %s.\n", output.FormatAmount(int64(o.Result.FeesPaid)))
			}
		case o.Err != nil:
			// Named by token id: with several bills in play, "it failed"
			// without saying which one is not actionable.
			output.Printf("Failed to redeem %s: %v\n", output.Sanitize(o.EntryID), output.AsCLIError(o.Err).Err)
		}
	}
	if len(outcomes) > 1 {
		output.Printf("%d of %d tokens redeemed.\n", paid, len(outcomes))
	}
}

// worthReportingRedeemRun reports whether the run has something to say beyond
// the returned error.
//
// A single bill that failed is fully described by its own error on stderr, and
// that is exactly what this command has always printed for it — so no result
// document is emitted, keeping single-bill behaviour byte-for-byte unchanged. As
// soon as there is more than one bill, or any bill actually paid out, the error
// alone cannot describe the run and the per-bill report becomes the point.
func worthReportingRedeemRun(outcomes []redeemOutcome) bool {
	if len(outcomes) > 1 {
		return true
	}
	for _, o := range outcomes {
		if o.Result != nil {
			return true
		}
	}
	return false
}

// firstRedeemError returns the first failed bill's own error, or nil.
//
// The original error, neither wrapped nor joined, so its classification,
// retryable flag and any wallet-side NWC code survive exactly as they would have
// for a single bill — AsCLIError unwraps to the inner *CLIError and prints that,
// so a wrapper's text would never reach the user anyway. The complete per-bill
// picture is on stdout instead.
func firstRedeemError(outcomes []redeemOutcome) error {
	for _, o := range outcomes {
		if o.Err != nil {
			return o.Err
		}
	}
	return nil
}

// redeemRecoveryHint lists the preimages of payments that really happened, for
// the case where the payouts succeeded but recording them locally did not.
//
// Every preimage is included rather than just a count: a preimage is the proof
// that a specific payment occurred, and it is the only thing that lets a person
// reconcile a ledger that now disagrees with reality.
func redeemRecoveryHint(outcomes []redeemOutcome) string {
	var preimages []string
	for _, o := range outcomes {
		if o.Result != nil {
			preimages = append(preimages, fmt.Sprintf("%s=%s", o.EntryID, o.Result.Preimage))
		}
	}
	if len(preimages) == 0 {
		return ""
	}
	return fmt.Sprintf("These payments went through (token=preimage): %s — those entries will incorrectly keep showing as held until you remove or reconcile them.",
		strings.Join(preimages, ", "))
}

// resolveHeldTokensForRedeem picks which held tokens to redeem: --token by ID
// (repeatable, or comma-separated), --all for every held token, or the
// single-entry auto-pick/interactive-pick that redeem has always done.
//
// Separate from resolveHeldToken rather than a replacement for it, because
// transfer, inspect and wallet protect all act on exactly one token and share
// that function — widening it would force a multi-token shape on three commands
// that have no use for one.
//
// Each returned pointer is re-resolved through l.Find, for the reason
// resolveHeldToken's own doc comment sets out at length: l.Held() returns
// copies, so a pointer into it looks live and silently does not write through.
func resolveHeldTokensForRedeem(cmd *cobra.Command, l *ledger.Ledger) ([]*ledger.Entry, error) {
	ids, _ := cmd.Flags().GetStringSlice("token")
	all, _ := cmd.Flags().GetBool("all")
	amountFlag, _ := cmd.Flags().GetString("amount")
	explicitInvoice, _ := cmd.Flags().GetString("invoice")

	if len(ids) > 0 && all {
		return nil, output.InvocationError(cmd, fmt.Errorf("got both --token and --all — pass one or the other"))
	}

	// --amount means something ENTIRELY different with --invoice (an override
	// for an amountless invoice, already fully validated and consumed by
	// invoiceAmountOverride before this function is ever reached) than
	// without it (which held token(s) to select). Only the latter belongs
	// here — an explicit --invoice still resolves its one bill through
	// --token/bare-pick below, exactly as it always has.
	if amountFlag != "" && explicitInvoice == "" {
		if len(ids) > 0 || all {
			return nil, output.InvocationError(cmd, fmt.Errorf("got --amount with --token/--all — pass one or the other"))
		}
		target, err := output.ParseAmount(amountFlag)
		if err != nil {
			return nil, output.InvalidInputError(cmd, amountFlag, err)
		}
		return selectHeldTokensForRedeemAmount(cmd, l, target)
	}

	if len(ids) > 0 {
		out := make([]*ledger.Entry, 0, len(ids))
		seen := make(map[string]struct{}, len(ids))
		for _, raw := range ids {
			id := strings.TrimSpace(raw)
			if id == "" {
				continue
			}
			// A bill named twice must not be redeemed twice. The second attempt
			// would fail anyway — the Hub deletes a spent slice — but it would
			// fail as a network timeout against a Hub that has gone
			// deliberately silent, reported as though something were wrong.
			if _, dup := seen[id]; dup {
				return nil, output.InvocationError(cmd, fmt.Errorf("token %q given more than once", output.Sanitize(id)))
			}
			seen[id] = struct{}{}
			e, err := heldTokenByID(cmd, l, id)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		if len(out) == 0 {
			return nil, output.InvocationError(cmd, fmt.Errorf("--token was given no ids"))
		}
		return out, nil
	}

	held := l.Held()
	if len(held) == 0 {
		return nil, output.NotFoundError(cmd, "", fmt.Errorf("you have no held cash tokens — receive one first with `cashctl receive <token>`"))
	}
	if all {
		out := make([]*ledger.Entry, 0, len(held))
		for _, h := range held {
			e, ok := l.Find(h.ID)
			if !ok {
				continue
			}
			out = append(out, e)
		}
		return out, nil
	}
	if len(held) == 1 {
		e, ok := l.Find(held[0].ID)
		if !ok {
			return nil, output.NotFoundError(cmd, held[0].ID, fmt.Errorf("no held token %q", held[0].ID))
		}
		return []*ledger.Entry{e}, nil
	}
	return pickHeldTokens(cmd, l, held)
}

// pickHeldTokens asks which of several held tokens to redeem, allowing more than
// one.
//
// Under --json/--yes this stays a usage error rather than quietly redeeming
// everything. That is a deliberate asymmetry with consolidate, which does
// process every group non-interactively: consolidating is reversible in the
// sense that the value stays yours, whereas redeeming pays out irreversibly, so
// a script that did not say which bills it meant must not have that decided for
// it. --all is how a script says it means all of them.
func pickHeldTokens(cmd *cobra.Command, l *ledger.Ledger, held []ledger.Entry) ([]*ledger.Entry, error) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	yesFlag, _ := cmd.Flags().GetBool("yes")
	tooManyErr := func() error {
		// This branch is only reached under --json/--yes — no terminal to
		// interactively pick from — so the hint points at --json too: wallet
		// show's plain-text listing deliberately never prints entry.ID
		// (docs/private/wallet-abstraction-plan.md), only --json does.
		return output.UsageError(cmd, fmt.Errorf("you hold %d cash tokens — specify which with --token <id> (repeatable, or comma-separated), or --all to redeem every one (see `cashctl wallet show --json`)", len(held)))
	}
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
	choice, err := PromptLine(fmt.Sprintf("Which one(s)? [1-%d, comma-separated, or 'all'] ", len(held)))
	if err != nil {
		return nil, output.RuntimeError(cmd, err)
	}
	choice = strings.TrimSpace(choice)

	var picked []ledger.Entry
	switch {
	case strings.EqualFold(choice, "all"):
		picked = held
	case choice == "":
		// A bare Enter is NOT "all" here, unlike pickHubGroups', because
		// this one pays out irreversibly: the cheap default must be the one
		// that spends nothing.
		return nil, tooManyErr()
	default:
		for _, part := range strings.Split(choice, ",") {
			idx := parseChoice(strings.TrimSpace(part), len(held))
			if idx < 0 {
				return nil, output.UsageError(cmd, fmt.Errorf("invalid selection %q", part))
			}
			picked = append(picked, held[idx])
		}
	}

	out := make([]*ledger.Entry, 0, len(picked))
	seen := make(map[string]struct{}, len(picked))
	for _, p := range picked {
		if _, dup := seen[p.ID]; dup {
			continue
		}
		seen[p.ID] = struct{}{}
		e, ok := l.Find(p.ID)
		if !ok {
			return nil, output.NotFoundError(cmd, p.ID, fmt.Errorf("no held token %q", p.ID))
		}
		out = append(out, e)
	}
	return out, nil
}

// heldTokenByID resolves one --token id to a live ledger entry, refusing a bill
// that our own ledger already records as spent.
//
// Factored out of resolveHeldToken so the multi-token path applies exactly the
// same rule to every id. The spent check is not merely tidy: a Hub deletes a
// bill once nothing is left on it and then stays silent about it, so dialling a
// spent bill waits out the whole timeout and reports a network failure. Our own
// ledger recorded the spend and is the better source — the Hub is deliberately
// refusing to confirm it.
func heldTokenByID(cmd *cobra.Command, l *ledger.Ledger, id string) (*ledger.Entry, error) {
	e, ok := l.Find(id)
	if !ok {
		return nil, output.NotFoundError(cmd, id, fmt.Errorf("no held token %q", id))
	}
	if spent := spentStatusDescription(e.Status); spent != "" {
		return nil, output.NotFoundError(cmd, id,
			fmt.Errorf("token %q was already %s, so it no longer exists on the Hub", id, spent))
	}
	return e, nil
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
		// Find returns an entry whatever its status, so --token could reach a
		// bill we already spent. Dialling one is not just wasteful, it hangs:
		// a Hub deletes a bill once nothing is left on it and then stays
		// silent about it, so the call waits out its whole timeout and
		// reports a network failure. Our own ledger is the better source
		// here -- it recorded the spend, and the Hub is deliberately
		// refusing to confirm it.
		//
		// The no-flag path below picks from l.Held() and so cannot hit this.
		if spent := spentStatusDescription(e.Status); spent != "" {
			return nil, output.NotFoundError(cmd, id,
				fmt.Errorf("token %q was already %s, so it no longer exists on the Hub", id, spent))
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

// spentStatusDescription puts a terminal ledger status into the words an
// error message wants, or returns "" for a token that is still held.
func spentStatusDescription(status string) string {
	switch status {
	case ledger.StatusRedeemed:
		return "redeemed"
	case ledger.StatusTransferred:
		return "transferred away"
	case ledger.StatusConsolidated:
		return "consolidated into another token"
	default:
		return ""
	}
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
// --as overrides everything; a cash-mode token uses its stored
// CashSecret — `cashctl receive` refuses to save a cash-mode entry
// without one (see ledger.Entry.CashSecret's own doc comment), so any
// entry reaching this point is guaranteed to have it; a connection-key-
// bound token currently requires an explicit --as (re-deriving a fresh
// live attestation automatically is a known gap — see ledger.Entry's own
// doc comment on why only a *reference* is stored); everything else
// defaults to the local identity.
func resolveCredential(cmd *cobra.Command, entry *ledger.Entry) (nipcash.Credential, error) {
	if as := asCredentialValue(cmd); as != "" {
		cred, err := credential.ParseCash(as)
		if err != nil {
			return nil, output.InvalidInputError(cmd, output.RedactSecretInput(as), err)
		}
		return cred, nil
	}
	if entry.IdentityRequired != nil && !*entry.IdentityRequired {
		return nipcash.BySecret(entry.CashSecret), nil
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
	// CheckClaim tries pubkey then cash mode live; no need to pre-decide.
	// Bounded, unlike a bare context.Background() (a real hang bug this
	// specific call had: with no deadline at all, a relay that accepts the
	// connection but never answers left `cashctl redeem`/`transfer`/
	// `consolidate` — every caller of resolveAmount — stuck forever,
	// confirmed live at 400+s; every other network call in this package
	// already bounds itself the same way).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// The credential is resolved here rather than passed in: bill methods authorize
	// per item now, so even this read has to say who is asking.
	cred, err := resolveCredential(cmd, entry)
	if err != nil {
		return 0, err
	}
	myPubHex, _ := localPubKeyHex(cmd)
	result, err := sourceClient.CheckClaim(ctx, cred, tok, myPubHex)
	if errors.Is(err, nipcash.ErrClaimNotFound) {
		return 0, output.NotFoundError(cmd, entry.ID, fmt.Errorf("couldn't determine this token's amount — check `cashctl wallet show`, or that it's still valid"))
	}
	if err != nil {
		return 0, classifyCashTokenNWCErr(cmd, err)
	}
	// Same guard as the two quote paths, for the same reason and against the same
	// attacker: this also persists a Hub-supplied amount and sets Verified = true, and
	// it is reached by redeem, transfer AND consolidate, so it is the widest of the
	// three. The slice-aware validator rather than receive's, because the figure is
	// this holder's slice, which may legitimately be smaller than the bill's signed
	// total — see that function's own comment on why the ceiling carries over and the
	// floor does not.
	if vErr := validateQuotedAmounts(cmd, tok, result.MinterPubkey, redeemQuote{
		AmountMillis:        result.AmountMillis,
		RedeemFeeMillis:     result.RedeemFeeMillis,
		NetRedeemableMillis: result.NetRedeemableMillis,
	}); vErr != nil {
		return 0, vErr
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
	// AttestedAmountMillis is the whole bill's signed denomination, set only when the
	// mint signature actually verified.
	//
	// It exists because validateQuotedAmounts can only bound the Hub's figure from
	// ABOVE: a quote below the attested amount is what every slice of a
	// multi-recipient bill legitimately looks like, so it cannot be refused — but it
	// is also exactly what a Hub under-reporting a single-recipient bill looks like,
	// and the difference stays with the Hub. Since the client cannot tell those apart,
	// previewSuffix shows the user both numbers and lets them. Silently persisting
	// only the Hub's was the defect.
	AttestedAmountMillis *uint64
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
	cred, err := resolveCredential(cmd, entry)
	if err != nil {
		return redeemQuote{}, err
	}
	myPubHex, _ := localPubKeyHex(cmd)
	result, err := sourceClient.CheckClaim(ctx, cred, tok, myPubHex)
	if errors.Is(err, nipcash.ErrClaimNotFound) {
		return redeemQuote{}, output.NotFoundError(cmd, entry.ID, fmt.Errorf("couldn't determine this token's amount — check `cashctl wallet show`, or that it's still valid"))
	}
	if err != nil {
		return redeemQuote{}, classifyCashTokenNWCErr(cmd, err)
	}
	q := redeemQuote{
		AmountMillis:        result.AmountMillis,
		RedeemFeeMillis:     result.RedeemFeeMillis,
		NetRedeemableMillis: result.NetRedeemableMillis,
		ExpiresAt:           result.ExpiresAt,
	}
	if result.MinterPubkey != nil {
		q.AttestedAmountMillis = tok.AttestedAmountMillis
	}
	// Before the ledger sees any of it. Persisting first and validating after would
	// leave the poison behind even on the refusal path.
	if err := validateQuotedAmounts(cmd, tok, result.MinterPubkey, q); err != nil {
		return redeemQuote{}, err
	}
	entry.AmountMillis = &result.AmountMillis
	entry.ExpiresAt = result.ExpiresAt
	_ = l.SetVerified(entry.ID, true)
	return q, nil
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
	var parts []string
	if q.RedeemFeeMillis > 0 {
		parts = append(parts, fmt.Sprintf(
			"Fee: %s (you receive %s). Rejected instead of waived on a same-node match — retry with --invoice if so.",
			output.FormatAmount(int64(q.RedeemFeeMillis)), output.FormatAmount(int64(q.NetRedeemableMillis))))
	}
	// Deliberately NOT gated on the fee being nonzero: a Hub deflating the amount
	// reports a zero fee precisely so that the fee line — the only line there used to
	// be — stays hidden. Shown whenever the two figures disagree at all, since for a
	// slice of a multi-recipient bill they always will and that is worth seeing too.
	if q.AttestedAmountMillis != nil && *q.AttestedAmountMillis != q.AmountMillis {
		parts = append(parts, fmt.Sprintf(
			"This bill's signed provenance attests %s in total; the Hub quotes %s as yours.",
			output.FormatAmount(int64(*q.AttestedAmountMillis)), output.FormatAmount(int64(q.AmountMillis))))
	}
	return strings.Join(parts, " ")
}

// pickHeldTokensOrAll is pickHeldTokens' counterpart for a command in
// consolidate's own stakes class, not redeem's — the holding stays fully
// yours either way (a re-key, a status read), so there is nothing here an
// ambiguous selection could lose. Mirrors pickHubGroups (cash_consolidate.go)
// exactly, just over a plain []ledger.Entry instead of a hub-grouped map:
// --json/--yes or a single candidate defaults to all of them, the
// interactive prompt's bare Enter also means all, and an explicit selection
// still works for the caller who wants one specific entry.
func pickHeldTokensOrAll(cmd *cobra.Command, held []ledger.Entry) ([]*ledger.Entry, error) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	yesFlag, _ := cmd.Flags().GetBool("yes")
	if jsonMode || yesFlag || len(held) == 1 {
		return entryPointers(held), nil
	}

	output.Notef(false, "You hold %d eligible tokens:", len(held))
	for i, e := range held {
		amount := "unknown amount"
		if e.AmountMillis != nil {
			amount = output.FormatAmount(int64(*e.AmountMillis))
		}
		output.Notef(false, "  %d) %s   received %s", i+1, amount, formatReceivedDate(e.ReceivedAt))
	}
	choice, err := PromptLine(fmt.Sprintf("Which one(s)? [1-%d, comma-separated, or Enter for all] ", len(held)))
	if err != nil {
		return nil, output.RuntimeError(cmd, err)
	}
	choice = strings.TrimSpace(choice)
	if choice == "" || strings.EqualFold(choice, "all") {
		return entryPointers(held), nil
	}

	var picked []ledger.Entry
	for _, part := range strings.Split(choice, ",") {
		idx := parseChoice(strings.TrimSpace(part), len(held))
		if idx < 0 {
			return nil, output.UsageError(cmd, fmt.Errorf("invalid selection %q", part))
		}
		picked = append(picked, held[idx])
	}
	return entryPointers(picked), nil
}

// entryPointers takes the address of each element of a fresh copy of
// entries — never of the slice pickHeldTokensOrAll was handed, which may be
// reused or reordered by its own caller afterward.
func entryPointers(entries []ledger.Entry) []*ledger.Entry {
	out := make([]*ledger.Entry, len(entries))
	for i := range entries {
		e := entries[i]
		out[i] = &e
	}
	return out
}

// selectHeldTokensForRedeemAmount is --amount's own selection: which held
// token(s) land exactly target at the destination, net of whatever fee the
// Hub quotes, with none of it the caller's job to work out.
//
// Deliberately scoped to the two cases that are fully deterministic from
// data already in hand — an exact single-token match, or an exact same-Hub
// sum — and no further. A carve (splitting an existing token to hit an
// amount nothing already matches) is NOT attempted here, and that is a
// confirmed limit, not a shortcut: NIP-CASH's own quote response
// (RecipientStatus) exposes only AmountMillis/RedeemFeeMillis/
// NetRedeemableMillis/MinTransferMillis, never the raw redeem_fee_ppm/
// redeem_fee_base rate. One quote is one (gross, fee) sample; the fee
// formula has two independent unknowns, so the exact face value to carve
// for an arbitrary target cannot be computed in one deterministic pass —
// resolving it needs a second measurement (carve an estimate, quote the
// real fee, carve a corrective top-up, consolidate, redeem), which is its
// own design and audit pass, not a line item here. See
// docs/private/amount-first-decisions.md for the full trace, including the
// two earlier, wrong guesses this took to reach.
//
// Net, not gross: `redeem 500` means 500 lands at the destination, matching
// both LN convention (an invoice's amount is what the payee receives) and
// redeemInvoiceAmount's own existing rule for a whole-bill redeem. Matching
// is therefore against NetRedeemableMillis, each candidate's OWN live
// quote — not AmountMillis, the cached face value, which a nonzero fee
// would make wrong.
func selectHeldTokensForRedeemAmount(cmd *cobra.Command, l *ledger.Ledger, target uint64) ([]*ledger.Entry, error) {
	held := l.Held()
	if len(held) == 0 {
		return nil, output.NotFoundError(cmd, "", fmt.Errorf("you have no held cash tokens — receive one first with `cashctl receive <token>`"))
	}

	type quotedEntry struct {
		entry *ledger.Entry
		net   uint64
		hub   string
	}
	quotes := make([]quotedEntry, 0, len(held))
	var totalNet uint64
	for _, h := range held {
		if h.AmountMillis == nil {
			continue // nothing to quote; same guard SelectForAmount uses
		}
		e, ok := l.Find(h.ID)
		if !ok {
			continue
		}
		q, err := quoteHeldCandidate(cmd, e)
		if err != nil {
			// Best-effort: one candidate this process can't currently quote
			// (expired, a transient network hiccup, a credential this
			// invocation doesn't have) just isn't considered — it does not
			// fail the whole selection, the same way a single bad entry
			// doesn't abort consolidate's own auto-grouping.
			continue
		}
		totalNet += q.NetRedeemableMillis
		if q.NetRedeemableMillis == target {
			return []*ledger.Entry{e}, nil // exact single match — short-circuit, no further quoting needed
		}
		quotes = append(quotes, quotedEntry{entry: e, net: q.NetRedeemableMillis, hub: ledger.HubGroupKey(*e)})
	}

	byHub := make(map[string][]quotedEntry)
	for _, q := range quotes {
		if q.hub == "" {
			continue // no recoverable issuing Hub — ungroupable, same as consolidate's own rule
		}
		byHub[q.hub] = append(byHub[q.hub], q)
	}
	for _, group := range byHub {
		nets := make([]uint64, len(group))
		for i, g := range group {
			nets[i] = g.net
		}
		if idxs, ok := exactSubsetSumNet(nets, target); ok {
			out := make([]*ledger.Entry, len(idxs))
			for i, idx := range idxs {
				out[i] = group[idx].entry
			}
			return out, nil
		}
	}

	// Classified here, not left as the raw ledger error types transfer's own
	// SelectForAmount returns to ITS caller for classification — this
	// function has no other caller, so there is no benefit to deferring it,
	// and an unclassified error reaching EmitError would fall back to a
	// generic internal (exit 1) rather than the specific, actionable code
	// transfer's own identical cases already get.
	if totalNet < target {
		return nil, output.InvalidInputError(cmd, "", fmt.Errorf("not enough funds: you hold %s net redeemable, need %s",
			output.FormatAmount(int64(totalNet)), output.FormatAmount(int64(target))))
	}
	return nil, output.UsageError(cmd, fmt.Errorf("%w: you hold %s total, but no single Hub's tokens sum to exactly the %s you're redeeming",
		ledger.ErrFundsFragmented, output.FormatAmount(int64(totalNet)), output.FormatAmount(int64(target))))
}

// quoteHeldCandidate dials entry's own token standalone and returns its live
// redeem quote — the same resolveRedeemQuote every other redeem path uses,
// just with its own connection rather than a shared one, since selection
// runs before any entry has been committed to.
func quoteHeldCandidate(cmd *cobra.Command, entry *ledger.Entry) (redeemQuote, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, entry.Token)
	if err != nil {
		return redeemQuote{}, err
	}
	defer client.Close()
	l, err := ledger.Load()
	if err != nil {
		return redeemQuote{}, err
	}
	return resolveRedeemQuote(cmd, l, entry, client)
}

// exactSubsetSumNetLimit bounds the subset-sum search below, so a holder
// with an unusually large number of same-Hub tokens degrades to "no exact
// sum found" (same outcome as fragmentation) rather than a 2^N search —
// conservative, and documented rather than silent.
const exactSubsetSumNetLimit = 20

// exactSubsetSumNet finds indices into nets whose values sum to EXACTLY
// target, or reports none found. Exhaustive but bounded
// (exactSubsetSumNetLimit); realistic holdings are small, and this is
// selection logic, not a hot path. Plain []uint64 rather than a generic
// constraint: the only caller already has the net values in hand, and a
// method on a function-local struct type isn't legal Go anyway.
func exactSubsetSumNet(nets []uint64, target uint64) ([]int, bool) {
	if len(nets) > exactSubsetSumNetLimit {
		return nil, false
	}
	n := len(nets)
	for mask := 1; mask < (1 << n); mask++ {
		var sum uint64
		for i := 0; i < n; i++ {
			if mask&(1<<i) != 0 {
				sum += nets[i]
			}
		}
		if sum == target {
			var idxs []int
			for i := 0; i < n; i++ {
				if mask&(1<<i) != 0 {
					idxs = append(idxs, i)
				}
			}
			return idxs, true
		}
	}
	return nil, false
}

// invoiceAmountOverride is --invoice's own interaction with --amount
// (decided in docs/private/amount-first-decisions.md, question 3), mirroring
// how every LN wallet treats a pasted invoice's own amount field: locked
// when the invoice already fixes one, the only source of truth when it
// doesn't.
//
//   - invoice fixes an amount, --amount also given → usage error, naming the
//     invoice's own amount, so the conflict is legible rather than a silent
//     "which one wins".
//   - invoice is open (no encoded amount) and --amount is NOT given → usage
//     error. There is no other source for the figure; letting this reach the
//     Hub as a bare amountless request would fail far less legibly than
//     cashctl can fail it itself.
//   - invoice is open and --amount IS given → that amount is the answer,
//     returned here to travel as redeemPlan.InvoiceAmount. Bound-checked
//     against the bill's own live quote where the plan is built — not here,
//     before any bill is even selected.
//   - no --invoice at all → not called.
func invoiceAmountOverride(cmd *cobra.Command, invoice string) (*uint64, error) {
	amountFlag, _ := cmd.Flags().GetString("amount")
	fixedMloki, err := bolt11AmountMloki(invoice)
	if err != nil {
		return nil, output.InvalidInputError(cmd, invoice, err)
	}
	if fixedMloki != nil {
		if amountFlag != "" {
			return nil, output.InvocationError(cmd, fmt.Errorf(
				"this invoice already asks for %s — --amount is only for an invoice that doesn't fix one",
				output.FormatAmount(*fixedMloki)))
		}
		return nil, nil
	}
	if amountFlag == "" {
		return nil, output.InvocationError(cmd, fmt.Errorf(
			"this invoice doesn't encode an amount — pass --amount to say how much to redeem into it"))
	}
	amt, err := output.ParseAmount(amountFlag)
	if err != nil {
		return nil, output.InvalidInputError(cmd, amountFlag, err)
	}
	return &amt, nil
}
