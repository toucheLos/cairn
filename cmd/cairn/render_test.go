package main

import (
	"strings"
	"testing"
	"time"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/join"
	"github.com/touchelos/cairn/schema"
)

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := schema.ParseTime(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func rel(t *testing.T, at, sig string, r join.Relation, attrs map[string]string) join.Related {
	t.Helper()
	var job *schema.JobID
	if r == join.RelDirect {
		j, err := schema.ParseJobID("918633")
		if err != nil {
			t.Fatal(err)
		}
		job = j
	}
	return join.Related{
		Event: schema.Event{
			TS: ts(t, at), Cluster: "cluster-a", Node: "node-0045", JobID: job,
			Source: schema.SourceJournal, Class: schema.ClassFabricLinkFlap,
			Detail: schema.Detail{Signature: sig, Attrs: attrs},
		},
		Relation: r,
	}
}

// TestCollapsePreservesAlternation is the test that matters most here. A port
// cycling down/up/down/up/down is a failing cable; one down/up is a
// reconfiguration. Merging the three "down" events across the "up"s between them
// would erase exactly that distinction.
func TestCollapsePreservesAlternation(t *testing.T) {
	flap := []join.Related{
		rel(t, "2026-03-04T10:02:11.000000000Z", "mlx5.ib_event.link_down", join.RelNode, nil),
		rel(t, "2026-03-04T10:02:19.000000000Z", "mlx5.ib_event.link_up", join.RelNode, nil),
		rel(t, "2026-03-04T10:02:44.000000000Z", "mlx5.ib_event.link_down", join.RelNode, nil),
		rel(t, "2026-03-04T10:03:02.000000000Z", "mlx5.ib_event.link_up", join.RelNode, nil),
		rel(t, "2026-03-04T10:03:27.000000000Z", "mlx5.ib_event.link_down", join.RelNode, nil),
	}
	rows := collapse(flap)
	if len(rows) != 5 {
		t.Fatalf("collapse merged an alternating sequence into %d rows; the alternation "+
			"is the diagnosis and must survive", len(rows))
	}
	for _, r := range rows {
		if r.count != 1 {
			t.Errorf("row %q collapsed to ×%d in an alternating sequence", r.ev.Detail.Signature, r.count)
		}
	}
}

func TestCollapseMergesConsecutiveIdentical(t *testing.T) {
	same := []join.Related{
		rel(t, "2026-03-04T10:02:11.000000000Z", "mlx5.ib_event.link_down", join.RelNode, nil),
		rel(t, "2026-03-04T10:02:12.000000000Z", "mlx5.ib_event.link_down", join.RelNode, nil),
		rel(t, "2026-03-04T10:02:13.000000000Z", "mlx5.ib_event.link_down", join.RelNode, nil),
		rel(t, "2026-03-04T10:02:20.000000000Z", "mlx5.ib_event.link_up", join.RelNode, nil),
	}
	rows := collapse(same)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].count != 3 {
		t.Errorf("first row count = %d, want 3", rows[0].count)
	}
	if !rows[0].last.Equal(ts(t, "2026-03-04T10:02:13.000000000Z")) {
		t.Errorf("collapsed row lost the end of its run: %v", rows[0].last)
	}
	// The count must reach the reader, not just the struct.
	if out := renderRows(rows); !strings.Contains(out, "×3") {
		t.Errorf("the collapsed count is not rendered:\n%s", out)
	}
}

// TestCollapseRespectsDifferingAttrs: two events with the same signature but
// different extracted values are different observations.
func TestCollapseRespectsDifferingAttrs(t *testing.T) {
	rows := collapse([]join.Related{
		rel(t, "2026-03-04T10:02:11.000000000Z", "mlx5.ib_event.link_down", join.RelNode, map[string]string{"port": "1"}),
		rel(t, "2026-03-04T10:02:12.000000000Z", "mlx5.ib_event.link_down", join.RelNode, map[string]string{"port": "2"}),
	})
	if len(rows) != 2 {
		t.Errorf("events on different ports were merged into %d row(s)", len(rows))
	}
}

