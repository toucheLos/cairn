package collectors_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/collectors/gpu"
	"github.com/touchelos/cairn/collectors/journal"
	"github.com/touchelos/cairn/collectors/slurm"
	"github.com/touchelos/cairn/fixtures"
	"github.com/touchelos/cairn/schema"
)

const corpus = "../fixtures"

// implemented is the set of producers that have a collector in Phase 1.
// CLAUDE.md §6 scopes Phase 1 to slurm, journal, and gpu.
func registry() collectors.Registry {
	return collectors.Registry{slurm.New(), journal.New(), gpu.New()}
}

func implemented() map[schema.Source]bool {
	m := map[schema.Source]bool{}
	for _, c := range registry() {
		m[c.Source()] = true
	}
	return m
}

// TestReplay is the Phase 1 correctness test: every fixture, through every
// implemented collector, compared against the expected stream.
//
// The comparison is per-source. A fixture's expected/events.json is the complete
// answer including producers no collector reads yet, so comparing the whole
// stream would fail on fabric events that Phase 1 was never scoped to produce.
// Per-source keeps the target honest — each implemented collector must match its
// slice exactly — while letting coverage grow toward the full stream.
func TestReplay(t *testing.T) {
	fs, err := fixtures.LoadAll(corpus)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	impl := implemented()

	for _, f := range fs {
		t.Run(f.Meta.ID, func(t *testing.T) {
			env, req := setup(t, f)
			results := registry().Collect(context.Background(), env, req)
			got := collectors.MergeEvents(results)

			for _, r := range results {
				for _, w := range r.Warnings {
					t.Logf("[%s] warning: %s", r.Source, w)
				}
			}

			for src := range impl {
				wantSub := bySource(f.Expected, src)
				gotSub := bySource(got, src)
				if len(wantSub) == 0 && len(gotSub) == 0 {
					continue
				}
				wantEnc := mustEncode(t, wantSub)
				gotEnc := mustEncode(t, gotSub)
				if wantEnc != gotEnc {
					t.Errorf("source %q does not match expected/events.json\n--- want ---\n%s\n--- got ---\n%s\n%s",
						src, wantEnc, gotEnc, diffSummary(wantSub, gotSub))
				}
			}

			// Producers with no collector yet: report rather than assert.
			for _, src := range schema.AllSources() {
				if impl[src] {
					continue
				}
				if n := len(bySource(f.Expected, src)); n > 0 {
					t.Logf("not yet covered: %d event(s) from producer %q await a Phase 1+ collector", n, src)
				}
			}

			if unread := env.Unread(); len(unread) > 0 {
				t.Logf("input files no collector read: %v", unread)
			}
		})
	}
}

// TestEmittedEventsAreValid: a collector must never produce an event the schema
// rejects. Validate runs inside the encoder, so an invalid event would be
// discovered only when someone tried to write a bundle.
func TestEmittedEventsAreValid(t *testing.T) {
	fs, err := fixtures.LoadAll(corpus)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for _, f := range fs {
		env, req := setup(t, f)
		for _, r := range registry().Collect(context.Background(), env, req) {
			for i, e := range r.Events {
				if err := e.Validate(); err != nil {
					t.Errorf("%s: %s event %d is invalid: %v", f.Meta.ID, r.Source, i, err)
				}
			}
		}
	}
}

// TestCollectorsNeverFail exercises the §2.6 path: an environment where nothing
// is available must produce capability reports and no events, not an error and
// not a panic.
func TestCollectorsNeverFail(t *testing.T) {
	env := collectors.NewFixtureEnv(t.TempDir()) // no input/ at all
	req := collectors.Request{Cluster: "cluster-a"}

	results := registry().Collect(context.Background(), env, req)
	if len(results) != len(registry()) {
		t.Fatalf("got %d results, want %d", len(results), len(registry()))
	}
	for _, r := range results {
		if len(r.Events) != 0 {
			t.Errorf("%s produced %d events from an empty environment", r.Source, len(r.Events))
		}
		if len(r.Capabilities) == 0 {
			t.Errorf("%s reported no capabilities; doctor would have nothing to show", r.Source)
		}
		for _, c := range r.Capabilities {
			if c.Available {
				t.Errorf("%s reported capability %q as available in an empty environment", r.Source, c.Name)
			}
			if c.Detail == "" {
				t.Errorf("%s: capability %q is missing but says nothing about why", r.Source, c.Name)
			}
			if c.Reveals == "" {
				t.Errorf("%s: capability %q does not say what is lost without it", r.Source, c.Name)
			}
		}
	}
	if report := collectors.Report(results); !strings.Contains(report, "MISS") {
		t.Errorf("doctor report does not mark anything missing:\n%s", report)
	}
}

