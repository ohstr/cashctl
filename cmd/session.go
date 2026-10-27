package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"

	nipcashclient "github.com/ohstr/nmilat/nipcash/client"

	"github.com/ohstr/cashctl/internal/crashpoint"
	"github.com/ohstr/cashctl/internal/store"
)

// powMaxEnv caps the proof of work cashctl does for a Hub's relays (NIP-CASH
// §Proof of Work): a relay asking for more is not sent to.
const powMaxEnv = "CASHCTL_POW_MAX"

// defaultPowMax is the proof-of-work difficulty cashctl mines up to (Q-11).
// The time it may take is bounded by each call's deadline in nmilat.
const defaultPowMax = 28

// powMaxSetting is the config key of the proof-of-work cap.
const powMaxSetting = "pow_max_difficulty"

// parsePowMax reads a difficulty between 0 and 64.
func parsePowMax(source, v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 64 {
		return 0, fmt.Errorf("%s %q is not a difficulty between 0 and 64", source, v)
	}
	return n, nil
}

// powMax is the proof-of-work cap: --pow-max, else CASHCTL_POW_MAX, else the
// pow_max_difficulty setting (`cashctl config set`), else defaultPowMax.
func powMax() (int, error) {
	if f := RootCmd.PersistentFlags().Lookup("pow-max"); f != nil && f.Changed {
		return parsePowMax("--pow-max", f.Value.String())
	}
	if v := os.Getenv(powMaxEnv); v != "" {
		return parsePowMax(powMaxEnv, v)
	}
	if v, ok, err := store.Setting(powMaxSetting); err == nil && ok {
		return parsePowMax(powMaxSetting, v)
	}
	return defaultPowMax, nil
}

// mustPowMax is powMax, falling back to the default.
func mustPowMax() int {
	n, err := powMax()
	if err != nil {
		return defaultPowMax
	}
	return n
}

// sessionOptions are the options of every private-transport session cashctl
// opens (bill calls, lookups, joins, mints): the proof-of-work budget. An
// unreadable cap falls back to the default rather than lifting the
// cap.
func sessionOptions() []nipcashclient.SessionOption {
	return []nipcashclient.SessionOption{nipcashclient.WithPowBudget(mustPowMax(), 0)}
}

// callContext marks ctx for a call cashctl places: right after nmilat publishes
// a request, the calls.after_send crash point is hit; a crashpoints build may
// also drop publishes (publishFilter).
func callContext(ctx context.Context) context.Context {
	ctx = nipcashclient.WithAfterPublish(ctx, func() { crashpoint.Hit("calls.after_send") })
	if keep := publishFilter(); keep != nil {
		ctx = nipcashclient.WithPublishFilter(ctx, keep)
	}
	return ctx
}
