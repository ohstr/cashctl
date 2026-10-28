//go:build crashpoints

package cmd

import "testing"

// CASHCTL_DROP_PUBLISHES=n drops the first n publishes, then keeps the rest.
func TestPublishFilter_DropsTheFirstN(t *testing.T) {
	t.Setenv(dropPublishesEnv, "2")
	dropped.Store(0)
	keep := publishFilter()
	got := []bool{keep(nil), keep(nil), keep(nil)}
	if got[0] || got[1] || !got[2] {
		t.Errorf("kept %v, want [false false true]", got)
	}
	t.Setenv(dropPublishesEnv, "")
	if publishFilter() != nil {
		t.Error("a filter without the variable")
	}
}
