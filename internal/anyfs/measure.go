package anyfs

import "github.com/traumventures/offshore/internal/medium"

// RecordTransfers returns an observer that counts the page-ins (reads),
// writebacks and page-outs (writes) of the tiers below in r, per medium
// (medium.MediumOf of the event's tier), and passes every event on to
// next (nil: none). Workers report r's counters to the orchestrator,
// which turns them into measured medium profiles.
func RecordTransfers(r *medium.Recorder, next Observer) Observer {
	return func(e Event) {
		switch e.Type {
		case EvPageIn:
			r.Add(medium.MediumOf(e.Tier), false, e.Bytes, e.Seconds)
		case EvWriteback, EvPageOut:
			r.Add(medium.MediumOf(e.Tier), true, e.Bytes, e.Seconds)
		}
		next.emit(e)
	}
}
