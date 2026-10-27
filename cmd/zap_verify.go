package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipCZ"
	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// Cash Zaps (NIP-CZ). A zap bill (scheme 2) is checked only against the
// kind-9522 event it arrived with: decode runs the client rules 1-8 and never
// says "received"; receive runs the wallet rules 1-6, 8 and 9, where rule 9
// (the Hub's own answer about the recipient's slice) is the only proof the cash
// is yours. A Minter neither trusted here nor listed in the recipient's 10522 is
// contacted only after the user agrees.

// reasonMinterUntrusted is a zap whose minter needs the user's approval.
const reasonMinterUntrusted = "minter_untrusted"

// looksLikeZapEvent reports raw JSON of an event (a 9522 is pasted as JSON).
func looksLikeZapEvent(raw string) bool {
	s := strings.TrimSpace(raw)
	return strings.HasPrefix(s, "{") && strings.Contains(s, `"kind"`)
}

// readZapEvent reads a 9522 from raw JSON, a file path, or "-" (stdin).
func readZapEvent(cmd *cobra.Command, value string) (*nip01.Event, string, error) {
	raw := strings.TrimSpace(value)
	switch {
	case raw == stdinArg:
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, "", output.RuntimeError(cmd, err)
		}
		raw = strings.TrimSpace(string(b))
	case !strings.HasPrefix(raw, "{"):
		b, err := os.ReadFile(raw)
		if err != nil {
			return nil, "", output.InvalidInputError(cmd, raw, fmt.Errorf("not a zap event and not a readable file: %w", err))
		}
		raw = strings.TrimSpace(string(b))
	}
	var ev nip01.Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return nil, "", output.InvalidInputError(cmd, "", fmt.Errorf("not a Nostr event: %w", err))
	}
	if ev.Kind != nipCZ.KindCashZap {
		return nil, "", output.InvalidInputError(cmd, "", fmt.Errorf("a kind %d event, not a Cash Zap (kind %d)", ev.Kind, nipCZ.KindCashZap))
	}
	return &ev, raw, nil
}

// minterTrust is the reader's trust: trusted_minters.
type minterTrust map[string]string

func (m minterTrust) TrustsMinter(minterXOnly, chain string) bool {
	return m[strings.ToLower(minterXOnly)+"|"+chain] != ""
}

func loadMinterTrust() minterTrust {
	out := minterTrust{}
	list, err := config.TrustedMinters()
	if err != nil {
		return out
	}
	for _, m := range list {
		out[m.MinterXOnly+"|"+m.Chain] = "1"
	}
	return out
}

