package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nipcw"
	nipcwclient "github.com/ohstr/nmilat/nipcw/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/output"
)

// A join whose reply never came is looked up (NIP-CW §Lost Join Reply): done
// registers the wallet, pending waits, unknown means no wallet was made (the
// member may join again, with a new key), expired is kept and shown.

// pendingJoinOf is the nipcw/client form of a join row.
func pendingJoinOf(j config.Join) nipcwclient.PendingJoin {
	return nipcwclient.PendingJoin{HubPubkey: j.HubPubkey, Pubkey: j.MemberPubkey, ConnectionPubkey: j.ConnectionPubkey,
		ConnectionSecret: j.ConnectionSecret, Deadline: j.Deadline}
}

// joinCall is a join's --json "call", shown while its outcome is unknown.
func joinCall(j config.Join) map[string]any {
	call := map[string]any{
		"method":            nipcw.MethodCreateCircleWallet,
		"connection_pubkey": j.ConnectionPubkey,
		"hub_pubkey":        j.HubPubkey,
		"state":             j.State,
		"deadline":          j.Deadline,
		"settle_after":      j.Deadline + 1,
	}
	if j.RetryAfter > 0 {
		call["settle_after"] = j.RetryAfter
	}
	return call
}

// joinStillOpen is a join whose outcome is unknown: exit 8, nothing resent.
func joinStillOpen(cmd *cobra.Command, connectionPubkey string, deadline int64, cause error) error {
	j, ok, _ := config.FindJoin(connectionPubkey)
	if !ok {
		j = config.Join{ConnectionPubkey: connectionPubkey, Deadline: deadline, State: config.JoinWritten}
	}
	msg := "the join's outcome is unknown; its key is kept and nothing was resent — run `cashctl cash settle` after " +
		time.Unix(j.Deadline+1, 0).UTC().Format(time.RFC3339)
	if cause != nil {
		msg += fmt.Sprintf(" (%v)", cause)
	}
	return output.WithCall(output.IndeterminateError(cmd, "", errors.New(msg)), joinCall(j))
}

// joinOutcome is what a lookup did to one join.
type joinOutcome struct {
	Join    config.Join
	Summary string
	// Wallet is the wallet name a done join registered.
	Wallet string
	// Err is the classified error of a join left open or refused.
	Err error
}

// lookupJoinOnce asks the Hub about one join and records the answer.
func lookupJoinOnce(ctx context.Context, cmd *cobra.Command, client circleHub, j config.Join, cred nipcw.Credential, ask bool) joinOutcome {
	action, err := client.LookupJoin(ctx, pendingJoinOf(j), cred)
	if err != nil {
		return joinOutcome{Join: j, Summary: "no answer", Err: joinStillOpen(cmd, j.ConnectionPubkey, j.Deadline, err)}
	}
	switch action.Kind {
	case nipcwclient.JoinClose:
		r, err := registerJoinWallet(cmd, j, action.Response, ask)
		if err != nil {
			return joinOutcome{Join: j, Summary: "done, but not saved", Err: err}
		}
		j.State, j.WalletName = config.JoinDone, r.name
		return joinOutcome{Join: j, Summary: "joined: wallet " + r.name, Wallet: r.name}
	case nipcwclient.JoinWait:
		if action.Until != 0 {
			// pending, with a retry_after: the Hub answered.
			_ = config.MarkJoinLookup(j.ConnectionPubkey, true, false)
		}
		retry := action.Until
		if retry == 0 {
			retry = j.Deadline + 1
		}
		_ = config.SetJoinState(j.ConnectionPubkey, config.JoinPending, "pending", retry)
		j.State, j.RetryAfter = config.JoinPending, retry
		return joinOutcome{Join: j, Summary: "pending", Err: joinStillOpen(cmd, j.ConnectionPubkey, j.Deadline, nil)}
	case nipcwclient.JoinMayRejoin:
		_ = config.SetJoinState(j.ConnectionPubkey, config.JoinNotApplied, "unknown", 0)
		j.State = config.JoinNotApplied
		return joinOutcome{Join: j, Summary: "the Hub never made this wallet",
			Err: output.FinalConflictError(cmd, "", errors.New("the Hub never made this wallet; run `cashctl join` again (a new key, never this one)"))}
	}
	// JoinShow: kept for the member to see, never rejoined with this key.
	_ = config.SetJoinState(j.ConnectionPubkey, config.JoinShown, action.Reason, 0)
	j.State, j.Detail = config.JoinShown, action.Reason
	switch {
	case action.Reason == "join_expired":
		return joinOutcome{Join: j, Summary: "expired: the Hub no longer answers for this join",
			Err: output.WithReason(output.FinalConflictError(cmd, "", errors.New(
				"this join expired before its outcome was known; it is kept in `cashctl wallet show`, and is never rejoined with its key")), "join_expired")}
	case action.Reason == "lookup_unauthorized":
		return joinOutcome{Join: j, Summary: "the Hub refused the lookup's signer",
			Err: output.WithReason(output.AuthError(cmd, fmt.Errorf(
				"the Hub refused to look this join up for signer %s; the wallet may exist. Run `cashctl cash settle --as` with the key that joined", j.MemberPubkey)), "lookup_unauthorized")}
	}
	return joinOutcome{Join: j, Summary: "unreadable lookup answer (" + action.Reason + ")",
		Err: output.WithReason(output.WithCall(output.IndeterminateError(cmd, "", fmt.Errorf(
			"the Hub's answer about this join could not be used (%s); the join is kept", output.Sanitize(action.Reason))), joinCall(j)),
			strings.SplitN(action.Reason, ":", 2)[0])}
}

