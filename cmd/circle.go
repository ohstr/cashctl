package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcw"
	nipcwclient "github.com/ohstr/nmilat/nipcw/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/crashpoint"
	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/dial"
	"github.com/ohstr/cashctl/internal/output"
)

// Reasons of a refused or unfinished join (--json "reason").
const (
	reasonNotAccepting    = "not_accepting"
	reasonJoinUnavailable = "join_unavailable"
	reasonOverHubLimit    = "over_hub_limit"
	reasonNoRotation      = "no_rotation"
)

// withHeldWalletHint adds to a WALLET_EXISTS refusal which wallet here is
// that circle's, so "you already hold one" comes with the name to use it by.
// The Hub only says one exists; the local name is cashctl's to supply.
func withHeldWalletHint(err error, label string) error {
	ce, ok := err.(*output.CLIError)
	if !ok || ce.NWCCode != nipcw.ErrWalletExists {
		return err
	}
	s, loadErr := config.Load()
	if loadErr != nil {
		return err
	}
	ce.Hint = heldWalletHint(s, label)
	return ce
}

func heldWalletHint(s *config.Store, label string) string {
	if name := config.HintedName("circle", label); name != "" {
		if _, ok := s.Find(name); ok {
			return fmt.Sprintf("It is registered here as %q — use it with `-c %s` or `cashctl wallet use %s`.", name, name, name)
		}
	}
	var circles []string
	for _, c := range s.Connections {
		if strings.HasPrefix(c.Name, "circle:") {
			circles = append(circles, c.Name)
		}
	}
	if len(circles) > 0 {
		return fmt.Sprintf("Your circle wallets here: %s — use one with `-c <name>`.", strings.Join(circles, ", "))
	}
	return "It is not registered on this device (joined from another one, or removed); `cashctl connect list` shows what is."
}

func newCircleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "circle",
		Short: "Join a circle to get a personal Lightning wallet",
		RunE:  groupRunE,
	}
	cmd.AddCommand(newCircleJoinCmd(), newCircleRequestCmd())
	return cmd
}

func newCircleJoinCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "join [hub-connection] [max-amount]",
		Short: "Join a circle",
		Long: `Joins a circle from its circlehub1... string. The join always travels over
the Hub's private transport, so the circle's other members never see it; when
the Hub's signed announcement can't be found, nothing is sent (try again later).

Before anything is sent, the Hub must answer as the identity and chain the
string names. A string whose signature can't be confirmed is used only after
you agree once (or pass --allow-unverified).`,
		Example: `  cashctl join circlehub1... 100
  cashctl join circlehub1... --max-amount 100 --max-receive 500
  cashctl join - 100 < hub.txt    # read the connection from stdin, keeping it out of ps and shell history`,
		Args: output.MaximumNArgs(2),
		RunE: runCircleJoin,
	}
	cmd.Flags().String("hub", "", "the Circle Hub's circlehub1... string")
	cmd.Flags().String("max-amount", "", fmt.Sprintf("requested spend cap, in %s — required", output.CurrencyUnit))
	cmd.Flags().String("max-receive", "", fmt.Sprintf("requested receive cap, in %s — required unless the Hub sets a default", output.CurrencyUnit))
	cmd.Flags().Duration("expiry", 0, "requested expiry duration (0 = Hub default)")
	cmd.Flags().String("budget-renewal", "", "daily|weekly|monthly|yearly|never (default: Hub default)")
	cmd.Flags().String("as", "", "override credential (defaults to your local identity)")
	cmd.Flags().Bool("allow-unverified", false, "use a string whose signature can't be confirmed (asked once per string otherwise)")
	cmd.Flags().String("backup-file", "", "write the new wallet's connection to this file (0600) when the Hub can't rotate a lost one")
	cmd.Flags().Bool("accept-no-rotation", false, "join a Hub that can't rotate a lost connection without --backup-file (needed under --json/--yes)")
	return cmd
}

