package anyfs

import (
	"testing"

	"github.com/traumventures/offshore/internal/medium"
)

func TestRecordTransfers(t *testing.T) {
	var r medium.Recorder
	var passed int
	obs := RecordTransfers(&r, func(Event) { passed++ })
	obs(Event{Type: EvPageIn, Tier: "store", Bytes: 100, Seconds: 0.1})
	obs(Event{Type: EvWriteback, Tier: "volume>store", Bytes: 200, Seconds: 0.1})
	obs(Event{Type: EvPageFault, Tier: "files", Bytes: 300, Seconds: 0.1})
	obs(Event{Type: EvReclaim, Tier: "local", Bytes: 300})
	cs := r.Snapshot()
	if passed != 4 || len(cs) != 2 || cs[0].Medium != "store" || cs[0].Reads.N != 1 || cs[1].Medium != "volume" || cs[1].Writes.X != 200 {
		t.Fatalf("passed %d counters %+v", passed, cs)
	}
	RecordTransfers(&r, nil)(Event{Type: EvPageOut, Tier: "volume", Bytes: 1, Seconds: 1})
}
