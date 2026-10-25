// Package crashpoint names the points where a test may stop cashctl dead, to
// prove that what was written before that point is enough to recover.
//
// Hit(name) does nothing in a release build. Built with the crashpoints tag,
// it kills the process (SIGKILL, no deferred code runs) when
// CASHCTL_CRASHPOINT equals name, so the integration suite can crash the real
// binary at that point. Without the tag the environment can never crash it.
// Unit tests use SetHook instead, in any build.
package crashpoint

import "sync/atomic"

// EnvVar names the crash point a crashpoints build kills itself at.
const EnvVar = "CASHCTL_CRASHPOINT"

var hook atomic.Pointer[func(name string)]

// Hit marks the crash point name.
func Hit(name string) {
	if h := hook.Load(); h != nil {
		(*h)(name)
	}
	kill(name)
}

// SetHook makes every Hit call fn first, typically to panic at one named
// point, and returns a function that removes it.
func SetHook(fn func(name string)) (restore func()) {
	prev := hook.Swap(&fn)
	return func() { hook.Store(prev) }
}
