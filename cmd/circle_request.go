package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipcw"
	nipcwclient "github.com/ohstr/nmilat/nipcw/client"
	"github.com/ohstr/nmilat/utils"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/crashpoint"
	"github.com/ohstr/cashctl/internal/output"
)

// Member requests (NIP-CW §Recovering a Lost Connection). The member signs a
// kind-23199 request with the join's key; the circle's host submits it to the
// Hub through its own administration, so cashctl never sends it. A rotate's
// new connection key is made here and written ahead before the request is
// shown; `cash settle` then looks it up with the join lookup naming that key,
// and installs it in place of the old one once the rotation completes.

const (
	reasonRotationNotServed = "rotation_not_served"
	reasonRotationOpen      = "rotation_open"
	reasonMemberMismatch    = "member_mismatch"
	reasonRotationNotMade   = "rotation_not_made"
)

// memberRequestValidity is how long a Hub accepts a member request after its
// created_at: 24 hours.
const memberRequestValidity = 86400

func newCircleRequestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "request <rotate|freeze|unfreeze> [wallet]",
		Short: "Sign a member request (rotate, freeze, unfreeze) for the circle's host to submit",
		Long: `Signs a member request for one of your circle wallets with the key that joined
it, and prints it: give it to the circle's host, who submits it to the Hub. It is
valid for 24 hours. cashctl never sends it itself.

  rotate    moves the wallet to a new connection key made on this device, written
            down before the request is printed. After the Hub's rotation delay,
            ` + "`cashctl cash settle`" + ` installs the new key in place of the old one. Until
            then, using the wallet with its old key cancels the rotation.
  freeze    makes the Hub refuse every request by the wallet's current key, at once
            (for a lost or stolen device).
  unfreeze  lifts a freeze.

[wallet] is a registered circle wallet's name or wallet_pubkey (default: the
current wallet). For a wallet not held here (a lost device), pass --wallet
<wallet_pubkey> and --hub <circlehub1...> of its circle.`,
		Example: `  cashctl circle request freeze --wallet <wallet_pubkey> --hub circlehub1...
  cashctl circle request rotate circle-1
  cashctl circle request unfreeze circle-1 --json`,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := output.MinimumNArgs(1)(cmd, args); err != nil {
				return err
			}
			return output.MaximumNArgs(2)(cmd, args)
		},
		RunE: runCircleRequest,
	}
	cmd.Flags().String("wallet", "", "the wallet's wallet_pubkey, for a wallet not registered here")
	cmd.Flags().String("hub", "", "the circle's circlehub1... string (needed for a wallet not registered here, and for a rotate when none is held)")
	cmd.Flags().String("as", "", "the key that joined, pubkey:<privkey> (defaults to your local identity)")
	return cmd
}

// requestWallet is the wallet a member request names.
type requestWallet struct {
	name   string
	wallet config.CircleWallet
	held   bool
}

