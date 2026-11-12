package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nip19"
	"github.com/ohstr/nmilat/nipCZ"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// Cash Zap send (NIP-CZ §Sending). A zap is a cash_transfer of the zap amount
// to the recipient's identity — with zap_target for a pubkey recipient on an
// event — placed through the SAME write-ahead/settle call engine a plain
// transfer uses (placeTransfer), so a lost reply recovers identically. On the
// reply, a kind-9522 is built from the bill's SIGNED issued_at/expires_at and
// published to the recipient's relays. The recipient's own token is also kept
// locally as a hand-off (wallet history) so a failed publish never strands it.

// newCashZapCmd is the `cash zap` group (currently just `send`). The user's own
// kind-10522 lives under `cash zap-info`, unchanged.
func newCashZapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "zap",
		Short: "Send a Cash Zap (NIP-CZ): pay a zap with a held cash token",
		RunE:  groupRunE,
	}
	cmd.AddCommand(newCashZapSendCmd())
	return cmd
}

func newCashZapSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send [amount] [recipient]",
		Short: "Zap a recipient with a held cash token, publishing a kind-9522",
		Long: `Sends a Cash Zap: transfers the zap amount from a held, pubkey-bound token to the
recipient's identity and publishes a kind-9522 carrying it. Zapping a nostr event
(--event) with a pubkey recipient makes a countable zap; a zap to a connection key
(--platform + --ia) or with no event is valid but never counted.

--json fields:
  zap_event_id      the published kind-9522's id
  zap_event         the signed kind-9522
  amount_millis     the zap amount
  recipient         the recipient's pubkey (or connection key)
  countable         whether it is a countable zap, with reason when not
  published_to      relays that accepted the 9522
  relays_failed     relays that did not
  recipient_token   the token the recipient can also receive directly (kept in
                    wallet history until claimed, so a failed publish never loses it)`,
		Example: `  cashctl cash zap send 21 <recipient-pubkey> --event note.json
  cashctl cash zap send 100 <connection-key> --platform discord --ia <ia-pubkey>`,
		Args: output.MaximumNArgs(2),
		RunE: runCashZapSend,
	}
	cmd.Flags().String("to", "", "recipient — hex pubkey or npub1… (or a connection key with --platform)")
	cmd.Flags().String("amount", "", fmt.Sprintf("the zap amount, in %s", output.CurrencyUnit))
	cmd.Flags().String("event", "", "the event being zapped: a JSON event file, '-' for stdin, or raw JSON (pubkey recipient only)")
	cmd.Flags().String("content", "", "optional zap comment (the 9522 content)")
	cmd.Flags().String("platform", "", "the recipient is a connection key on this platform (e.g. discord, x) — requires --ia")
	cmd.Flags().String("ia", "", "Identity Authority (hex pubkey) vouching for a connection-key recipient")
	cmd.Flags().String("token", "", "which held token to zap from (auto-picked if omitted)")
	cmd.Flags().String("as", "", "override credential")
	cmd.Flags().StringSlice("relay", nil, "also publish the 9522 to these relays (repeatable)")
	return cmd
}