// disambiguateJoinArgs decides which of join's up-to-2 positional args is
// the hub connection and which is the max-amount — sniffed by shape, the
// same way disambiguateTransferArgs picks apart transfer's target/amount
// pair (cash_transfer.go): a circlehub1... string never parses as an
// amount, so whichever arg does is the max-amount, regardless of which
// side it's on.
//
// When NEITHER side parses as an amount (a malformed one, most often),
// looksLikeHubArg breaks the tie: exactly one side being hub-shaped means
// the OTHER side is the (malformed) amount, so the error never blames (and
// echoes) the hub string, secret included. Genuinely ambiguous input (both
// or neither look hub-shaped) falls back to the documented hub-first order.
func disambiguateJoinArgs(args []string) (positionalHub, positionalMaxAmount string) {
	looksLikeAmount := func(s string) bool {
		_, err := output.ParseAmount(s)
		return err == nil
	}
	looksLikeHubArg := func(s string) bool {
		switch dial.Sniff(s) {
		case dial.KindCircleHub, dial.KindCashHub:
			return true
		default:
			return false
		}
	}
	switch len(args) {
	case 1:
		if looksLikeAmount(args[0]) {
			positionalMaxAmount = args[0]
		} else {
			positionalHub = args[0]
		}
	case 2:
		amount0, amount1 := looksLikeAmount(args[0]), looksLikeAmount(args[1])
		hub0, hub1 := looksLikeHubArg(args[0]), looksLikeHubArg(args[1])
		switch {
		case amount0 && !amount1:
			positionalMaxAmount, positionalHub = args[0], args[1]
		case amount1 && !amount0:
			positionalHub, positionalMaxAmount = args[0], args[1]
		case hub0 && !hub1:
			positionalHub, positionalMaxAmount = args[0], args[1]
		case hub1 && !hub0:
			positionalHub, positionalMaxAmount = args[1], args[0]
		default:
			positionalHub, positionalMaxAmount = args[0], args[1]
		}
	}
	return positionalHub, positionalMaxAmount
}

// circleHub is a Circle Wallet Hub connection that passed its online check:
// nipcw/client's Client, behind an interface so tests can stand in for it.
type circleHub interface {
	// Info is the Hub's get_info, online check included.
	Info(ctx context.Context) (*nip47.GetInfoResult, error)
	Verified() bool
	Join(ctx context.Context, p nipcw.CreateCircleWalletParams, o nipcwclient.JoinOptions, j nipcwclient.JoinJournal) (*nipcwclient.JoinResult, error)
	LookupJoin(ctx context.Context, pj nipcwclient.PendingJoin, cred nipcw.Credential) (nipcwclient.JoinAction, error)
	Close()
}

// liveCircleHub is the real circleHub.
type liveCircleHub struct {
	*nipcwclient.Client
}

// Info is the get_info nipcw/client read and checked (the online check)
// when it connected: join shows the terms and the network from it.
func (h liveCircleHub) Info(context.Context) (*nip47.GetInfoResult, error) {
	gi := h.Client.Info()
	if gi == nil || gi.CircleWallet == nil {
		return nil, fmt.Errorf("%w: no circle_wallet block", nipcwclient.ErrHubNonConforming)
	}
	return gi, nil
}

// dialCircleHub connects to a circlehub1 string and runs the online check.
func dialCircleHub(ctx context.Context, circlehub string) (circleHub, error) {
	c, err := nipcwclient.Connect(ctx, circlehub, nipcwclient.Options{Session: sessionOptions()})
	if err != nil {
		return nil, err
	}
	return liveCircleHub{c}, nil
}

// dialCircleHubFn is dialCircleHub, swapped out in tests.
var dialCircleHubFn = dialCircleHub

// classifyHubDialErr is a failed connect or get_info: nothing was sent.
func classifyHubDialErr(cmd *cobra.Command, err error) error {
	switch {
	case isConnectionMismatch(err):
		return connectionMismatchErr(cmd, err)
	case errors.Is(err, nipcwclient.ErrHubNonConforming):
		return output.RuntimeError(cmd, fmt.Errorf("this Hub's get_info is not a conforming Circle Wallet Hub's, so cashctl won't join it; nothing was sent (%w)", err))
	}
	return classifyNWCErr(cmd, err)
}

