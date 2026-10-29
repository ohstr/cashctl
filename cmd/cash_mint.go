package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nipIC"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/crashpoint"
	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// cashHubEnv carries the Cash Hub connection out of argv.
const cashHubEnv = "CASHCTL_CASH_HUB"

// purposeMint is a mint_cash call's purpose.
const purposeMint = "mint"

// cashHub is a Cash Hub connection: nipcash/client's HubClient, behind an
// interface so tests can stand in for it.
type cashHub interface {
	Info(ctx context.Context) (*nipcashclient.HubInfo, error)
	MintCash(ctx context.Context, p nipcash.MintCashParams, j nipcashclient.Journal, device string) (*nipcash.MintCashResult, error)
	LookupMint(ctx context.Context, pc nipcash.PendingCall) (nipcashclient.LookupOutcome, error)
	Close()
}

// dialCashHubFn dials a cashhub1 string. Swapped in tests.
var dialCashHubFn = func(ctx context.Context, cashhub string) (cashHub, error) {
	return nipcashclient.ConnectCashHub(ctx, cashhub, sessionOptions()...)
}

func newCashMintCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mint <amount>",
		Short: "Mint a bill on a Cash Hub you hold the connection of",
		Long: `Mints a bill of <amount> over a Cash Hub connection (a cashhub1... string,
from --hub, or the CASHCTL_CASH_HUB environment variable to keep it out of
argv; "-" reads it from stdin). Before anything is sent, the Hub must answer as
the identity and chain the string names; a string whose signature can't be
confirmed is used only after you agree once (or --allow-unverified).

The call is written down with its request id and deadline before it is sent.
A lost reply is looked up by ` + "`cash settle`" + `; a mint the Hub never saw is placed
again only with ` + "`cash settle --replace <call>`" + `.

--to cash (the default) makes a cash note to hand out; --to <your own key>
keeps the bill here; any other target gets it as a gift.`,
		Example: `  CASHCTL_CASH_HUB=cashhub1... cashctl cash mint 100
  cashctl cash mint 100 --hub - --to npub1... < hub.txt`,
		Args: output.ExactArgs(1),
		RunE: runCashMint,
	}
	cmd.Flags().String("hub", "", "the Cash Hub's cashhub1... string (or - for stdin; default $"+cashHubEnv+")")
	cmd.Flags().String("to", "cash", "who the bill is for: cash, your own key, a pubkey/npub/name@domain, or connection:<platform>:<id>:<ia>")
	cmd.Flags().Duration("expiry", 0, "requested expiry (0 = the Hub's own)")
	cmd.Flags().Bool("allow-unverified", false, "use a string whose signature can't be confirmed (asked once per string otherwise)")
	return cmd
}

