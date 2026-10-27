package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"

	nipcashclient "github.com/ohstr/nmilat/nipcash/client"

	"github.com/ohstr/cashctl/internal/crashpoint"
)

// powMaxEnv caps the proof of work cashctl does for a Hub's relays (NIP-CASH
// §Proof of Work): a relay asking for more is not sent to.
const powMaxEnv = "CASHCTL_POW_MAX"

// defaultPowMax is the proof-of-work difficulty cashctl mines up to (Q-11).
// The time it may take is bounded by each call's deadline in nmilat.
const defaultPowMax = 28

// powMax is the proof-of-work cap: CASHCTL_POW_MAX, else defaultPowMax.
func powMax() (int, error) {
	v := os.Getenv(powMaxEnv)
	if v == "" {
		return defaultPowMax, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 64 {
		return 0, fmt.Errorf("%s=%q is not a difficulty between 0 and 64", powMaxEnv, v)
	}
	return n, nil
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
// unreadable CASHCTL_POW_MAX falls back to the default rather than lifting the
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