// joinRequest is what `join` was asked, kept with the join as its terms.
type joinRequest struct {
	MaxAmountMillis  uint64 `json:"max_amount_millis"`
	MaxReceiveMillis uint64 `json:"max_receive_millis"`
	ExpirySecs       int64  `json:"expiry_secs,omitempty"`
	BudgetRenewal    string `json:"budget_renewal,omitempty"`
	Network          string `json:"network,omitempty"`
}

func runCircleJoin(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	hubFlag, _ := cmd.Flags().GetString("hub")
	maxAmountFlag, _ := cmd.Flags().GetString("max-amount")
	maxReceiveFlag, _ := cmd.Flags().GetString("max-receive")
	expiry, _ := cmd.Flags().GetDuration("expiry")
	budgetRenewal, _ := cmd.Flags().GetString("budget-renewal")
	backupFile, _ := cmd.Flags().GetString("backup-file")
	asFlag := asCredentialValue(cmd)

	positionalHub, positionalMaxAmount := disambiguateJoinArgs(args)
	hub, err := resolvePositionalOrFlag(cmd, positionalHub, "hub", hubFlag)
	if err != nil {
		return err
	}
	if hub, err = argOrStdin(cmd, hub); err != nil {
		return err
	}
	// Both arguments named whenever the max amount is missing too: a hint
	// naming only the hub led straight to the next refusal (I-29).
	if hub == "" {
		if positionalMaxAmount == "" && strings.TrimSpace(maxAmountFlag) == "" {
			return output.InvocationError(cmd, fmt.Errorf("a Circle Hub connection and a max amount are required — pass both (cashctl join <hub-connection> <max-amount>) or use --hub and --max-amount"))
		}
		return output.InvocationError(cmd, fmt.Errorf("a Circle Hub connection is required — pass it directly (cashctl join <hub-connection> <max-amount>) or via --hub"))
	}
	maxAmount, err := resolvePositionalOrFlagAmount(cmd, positionalMaxAmount, "max-amount", maxAmountFlag)
	if err != nil {
		return err
	}
	// NIP-CW requires max_amount on every create_circle_wallet request, and
	// there's no "0 means unlimited" convention for it.
	if maxAmount == 0 {
		// Absent and zero are different mistakes: someone passing 0 has
		// almost certainly assumed the "0 means unlimited" convention this
		// method does not have, so say that instead.
		if positionalMaxAmount != "" || maxAmountFlag != "" {
			return output.InvalidInputError(cmd, "0", fmt.Errorf("0 isn't a spend cap — there's no \"0 means unlimited\" convention here, so pass the most this wallet may spend"))
		}
		return output.InvocationError(cmd, fmt.Errorf("a max amount is required — pass it directly (cashctl join <hub-connection> <max-amount>) or via --max-amount"))
	}
	var maxReceive uint64
	if strings.TrimSpace(maxReceiveFlag) != "" {
		if maxReceive, err = output.ParseAmount(maxReceiveFlag); err != nil {
			return output.InvalidInputError(cmd, maxReceiveFlag, err)
		}
	}

	c, err := readCircleHub(cmd, hub)
	if err != nil {
		return err
	}
	label, _ := c.label()
	var warnings []string
	if !c.Verified {
		warnings = append(warnings, warnUnverified(jsonMode, c))
	}

	cred, err := joinCredential(cmd, asFlag)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var client circleHub
	var info *nip47.GetInfoResult
	err = WithSpinner(jsonMode, "Checking the Hub...", func() error {
		var dErr error
		if client, dErr = dialCircleHubFn(ctx, c.Raw); dErr != nil {
			return dErr
		}
		info, dErr = client.Info(ctx)
		return dErr
	})
	if client != nil {
		defer client.Close()
	}
	if err != nil {
		return classifyHubDialErr(cmd, err)
	}
	// Only after the online check passed: the Hub answers as the identity
	// the string names, so the one question left is the string's signature.
	if proceed, err := confirmUnverified(cmd, c); err != nil || !proceed {
		if err == nil {
			output.Println("Not sent.")
		}
		return err
	}
	isTest := testNetwork(jsonMode, info.Network)
	cw := info.CircleWallet
	if cw.Accepting != nil && !*cw.Accepting {
		return joinFailed(cmd, client, &joinJournal{}, cred, nipcwclient.ErrNotAccepting, label, warnings, isTest)
	}
	if maxReceive == 0 {
		if cw.PerWalletMaxReceiveMillis <= 0 {
			return output.InvocationError(cmd, fmt.Errorf("this Hub sets no default receive cap, so --max-receive is required"))
		}
		maxReceive = uint64(cw.PerWalletMaxReceiveMillis)
		output.Notef(jsonMode, "receive cap: %s (the Hub's default)", output.FormatAmount(int64(maxReceive)))
	}
	if !confirmJoinTerms(cmd, c, info) {
		output.Println("Not sent.")
		return nil
	}

	req := joinRequest{MaxAmountMillis: maxAmount, MaxReceiveMillis: maxReceive, ExpirySecs: int64(expiry / time.Second),
		BudgetRenewal: budgetRenewal, Network: info.Network}
	journal := &joinJournal{conn: c, request: req}
	refusedNoRotation := false
	opts := nipcwclient.JoinOptions{WarnNoRotation: func(context.Context) bool {
		w := "this Hub can't rotate a lost connection: if you lose this wallet's connection key, its balance can't be reached by anyone"
		warnings = append(warnings, w)
		output.Notef(jsonMode, "Warning: %s.", w)
		// Warned before the join (NIP-CW §Recovering a Lost Connection): with
		// no terminal to answer, only a backup or an explicit flag joins.
		yes, _ := cmd.Flags().GetBool("yes")
		accept, _ := cmd.Flags().GetBool("accept-no-rotation")
		if (jsonMode || yes) && backupFile == "" && !accept {
			refusedNoRotation = true
			return false
		}
		if backupFile == "" && !jsonMode {
			if yes, _ := cmd.Flags().GetBool("yes"); !yes {
				output.Fprintf(os.Stderr, "Save a backup of the new wallet's connection to a file (path, or Enter to skip): ")
				line, _ := stdin.ReadString('\n')
				backupFile = strings.TrimSpace(line)
			}
		}
		return Confirm(cmd, true, "Join anyway?")
	}}

	var result *nipcwclient.JoinResult
	err = WithSpinner(jsonMode, "Joining...", func() error {
		var jErr error
		result, jErr = client.Join(callContext(ctx), nipcw.CreateCircleWalletParams{
			Credential: cred, MaxAmountMillis: maxAmount, MaxReceiveMillis: maxReceive,
			Expiry: expiry, BudgetRenewal: budgetRenewal,
		}, opts, journal)
		return jErr
	})
	if err != nil && refusedNoRotation && errors.Is(err, nipcwclient.ErrJoinDeclined) {
		if journal.written != nil {
			_ = config.SetJoinState(journal.written.ConnectionPubkey, config.JoinNotApplied, "not_sent", 0)
		}
		return output.WithReason(output.UsageError(cmd, errors.New(
			"this Hub can't rotate a lost connection, so a lost key loses the wallet; nothing was sent. Join with --backup-file <path> to keep a copy, or --accept-no-rotation")), reasonNoRotation)
	}
	if err != nil {
		return joinFailed(cmd, client, journal, cred, err, label, warnings, isTest)
	}
	j, _, _ := config.FindJoin(result.Pending.ConnectionPubkey)
	return finishJoin(cmd, j, result.CreateCircleWalletResponse, joinView{
		label: label, warnings: warnings, testNetwork: isTest, backupFile: backupFile, requested: maxAmount,
	})
}

