package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// walletBalanceLine is one row of the itemized breakdown — a live wallet
// or a held cash token, each contributing to the unified total. Name
// stays the raw ledger ID for a held token (scriptable, --json only);
// DisplayName is what a human actually sees printed under --breakdown —
// never the raw ID (see docs/private/wallet-abstraction-plan.md).
type walletBalanceLine struct {
	Name        string `json:"name"`
	DisplayName string `json:"-"`
	AmountMloki int64  `json:"amount_mloki"`
	Kind        string `json:"kind"`               // "wallet" | "held_token"
	Stranded    bool   `json:"stranded,omitempty"` // expired wallet, showing a cached figure
	// Expired marks a held cash token past its cached Hub-side redemption
	// deadline (Entry.ExpiresAt) — unlike Stranded, this isn't "a real
	// balance we can't currently reach": NIP-CASH tokens are simply
	// unclaimable once expired, so its amount is never added to total.
	// Still listed here (not silently dropped) so a holder isn't left
	// wondering where the token in `wallet show`/`wallet history` went.
	Expired bool `json:"expired,omitempty"`
}

func runWalletBalance(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	breakdown, _ := cmd.Flags().GetBool("breakdown")
	from, _ := cmd.Flags().GetString("from")
	// -c/--connection scopes balance to that one wallet, exactly as --from
	// does: it used to be ignored, so `balance -c savings` summed every
	// wallet and printed a total that looked like savings' alone.
	if conn, _ := cmd.Flags().GetString("connection"); conn != "" {
		if from != "" && from != conn {
			return output.InvocationError(cmd, fmt.Errorf("got both --from (%q) and -c/--connection (%q) with different values — pass only one", output.Sanitize(from), output.Sanitize(conn)))
		}
		from = conn
	}

	if from != "" {
		return runWalletBalanceFrom(cmd, from, jsonMode)
	}

	s, err := config.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}

	var lines []walletBalanceLine
	var total, stranded, expiredHeld int64
	// unreachable names every registered wallet liveBalanceMloki couldn't
	// dial at all (not EXPIRED — that's already handled as "stranded",
	// with a cached figure still shown). A dial/network failure here used
	// to just skip the wallet with no trace in the output, so its balance
	// silently vanished from the total with no way to tell "this wallet is
	// empty" from "this wallet is down right now and might hold anything."
	var unreachable []string
	now := time.Now().Unix()

	_ = WithSpinner(jsonMode, "Fetching balances...", func() error {
		for _, c := range s.Connections {
			amount, isStranded, err := liveBalanceMloki(c)
			if err != nil {
				unreachable = append(unreachable, c.Name)
				continue
			}
			lines = append(lines, walletBalanceLine{Name: c.Name, DisplayName: c.Name, AmountMloki: amount, Kind: "wallet", Stranded: isStranded})
			total += amount
			if isStranded {
				stranded += amount
			}
			if amount >= 0 {
				s.SetLastKnownBalance(c.Name, amount)
			}
		}
		return nil
	})
	_ = s.Save() // best-effort cache update; a failure here shouldn't fail the whole command

	heldLines, heldTotal, heldExpired := summarizeHeldTokens(l.Held(), now)
	lines = append(lines, heldLines...)
	total += heldTotal
	expiredHeld += heldExpired

	if jsonMode {
		output.PrintJSON(map[string]any{
			"total_mloki":        total,
			"stranded_mloki":     stranded,
			"expired_held_mloki": expiredHeld,
			"unreachable":        output.NonNil(unreachable),
			"breakdown":          output.NonNil(lines),
		})
		return nil
	}

	switch {
	case stranded > 0 && expiredHeld > 0:
		fmt.Printf("%s total — %s stranded (expired wallet), %s in expired held tokens (not counted)\n",
			output.FormatAmount(total), output.FormatAmount(stranded), output.FormatAmount(expiredHeld))
	case stranded > 0:
		fmt.Printf("%s total — %s stranded (expired)\n", output.FormatAmount(total), output.FormatAmount(stranded))
	case expiredHeld > 0:
		fmt.Printf("%s total — %s in expired held tokens (not counted)\n", output.FormatAmount(total), output.FormatAmount(expiredHeld))
	default:
		fmt.Printf("%s total\n", output.FormatAmount(total))
	}
	if len(unreachable) > 0 {
		fmt.Printf("(couldn't reach %s — not counted, may still hold funds)\n", strings.Join(unreachable, ", "))
	}
	if breakdown {
		for _, line := range lines {
			marker := ""
			if line.Expired || line.Stranded {
				marker = " [expired]"
			}
			fmt.Printf("  %-20s %s%s\n", line.DisplayName, output.FormatAmount(line.AmountMloki), marker)
		}
	}
	return nil
}

