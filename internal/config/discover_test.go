package config

import (
	"fmt"
	"testing"
)

// D-1: a flood of global listings never prunes a trusted event, however old.
func TestPruneDiscoverEvents_KeepsTrusted(t *testing.T) {
	withTempConfigDir(t)
	if err := PutDiscoverEvent("rec", 31191, "curator", 1, `{"id":"rec"}`, OriginTrusted, ""); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		id := fmt.Sprintf("junk%02d", i)
		if err := PutDiscoverEvent(id, 31190, "stranger", int64(100+i), `{"id":"`+id+`"}`, OriginGlobal, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := PruneDiscoverEvents(5); err != nil {
		t.Fatal(err)
	}
	raws, err := DiscoverEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(raws) != 6 || raws[0] != `{"id":"rec"}` {
		t.Fatalf("kept %d events, first %q: want the trusted one and the 5 newest global", len(raws), raws[0])
	}
}