// settleJoinNow looks a join up right after it was sent and its outcome was
// unknown, reporting it as join would.
func settleJoinNow(cmd *cobra.Command, client circleHub, j config.Join, cred nipcw.Credential, v joinView) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	action, err := client.LookupJoin(ctx, pendingJoinOf(j), cred)
	if err == nil && action.Kind == nipcwclient.JoinClose {
		return finishJoin(cmd, j, action.Response, v)
	}
	if err != nil {
		return joinStillOpen(cmd, j.ConnectionPubkey, j.Deadline, err)
	}
	stub := &fixedLookup{action: action}
	return lookupJoinOnce(ctx, cmd, stub, j, cred, true).Err
}

// fixedLookup replays one lookup answer already received.
type fixedLookup struct {
	circleHub
	action nipcwclient.JoinAction
}

func (f *fixedLookup) LookupJoin(context.Context, nipcwclient.PendingJoin, nipcw.Credential) (nipcwclient.JoinAction, error) {
	return f.action, nil
}

// joinSettleRow is one join in `cash settle`'s report.
type joinSettleRow struct {
	ConnectionPubkey string `json:"connection_pubkey"`
	HubPubkey        string `json:"hub_pubkey"`
	Label            string `json:"hub_label,omitempty"`
	State            string `json:"state"`
	Detail           string `json:"detail,omitempty"`
	Deadline         int64  `json:"deadline"`
	RetryAfter       int64  `json:"retry_after,omitempty"`
	Wallet           string `json:"wallet,omitempty"`
	Summary          string `json:"summary,omitempty"`
	HubVerified      bool   `json:"hub_verified"`
}

func joinRowOf(j config.Join, summary string) joinSettleRow {
	return joinSettleRow{ConnectionPubkey: j.ConnectionPubkey, HubPubkey: j.HubPubkey, Label: output.Sanitize(j.HubLabel),
		State: j.State, Detail: j.Detail, Deadline: j.Deadline, RetryAfter: j.RetryAfter, Wallet: j.WalletName,
		Summary: summary, HubVerified: j.ConnVerified}
}