func sample(t *testing.T) join.Result {
	t.Helper()
	job, err := schema.ParseJobID("918633")
	if err != nil {
		t.Fatal(err)
	}
	evs := []join.Related{
		// Far-away node-scoped evidence, first to be dropped.
		rel(t, "2026-03-04T09:50:00.000000000Z", "mlx5.far.away.one", join.RelNode, nil),
		rel(t, "2026-03-04T09:51:00.000000000Z", "mlx5.far.away.two", join.RelNode, nil),
		rel(t, "2026-03-04T10:02:11.000000000Z", "mlx5.ib_event.link_down", join.RelNode, nil),
		rel(t, "2026-03-04T10:03:31.000000000Z", "slurm.sacct.state.FAILED", join.RelDirect, nil),
	}
	return join.Result{
		Job:    job,
		Nodes:  []schema.Hostname{"node-0045"},
		Window: schema.Window{Start: ts(t, "2026-03-04T09:48:31.000000000Z"), End: ts(t, "2026-03-04T10:08:31.000000000Z")},
		Events: evs,
	}
}

// TestBudgetNeverDropsDirectEvidence: the direct events are the answer to the
// question that was asked. A report that drops them to fit a limit has failed at
// its only job.
func TestBudgetNeverDropsDirectEvidence(t *testing.T) {
	res := sample(t)
	for _, b := range []int{1, 10, 50, 120, 200} {
		out := renderText(res, nil, "cluster-a", schema.Redaction{Mode: "none"}, renderOpts{Budget: b})
		if !strings.Contains(out, "slurm.sacct.state.FAILED") {
			t.Errorf("budget %d dropped the direct event:\n%s", b, out)
		}
	}
}

// TestBudgetDropsFurthestFirst: correlation weakens with distance, so that is
// where the least evidence per token sits.
func TestBudgetDropsFurthestFirst(t *testing.T) {
	res := sample(t)
	out := renderText(res, nil, "cluster-a", schema.Redaction{Mode: "none"}, renderOpts{Budget: 190})
	if strings.Contains(out, "mlx5.far.away.one") {
		t.Errorf("the furthest event survived while nearer ones were considered:\n%s", out)
	}
	if !strings.Contains(out, "omitted") {
		t.Errorf("events were dropped without saying so — silent truncation reads as complete:\n%s", out)
	}
}

// TestBudgetReportsWhatItDropped. Silent truncation is the failure mode: a
// shortened evidence list still reads as the whole story.
func TestBudgetReportsWhatItDropped(t *testing.T) {
	out := renderText(sample(t), nil, "cluster-a", schema.Redaction{Mode: "none"}, renderOpts{Budget: 150})
	if !strings.Contains(out, "omitted to fit") || !strings.Contains(out, "--budget 0") {
		t.Errorf("the truncation notice does not say what happened or how to get everything:\n%s", out)
	}
}

func TestUnlimitedBudgetKeepsEverything(t *testing.T) {
	out := renderText(sample(t), nil, "cluster-a", schema.Redaction{Mode: "none"}, renderOpts{Budget: 0})
	for _, sig := range []string{"mlx5.far.away.one", "mlx5.far.away.two", "mlx5.ib_event.link_down", "slurm.sacct.state.FAILED"} {
		if !strings.Contains(out, sig) {
			t.Errorf("--budget 0 dropped %s", sig)
		}
	}
	if strings.Contains(out, "omitted") {
		t.Error("--budget 0 reported a truncation")
	}
}

// TestCapabilitiesSurviveTheBudget: what cairn could NOT see must never be
// dropped to make room for more of what it could. A truncated evidence list with
// the gaps removed reads as complete.
func TestCapabilitiesSurviveTheBudget(t *testing.T) {
	results := []collectors.Result{{
		Source: schema.SourceGPU,
		Capabilities: []collectors.Capability{{
			Name: "nvidia-smi", Detail: "not present", Reveals: "driver and CUDA versions",
		}},
	}}
	out := renderText(sample(t), results, "cluster-a", schema.Redaction{Mode: "none"}, renderOpts{Budget: 1})
	if !strings.Contains(out, "nvidia-smi") || !strings.Contains(out, "driver and CUDA versions") {
		t.Errorf("a tiny budget dropped the capability report:\n%s", out)
	}
}