// readCircleHub reads join's hub argument: a circlehub1 string only.
func readCircleHub(cmd *cobra.Command, hub string) (*conn, error) {
	switch dial.Sniff(hub) {
	case dial.KindCashHub:
		return nil, output.WithReason(output.InvalidInputError(cmd, hub, fmt.Errorf(
			"that's a Cash Hub connection (for minting cash), not a Circle Hub connection")), reasonConnectionInvalid)
	case dial.KindNWCURI:
		return nil, output.WithReason(output.InvalidInputError(cmd, hub, fmt.Errorf(
			"cashctl joins a circle only from its circlehub1... string; ask the circle's host for it")), reasonConnectionInvalid)
	}
	return readConnection(cmd, hub, nipcw.CircleHubConnectionHRP)
}

// joinCredential is --as, else the local identity.
func joinCredential(cmd *cobra.Command, asFlag string) (nipcw.Credential, error) {
	if asFlag != "" {
		cred, err := credential.ParseCircle(asFlag)
		if err != nil {
			return nipcw.Credential{}, output.InvalidInputError(cmd, output.RedactSecretInput(asFlag), err)
		}
		return cred, nil
	}
	cred, err := localCircleCredential(cmd)
	if err != nil {
		return nipcw.Credential{}, output.RuntimeError(cmd, err)
	}
	return cred, nil
}

