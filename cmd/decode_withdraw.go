package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
)

// decode of the withdrawal artefacts (NIP-CASH §Redeeming On-chain): a quote
// (kind 23187), a receipt (kind 23188), a payout object and a witness address.
// Nothing here contacts a Hub; --hub checks a quote's author.

// readJSONArg is a JSON argument: given inline, as a file, or - for stdin.
func readJSONArg(cmd *cobra.Command, arg string) (string, error) {
	s := strings.TrimSpace(arg)
	switch {
	case s == stdinArg:
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", output.RuntimeError(cmd, err)
		}
		return strings.TrimSpace(string(b)), nil
	case strings.HasPrefix(s, "{"):
		return s, nil
	}
	b, err := os.ReadFile(s)
	if err != nil {
		return "", output.InvalidInputError(cmd, s, fmt.Errorf("not JSON and not a readable file: %w", err))
	}
	return strings.TrimSpace(string(b)), nil
}

func tagValues(ev *nip01.Event) map[string][]string {
	out := map[string][]string{}
	for _, t := range ev.Tags {
		if len(t) > 1 {
			out[t[0]] = t[1:]
		}
	}
	return out
}

func decodeWithdrawEvent(cmd *cobra.Command, raw string, jsonMode bool) error {
	var ev nip01.Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return output.InvalidInputError(cmd, "", err)
	}
	out := map[string]any{"id": ev.ID, "pubkey": ev.PubKey, "created_at": ev.CreatedAt, "signature_valid": ev.Verify(nip01.WithoutPowCheck()) == nil}
	switch ev.Kind {
	case nipcash.KindWithdrawQuote:
		q, err := nipcash.ParseWithdrawQuote(raw)
		if err != nil {
			return output.InvalidInputError(cmd, "", err)
		}
		out["type"] = "withdraw_quote"
		out["wallet_pubkey"], out["identity_value"], out["address"] = q.Wallet, q.IdentityValue, q.Address
		if q.IAPubkey != "" {
			out["ia_pubkey"] = q.IAPubkey
		}
		out["amount_millis"], out["network_fee_millis"], out["hub_fee_millis"] = q.AmountMillis, q.NetworkFeeMillis, q.HubFeeMillis
		out["payout_millis"], out["fee_millis"], out["sent_by"], out["holds_until"] = q.PayoutMillis, q.FeeMillis(), q.SentBy, q.HoldsUntil
		if hub, _ := cmd.Flags().GetString("hub"); hub != "" {
			out["hub_verified"] = nipcash.VerifyWithdrawQuote(q, hub) == nil
		}
	case nipcash.KindWithdrawReceipt:
		out["type"] = "withdraw_receipt"
		for k, v := range tagValues(&ev) {
			switch k {
			case "d":
				out["wallet_pubkey"] = v[0]
			case "q":
				out["quote_id"] = v[0]
			default:
				out[k] = v[0]
			}
		}
	default:
		return output.InvalidInputError(cmd, "", fmt.Errorf("a kind %d event, not a withdraw quote or receipt", ev.Kind))
	}
	if jsonMode {
		output.PrintJSON(out)
		return nil
	}
	output.Printf("type: %v\n", out["type"])
	for _, k := range []string{"wallet_pubkey", "address", "amount_millis", "network_fee_millis", "hub_fee_millis", "fee_millis", "payout_millis", "sent_by", "holds_until", "quote_id", "request_hash"} {
		if v, ok := out[k]; ok {
			output.Printf("%s: %v\n", k, v)
		}
	}
	output.Printf("signature: %s\n", map[bool]string{true: "valid", false: "INVALID"}[out["signature_valid"] == true])
	return nil
}

func decodePayout(cmd *cobra.Command, raw string, jsonMode bool) error {
	p, err := nipcash.ParsePayout([]byte(raw))
	if err != nil {
		var gone *nipcash.BillGone
		if errors.As(err, &gone) {
			return output.InvalidInputError(cmd, "", errors.New("that is a tombstone, not a payout"))
		}
		return output.InvalidInputError(cmd, "", err)
	}
	out := map[string]any{"type": "payout", "state": string(p.State), "address": p.Address, "amount_millis": p.AmountMillis,
		"fee_millis": p.FeeMillis, "payout_millis": p.PayoutMillis, "sent_by": p.SentBy, "final_depth": p.FinalDepth,
		"signed": p.Signed, "tx_present": p.Tx != ""}
	if p.Txid != "" {
		out["txid"] = p.Txid
	}
	if p.Vout != nil {
		out["vout"] = *p.Vout
	}
	if p.Depth != nil {
		out["depth"] = *p.Depth
	}
	if jsonMode {
		output.PrintJSON(out)
		return nil
	}
	output.Printf("type: payout\nstate: %s\naddress: %s\npayout: %s by %s\n", p.State, p.Address,
		output.FormatAmount(int64(p.PayoutMillis)), time.Unix(p.SentBy, 0).UTC().Format(time.RFC3339)) //nolint:gosec // amounts fit
	return nil
}

func decodeWitnessAddress(cmd *cobra.Command, value string, jsonMode bool) error {
	addr, err := nipcash.NormalizeAddress(value)
	if err != nil {
		return output.InvalidInputError(cmd, value, err)
	}
	var hrps []string
	for _, chain := range []string{"bitcoin", "flokicoin"} {
		h, _ := nipcash.WithdrawHRPs(chain)
		hrps = append(hrps, h...)
	}
	st, hrp, err := nipcash.ClassifyAddressAmong(addr, hrps)
	if err != nil {
		return output.InvalidInputError(cmd, value, addressRefusal(err))
	}
	out := map[string]any{"type": "address", "address": addr, "hrp": hrp, "script_type": st, "script_name": scriptName(st)}
	if jsonMode {
		output.PrintJSON(out)
		return nil
	}
	output.Printf("type: address\nhrp: %s\nscript: %s\n", hrp, scriptName(st))
	return nil
}

func scriptName(st nipcash.ScriptType) string {
	switch {
	case st.Version == 0 && st.ProgramLen == 20:
		return "P2WPKH"
	case st.Version == 0 && st.ProgramLen == 32:
		return "P2WSH"
	case st.Version == 1 && st.ProgramLen == 32:
		return "P2TR"
	}
	return fmt.Sprintf("witness v%d, %d-byte program", st.Version, st.ProgramLen)
}