// zapRecipient resolves the --to/positional recipient to a nipCZ.Identity: a
// pubkey (hex or npub) by default, or a connection key when --platform is set
// (which then requires --ia). Phase Z takes a connection key as raw hex; it does
// not derive one from a platform user id (NIP-IC), which is a later step.
func zapRecipient(cmd *cobra.Command, toValue string) (nipCZ.Identity, error) {
	platform, _ := cmd.Flags().GetString("platform")
	ia, _ := cmd.Flags().GetString("ia")
	toValue = strings.TrimSpace(toValue)
	if platform != "" {
		if !isHexKey(strings.ToLower(toValue)) {
			return nipCZ.Identity{}, output.InvalidInputError(cmd, toValue, errors.New("a connection-key recipient is given as its 64-hex connection key"))
		}
		if !isHexKey(strings.ToLower(ia)) {
			return nipCZ.Identity{}, output.InvocationError(cmd, errors.New("a connection-key recipient needs --ia <ia-pubkey>"))
		}
		return nipCZ.Identity{Value: strings.ToLower(toValue), Platform: platform}, nil
	}
	hex := strings.ToLower(toValue)
	if strings.HasPrefix(hex, "npub1") {
		h, err := nip19.DecodePublicKey(toValue)
		if err != nil {
			return nipCZ.Identity{}, output.InvalidInputError(cmd, toValue, err)
		}
		hex = h
	}
	if !isHexKey(hex) {
		return nipCZ.Identity{}, output.InvalidInputError(cmd, toValue, errors.New("recipient must be a hex pubkey or npub1… (or a connection key with --platform)"))
	}
	return nipCZ.Identity{Value: hex}, nil
}

// zapTargetEvent reads and verifies the --event being zapped (pubkey recipient
// only). nil when --event is absent.
func zapTargetEvent(cmd *cobra.Command, recipient nipCZ.Identity) (*nip01.Event, error) {
	ev, _ := cmd.Flags().GetString("event")
	if strings.TrimSpace(ev) == "" {
		return nil, nil
	}
	if recipient.IsConnectionKey() {
		return nil, output.InvocationError(cmd, errors.New("a connection-key zap carries no event; drop --event"))
	}
	raw := strings.TrimSpace(ev)
	switch {
	case raw == stdinArg:
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, output.RuntimeError(cmd, err)
		}
		raw = strings.TrimSpace(string(b))
	case !strings.HasPrefix(raw, "{"):
		b, err := os.ReadFile(raw)
		if err != nil {
			return nil, output.InvalidInputError(cmd, raw, fmt.Errorf("not JSON and not a readable file: %w", err))
		}
		raw = strings.TrimSpace(string(b))
	}
	var target nip01.Event
	if err := json.Unmarshal([]byte(raw), &target); err != nil {
		return nil, output.InvalidInputError(cmd, "", fmt.Errorf("not a Nostr event: %w", err))
	}
	// Rule 7 trusts the target's tags only after its signature.
	if err := target.Verify(); err != nil {
		return nil, output.InvalidInputError(cmd, "", fmt.Errorf("the event's signature doesn't check out: %w", err))
	}
	return &target, nil
}

// pickZapSource chooses the held, pubkey-bound token to zap from: --token when
// given, else the PlanSend ordering (a recipient-listed minter first, then the
// longest life) over the bills bound to the local pubkey. The chosen bill must
// be bound to self, since the local key both proves the transfer and signs the
// 9522.
func pickZapSource(cmd *cobra.Command, l ledger.Store, self string, info *nipCZ.CashZapInfo) (*ledger.Entry, error) {
	if tok, _ := cmd.Flags().GetString("token"); tok != "" {
		e, err := resolveHeldToken(cmd, l)
		if err != nil {
			return nil, err
		}
		return e, nil
	}
	held, err := pickableHeld(cmd, l)
	if err != nil {
		return nil, err
	}
	var bound []ledger.Entry
	byWallet := map[string]*ledger.Entry{}
	var bills []nipcash.Token
	for i := range held {
		e := held[i]
		if !strings.EqualFold(e.IdentityPubkey, self) {
			continue
		}
		tok, derr := nipcash.Decode(e.Token)
		if derr != nil {
			continue
		}
		bound = append(bound, e)
		cp := e
		byWallet[strings.ToLower(tok.WalletPubkey)] = &cp
		bills = append(bills, tok)
	}
	if len(bound) == 0 {
		return nil, output.NotFoundError(cmd, "", errors.New(
			"no held token bound to your identity to zap from; receive one to your pubkey first, or move one with `cashctl transfer <amount> <your-pubkey>`"))
	}
	ordered, perr := nipCZ.PlanSend(info, bills, time.Now())
	if perr != nil {
		// No bill on a chain the recipient accepts: surface it.
		return nil, output.InvalidInputError(cmd, "", perr)
	}
	if e, ok := byWallet[strings.ToLower(ordered[0].WalletPubkey)]; ok {
		return e, nil
	}
	return &bound[0], nil
}