func runCashMint(cmd *cobra.Command, args []string) error {
	if err := rejectConnectionFlag(cmd); err != nil {
		return err
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	amount, err := output.ParseAmount(args[0])
	if err != nil || amount == 0 {
		if err == nil {
			err = errors.New("a bill needs an amount above zero")
		}
		return output.InvalidInputError(cmd, args[0], err)
	}
	hub, _ := cmd.Flags().GetString("hub")
	if hub == "" {
		hub = os.Getenv(cashHubEnv)
	}
	if hub, err = argOrStdin(cmd, hub); err != nil {
		return err
	}
	if strings.TrimSpace(hub) == "" {
		return output.InvocationError(cmd, fmt.Errorf("a Cash Hub connection is required: --hub <cashhub1...|-> or $%s", cashHubEnv))
	}
	c, err := readConnection(cmd, hub, "cashhub")
	if err != nil {
		return err
	}
	var warnings []string
	if !c.Verified {
		warnings = append(warnings, warnUnverified(jsonMode, c))
	}
	toFlag, _ := cmd.Flags().GetString("to")
	rt, err := credential.ParseTarget(toFlag)
	if err != nil {
		var needsIA *credential.NeedsIAError
		if errors.As(err, &needsIA) {
			return output.InvalidInputError(cmd, toFlag, errors.New("an nconnection needs its Identity Authority: pass connection:<platform>:<id>:<ia-pubkey>"))
		}
		return output.InvalidInputError(cmd, toFlag, err)
	}
	recipient, ok := rt.Target.(nipcash.Recipient)
	if !ok {
		return output.InvalidInputError(cmd, toFlag, errors.New("not a target a bill can be minted to"))
	}
	expiry, _ := cmd.Flags().GetDuration("expiry")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var client cashHub
	var info *nipcashclient.HubInfo
	err = WithSpinner(jsonMode, "Checking the Hub...", func() error {
		var dErr error
		if client, dErr = dialCashHubFn(ctx, c.Raw); dErr != nil {
			return dErr
		}
		info, dErr = client.Info(ctx)
		return dErr
	})
	if client != nil {
		defer client.Close()
	}
	if err != nil {
		if isConnectionMismatch(err) {
			return connectionMismatchErr(cmd, err)
		}
		return output.NetworkError(cmd, fmt.Errorf("couldn't reach the Cash Hub; nothing was sent: %w", err))
	}
	if proceed, err := confirmUnverified(cmd, c); err != nil || !proceed {
		if err == nil {
			output.Println("Not sent.")
		}
		return err
	}
	isTest := testNetwork(jsonMode, info.Network)
	if !info.LookupOffered {
		return output.RuntimeError(cmd, errors.New("this Hub does not offer cash_lookup, so a lost reply could never be recovered; cashctl won't mint on it. Nothing was sent"))
	}

	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	device, err := deviceIDFn()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	spec := callSpec{Purpose: purposeMint, AuthorizerKind: ledger.AuthCashHubClient, TargetCredential: c.Raw, Chain: c.chain()}
	j := &ledgerJournal{l: l, spec: spec}
	var res *nipcash.MintCashResult
	err = WithSpinner(jsonMode, "Minting...", func() error {
		var mErr error
		res, mErr = client.MintCash(callContext(ctx), nipcash.MintCashParams{Recipients: []nipcash.Allocation{nipcash.Send(recipient, amount)}, Expiry: expiry}, j, device)
		return mErr
	})
	if err != nil {
		return placeErr(cmd, l, j, err, "mint")
	}
	crashpoint.Hit("calls.before_apply")
	call := j.call()
	out, err := applyMintResult(cmd, l, call, res)
	if err != nil {
		call.State, call.Detail, call.UpdatedAt = ledger.CallStuck, "invalid_result", time.Now().Unix()
		_ = l.Save()
		return output.WithCall(output.IndeterminateError(cmd, "", fmt.Errorf(
			"the Hub answered, but the bill it returned doesn't check out (%v); the call is kept — `cashctl cash settle` asks again", err)),
			map[string]any{"call": call.CallID, "method": call.Method, "state": call.State, "detail": call.Detail})
	}
	callID := call.CallID
	closeCall(l, call, ledger.CallDone, "")
	if err := l.Save(); err != nil {
		recovery := out.Token
		if out.Handoff != "" {
			recovery = out.Handoff
		}
		return output.RecoveryError(cmd, err, "The bill was minted but could not be saved. Keep it: "+recovery)
	}

	if jsonMode {
		doc := map[string]any{
			"cash_token": out.Token, "wallet_pubkey": res.WalletPubkey, "amount_millis": amount,
			"expires_at": expiryPtr(res.ExpiresAt), "call": callID,
			"network": info.Network, "chain": info.Chain, "test_network": isTest, "hub_verified": c.Verified,
			"warnings": output.NonNil(warnings),
		}
		if out.Handoff != "" {
			doc["handoff"] = out.Handoff
		}
		if out.EntryID != "" {
			doc["entry"] = out.EntryID
		}
		output.PrintJSON(doc)
		return nil
	}
	output.Printf("Minted %s (%s).\n", output.FormatAmount(int64(amount)), out.Summary) //nolint:gosec // amounts fit
	if out.Handoff != "" {
		output.Printf("Hand this out (anyone holding it can take the cash):\n%s\n", out.Handoff)
	} else if out.EntryID == "" {
		output.Printf("Give this to its recipient:\n%s\n", out.Token)
	}
	return nil
}

// mintOutcome is what applyMintResult did with a minted bill.
type mintOutcome struct {
	Token, Handoff, EntryID, Summary string
}

// applyMintResult checks a minted bill and records it: kept here when it is for
// the local identity, else a hand-off (a cash note, or a gift to another
// identity). The token must be the wallet the Hub names and carry the Hub
// identity's mint signature.
func applyMintResult(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, res *nipcash.MintCashResult) (mintOutcome, error) {
	if res == nil || res.CashToken == "" {
		if res != nil && res.TokenTombstone != nil {
			return mintOutcome{Summary: "the bill it made is already spent"}, nil
		}
		return mintOutcome{}, errors.New("no cash_token")
	}
	tok, err := nipcash.Decode(res.CashToken)
	if err != nil {
		return mintOutcome{}, err
	}
	if tok.WalletPubkey != res.WalletPubkey {
		return mintOutcome{}, fmt.Errorf("the token names wallet %s, the reply %s", tok.WalletPubkey, res.WalletPubkey)
	}
	minter, ok := nipcash.VerifyProvenance(tok)
	if !ok || !nipcash.HubIdentityEqual(c.HubIdentity, minter) {
		return mintOutcome{}, errors.New("the bill is not signed by the Hub's identity")
	}
	token := strings.ToLower(res.CashToken)
	req, _ := mintRequestOf(c)
	amount := c.AmountMillis
	out := mintOutcome{Token: token}
	kind, value := "", ""
	if len(req.Recipients) > 0 {
		kind, value = req.Recipients[0].IdentityType, req.Recipients[0].IdentityValue
	}
	switch {
	case kind == "cash":
		secret := callSecretOf(c, ledger.RoleNewIdentity)
		out.Handoff = token + "#" + secret
		l.AddHandoff(ledger.Handoff{Kind: ledger.HandoffCashNote, Handoff: out.Handoff, AmountMillis: amount, Target: "cash", WalletPubkey: res.WalletPubkey, ExpiresAt: expiryPtr(res.ExpiresAt)})
		out.Summary = "a cash note, kept in `cashctl wallet history`"
	case kind == "pubkey" && localPubIs(cmd, value):
		e, err := l.Add(ledger.Entry{Token: token, WalletPubkey: res.WalletPubkey, AmountMillis: &amount, Verified: true,
			MinterPubkey: &minter, IdentityRequired: ptrTo(true), ExpiresAt: expiryPtr(res.ExpiresAt), CreatedByCall: c.CallID})
		if err != nil && !errors.Is(err, ledger.ErrAlreadyHeld) {
			return mintOutcome{}, err
		}
		if e != nil {
			out.EntryID = e.ID
		}
		out.Summary = "kept here"
	default:
		l.AddHandoff(ledger.Handoff{Kind: ledger.HandoffGift, Handoff: token, AmountMillis: amount, Target: "identity", WalletPubkey: res.WalletPubkey, ExpiresAt: expiryPtr(res.ExpiresAt)})
		out.Summary = "for its recipient, kept in `cashctl wallet history`"
	}
	l.AppendHistory("mint", fmt.Sprintf("minted %s", output.FormatAmount(int64(amount)))) //nolint:gosec // amounts fit
	return out, nil
}

// mintRequest is the part of a mint_cash request a lookup or re-placement needs.
type mintRequest struct {
	Recipients []nipcash.RecipientParam `json:"recipients"`
	Expiry     int64                    `json:"expiry"`
}

func mintRequestOf(c *ledger.Call) (mintRequest, error) {
	var req mintRequest
	if err := json.Unmarshal([]byte(c.ParamsJSON), &req); err != nil {
		return req, fmt.Errorf("call %s: unreadable request: %w", c.CallID, err)
	}
	return req, nil
}

// mintParams rebuilds a mint_cash call's params.
func mintParams(c *ledger.Call) (nipcash.MintCashParams, error) {
	req, err := mintRequestOf(c)
	if err != nil {
		return nipcash.MintCashParams{}, err
	}
	p := nipcash.MintCashParams{RequestID: c.RequestID, Expiry: time.Duration(req.Expiry) * time.Second}
	for _, r := range req.Recipients {
		var to nipcash.Recipient
		switch r.IdentityType {
		case "cash":
			ct, err := nipcash.CashTargetFromSecret(callSecretOf(c, ledger.RoleNewIdentity))
			if err != nil {
				return p, fmt.Errorf("call %s keeps no cash secret: %w", c.CallID, err)
			}
			to = ct
		case "pubkey":
			to = nipcash.Pubkey(r.IdentityValue)
		case "connection_key":
			ck, err := nipcash.ResolvedConnectionKey(nipIC.ConnectionKey(r.IdentityValue), "", r.IAPubkey)
			if err != nil {
				return p, err
			}
			to = ck
		default:
			return p, fmt.Errorf("call %s: unknown recipient %q", c.CallID, r.IdentityType)
		}
		p.Recipients = append(p.Recipients, nipcash.Send(to, r.AmountMillis))
	}
	return p, nil
}

// applyMintLookup applies a mint a lookup says happened.
func applyMintLookup(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, raw json.RawMessage) (string, error) {
	p, err := mintParams(c)
	if err != nil {
		return "", err
	}
	res, err := p.ParseLookupResult(raw)
	if err != nil {
		return "", err
	}
	out, err := applyMintResult(cmd, l, c, res)
	if err != nil {
		return "", err
	}
	return "minted: " + out.Summary, nil
}