func runCircleRequest(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	action := nipcw.MemberAction(strings.ToLower(args[0]))
	switch action {
	case nipcw.ActionRotate, nipcw.ActionFreeze, nipcw.ActionUnfreeze:
	default:
		return output.InvocationErrorInput(cmd, args[0], fmt.Errorf("unknown member request %q: rotate, freeze or unfreeze", args[0]))
	}
	ref := ""
	if len(args) > 1 {
		ref = args[1]
	}
	walletFlag, _ := cmd.Flags().GetString("wallet")
	hubFlag, _ := cmd.Flags().GetString("hub")

	var hub *conn
	if hubFlag != "" {
		raw, err := argOrStdin(cmd, hubFlag)
		if err != nil {
			return err
		}
		if hub, err = readCircleHub(cmd, raw); err != nil {
			return err
		}
	}
	rw, err := resolveRequestWallet(cmd, ref, walletFlag, hub)
	if err != nil {
		return err
	}
	w := rw.wallet
	if !w.Rotatable() {
		return output.WithReason(output.UsageError(cmd, errors.New(
			"this wallet's details carry no rotation delay, so its Hub can't rotate, freeze or unfreeze it; nothing was signed")), reasonRotationNotServed)
	}

	priv, err := memberPrivKey(cmd)
	if err != nil {
		return err
	}
	signer, err := utils.GetPublicKey(priv)
	if err != nil {
		return output.InvalidInputError(cmd, "", fmt.Errorf("--as: %w", err))
	}
	if w.MemberPubkey != "" && w.MemberPubkey != signer {
		return output.WithReason(output.AuthError(cmd, fmt.Errorf(
			"this wallet was joined by %s, not %s: only that key's requests count; pass it with --as pubkey:<privkey>. Nothing was signed", w.MemberPubkey, signer)), reasonMemberMismatch)
	}

	if action != nipcw.ActionRotate {
		ev, err := nipcw.NewMemberRequest(priv, w.HubPubkey, action, w.WalletPubkey, "")
		if err != nil {
			return output.RuntimeError(cmd, err)
		}
		return printMemberRequest(jsonMode, ev, action, w, "", false)
	}

	// A rotate: one open rotation per wallet; its request is shown again.
	if open, ok, err := config.OpenRotation(w.WalletPubkey); err != nil {
		return output.RuntimeError(cmd, err)
	} else if ok {
		var ev nip01.Event
		if json.Unmarshal(open.Event, &ev) == nil && ev.ID != "" && time.Now().Unix() <= open.Deadline {
			output.Notef(jsonMode, "This wallet already has a rotate request open (new key %s); here it is again.", open.ConnectionPubkey)
			return printMemberRequest(jsonMode, &ev, action, w, open.ConnectionPubkey, true)
		}
		return output.WithReason(output.FinalConflictError(cmd, "", fmt.Errorf(
			"this wallet already has a rotation open (new key %s); `cashctl cash settle` follows it until the Hub accepts, completes or forgets it", open.ConnectionPubkey)), reasonRotationOpen)
	}
	hubConn := ""
	if hub != nil {
		hubConn = hub.Raw
	} else if others, _ := config.OtherHubConnections(w.HubPubkey, ""); len(others) > 0 {
		hubConn = others[0]
	}
	if hubConn == "" {
		return output.InvocationError(cmd, errors.New(
			"a rotate needs the circle's circlehub1... string to follow the rotation, and none is held here: pass --hub"))
	}
	if hub == nil {
		if hub, err = readCircleHub(cmd, hubConn); err != nil {
			return err
		}
	}
	key, err := nipcw.NewConnectionKey()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	ev, err := nipcw.NewMemberRequest(priv, w.HubPubkey, action, w.WalletPubkey, key.Pubkey())
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	device, err := deviceIDFn()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	label, _ := hub.label()
	if err := config.WriteJoin(config.Join{
		ConnectionPubkey: key.Pubkey(), ConnectionSecret: key.Secret(),
		HubPubkey: w.HubPubkey, MemberPubkey: signer, HubConnection: hubConn,
		HubIdentity: hub.hubIdentity(), Chain: hub.chain(), HubLabel: label, ConnVerified: hub.Verified,
		Deadline: int64(ev.CreatedAt) + memberRequestValidity, DeviceID: device,
		Purpose: config.JoinPurposeRotation, WalletPubkey: w.WalletPubkey, WalletName: rw.name, Event: raw,
	}); err != nil {
		return output.RuntimeError(cmd, fmt.Errorf("the new key could not be written down, so no request was signed for it: %w", err))
	}
	crashpoint.Hit("rotation.after_writeahead")
	return printMemberRequest(jsonMode, ev, action, w, key.Pubkey(), false)
}