func runCashZapSend(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	toFlag, _ := cmd.Flags().GetString("to")
	amountFlag, _ := cmd.Flags().GetString("amount")

	positionalTo, positionalAmount := disambiguateTransferArgs(args)
	toValue, err := resolvePositionalOrFlag(cmd, positionalTo, "to", toFlag)
	if err != nil {
		return err
	}
	if toValue == "" {
		return output.InvocationError(cmd, errors.New("say who to zap: a recipient pubkey/npub (cashctl cash zap send <amount> <recipient>)"))
	}
	amount, err := resolvePositionalOrFlagAmount(cmd, positionalAmount, "amount", amountFlag)
	if err != nil {
		return err
	}
	if amount == 0 {
		return output.UsageError(cmd, errors.New("the zap amount must be greater than 0"))
	}

	self, err := requireIdentity(cmd, "sending a Cash Zap")
	if err != nil {
		return err
	}
	recipient, err := zapRecipient(cmd, toValue)
	if err != nil {
		return err
	}
	ia, _ := cmd.Flags().GetString("ia")
	if recipient.IsConnectionKey() {
		ia = strings.ToLower(strings.TrimSpace(ia))
	} else {
		ia = ""
	}
	target, err := zapTargetEvent(cmd, recipient)
	if err != nil {
		return err
	}

	l, err := loadStore(cmd)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	defer func() { _ = l.Close() }()
	if err := settleBeforeSpend(cmd, l); err != nil {
		return err
	}

	// The recipient's 10522 both filters the source minter (PlanSend) and gives
	// the publish relays. Read only for a pubkey recipient (a connection key has
	// none).
	var info *nipCZ.CashZapInfo
	var recipRead []string
	if !recipient.IsConnectionKey() {
		info, recipRead = recipientZapInfo(cmd, recipient.Value)
	}

	entry, err := pickZapSource(cmd, l, self, info)
	if err != nil {
		return err
	}
	if !strings.EqualFold(entry.IdentityPubkey, self) {
		return output.UsageError(cmd, fmt.Errorf(
			"token %s is not bound to your identity, so it can't sign this zap; move it with `cashctl transfer <amount> %s` first", entry.ID, npubOrHex(self)))
	}
	if err := refuseLocked(cmd, l, []string{entry.ID}); err != nil {
		return err
	}
	cred, err := resolveCredential(cmd, entry)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var client *nipcashclient.Client
	err = WithSpinner(jsonMode, "Connecting...", func() error {
		c, cErr := dialBill(ctx, entry.Token)
		if cErr != nil {
			return cErr
		}
		client = c
		return nil
	})
	if err != nil {
		return classifyCashTokenNWCErr(cmd, err)
	}
	defer client.Close()

	var billAmount uint64
	err = WithSpinner(jsonMode, "Checking amount...", func() error {
		a, aErr := resolveAmount(cmd, l, entry, client)
		billAmount = a
		return aErr
	})
	if err != nil {
		return err
	}
	if amount > billAmount {
		return output.UsageError(cmd, fmt.Errorf("can't zap %s — the chosen token only holds %s", output.FormatAmount(int64(amount)), output.FormatAmount(int64(billAmount)))) //nolint:gosec // amounts fit
	}

	// Build + validate the transfer params with the SDK (connection-key resolution,
	// target/ia rules), then set the real held amount so a partial zap leaves the
	// correct remainder (ZapTransfer defaults CurrentAmount to the zap amount).
	params, err := nipCZ.ZapTransfer(cred, true, recipient, ia, amount, target)
	if err != nil {
		return output.InvalidInputError(cmd, "", err)
	}
	params.CurrentAmount = billAmount

	what := fmt.Sprintf("Zap %s to %s", output.FormatAmount(int64(amount)), shortFingerprint(recipient.Value)) //nolint:gosec // amounts fit
	if target != nil {
		what += " on an event"
	}
	if !Confirm(cmd, false, what+"?") {
		output.Println("Cancelled.")
		return nil
	}

	kind, authPub, authSecret := authorizerOf(cmd, entry, cred)
	spec := callSpec{Purpose: purposeTransfer, SourceEntryIDs: []string{entry.ID}, AuthorizerKind: kind,
		AuthorizerPubkey: authPub, AuthorizerSecret: authSecret, TargetCredential: entry.Token}
	entryID := entry.ID
	var result *nipcash.CashTransferResult
	var call *ledger.Call
	err = WithSpinner(jsonMode, "Zapping...", func() error {
		var cErr error
		result, call, cErr = placeTransfer(ctx, cmd, l, client, spec, params)
		return cErr
	})
	if err != nil {
		return err
	}
	entry, _ = l.Find(entryID)
	_ = l.SetStatus(entry.ID, ledger.StatusTransferred)

	// Build the 9522 from the reply's SIGNED issued_at/expires_at. The signer is
	// the local key the bill is bound to. If this fails the value has still moved,
	// so it is saved as an ordinary transfer and reported as "not a countable zap".
	signerPriv, err := localPrivKey(cmd)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	content, _ := cmd.Flags().GetString("content")
	zapEv, zerr := nipCZ.NewCashZap(nipCZ.CashZapParams{
		Recipient: recipient, Amount: amount, IA: ia, Target: target, Content: content, Transfer: result,
	}, signerPriv)

	var accepted, failed []string
	if zerr == nil {
		writeRelays := userRelays(cmd, self)
		extra, _ := cmd.Flags().GetStringSlice("relay")
		relays := nipCZ.PublishRelays(info, recipRead, append(writeRelays, extra...), nil)
		if len(relays) > 0 {
			pctx, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
			accepted, failed = publishEventFn(pctx, relays, zapEv)
			pcancel()
		}
	}

	return saveZapResult(cmd, l, result, call, entry, amount, recipient, zapEv, zerr, target, accepted, failed)
}

