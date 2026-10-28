package crashpoint

import "testing"

func TestSetHook_SeesEveryHitUntilRestored(t *testing.T) {
	var seen []string
	restore := SetHook(func(name string) { seen = append(seen, name) })
	Hit("a")
	Hit("b")
	restore()
	Hit("c")
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "b" {
		t.Errorf("hook saw %v, want [a b]", seen)
	}
}

func TestSetHook_CanPanicAtANamedPoint(t *testing.T) {
	defer SetHook(func(name string) {
		if name == "calls.after_send" {
			panic(name)
		}
	})()
	Hit("calls.before_send")
	defer func() {
		if r := recover(); r != "calls.after_send" {
			t.Errorf("recovered %v, want a panic at calls.after_send", r)
		}
	}()
	Hit("calls.after_send")
	t.Error("Hit returned past the hooked point")
}