// resolveRequestWallet finds the wallet ref names (a registered name or
// wallet_pubkey), else the current wallet; --wallet with --hub names one not
// held here.
func resolveRequestWallet(cmd *cobra.Command, ref, walletFlag string, hub *conn) (requestWallet, error) {
	s, err := config.Load()
	if err != nil {
		return requestWallet{}, output.RuntimeError(cmd, err)
	}
	if ref == "" && walletFlag != "" {
		ref = strings.ToLower(walletFlag)
	}
	var found *config.Connection
	switch {
	case ref != "":
		if c, ok := s.Find(ref); ok {
			found = c
		} else {
			for i := range s.Connections {
				if s.Connections[i].Kind == config.KindCircle && s.Connections[i].WalletPubkey == strings.ToLower(ref) {
					found = &s.Connections[i]
				}
			}
		}
	default:
		explicit, _ := cmd.Flags().GetString("connection")
		if explicit != "" {
			found, _ = s.Find(explicit)
		} else {
			found, _ = s.DefaultConnection()
		}
	}
	if found != nil {
		w, ok, err := config.ParseCircleWallet(found.Value)
		if !ok || err != nil {
			return requestWallet{}, output.InvalidInputError(cmd, found.Name, fmt.Errorf("%q is not a circle wallet; member requests name circle wallets only", found.Name))
		}
		if hub != nil && hub.Circle.Pubkey != w.HubPubkey {
			return requestWallet{}, output.InvalidInputError(cmd, "", fmt.Errorf("--hub is another circle's string than wallet %q's", found.Name))
		}
		if w.MemberPubkey == "" {
			w.MemberPubkey = joinMemberOf(found.Name)
		}
		return requestWallet{name: found.Name, wallet: w, held: true}, nil
	}
	if walletFlag == "" || hub == nil {
		if ref == "" {
			return requestWallet{}, output.NotFoundError(cmd, "", errors.New(noWalletMessage()))
		}
		return requestWallet{}, output.NotFoundError(cmd, ref, fmt.Errorf(
			"no circle wallet %q here; for a wallet not held on this device pass --wallet <wallet_pubkey> --hub <circlehub1...>", output.Sanitize(ref)))
	}
	wp := strings.ToLower(walletFlag)
	if !isHex64(wp) {
		return requestWallet{}, output.InvalidInputError(cmd, walletFlag, errors.New("--wallet must be the wallet's 64-hex wallet_pubkey"))
	}
	return requestWallet{wallet: config.CircleWallet{WalletPubkey: wp, HubPubkey: hub.Circle.Pubkey,
		HubIdentity: hub.hubIdentity(), Chain: hub.chain()}}, nil
}

// joinMemberOf is the pubkey of the join that made the wallet named name, ""
// when no join row says.
func joinMemberOf(name string) string {
	joins, _ := config.Joins()
	for _, j := range joins {
		if !j.IsRotation() && j.State == config.JoinDone && j.WalletName == name {
			return j.MemberPubkey
		}
	}
	return ""
}

// memberPrivKey is the key a member request is signed with: --as
// (pubkey:<privkey>, else CASHCTL_AS), else the local identity.
func memberPrivKey(cmd *cobra.Command) (string, error) {
	if as := asCredentialValue(cmd); as != "" {
		priv, ok := strings.CutPrefix(as, "pubkey:")
		if !ok || !isHex64(strings.ToLower(priv)) {
			return "", output.InvalidInputError(cmd, output.RedactSecretInput(as), errors.New("a member request is signed with pubkey:<privkey>"))
		}
		return strings.ToLower(priv), nil
	}
	return localPrivKey(cmd)
}