// TestRenderIsDeterministic — §2.7 reaches the rendered output too, or two runs
// cannot be diffed.
func TestRenderIsDeterministic(t *testing.T) {
	for _, budget := range []int{0, 150, 4000} {
		first := renderText(sample(t), nil, "cluster-a", schema.Redaction{Mode: "none"}, renderOpts{Budget: budget})
		for i := 0; i < 50; i++ {
			got := renderText(sample(t), nil, "cluster-a", schema.Redaction{Mode: "none"}, renderOpts{Budget: budget})
			if got != first {
				t.Fatalf("budget %d: render differs on run %d:\n%s\nvs\n%s", budget, i, first, got)
			}
		}
	}
}

func TestUnredactedBundleSaysSo(t *testing.T) {
	out := renderText(sample(t), nil, "cluster-a", schema.Redaction{Mode: "none"}, renderOpts{})
	if !strings.Contains(out, "real host and account names") {
		t.Errorf("an unredacted bundle does not warn that it is unredacted:\n%s", out)
	}
	out = renderText(sample(t), nil, "cluster-a",
		schema.Redaction{Mode: "pseudonymize", SaltID: "sha256:abcd1234"}, renderOpts{})
	if !strings.Contains(out, "sha256:abcd1234") {
		t.Errorf("a redacted bundle does not carry its salt id:\n%s", out)
	}
}

func TestEmptyResultIsExplained(t *testing.T) {
	job, _ := schema.ParseJobID("999999")
	out := renderText(join.Result{Job: job}, nil, "cluster-a", schema.Redaction{Mode: "none"}, renderOpts{})
	if !strings.Contains(out, "No events") || !strings.Contains(out, "cairn doctor") {
		t.Errorf("an empty result does not explain itself or point anywhere useful:\n%s", out)
	}
}

// TestRelationsAreDistinguishable: a direct event is a fact about the job; a
// node-scoped one is the join proposing a connection. Rendering them identically
// invites a confident conclusion from a coincidence.
func TestRelationsAreDistinguishable(t *testing.T) {
	out := renderText(sample(t), nil, "cluster-a", schema.Redaction{Mode: "none"}, renderOpts{Budget: 0})
	if !strings.Contains(out, "* = carries this job's id") {
		t.Error("the legend does not explain the relation markers")
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "slurm.sacct.state.FAILED") && !strings.Contains(line, " * ") {
			t.Errorf("the direct event is not marked:\n%s", line)
		}
		if strings.Contains(line, "mlx5.ib_event.link_down") && strings.Contains(line, " * ") {
			t.Errorf("a node-scoped event is marked as direct:\n%s", line)
		}
	}
}

func TestEstimateTokensErrsHigh(t *testing.T) {
	// The guard rail should never under-report: coming in under budget is fine,
	// silently blowing a context window is not.
	if got := estimateTokens(""); got != 0 {
		t.Errorf("estimateTokens(\"\") = %d, want 0", got)
	}
	if got := estimateTokens("abcd"); got != 1 {
		t.Errorf("estimateTokens(4 bytes) = %d, want 1", got)
	}
	if got := estimateTokens("abcde"); got != 2 {
		t.Errorf("estimateTokens(5 bytes) = %d, want 2 (rounds up)", got)
	}
}

func TestWindowAround(t *testing.T) {
	job, _ := schema.ParseJobID("918633")
	evs := []schema.Event{
		{TS: ts(t, "2026-03-04T10:00:00.000000000Z"), JobID: job},
		{TS: ts(t, "2026-03-04T10:05:00.000000000Z"), JobID: job},
		// Another job's event must not widen this job's window.
		{TS: ts(t, "2026-03-04T23:00:00.000000000Z"), JobID: mustJob(t, "999999")},
	}
	w := windowAround(evs, job, 10*time.Minute, 5*time.Minute)
	if !w.Start.Equal(ts(t, "2026-03-04T09:50:00.000000000Z")) {
		t.Errorf("window start = %v", w.Start)
	}
	if !w.End.Equal(ts(t, "2026-03-04T10:10:00.000000000Z")) {
		t.Errorf("window end = %v (another job's event widened it)", w.End)
	}
	// No events for the job: zero window, so downstream collectors fall back to
	// their own caps rather than trawling.
	if w := windowAround(nil, job, time.Minute, time.Minute); !w.Start.IsZero() {
		t.Errorf("a job with no events produced a window: %v", w)
	}
}

func mustJob(t *testing.T, s string) *schema.JobID {
	t.Helper()
	j, err := schema.ParseJobID(s)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
