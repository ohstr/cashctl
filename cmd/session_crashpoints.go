//go:build crashpoints

package cmd

import (
	"os"
	"strconv"
	"sync/atomic"

	"github.com/ohstr/nmilat/nip01"
)

// dropPublishesEnv makes a crashpoints build drop its first n request
// publishes, as if lost on the way (E-07).
const dropPublishesEnv = "CASHCTL_DROP_PUBLISHES"

var dropped atomic.Int64

// publishFilter drops the first CASHCTL_DROP_PUBLISHES publishes.
func publishFilter() func(*nip01.Event) bool {
	n, err := strconv.Atoi(os.Getenv(dropPublishesEnv))
	if err != nil || n <= 0 {
		return nil
	}
	return func(*nip01.Event) bool { return dropped.Add(1) > int64(n) }
}