// settleJoins looks up every open join that is due, and reports whether one
// placed from this device is still open.
func settleJoins(cmd *cobra.Command, device string) (rows []joinSettleRow, stillOpen bool) {
	joins, err := config.Joins()
	if err != nil {
		return nil, false
	}
	asFlag := asCredentialValue(cmd)
	now := time.Now().Unix()
	for _, j := range joins {
		if !j.Open() {
			if j.State == config.JoinShown {
				rows = append(rows, joinRowOf(j, ""))
			}
			continue
		}
		due := j.Deadline + 1
		if j.RetryAfter > 0 {
			due = j.RetryAfter
		}
		if j.State == config.JoinWritten && now < j.Deadline+1 {
			// The reply may still come to the join that sent it.
			due = j.Deadline + 1
		}
		if now < due {
			rows = append(rows, joinRowOf(j, "not due yet"))
			stillOpen = stillOpen || j.DeviceID == device
			continue
		}
		cred, summary := settleJoinCredential(cmd, j, asFlag)
		if summary != "" {
			rows = append(rows, joinRowOf(j, summary))
			stillOpen = stillOpen || j.DeviceID == device
			continue
		}
		out := settleOneJoin(cmd, j, cred)
		rows = append(rows, joinRowOf(out.Join, out.Summary))
		if out.Join.Open() && j.DeviceID == device {
			stillOpen = true
		}
	}
	return rows, stillOpen
}

// settleJoinCredential is the credential a join's lookup is signed with: --as,
// else the local identity when it is the one that joined. summary says why
// there is none.
func settleJoinCredential(cmd *cobra.Command, j config.Join, asFlag string) (nipcw.Credential, string) {
	if asFlag != "" {
		cred, err := joinCredential(cmd, asFlag)
		if err != nil {
			return nipcw.Credential{}, "--as is not a circle credential"
		}
		return cred, ""
	}
	if pub, err := localPubKeyHex(cmd); err != nil || pub != j.MemberPubkey {
		return nipcw.Credential{}, "joined under --as: run `cashctl cash settle --as pubkey:<privkey>` with that key"
	}
	cred, err := localCircleCredential(cmd)
	if err != nil {
		return nipcw.Credential{}, "the local identity can't be read"
	}
	return cred, ""
}

// settleOneJoin dials the join's Hub and looks the join up once.
func settleOneJoin(cmd *cobra.Command, j config.Join, cred nipcw.Credential) joinOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := j.HubConnection
	unanswered := !j.LookupAnswered && time.Now().Unix() > j.Deadline+joinUnansweredAfter
	if unanswered && !j.AltLookupTried {
		// Every lookup omitted: the string it joined with may have been
		// revoked. Sign the next lookup's bill proof with another string of
		// the same circle, once (R-CW-21).
		if others, _ := config.OtherHubConnections(j.HubPubkey, j.HubConnection); len(others) > 0 {
			conn = others[0]
			_ = config.MarkJoinLookup(j.ConnectionPubkey, false, true)
			unanswered = false
		}
	}
	out := lookupJoinVia(ctx, cmd, conn, j, cred)
	if unanswered && out.Join.Open() {
		if fresh, ok, _ := config.FindJoin(j.ConnectionPubkey); ok && !fresh.LookupAnswered {
			_ = config.SetJoinState(j.ConnectionPubkey, config.JoinShown, "lookup_unanswered", 0)
			out.Join.State, out.Join.Detail = config.JoinShown, "lookup_unanswered"
			out.Summary = "no lookup was ever answered: the join is kept, shown, and never rejoined with its key"
		}
	}
	return out
}

// joinUnansweredAfter is how long past a join's deadline its lookups may all
// go unanswered before another string of the circle is tried, then the join
// is shown as lookup_unanswered (NIP-CW §Lost Join Reply).
const joinUnansweredAfter = 360

// lookupJoinVia dials conn (a circlehub1 of the join's circle) and looks the
// join up once.
func lookupJoinVia(ctx context.Context, cmd *cobra.Command, conn string, j config.Join, cred nipcw.Credential) joinOutcome {
	client, err := dialCircleHubFn(ctx, conn)
	if err != nil {
		return joinOutcome{Join: j, Summary: "Hub unreachable: " + output.Sanitize(err.Error())}
	}
	defer client.Close()
	return lookupJoinOnce(ctx, cmd, client, j, cred, false)
}