// confirmJoinTerms shows what the Hub offers and asks before joining; --yes
// and --json skip the question.
func confirmJoinTerms(cmd *cobra.Command, c *conn, info *nip47.GetInfoResult) bool {
	jsonMode, _ := cmd.Flags().GetBool("json")
	if jsonMode {
		return true
	}
	cw := info.CircleWallet
	name := "this circle"
	if l, ok := c.label(); ok && l != "" {
		name = fmt.Sprintf("%q (not confirmed)", output.Sanitize(l))
	}
	output.Notef(false, "Joining %s at Hub %s%s:", name, shortFingerprint(c.hubIdentity()), map[bool]string{true: "", false: " — Hub not confirmed"}[c.Verified])
	output.Notef(false, "  in-circle fee: %d ppm", cw.FeesPpm)
	if cw.FeeReserve != nil {
		output.Notef(false, "  routing fee reserve: %d ppm, at least %s", cw.FeeReserve.Ppm, output.FormatAmount(cw.FeeReserve.MinMillis))
	}
	if cw.DustThresholdMillis != nil {
		output.Notef(false, "  dust threshold: %s", output.FormatAmount(*cw.DustThresholdMillis))
	}
	if cw.AvailableMillis > 0 {
		output.Notef(false, "  available: up to %s (may be stale)", output.FormatAmount(cw.AvailableMillis))
	}
	if cw.MaxExpSecs > 0 {
		output.Notef(false, "  longest expiry: %s", (time.Duration(cw.MaxExpSecs) * time.Second).String())
	}
	if cw.CirclePolicy != "" {
		output.Notef(false, "  membership: %s", circlePolicyPhrase(cw.CirclePolicy))
	}
	output.Notef(false, "  chain: %s", output.Sanitize(info.Chain))
	if cw.RotationDelaySecs != nil {
		output.Notef(false, "  a lost connection can be rotated (after %ds)", *cw.RotationDelaySecs)
	} else {
		output.Notef(false, "  a lost connection can't be rotated")
	}
	return Confirm(cmd, true, "Join?")
}

// joinJournal writes a join ahead through config.WriteJoin before it is sent.
type joinJournal struct {
	conn    *conn
	request joinRequest
	written *nipcwclient.PendingJoin
}

