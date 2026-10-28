package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"

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

// parsePowMax reads a difficulty between 1 and 64.
func parsePowMax(source, v string) (int, error) {
	n, err := strconv.Atoi(v)
	// 0 is refused: nmilat reads a zero budget as no cap at all.
	if err != nil || n < 1 || n > 64 {
		return 0, fmt.Errorf("%s %q is not a difficulty between 1 and 64", source, v)
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

// writtenAheadKey holds, in a call's context, whether its journal has written
// the call ahead yet.
type writtenAheadKey struct{}

// markWrittenAhead records in ctx that the call was written ahead, so the
// publishes that follow are the call's own.
func markWrittenAhead(ctx context.Context) {
	if w, ok := ctx.Value(writtenAheadKey{}).(*atomic.Bool); ok {
		w.Store(true)
	}
}

// callContext marks ctx for a call cashctl places: right after nmilat publishes
// the call itself, the calls.after_send crash point is hit (not for a read it
// publishes first, such as an Order connection's terms, before anything was
// written ahead); a crashpoints build may also drop publishes (publishFilter).
func callContext(ctx context.Context) context.Context {
	written := new(atomic.Bool)
	ctx = context.WithValue(ctx, writtenAheadKey{}, written)
	ctx = nipcashclient.WithAfterPublish(ctx, func() {
		if written.Load() {
			crashpoint.Hit("calls.after_send")
		}
	})
	if keep := publishFilter(); keep != nil {
		ctx = nipcashclient.WithPublishFilter(ctx, keep)
	}
	return ctx
}