// TestPanickingCollectorIsContained: the interface says a collector must not
// panic, and safeCollect makes that true rather than merely stated. One
// misbehaving collector must not cost an admin the events that would have
// answered their question.
func TestPanickingCollectorIsContained(t *testing.T) {
	reg := collectors.Registry{panicky{}, slurm.New()}
	env := collectors.NewFixtureEnv(corpus + "/001-oom-cgroup")
	results := reg.Collect(context.Background(), env, collectors.Request{Cluster: "cluster-a"})

	var slurmEvents int
	var sawWarning bool
	for _, r := range results {
		if r.Source == schema.SourceSlurm {
			slurmEvents = len(r.Events)
		}
		for _, w := range r.Warnings {
			if strings.Contains(w, "panicked") {
				sawWarning = true
			}
		}
	}
	if slurmEvents == 0 {
		t.Error("a panicking collector suppressed another collector's events")
	}
	if !sawWarning {
		t.Error("the panic was swallowed without a warning")
	}
}

type panicky struct{}

func (panicky) Source() schema.Source { return schema.SourceFabric }
func (panicky) Collect(context.Context, collectors.Env, collectors.Request) collectors.Result {
	panic("deliberate panic from a test collector")
}

// TestDeterministicAcrossRuns: the same fixture must produce byte-identical
// output every time (§2.7). Map iteration and regexp alternation are both places
// this could quietly fail.
func TestDeterministicAcrossRuns(t *testing.T) {
	fs, err := fixtures.LoadAll(corpus)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for _, f := range fs {
		env, req := setup(t, f)
		first := mustEncode(t, collectors.MergeEvents(registry().Collect(context.Background(), env, req)))
		for i := 0; i < 20; i++ {
			env2, req2 := setup(t, f)
			got := mustEncode(t, collectors.MergeEvents(registry().Collect(context.Background(), env2, req2)))
			if got != first {
				t.Fatalf("%s: collection is not deterministic on run %d:\n%s\nvs\n%s", f.Meta.ID, i, first, got)
			}
		}
	}
}

// setup builds the environment and request for replaying one fixture.
//
// The job and node come from meta.incident, never from the expected events. A
// harness that derived them from the answer would be handing the collector the
// thing it is supposed to be finding.
func setup(t *testing.T, f *fixtures.Fixture) (*collectors.FixtureEnv, collectors.Request) {
	t.Helper()
	env := collectors.NewFixtureEnv(f.Dir)
	env.Host = f.Meta.Incident.Node

	req := collectors.Request{Cluster: schema.ClusterName(f.Meta.Cluster)}
	if f.Meta.Incident.Job != "" {
		j, err := schema.ParseJobID(f.Meta.Incident.Job)
		if err != nil {
			t.Fatalf("%s: meta.incident.job: %v", f.Meta.ID, err)
		}
		req.Job = j
	}
	if f.Meta.Incident.Node != "" {
		req.Nodes = []schema.Hostname{schema.Hostname(f.Meta.Incident.Node)}
	}
	return env, req
}

func bySource(evs []schema.Event, src schema.Source) []schema.Event {
	var out []schema.Event
	for _, e := range evs {
		if e.Source == src {
			out = append(out, e)
		}
	}
	return out
}

func mustEncode(t *testing.T, evs []schema.Event) string {
	t.Helper()
	data, err := schema.EncodeEvents(evs)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	return string(data)
}

// diffSummary lists signatures present on one side only. Comparing two JSON
// blobs by eye is slow; naming the missing and extra signatures is usually
// enough to see what went wrong.
func diffSummary(want, got []schema.Event) string {
	count := func(evs []schema.Event) map[string]int {
		m := map[string]int{}
		for _, e := range evs {
			m[fmt.Sprintf("%s %s", e.Class, e.Detail.Signature)]++
		}
		return m
	}
	w, g := count(want), count(got)
	var lines []string
	for k, n := range w {
		if g[k] < n {
			lines = append(lines, fmt.Sprintf("  missing (%d): %s", n-g[k], k))
		}
	}
	for k, n := range g {
		if w[k] < n {
			lines = append(lines, fmt.Sprintf("  extra   (%d): %s", n-w[k], k))
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "  (same signatures; the difference is in timestamps, nodes, job ids, or attrs)"
	}
	return strings.Join(lines, "\n")
}