func (j *joinJournal) WriteAhead(ctx context.Context, pj nipcwclient.PendingJoin) error {
	device, err := deviceIDFn()
	if err != nil {
		return err
	}
	terms, _ := json.Marshal(j.request)
	label, _ := j.conn.label()
	if err := config.WriteJoin(config.Join{
		ConnectionPubkey: pj.ConnectionPubkey, ConnectionSecret: pj.ConnectionSecret,
		HubPubkey: pj.HubPubkey, MemberPubkey: pj.Pubkey, HubConnection: j.conn.Raw,
		HubIdentity: j.conn.hubIdentity(), Chain: j.conn.chain(), HubLabel: label,
		ConnVerified: j.conn.Verified, Deadline: pj.Deadline, DeviceID: device, Terms: string(terms),
	}); err != nil {
		return err
	}
	j.written = &pj
	markWrittenAhead(ctx)
	crashpoint.Hit("joins.after_writeahead")
	return nil
}

// joinFailed handles a join that returned an error: before anything was
// sent, a refusal, or an outcome to look up.
func joinFailed(cmd *cobra.Command, client circleHub, journal *joinJournal, cred nipcw.Credential, err error, label string, warnings []string, isTest bool) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	forget := func() {
		if journal.written != nil {
			if dErr := config.SetJoinState(journal.written.ConnectionPubkey, config.JoinNotApplied, "not_sent", 0); dErr != nil {
				output.Notef(jsonMode, "Could not forget the unused connection key: %v", dErr)
			}
		}
	}
	var indeterminate *nipcwclient.JoinIndeterminateError
	var walletErr *relayclient.WalletError
	switch {
	case errors.Is(err, nipcwclient.ErrJoinDeclined):
		forget()
		output.Println("Not sent.")
		return nil
	case errors.Is(err, nipcwclient.ErrNotAccepting):
		return output.WithReason(output.AuthError(cmd, errors.New("this circle isn't taking new members now; nothing was sent")), reasonNotAccepting)
	case errors.Is(err, nipcwclient.ErrNoAnnouncement):
		forget()
		return output.WithReason(output.NetworkError(cmd, errors.New(
			"couldn't find this Hub's signed announcement, so the join can't be private and wasn't sent; try again later")), reasonJoinUnavailable)
	case errors.Is(err, nipcwclient.ErrOverHubLimit):
		return output.WithReason(output.InvalidInputError(cmd, "", fmt.Errorf("over this Hub's limits; nothing was sent: %w", err)), reasonOverHubLimit)
	case isConnectionMismatch(err), errors.Is(err, nipcwclient.ErrHubNonConforming):
		forget()
		return classifyHubDialErr(cmd, err)
	case errors.Is(err, nipcwclient.ErrJoinNotSent):
		forget()
		if errors.Is(err, config.ErrJoinReadBack) {
			return output.RuntimeError(cmd, fmt.Errorf("not sent: %w", err))
		}
		return output.NetworkError(cmd, fmt.Errorf("the join was not sent: %w", err))
	case errors.As(err, &indeterminate):
		// The join may have made the wallet: ask the Hub now (a front-runner's
		// copy may already be done).
		j, ok, fErr := config.FindJoin(indeterminate.Pending.ConnectionPubkey)
		if fErr != nil || !ok {
			return output.RuntimeError(cmd, fmt.Errorf("the join's outcome is unknown and its record could not be read: %v", fErr))
		}
		return settleJoinNow(cmd, client, j, cred, joinView{label: label, warnings: warnings, testNetwork: isTest})
	case errors.As(err, &walletErr) && walletErr.Code == nipcw.ErrRequestReused:
		// Not a failure (NIP-CW §Errors): the join stays open, its secret kept,
		// and is looked up.
		if journal.written != nil {
			return joinStillOpen(cmd, journal.written.ConnectionPubkey, journal.written.Deadline, err)
		}
		return output.IndeterminateError(cmd, "", err)
	case errors.As(err, &walletErr):
		// An explicit answer: no wallet was made for this proof.
		if journal.written != nil {
			_ = config.SetJoinState(journal.written.ConnectionPubkey, config.JoinNotApplied, walletErr.Code, 0)
		}
		return withHeldWalletHint(classifyNWCErr(cmd, err), label)
	}
	if journal.written != nil {
		// Sent, with an outcome nobody named: keep the key and look it up later.
		return joinStillOpen(cmd, journal.written.ConnectionPubkey, journal.written.Deadline, err)
	}
	return classifyNWCErr(cmd, err)
}