// saveZapResult applies the zap's cash_transfer to the ledger — the same
// remainder, hand-off, close-call and save discipline as a plain transfer
// (printAndSaveTransferResult) — then renders the zap result. The recipient's
// token is kept as a hand-off so a failed 9522 publish never strands it.
//
// zapErr != nil means the transfer landed but no 9522 could be built (e.g. the
// Hub returned no zap signature): the value is kept and the result is reported
// as "not a countable zap", never lost.
func saveZapResult(cmd *cobra.Command, l ledger.Store, result *nipcash.CashTransferResult, call *ledger.Call, entry *ledger.Entry,
	amount uint64, recipient nipCZ.Identity, zapEv *nip01.Event, zapErr error, target *nip01.Event, accepted, failed []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	closeCall(l, call, ledger.CallDone, "")
	l.AppendHistory("zap", fmt.Sprintf("zapped %s to %s", output.FormatAmount(int64(amount)), shortFingerprint(recipient.Value))) //nolint:gosec // amounts fit

	// Remainder handling mirrors printAndSaveTransferResult (cash_transfer.go): a
	// split leaves a new wallet under the source's own identity.
	var remainderEntry *ledger.Entry
	if result.RemainderWalletToken != "" {
		remainder := credentialModeOf(*entry)
		remainder.MinterPubkey = entry.MinterPubkey
		remainder.Token = strings.ToLower(result.RemainderWalletToken)
		remainder.WalletPubkey = result.RemainderWalletPubkey
		remainder.Verified, remainder.MinterPubkey = returnedBillCheck(remainder.Token, entry.MinterPubkey)
		if !remainder.Verified {
			output.Notef(jsonMode, unverifiedReturnNote)
		}
		remainder.CreatedByCall = ownCallID(call)
		remainderEntry, _ = l.Add(remainder)
		if result.RemainingAmountMillis != nil && *result.RemainingAmountMillis <= maxSaneAmountMillis {
			remainderEntry.AmountMillis = result.RemainingAmountMillis
		}
	}

	// The recipient's own token — what they can `cashctl receive` directly, the
	// same string the published 9522 carries. Kept as a hand-off so a failed
	// publish is recoverable (TODO(phase-Z): auto-republish on `cash settle`).
	recipientToken, rtErr := result.RecipientToken(entry.Token)
	if rtErr != nil {
		recipientToken = ""
	}
	recordHandoff(l, "", recipientToken, amount, "Cash Zap to "+shortFingerprint(recipient.Value))

	if err := l.Save(); err != nil {
		var parts []string
		if recipientToken != "" {
			parts = append(parts, "the recipient's own token — save this now, it will not be shown again: "+recipientToken)
		}
		if remainder := entryRecoveryString(remainderEntry); remainder != "" {
			parts = append(parts, "your own remainder: "+remainder)
		}
		recovery := ""
		if len(parts) > 0 {
			recovery = "Save these: " + strings.Join(parts, "; ")
		}
		return reportUnsavedResult(cmd, err, "Zap", recovery)
	}

	// countable: a pubkey zap on an event whose reply was zap-signed and that a
	// relay accepted. Everything else is valid but never counted (NIP-CZ §Counting).
	countable := false
	reason := ""
	switch {
	case recipient.IsConnectionKey():
		reason = "a zap to a connection key is never counted"
	case target == nil:
		reason = "a zap on no event is never counted"
	case zapErr != nil:
		reason = "the Hub returned no zap signature, so it is not counted"
	case len(accepted) == 0:
		reason = "no relay accepted the 9522"
	default:
		countable = true
	}

	if jsonMode {
		out := map[string]any{
			"zapped": true, "amount_millis": amount, "recipient": recipient.Value,
			"countable": countable, "published_to": output.NonNil(accepted), "relays_failed": output.NonNil(failed),
		}
		if reason != "" {
			out["reason"] = reason
		}
		if zapEv != nil {
			out["zap_event_id"], out["zap_event"] = zapEv.ID, zapEv
		}
		if zapErr != nil {
			out["zap_error"] = output.Sanitize(zapErr.Error())
		}
		if recipient.IsConnectionKey() {
			out["recipient_platform"] = recipient.Platform
		}
		if recipientToken != "" {
			out["recipient_token"] = recipientToken
		}
		if remainderEntry != nil {
			out["remainder_entry"] = remainderEntry
		}
		output.PrintJSON(out)
		return nil
	}

	if zapErr != nil {
		output.Printf("Transferred %s to %s, but no Cash Zap could be published (%s). The value moved; the recipient can receive it with the token below.\n",
			output.FormatAmount(int64(amount)), shortFingerprint(recipient.Value), output.Sanitize(zapErr.Error())) //nolint:gosec // amounts fit
		if recipientToken != "" {
			output.Printf("Give this to them: cashctl receive %s\n", recipientToken)
		}
		return nil
	}
	output.Printf("Zapped %s to %s (9522 %s).\n", output.FormatAmount(int64(amount)), shortFingerprint(recipient.Value), zapEv.ID) //nolint:gosec // amounts fit
	if len(accepted) > 0 {
		output.Printf("Published to %s.\n", strings.Join(accepted, ", "))
	} else {
		output.Println("No relay accepted the 9522 yet. The recipient's token is kept in `cashctl wallet history` — hand it over or re-publish.")
	}
	if !countable {
		output.Printf("Note: %s.\n", reason)
	}
	return nil
}
