package cmd

import (
	"context"
	"errors"
	"fmt"
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
}

func runWalletBalance(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	breakdown, _ := cmd.Flags().GetBool("breakdown")
	from, _ := cmd.Flags().GetString("from")

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
	var total, stranded int64

	for _, c := range s.Connections {
		amount, isStranded, err := liveBalanceMloki(c)
		if err != nil {
			continue // unreachable right now — omitted, not fatal to the whole command
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
	_ = s.Save() // best-effort cache update; a failure here shouldn't fail the whole command

	for _, e := range l.Held() {
		if e.AmountMillis == nil {
			continue
		}
		amount := int64(*e.AmountMillis)
		lines = append(lines, walletBalanceLine{
			Name:        e.ID,
			DisplayName: fmt.Sprintf("held cash (%s)", formatReceivedDate(e.ReceivedAt)),
			AmountMloki: amount,
			Kind:        "held_token",
		})
		total += amount
	}

	if jsonMode {
		output.PrintJSON(map[string]any{
			"total_mloki":    total,
			"stranded_mloki": stranded,
			"breakdown":      lines,
		})
		return nil
	}

	if stranded > 0 {
		fmt.Printf("%d %s total — %d %s in an expired wallet (renew or it may be swept)\n", total, output.CurrencyUnit, stranded, output.CurrencyUnit)
	} else {
		fmt.Printf("%d %s total\n", total, output.CurrencyUnit)
	}
	if breakdown {
		for _, line := range lines {
			marker := ""
			if line.Stranded {
				marker = " [expired — money-moving disabled]"
			}
			fmt.Printf("  %-20s %d %s%s\n", line.DisplayName, line.AmountMloki, output.CurrencyUnit, marker)
		}
	}
	return nil
}

func runWalletBalanceFrom(cmd *cobra.Command, from string, jsonMode bool) error {
	s, err := config.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	if c, ok := s.Find(from); ok {
		amount, stranded, err := liveBalanceMloki(*c)
		if err != nil {
			return classifyNWCErr(cmd, err)
		}
		if jsonMode {
			output.PrintJSON(map[string]any{"name": from, "amount_mloki": amount, "stranded": stranded})
			return nil
		}
		fmt.Printf("%d %s\n", amount, output.CurrencyUnit)
		return nil
	}
	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	if e, ok := l.Find(from); ok && e.AmountMillis != nil {
		if jsonMode {
			output.PrintJSON(map[string]any{"name": from, "amount_mloki": *e.AmountMillis})
			return nil
		}
		fmt.Printf("%d %s\n", *e.AmountMillis, output.CurrencyUnit)
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