// summarizeHeldTokens turns held cash-token entries into balance lines,
// splitting each entry's amount into the counted total or expiredHeld
// depending on whether now is past its cached Entry.ExpiresAt — nil
// ExpiresAt (never learned, e.g. a split remainder with no CheckClaim of
// its own yet) is always treated as not expired. Pure and separately
// tested so the exclusion rule itself doesn't need a live Hub or a real
// ledger.Load to verify (see wallet_balance_test.go).
func summarizeHeldTokens(held []ledger.Entry, now int64) (lines []walletBalanceLine, total, expiredHeld int64) {
	for _, e := range held {
		if e.AmountMillis == nil {
			continue
		}
		amount := int64(*e.AmountMillis)
		expired := e.ExpiresAt != nil && *e.ExpiresAt <= now
		lines = append(lines, walletBalanceLine{
			Name:        e.ID,
			DisplayName: fmt.Sprintf("held cash (%s)", formatReceivedDate(e.ReceivedAt)),
			AmountMloki: amount,
			Kind:        "held_token",
			Expired:     expired,
		})
		if expired {
			expiredHeld += amount
		} else {
			total += amount
		}
	}
	return lines, total, expiredHeld
}

func runWalletBalanceFrom(cmd *cobra.Command, from string, jsonMode bool) error {
	s, err := config.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	if c, ok := s.Find(from); ok {
		var amount int64
		var stranded bool
		err = WithSpinner(jsonMode, "Fetching balance...", func() error {
			var bErr error
			amount, stranded, bErr = liveBalanceMloki(*c)
			return bErr
		})
		if err != nil {
			return classifyNWCErr(cmd, err)
		}
		if jsonMode {
			output.PrintJSON(map[string]any{"name": from, "amount_mloki": amount, "stranded": stranded})
			return nil
		}
		fmt.Printf("%s\n", output.FormatAmount(amount))
		return nil
	}
	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	if e, ok := l.Find(from); ok && e.AmountMillis != nil {
		// Status, not just existence: l.Find matches any entry regardless
		// of status, so a spent/transferred/redeemed/consolidated token's
		// old cached amount used to be reported as if it were still a real
		// balance — the whole point of `status` is that it no longer is.
		if e.Status != ledger.StatusHeld {
			return output.NotFoundError(cmd, from, fmt.Errorf("token %q is no longer held (status: %s) — it has no balance to report", from, e.Status))
		}
		if jsonMode {
			output.PrintJSON(map[string]any{"name": from, "amount_mloki": *e.AmountMillis})
			return nil
		}
		fmt.Printf("%s\n", output.FormatAmount(int64(*e.AmountMillis)))
		return nil
	}
	return output.NotFoundError(cmd, from, fmt.Errorf("no wallet or held token named %q", from))
}

// liveBalanceMloki dials c and calls get_balance. On an EXPIRED decline
// specifically, falls back to the cached LastKnownBalanceMloki (if any),
// flagged as stranded — see Connection's own doc comment for why that's
// the only way to show a figure at all for an expired wallet.
func liveBalanceMloki(c config.Connection) (amount int64, stranded bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := DialGeneric(ctx, c.Value)
	if err != nil {
		return 0, false, err
	}
	defer client.Close()

	result, err := client.GetBalance(ctx)
	if err == nil {
		return result.BalanceMloki, false, nil
	}
	var walletErr *relayclient.WalletError
	if errors.As(err, &walletErr) && walletErr.Code == "EXPIRED" && c.LastKnownBalanceMloki != nil {
		return *c.LastKnownBalanceMloki, true, nil
	}
	return 0, false, err
}