// ruleRow is one rule in --json.
type ruleRow struct {
	N      int    `json:"n"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

func ruleRows(v nipCZ.Verdict) []ruleRow {
	rows := make([]ruleRow, 0, len(v.Rules))
	for _, r := range v.Rules {
		reason := r.Reason
		if r.Skipped && reason == "" {
			reason = "skipped"
		}
		rows = append(rows, ruleRow{N: r.N, OK: r.OK, Reason: output.Sanitize(reason)})
	}
	return rows
}

func printRules(v nipCZ.Verdict) {
	for _, r := range v.Rules {
		mark := "ok"
		switch {
		case r.Skipped:
			mark = "skipped"
		case !r.OK:
			mark = "FAILED"
		}
		line := fmt.Sprintf("rule %d: %s", r.N, mark)
		if r.Reason != "" && !r.OK {
			line += " — " + output.Sanitize(r.Reason)
		}
		output.Println(line)
	}
}

// decodeCashZap shows a 9522 under the client rules 1-8 (signature first). It
// never says received; --check adds rule 9 read-only for the local identity.
func decodeCashZap(cmd *cobra.Command, value string, jsonMode, check bool) error {
	ev, _, err := readZapEvent(cmd, value)
	if err != nil {
		return err
	}
	trust := loadMinterTrust()
	var target *nip01.Event
	if tf, _ := cmd.Flags().GetString("target"); tf != "" {
		b, err := os.ReadFile(tf)
		if err != nil {
			return output.InvalidInputError(cmd, tf, err)
		}
		target = &nip01.Event{}
		if err := json.Unmarshal(b, target); err != nil {
			return output.InvalidInputError(cmd, tf, fmt.Errorf("not a Nostr event: %w", err))
		}
	}
	var info *nipCZ.CashZapInfo
	if pz, err := nipCZ.ParseCashZap(ev); err == nil && !pz.Recipient.IsConnectionKey() {
		info, _ = recipientZapInfo(cmd, pz.Recipient.Value)
	}
	v := nipCZ.VerifyClient(ev, target, info, trust)
	out := map[string]any{"type": "cash_zap", "rules": ruleRows(v), "countable": v.Countable, "minter_pubkey": v.Minter}
	if v.Reason != "" {
		out["reason"] = v.Reason
	}
	z, perr := nipCZ.ParseCashZap(ev)
	if perr == nil {
		out["amount_millis"], out["chain"], out["recipient"] = z.Amount, z.Chain, z.Recipient.Value
		out["wallet_pubkey"] = z.W
		if z.Recipient.IsConnectionKey() {
			out["recipient_platform"], out["ia_pubkey"] = z.Recipient.Platform, z.IA
		}
		if z.Sender != nil {
			// P is a connection key, credited only for a Proxy Agent trusted here.
			out["via"] = ev.PubKey
			out["sender_credited"] = v.CreditSender
		}
		if nipCZ.PossiblyExpired(z, time.Now()) {
			out["possibly_expired"] = true
		}
	}
	if check && perr == nil && v.Valid() {
		out["check"] = zapRule9ReadOnly(cmd, ev, trust)
	}
	if jsonMode {
		output.PrintJSON(out)
		return nil
	}
	output.Println("type: cash_zap (kind 9522)")
	printRules(v)
	if perr == nil {
		output.Printf("amount: %s on %s, for %s\n", output.FormatAmount(int64(z.Amount)), output.Sanitize(z.Chain), shortFingerprint(z.Recipient.Value)) //nolint:gosec // amounts fit
		if z.Sender != nil {
			output.Printf("via: %s (%s)\n", shortFingerprint(ev.PubKey), map[bool]string{true: "a Proxy Agent you trust", false: "not credited"}[v.CreditSender])
		}
		if nipCZ.PossiblyExpired(z, time.Now()) {
			output.Println("possibly expired")
		}
	}
	if v.Countable {
		output.Println("countable: yes — this is a zap, not money received; only `cashctl receive` proves that")
	} else {
		output.Printf("countable: no (%s)\n", output.Sanitize(v.Reason))
	}
	if c, ok := out["check"].(map[string]any); ok {
		output.Printf("check: %v\n", c["result"])
	}
	return nil
}

// zapRule9ReadOnly runs rule 9 for the local identity without changing
// anything: cash_status mine. A minter not trusted or listed is not contacted.
func zapRule9ReadOnly(cmd *cobra.Command, ev *nip01.Event, trust nipCZ.Trust) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v := nipCZ.VerifyWallet(ctx, ev, nil, trust, false, func(ctx context.Context, z *nipCZ.CashZap) error {
		return zapStatusMine(ctx, cmd, z)
	})
	r9, _ := v.Rule(9)
	switch {
	case v.Received():
		return map[string]any{"result": "the zap's slice is yours and unclaimed", "rule9": true}
	case v.MinterUnlisted:
		return map[string]any{"result": "not checked: the minter is not trusted here", "rule9": false}
	default:
		return map[string]any{"result": output.Sanitize(r9.Reason), "rule9": false}
	}
}

// zapStatusMine is rule 9 for a pubkey p: the local identity must be p, and
// cash_status mine must show its own unclaimed slice of exactly the amount.
func zapStatusMine(ctx context.Context, cmd *cobra.Command, z *nipCZ.CashZap) error {
	if z.Recipient.IsConnectionKey() {
		return errors.New("a zap to a connection key is checked by claiming it, not read-only")
	}
	mine, err := localPubKeyHex(cmd)
	if err != nil {
		return err
	}
	if mine != z.Recipient.Value {
		return fmt.Errorf("this zap is for %s, not your identity", shortFingerprint(z.Recipient.Value))
	}
	zc, err := nipCZ.ZapContextFromEvent(z)
	if err != nil {
		return err
	}
	cred, err := localCashCredential(cmd)
	if err != nil {
		return err
	}
	client, err := dialZapFn(ctx, z.Cash, zc)
	if err != nil {
		return err
	}
	defer client.Close()
	res, err := client.CashStatus(ctx, cred, "mine")
	if err != nil {
		return err
	}
	return nipCZ.CheckStatus(z, res)
}

// zapStatusClient is the part of a dialled zap bill rule 9 uses.
type zapStatusClient interface {
	CashStatus(ctx context.Context, cred nipcash.Credential, scope string) (*nipcash.CashStatusResult, error)
	Close()
}

// zapInfoFn reads a pubkey's newest Cash Zap Info (kind 10522) and NIP-65 read
// relays (kind 10002) from relays. Swapped in tests.
var zapInfoFn = func(ctx context.Context, relays []string, pubkey string) (*nipCZ.CashZapInfo, []string) {
	f := &nip01.SubscriptionFilter{Kinds: []int{nipCZ.KindCashZapInfo, 10002}, Authors: []string{pubkey}}
	var info, nip65 *nip01.Event
	for _, r := range relays {
		u, err := url.Parse(r)
		if err != nil {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		evs, err := relayclient.ReadEventsFromRelay(rctx, u, nip01.NewSubscriptionFilterGroup(f))
		cancel()
		if err != nil {
			continue
		}
		for _, ev := range evs {
			if ev.PubKey != pubkey || ev.Verify() != nil {
				continue
			}
			switch {
			case ev.Kind == nipCZ.KindCashZapInfo && (info == nil || ev.CreatedAt > info.CreatedAt):
				info = ev
			case ev.Kind == 10002 && (nip65 == nil || ev.CreatedAt > nip65.CreatedAt):
				nip65 = ev
			}
		}
	}
	var parsed *nipCZ.CashZapInfo
	if info != nil {
		parsed, _ = nipCZ.ParseCashZapInfo(info)
	}
	var read []string
	if nip65 != nil {
		for _, t := range nip65.Tags {
			if len(t) >= 2 && t[0] == "r" && (len(t) == 2 || t[2] == "read") {
				read = append(read, t[1])
			}
		}
	}
	return parsed, read
}

// recipientZapInfo is a recipient's 10522 and NIP-65 read relays, read from
// --relay; nil without relays (rule 8 then trusts the local list only).
func recipientZapInfo(cmd *cobra.Command, pubkey string) (*nipCZ.CashZapInfo, []string) {
	relays, _ := cmd.Flags().GetStringSlice("relay")
	if len(relays) == 0 || pubkey == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return zapInfoFn(ctx, relays, pubkey)
}

// receiveZap receives the bill a 9522 carries: rules 1-6 and 8 before any
// network but the recipient's 10522, the user's approval for an untrusted
// minter, then rule 9: for a pubkey p, the Hub's own row for the local
// identity; for a connection key p, a full in-place transfer to the local
// pubkey signed with that key (--as connection-key:...), placed through the
// call engine.
func receiveZap(cmd *cobra.Command, ev *nip01.Event, raw string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	trust := loadMinterTrust()
	approved, _ := cmd.Flags().GetBool("trust-minter")
	z, err := nipCZ.ParseCashZap(ev)
	if err != nil {
		pre := nipCZ.VerifyWallet(context.Background(), ev, nil, trust, approved, nil)
		return output.InvalidInputError(cmd, "", fmt.Errorf("this Cash Zap fails %s", firstFailed(pre)))
	}
	mine, idErr := localPubKeyHex(cmd)
	var cred nipcash.Credential
	var info *nipCZ.CashZapInfo
	if z.Recipient.IsConnectionKey() {
		as := asCredentialValue(cmd)
		if !strings.HasPrefix(as, "connection-key:") {
			return output.UsageError(cmd, fmt.Errorf(
				"this zap is for a %s connection key: claim it with --as connection-key:<privkey>,%s,<external-id>,<attestation-file>",
				output.Sanitize(z.Recipient.Platform), output.Sanitize(z.Recipient.Platform)))
		}
		if cred, err = credential.ParseCash(as); err != nil {
			return output.InvalidInputError(cmd, output.RedactSecretInput(as), err)
		}
		if idErr != nil {
			return output.RuntimeError(cmd, fmt.Errorf("a claimed zap moves to your local identity, which isn't set up: %w", idErr))
		}
		output.Notef(jsonMode, "This zap names Identity Authority %s for its connection key.", shortFingerprint(z.IA))
	} else {
		if idErr != nil || mine != z.Recipient.Value {
			return output.InvalidInputError(cmd, "", fmt.Errorf("this zap is for %s, not your identity", shortFingerprint(z.Recipient.Value)))
		}
		info, _ = recipientZapInfo(cmd, z.Recipient.Value)
	}
	pre := nipCZ.VerifyWallet(context.Background(), ev, info, trust, approved, nil)
	if !pre.Valid() {
		return output.InvalidInputError(cmd, "", fmt.Errorf("this Cash Zap fails %s", firstFailed(pre)))
	}
	if pre.MinterUnlisted && !approved {
		if jsonMode {
			return output.WithReason(output.UsageError(cmd, errors.New(
				"this zap's minter is neither trusted here nor listed by you, so its network can't be verified; nothing was contacted. Pass --trust-minter only if you trust it")), reasonMinterUntrusted)
		}
		if !Confirm(cmd, false, fmt.Sprintf("The minter %s is not one you trust, so its network can't be verified. Contact it to check this zap?", shortFingerprint(pre.Minter))) {
			output.Println("Not received.")
			return nil
		}
		approved = true
	}
	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	if held, ok := l.FindByToken(z.Cash); ok && (held.Status == ledger.StatusHeld || held.Status == ledger.StatusZapClaiming) {
		return output.ConflictError(cmd, "", ledger.ErrAlreadyHeld)
	}
	minter := pre.Minter
	amount := z.Amount
	entry := ledger.Entry{Token: strings.ToLower(z.Cash), WalletPubkey: z.W, Secret: z.Token.Secret, RelayURLs: z.Token.RelayURLs,
		IdentityRequired: ptrTo(true), AmountMillis: &amount, Verified: true, MinterPubkey: &minter, ZapEvent: raw}
	if z.Recipient.IsConnectionKey() {
		entry.ConnectionKeyPlatform, entry.IAPubkey = z.Recipient.Platform, z.IA
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var v nipCZ.Verdict
	var claimErr error
	var claimed *ledger.Entry
	if z.Recipient.IsConnectionKey() {
		// The claim is a call: its bill is written down first, not held until
		// the Hub answers.
		claimed, err = l.Add(entry)
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
		_ = l.SetStatus(claimed.ID, ledger.StatusZapClaiming)
		if err := l.Save(); err != nil {
			return output.RuntimeError(cmd, err)
		}
	}
	_ = WithSpinner(jsonMode, "Checking the zap with its Hub...", func() error {
		v = nipCZ.VerifyWallet(ctx, ev, info, trust, approved, func(ctx context.Context, z *nipCZ.CashZap) error {
			if !z.Recipient.IsConnectionKey() {
				return zapStatusMine(ctx, cmd, z)
			}
			claimErr = claimZap(ctx, cmd, l, claimed, z, cred, mine)
			return claimErr
		})
		return nil
	})
	if claimErr != nil {
		var ce *output.CLIError
		isCLI := errors.As(claimErr, &ce)
		if isCLI && ce.Code == output.CodeIndeterminate {
			return claimErr
		}
		if e, ok := l.Find(claimed.ID); ok && e.Status == ledger.StatusZapClaiming {
			_ = l.SetStatus(e.ID, ledger.StatusNotReceived)
			_ = l.Save()
		}
		if isCLI {
			return claimErr
		}
	}
	if !v.Received() {
		r9, _ := v.Rule(9)
		if r9.Err != nil && (errors.Is(r9.Err, context.DeadlineExceeded) || errors.Is(r9.Err, context.Canceled)) {
			return output.NetworkError(cmd, fmt.Errorf("the Hub didn't answer, which says nothing about this zap; try again: %w", r9.Err))
		}
		if errors.Is(r9.Err, nipCZ.ErrRule9Gone) {
			return output.SpentError(cmd, "", errors.New("the Hub says this zap's bill is gone"))
		}
		return output.InvalidInputError(cmd, "", fmt.Errorf("not received: %s", firstFailed(v)))
	}
	var expires *int64
	if v.ExpiresAtTrusted && z.ExpiresAt != nil && *z.ExpiresAt > 0 {
		at := int64(*z.ExpiresAt) //nolint:gosec // unix time
		expires = &at
	}
	added := claimed
	if added == nil {
		entry.ExpiresAt = expires
		if added, err = l.Add(entry); err != nil {
			return output.RuntimeError(cmd, err)
		}
	} else if e, ok := l.Find(added.ID); ok {
		e.ExpiresAt = expires
		added = e
	}
	l.AppendHistory("receive", fmt.Sprintf("received a %s Cash Zap", output.FormatAmount(int64(amount)))) //nolint:gosec // amounts fit
	if err := l.Save(); err != nil {
		return output.RuntimeError(cmd, err)
	}
	if jsonMode {
		output.PrintJSON(map[string]any{"received": true, "entry": added, "zap": true, "rules": ruleRows(v), "minter_pubkey": minter})
		return nil
	}
	output.Printf("Received a Cash Zap of %s (entry %s).\n", output.FormatAmount(int64(amount)), added.ID) //nolint:gosec // amounts fit
	output.Println("Tip: `cashctl consolidate` re-issues it as an ordinary bill, which unlinks it from the public zap.")
	return nil
}

// claimZap is rule 9 for a connection-key zap: a full in-place cash_transfer
// of the zap bill to the local pubkey, signed with the connection key, placed
// through the call engine (written ahead, looked up when lost), and checked
// to be exactly that (nipCZ.CheckInPlaceTransfer).
func claimZap(ctx context.Context, cmd *cobra.Command, l *ledger.Ledger, e *ledger.Entry, z *nipCZ.CashZap, cred nipcash.Credential, mine string) error {
	caller, err := dialCallerFn(ctx, z.Cash)
	if err != nil {
		return err
	}
	defer caller.Close()
	spec := callSpec{Purpose: purposeZapClaim, SourceEntryIDs: []string{e.ID}, AuthorizerKind: ledger.AuthAsCredential, TargetCredential: e.Token}
	res, call, err := placeTransfer(ctx, cmd, l, caller, spec, nipcash.CashTransferParams{Credential: cred, To: nipcash.Pubkey(mine), CurrentAmount: z.Amount})
	if err != nil {
		return err
	}
	if err := nipCZ.CheckInPlaceTransfer(z, res); err != nil {
		// An answer that doesn't check out keeps the call and the bill.
		call.State, call.Detail = ledger.CallStuck, "invalid_result"
		_ = l.Save()
		return output.IndeterminateError(cmd, "", fmt.Errorf("the Hub's answer to the claim doesn't check out (%v); the claim is kept", err))
	}
	if e, ok := l.Find(e.ID); ok {
		claimedByLocalKey(e)
	}
	closeCall(l, call, ledger.CallDone, "")
	return nil
}

func firstFailed(v nipCZ.Verdict) string {
	for _, r := range v.Rules {
		if !r.OK && !r.Skipped {
			return fmt.Sprintf("rule %d: %s", r.N, output.Sanitize(r.Reason))
		}
	}
	for _, r := range v.Rules {
		if r.Skipped {
			return fmt.Sprintf("rule %d: %s", r.N, output.Sanitize(r.Reason))
		}
	}
	return "its checks"
}

// findZapEvents queries relays for the 9522s carrying a zap bill (#w), only
// on relays the user named: never the token's own hints.
var findZapEvents = func(ctx context.Context, relays []string, walletPubkey string) ([]*nip01.Event, error) {
	var out []*nip01.Event
	var lastErr error
	for _, r := range relays {
		u, err := url.Parse(r)
		if err != nil {
			lastErr = err
			continue
		}
		f := &nip01.SubscriptionFilter{Kinds: []int{nipCZ.KindCashZap}, Tags: map[string][]string{"w": {walletPubkey}}}
		evs, err := relayclient.ReadEventsFromRelay(ctx, u, nip01.NewSubscriptionFilterGroup(f))
		if err != nil {
			lastErr = err
			continue
		}
		out = append(out, evs...)
	}
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

// findZapFor finds, on the given relays, the 9522 a zap bill came with.
func findZapFor(cmd *cobra.Command, tok nipcash.Token) (*nip01.Event, string, error) {
	relays, _ := cmd.Flags().GetStringSlice("relay")
	if len(relays) == 0 {
		return nil, "", output.InvocationError(cmd, errors.New("--find-zap needs --relay <url> (never the token's own relay hints)"))
	}
	// Also where the local identity reads its zaps: its own 10522 relays, else
	// its NIP-65 read relays (NIP-CZ IncomingRelays). Never the token's hints.
	if mine, err := localPubKeyHex(cmd); err == nil {
		own, read := recipientZapInfo(cmd, mine)
		relays = append(relays, nipCZ.IncomingRelays(own, read, nil)...)
		slices.Sort(relays)
		relays = slices.Compact(relays)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	evs, err := findZapEvents(ctx, relays, tok.WalletPubkey)
	if err != nil {
		return nil, "", output.NetworkError(cmd, err)
	}
	var zaps []*nipCZ.CashZap
	for _, ev := range evs {
		if z, err := nipCZ.ParseCashZap(ev); err == nil && strings.EqualFold(z.W, tok.WalletPubkey) {
			zaps = append(zaps, z)
		}
	}
	if len(zaps) == 0 {
		return nil, "", output.NotFoundError(cmd, "", errors.New("no Cash Zap for this bill on those relays"))
	}
	z, err := nipCZ.SelectTrustedCopy(zaps, nil, loadMinterTrust())
	if err != nil {
		z, err = nipCZ.SelectCopy(zaps)
		if err != nil {
			return nil, "", output.InvalidInputError(cmd, "", err)
		}
	}
	b, _ := json.Marshal(z.Event)
	return z.Event, string(b), nil
}

// claimedByLocalKey records a zap bill a claim moved onto the local pubkey: no
// longer bound to the connection key, held.
func claimedByLocalKey(e *ledger.Entry) {
	e.ConnectionKeyPlatform, e.ConnectionKeyExternalID, e.AttestationEventID, e.IAPubkey = "", "", "", ""
	e.IdentityRequired = ptrTo(true)
	e.Status = ledger.StatusHeld
}