// joinView is how a finished join is reported.
type joinView struct {
	label       string
	warnings    []string
	testNetwork bool
	backupFile  string
	requested   uint64
}

// registeredJoin is a wallet a join made, now held here.
type registeredJoin struct {
	name       string
	setDefault bool
	wasEmpty   bool
	wallet     config.CircleWallet
	request    joinRequest
}

// registerJoinWallet stores the wallet a join made, from its checked details,
// as fields (never a URI), and marks the join done in the same save. ask is
// whether to ask about the default wallet (join itself); `cash settle` sets
// a first wallet as the default without asking.
func registerJoinWallet(cmd *cobra.Command, j config.Join, resp *nipcw.CreateCircleWalletResponse, ask bool) (registeredJoin, error) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	var r registeredJoin
	if resp.ConnectionPubkey != "" && resp.ConnectionPubkey != j.ConnectionPubkey {
		return r, output.IndeterminateError(cmd, "", fmt.Errorf(
			"the Hub's details name connection key %s, not this join's %s; the join is kept", resp.ConnectionPubkey, j.ConnectionPubkey))
	}
	if j.ConnectionSecret == "" {
		return r, output.RuntimeError(cmd, fmt.Errorf("join %s no longer holds its connection secret", j.ConnectionPubkey))
	}
	_ = json.Unmarshal([]byte(j.Terms), &r.request)
	verified := j.ConnVerified
	r.wallet = config.CircleWallet{WalletPubkey: resp.WalletPubkey, Relays: resp.Relays, Secret: j.ConnectionSecret,
		HubPubkey: j.HubPubkey, HubIdentity: j.HubIdentity, Chain: j.Chain, HubVerified: &verified,
		Network: r.request.Network, Label: j.HubLabel, MemberPubkey: j.MemberPubkey, DetailsKnown: true,
		RotationDelaySecs: resp.RotationDelaySecs, DustThresholdMillis: resp.DustThresholdMillis}
	value, err := r.wallet.Encode()
	if err != nil {
		return r, output.RuntimeError(cmd, err)
	}
	s, err := config.Load()
	if err != nil {
		return r, output.RuntimeError(cmd, err)
	}
	r.wasEmpty = s.IsEmpty()
	r.name = s.SuggestName("circle", j.HubLabel)
	if err := s.Add(r.name, value); err != nil {
		return r, output.RuntimeError(cmd, err)
	}
	s.SetKind(r.name, config.KindCircle)
	s.AnsweredJoin(j.ConnectionPubkey, r.name)

	switch {
	case !ask:
		r.setDefault = r.wasEmpty
	case r.wasEmpty:
		r.setDefault = jsonMode || Confirm(cmd, true, "This is your first wallet — use it as your default?")
	default:
		// Same rule as `connect add`: --yes never silently replaces an
		// existing default.
		yes, _ := cmd.Flags().GetBool("yes")
		r.setDefault = !jsonMode && !yes && Confirm(cmd, false, "Set as your default?")
	}
	if r.setDefault {
		_ = s.SetDefault(r.name)
	}
	if err := s.Save(); err != nil {
		// The wallet exists on the Hub and its secret is only here: the Hub
		// never had it. Hand the fields over, never a URI.
		backup, _ := r.wallet.Backup()
		return r, output.RecoveryError(cmd, err, fmt.Sprintf(
			"Your new wallet was created but could not be saved. Save this as a file and register it with `cashctl connect add %s <file>`: %s", r.name, backup))
	}
	return r, nil
}

