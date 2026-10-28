package cmd

import (
	"strings"
	"time"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// openCall is one call with no final answer, as `wallet show` reports it:
// enough to recognise it, and every secret it generated. A secret is shown in
// full on purpose: until the call is settled it may be the only copy of what
// spends a bill (NIP-CASH: surface any unreconciled secret to the user).
type openCall struct {
	Call         string           `json:"call"`
	Method       string           `json:"method"`
	Purpose      string           `json:"purpose"`
	State        string           `json:"state"`
	Detail       string           `json:"detail,omitempty"`
	Deadline     int64            `json:"deadline"`
	TokenIDs     []string         `json:"token_ids"`
	AmountMillis uint64           `json:"amount_millis"`
	Chain        string           `json:"chain"`
	Device       string           `json:"device"`
	Secrets      []openCallSecret `json:"secrets,omitempty"`
}

type openCallSecret struct {
	Role   string `json:"role"`
	Secret string `json:"secret"`
}

// openCalls is every open call in the ledger, with whether it was placed from
// this wallet file ("this") or a copy of it ("other").
func openCalls(l *ledger.Ledger, device string) []openCall {
	var out []openCall
	for _, c := range l.Calls {
		if !c.Open() {
			continue
		}
		oc := openCall{Call: c.CallID, Method: c.Method, Purpose: c.Purpose, State: c.State, Detail: c.Detail,
			Deadline: c.Deadline, TokenIDs: output.NonNil(c.SourceEntryIDs), AmountMillis: c.AmountMillis,
			Chain: c.Chain, Device: "other"}
		if c.DeviceID == device {
			oc.Device = "this"
		}
		for _, s := range c.Secrets {
			oc.Secrets = append(oc.Secrets, openCallSecret{Role: s.Role, Secret: s.Secret})
		}
		out = append(out, oc)
	}
	return out
}

// printOpenCalls is openCalls in text, on stdout after the rest of `wallet
// show`.
func printOpenCalls(l *ledger.Ledger, device string) {
	calls := openCalls(l, device)
	if len(calls) == 0 {
		return
	}
	output.Printf("\nCalls with no answer yet: %d — each bill they spend is used again only once its Hub says what happened\n", len(calls))
	for _, c := range calls {
		output.Printf("  call %s   %s of %s   %s   deadline %s\n", c.Call, c.Purpose, strings.Join(c.TokenIDs, ","),
			output.FormatAmount(int64(c.AmountMillis)), time.Unix(c.Deadline, 0).UTC().Format("2006-01-02 15:04 UTC")) //nolint:gosec // amounts fit
		for _, s := range c.Secrets {
			output.Printf("     new code (%s), keep it — it may be the only one that spends the bill: %s\n", s.Role, s.Secret)
		}
	}
}