// printMemberRequest shows the signed request: the event on stdout (the
// whole --json document under --json), what to do with it on stderr.
func printMemberRequest(jsonMode bool, ev *nip01.Event, action nipcw.MemberAction, w config.CircleWallet, newKey string, again bool) error {
	submitWithin := int64(ev.CreatedAt) + memberRequestValidity
	next := "give this request to the circle's host to submit; it is valid for 24 hours"
	if action == nipcw.ActionRotate {
		next += "; then run `cashctl cash settle` to follow the rotation (using the wallet with its old key meanwhile cancels it)"
	}
	if jsonMode {
		out := map[string]any{
			"event": ev, "action": string(action), "wallet_pubkey": w.WalletPubkey, "hub_pubkey": w.HubPubkey,
			"submit_within": submitWithin, "next_step": next,
		}
		if newKey != "" {
			out["new_connection_pubkey"] = newKey
		}
		if again {
			out["repeated"] = true
		}
		output.PrintJSON(out)
		return nil
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	output.Println(string(b))
	output.Notef(false, "Signed a %s request for wallet %s. Give it to the circle's host; it is valid for 24 hours (until %s).",
		action, w.WalletPubkey, time.Unix(submitWithin, 0).UTC().Format(time.RFC3339))
	if newKey != "" {
		output.Notef(false, "New connection key %s is kept here. Run `cashctl cash settle` after the Hub's rotation delay to install it; using the wallet before then cancels the rotation.", newKey)
	}
	return nil
}

// lookupRotationOnce asks the Hub about one rotate request with the join
// lookup naming its new key, and records the answer: pending (no record yet,
// or the rotation waits for its delay) waits; done installs the new key in
// place of the old one; unknown means the Hub never accepted it, so the key
// is forgotten.
func lookupRotationOnce(ctx context.Context, cmd *cobra.Command, client circleHub, j config.Join, cred nipcw.Credential) joinOutcome {
	action, err := client.LookupJoin(ctx, pendingJoinOf(j), cred)
	if err != nil {
		return joinOutcome{Join: j, Summary: "no answer", Err: rotationStillOpen(cmd, j, err)}
	}
	switch action.Kind {
	case nipcwclient.JoinClose:
		name, err := finishRotation(cmd, j, action.Response)
		if err != nil {
			return joinOutcome{Join: j, Summary: "done, but not saved", Err: err}
		}
		j.State, j.WalletName = config.JoinDone, name
		return joinOutcome{Join: j, Summary: "rotated: wallet " + name + " now uses the new key", Wallet: name}
	case nipcwclient.JoinWait:
		if action.Until != 0 {
			_ = config.MarkJoinLookup(j.ConnectionPubkey, true, false)
		}
		retry := action.Until
		if retry == 0 {
			// No answer: ask again in a few minutes.
			retry = time.Now().Unix() + joinUnansweredAfter
		}
		_ = config.SetJoinState(j.ConnectionPubkey, config.JoinPending, "pending", retry)
		j.State, j.RetryAfter = config.JoinPending, retry
		return joinOutcome{Join: j, Summary: "pending until " + time.Unix(retry, 0).UTC().Format(time.RFC3339), Err: rotationStillOpen(cmd, j, nil)}
	case nipcwclient.JoinMayRejoin:
		_ = config.SetJoinState(j.ConnectionPubkey, config.JoinNotApplied, "unknown", 0)
		j.State = config.JoinNotApplied
		return joinOutcome{Join: j, Summary: "the Hub never accepted this rotation",
			Err: output.WithReason(output.FinalConflictError(cmd, "", errors.New(
				"the Hub never accepted this rotation (not submitted within 24 hours, or refused); its new key is forgotten and the wallet keeps its old one — sign a new request with `cashctl circle request rotate`")), reasonRotationNotMade)}
	}
	_ = config.SetJoinState(j.ConnectionPubkey, config.JoinShown, action.Reason, 0)
	j.State, j.Detail = config.JoinShown, action.Reason
	return joinOutcome{Join: j, Summary: "unreadable lookup answer (" + action.Reason + ")",
		Err: output.WithReason(output.IndeterminateError(cmd, "", fmt.Errorf(
			"the Hub's answer about this rotation could not be used (%s); its new key is kept in `cashctl wallet show`", output.Sanitize(action.Reason))),
			strings.SplitN(action.Reason, ":", 2)[0])}
}

// rotationStillOpen is a rotation whose outcome is not known yet: exit 8.
func rotationStillOpen(cmd *cobra.Command, j config.Join, cause error) error {
	msg := "the rotation is not complete yet; its new key is kept — run `cashctl cash settle` again later"
	if cause != nil {
		msg += fmt.Sprintf(" (%v)", cause)
	}
	call := joinCall(j)
	call["method"] = "rotate"
	call["wallet_pubkey"] = j.WalletPubkey
	return output.WithCall(output.IndeterminateError(cmd, "", errors.New(msg)), call)
}

// finishRotation installs the new key of a completed rotation in the wallet it
// moved, in the same save that closes the rotation; a wallet not held here (a
// lost device) is registered from the details.
func finishRotation(cmd *cobra.Command, j config.Join, resp *nipcw.CreateCircleWalletResponse) (string, error) {
	if resp.ConnectionPubkey != j.ConnectionPubkey || resp.WalletPubkey != j.WalletPubkey {
		return "", output.IndeterminateError(cmd, "", fmt.Errorf(
			"the Hub's details name wallet %s with key %s, not this rotation's %s with %s; the rotation is kept",
			resp.WalletPubkey, resp.ConnectionPubkey, j.WalletPubkey, j.ConnectionPubkey))
	}
	if j.ConnectionSecret == "" {
		return "", output.RuntimeError(cmd, fmt.Errorf("rotation %s no longer holds its connection secret", j.ConnectionPubkey))
	}
	s, err := config.Load()
	if err != nil {
		return "", output.RuntimeError(cmd, err)
	}
	var target *config.Connection
	for i := range s.Connections {
		c := &s.Connections[i]
		if c.Kind == config.KindCircle && c.WalletPubkey == j.WalletPubkey && (target == nil || c.Name == j.WalletName) {
			target = c
		}
	}
	if target == nil {
		r, err := registerJoinWallet(cmd, j, resp, false)
		return r.name, err
	}
	w, _, err := config.ParseCircleWallet(target.Value)
	if err != nil {
		return "", output.RuntimeError(cmd, err)
	}
	w.Secret, w.Relays = j.ConnectionSecret, resp.Relays
	w.MemberPubkey, w.DetailsKnown = j.MemberPubkey, true
	w.RotationDelaySecs, w.DustThresholdMillis, w.DeleteSentAt = resp.RotationDelaySecs, resp.DustThresholdMillis, 0
	value, err := w.Encode()
	if err != nil {
		return "", output.RuntimeError(cmd, err)
	}
	target.Value = value
	name := target.Name
	s.AnsweredJoin(j.ConnectionPubkey, name)
	if err := s.Save(); err != nil {
		backup, _ := w.Backup()
		return "", output.RecoveryError(cmd, err, fmt.Sprintf(
			"The rotation completed but the new key could not be saved. Save this as a file and register it with `cashctl connect add <name> <file>`: %s", backup))
	}
	return name, nil
}

// rotationUseKey marks a context whose caller agreed to use a wallet with an
// open rotation, which cancels it.
type rotationUseKey struct{}

// errRotationOpen is DialGeneric's refusal of a circle wallet with an open
// rotate request from a caller that did not agree to cancel it.
var errRotationOpen = errors.New("this wallet has a rotation open: using it with its old key now cancels the rotation")

func rotationUseAllowed(ctx context.Context) bool {
	v, _ := ctx.Value(rotationUseKey{}).(bool)
	return v
}

// openRotationFor is the open rotate request of the circle wallet value, if
// it is one.
func openRotationFor(value string) (config.Join, bool) {
	w, ok, err := config.ParseCircleWallet(value)
	if !ok || err != nil {
		return config.Join{}, false
	}
	j, open, _ := config.OpenRotation(w.WalletPubkey)
	return j, open
}

// allowRotationUse asks, before a command dials the circle wallet value whose
// rotate request is open here, whether to use it anyway, which cancels the
// rotation (NIP-CW §Recovering a Lost Connection): --yes agrees, --json alone
// does not, and a no sends nothing.
func allowRotationUse(cmd *cobra.Command, ctx context.Context, value string) (context.Context, error) {
	j, open := openRotationFor(value)
	if !open {
		return ctx, nil
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	yes, _ := cmd.Flags().GetBool("yes")
	refuse := output.WithReason(output.UsageError(cmd, fmt.Errorf(
		"this wallet has a rotation open (new key %s): using it now cancels the rotation you requested; nothing was sent. Pass --yes to cancel it deliberately", j.ConnectionPubkey)), reasonRotationOpen)
	switch {
	case yes:
	case jsonMode:
		return ctx, refuse
	case !Confirm(cmd, false, "Using this wallet now cancels the rotation you requested; continue?"):
		return ctx, refuse
	}
	return context.WithValue(ctx, rotationUseKey{}, true), nil
}
