package chain

import "testing"

// The built-in default must stay Flokicoin, byte-for-byte. A cashctl build ships
// as a Flokicoin client unless a fork deliberately reassigns Native, so a change
// to either value here changes what every default cashctl user sees — it must be
// intentional, not an accidental edit.
func TestNativeDefaultsToFlokicoin(t *testing.T) {
	want := Chain{Name: "flokicoin", Unit: "loki", SubunitRatio: 1000, HRP: "lokicash"}
	if Flokicoin != want {
		t.Fatalf("Flokicoin = %+v, want %+v", Flokicoin, want)
	}
	if Native != Flokicoin {
		t.Fatalf("Native = %+v, want the Flokicoin default %+v", Native, Flokicoin)
	}
}