// finishJoin registers the wallet a join made and reports it.
func finishJoin(cmd *cobra.Command, j config.Join, resp *nipcw.CreateCircleWalletResponse, v joinView) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	r, err := registerJoinWallet(cmd, j, resp, true)
	if err != nil {
		return err
	}
	name, setDefault, wasEmpty, w, req := r.name, r.setDefault, r.wasEmpty, r.wallet, r.request
	if v.requested == 0 {
		v.requested = req.MaxAmountMillis
	}
	backupPath := ""
	if v.backupFile != "" {
		backup, _ := w.Backup()
		if err := os.WriteFile(v.backupFile, append(backup, '\n'), 0o600); err != nil {
			output.Notef(jsonMode, "Could not write the backup file: %v (the wallet is saved here as %s)", err, name)
		} else {
			backupPath = v.backupFile
			output.Notef(jsonMode, "Backup written to %s; keep it private.", v.backupFile)
		}
	}

	if jsonMode {
		var expiresAt any
		if resp.ExpiresAt > 0 {
			expiresAt = time.Unix(resp.ExpiresAt, 0).UTC().Format(time.RFC3339)
		}
		out := map[string]any{
			"wallet": name, "default": setDefault,
			"response": map[string]any{
				"wallet_pubkey":         resp.WalletPubkey,
				"relays":                resp.Relays,
				"connection_pubkey":     j.ConnectionPubkey,
				"expires_at":            expiresAt,
				"max_amount_millis":     resp.MaxAmountMillis,
				"max_amount":            maxAmountText(resp.MaxAmountMillis),
				"max_receive_millis":    resp.MaxReceiveMillis,
				"fees_ppm":              resp.FeesPpm,
				"budget_renewal":        resp.BudgetRenewal,
				"dust_threshold_millis": resp.DustThresholdMillis,
				"rotation_delay_secs":   resp.RotationDelaySecs,
			},
			"terms":        req,
			"warnings":     output.NonNil(v.warnings),
			"network":      req.Network,
			"chain":        j.Chain,
			"test_network": req.Network != "mainnet",
			"hub_verified": j.ConnVerified,
		}
		if backupPath != "" {
			out["backup_file"] = backupPath
		}
		output.PrintJSON(out)
		return nil
	}
	greeting := "Joined!"
	if v.label != "" {
		greeting = fmt.Sprintf("Joined %q!", output.Sanitize(v.label))
	}
	renewal := resp.BudgetRenewal
	if renewal == "" {
		renewal = "never"
	}
	output.Printf("%s New wallet: %s (%s)\n", greeting, name, joinCapSummary(v.requested, resp, renewal))
	if !j.ConnVerified {
		output.Println("Hub not confirmed: its connection string's signature didn't verify.")
	}
	if setDefault {
		output.Printf("Default wallet set to %s.\n", name)
	} else if !wasEmpty {
		output.Printf("Saved as %s. Switch anytime with `cashctl wallet use %s`.\n", name, name)
	}
	return nil
}

// joinCapSummary describes the wallet's terms as the Hub resolved them: its cap
// (saying so when it differs from the one asked for), renewal, in-circle fee
// and expiry.
func joinCapSummary(requested uint64, resp *nipcw.CreateCircleWalletResponse, renewal string) string {
	capMillis := resp.MaxAmountMillis
	note := ""
	if capMillis == 0 {
		capMillis = requested
	} else if requested != 0 && capMillis != requested {
		note = fmt.Sprintf(", you asked for %s", output.FormatAmount(int64(requested)))
	}
	summary := fmt.Sprintf("max %s/%s%s", output.FormatAmount(int64(capMillis)), output.Sanitize(renewal), note)
	if resp.MaxReceiveMillis > 0 {
		summary += fmt.Sprintf("; receive up to %s", output.FormatAmount(int64(resp.MaxReceiveMillis)))
	}
	if resp.FeesPpm > 0 {
		summary += fmt.Sprintf("; in-circle fee %d ppm", resp.FeesPpm)
	}
	if resp.ExpiresAt > 0 {
		summary += "; expires " + time.Unix(resp.ExpiresAt, 0).UTC().Format("2006-01-02 15:04 UTC")
	}
	return summary
}

// maxAmountText is the granted cap in the unit `join` takes it in, next to
// max_amount_millis, so a caller can check it without converting.
func maxAmountText(millis uint64) string {
	return output.FormatAmount(int64(millis))
}
