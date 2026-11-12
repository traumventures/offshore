package orchestrator

import (
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/labels"
	"github.com/traumventures/offshore/internal/tracing"
)

// Per-session time series: workers sample each session cgroup every 5 s
// and report the last samples in their status; the orchestrator logs the
// new ones as session_sample events (retained like every event), keeps
// the last hour per session in memory for status/top detail views and
// exports them as OTEL gauges when telemetry is configured.

// sessionSeriesMax bounds the samples kept in memory per session (1 h).
const sessionSeriesMax = 720

// sessionSeriesTTL is how long the series of an ended session stays in
// memory (detail views of just finished sessions).
const sessionSeriesTTL = 10 * time.Minute

// sessionSeries is the recent time series of one session (lease).
type sessionSeries struct {
	machine string
	last    time.Time // newest sample logged
	seen    time.Time // when the session was last reported running
	samples []events.Sample
}

// sessionSamplesLocked logs the samples of machine id's sessions that are
// newer than the ones already logged.
func (o *Orchestrator) sessionSamplesLocked(id string, m *Machine) {
	st := m.Status
	if st == nil {
		return
	}
	now := o.now()
	var gauges []tracing.Gauge
	o.obs.mu.Lock()
	defer func() {
		o.obs.mu.Unlock()
		tracing.ExportGauges(gauges)
	}()
	for lease, s := range o.obs.series {
		if now.Sub(s.seen) > sessionSeriesTTL {
			delete(o.obs.series, lease)
		}
	}
	for _, se := range st.Sessions {
		if se.Lease == "" || len(se.Samples) == 0 {
			continue
		}
		s := o.obs.series[se.Lease]
		if s == nil {
			s = &sessionSeries{machine: id}
			o.obs.series[se.Lease] = s
		}
		s.seen = now
		t := o.obs.tasks[se.Lease].ev
		for _, smp := range se.Samples {
			if !smp.Time.After(s.last) {
				continue
			}
			s.last = smp.Time
			s.samples = append(s.samples, smp)
			if n := len(s.samples); n > sessionSeriesMax {
				s.samples = append(s.samples[:0:0], s.samples[n-sessionSeriesMax:]...)
			}
			sc := smp
			ev := events.Event{Time: smp.Time, Type: events.SessionSample, Task: se.Lease, Machine: id, Provider: m.Provider,
				Repo: t.Repo, Key: t.Key, Labels: labels.Clean(se.Labels), Sample: &sc}
			if ev.Repo == "" {
				ev.Repo = se.Repo
			}
			o.obs.emit(ev)
			attrs := []attribute.KeyValue{attribute.String("offshore.lease", se.Lease), attribute.String("offshore.machine", id),
				attribute.String("offshore.repo_id", ev.Repo), attribute.String("offshore.usage_key", ev.Key)}
			gauges = append(gauges,
				tracing.Gauge{Name: "offshore.session.cpu", Unit: "{core}", Value: smp.CPU, Time: smp.Time, Attrs: attrs},
				tracing.Gauge{Name: "offshore.session.memory", Unit: "By", Value: float64(smp.Memory), Time: smp.Time, Attrs: attrs},
				tracing.Gauge{Name: "offshore.session.io.read", Unit: "By", Value: float64(smp.IORead), Time: smp.Time, Attrs: attrs},
				tracing.Gauge{Name: "offshore.session.io.write", Unit: "By", Value: float64(smp.IOWrite), Time: smp.Time, Attrs: attrs},
				tracing.Gauge{Name: "offshore.session.pressure.cpu", Unit: "1", Value: smp.PSICPU, Time: smp.Time, Attrs: attrs},
				tracing.Gauge{Name: "offshore.session.pressure.memory", Unit: "1", Value: smp.PSIMemory, Time: smp.Time, Attrs: attrs},
				tracing.Gauge{Name: "offshore.session.pressure.io", Unit: "1", Value: smp.PSIIO, Time: smp.Time, Attrs: attrs},
			)
		}
	}
}

// SessionSamples returns the samples of a session (lease) since a time
// (zero: all): from memory for running and just ended sessions, else from
// the event log (last day).
func (o *Orchestrator) SessionSamples(lease string, since time.Time) []events.Sample {
	o.obs.mu.Lock()
	s := o.obs.series[lease]
	var out []events.Sample
	if s != nil {
		for _, smp := range s.samples {
			if smp.Time.After(since) || since.IsZero() {
				out = append(out, smp)
			}
		}
	}
	o.obs.mu.Unlock()
	if s != nil {
		return out
	}
	from := since
	if from.IsZero() {
		from = o.now().Add(-24 * time.Hour)
	}
	evs, _ := events.Read(o.obs.log.Dir(), from, time.Time{})
	for _, ev := range evs {
		if ev.Type == events.SessionSample && ev.Task == lease && ev.Sample != nil {
			out = append(out, *ev.Sample)
		}
	}
	return out
}
