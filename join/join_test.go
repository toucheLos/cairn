package join_test

import (
	"testing"
	"time"

	"github.com/touchelos/cairn/fixtures"
	"github.com/touchelos/cairn/join"
	"github.com/touchelos/cairn/schema"
)

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := schema.ParseTime(s)
	if err != nil {
		t.Fatalf("ParseTime(%q): %v", s, err)
	}
	return v
}

func jid(t *testing.T, s string) *schema.JobID {
	t.Helper()
	j, err := schema.ParseJobID(s)
	if err != nil {
		t.Fatalf("ParseJobID(%q): %v", s, err)
	}
	return j
}

func ev(t *testing.T, at, node, job string, src schema.Source, cls schema.Class, sig string) schema.Event {
	t.Helper()
	var j *schema.JobID
	if job != "" {
		j = jid(t, job)
	}
	return schema.Event{
		TS: ts(t, at), Cluster: "cluster-a", Node: schema.Hostname(node), JobID: j,
		Source: src, Class: cls, Detail: schema.Detail{Signature: sig},
	}
}

// TestForJobPullsInNodeScopedEvidence is the core claim of §6: given a jobid,
// return every event that bears on it — including the ones carrying no jobid,
// which are usually the ones that explain the failure.
func TestForJobPullsInNodeScopedEvidence(t *testing.T) {
	events := []schema.Event{
		// The cause: a kernel OOM with no job id at all.
		ev(t, "2026-03-04T09:14:02.000000000Z", "node-0042", "", schema.SourceJournal,
			schema.ClassResourceOOM, "kernel.memcg.oom_kill"),
		// The symptom, carrying the job id.
		ev(t, "2026-03-04T09:14:02.000000000Z", "node-0042", "918273.batch", schema.SourceSlurm,
			schema.ClassResourceOOM, "slurm.sacct.state.OUT_OF_MEMORY"),
		// A different job on the same node: must not be swept in.
		ev(t, "2026-03-04T09:14:02.000000000Z", "node-0042", "918274", schema.SourceSlurm,
			schema.ClassAppNonzeroExit, "slurm.sacct.state.FAILED"),
		// The same class on a node the job never touched.
		ev(t, "2026-03-04T09:14:02.000000000Z", "node-9999", "", schema.SourceJournal,
			schema.ClassResourceOOM, "kernel.memcg.oom_kill"),
	}

	got := join.ForJob(events, jid(t, "918273"), join.Options{})

	sigs := map[string]join.Relation{}
	for _, r := range got.Events {
		sigs[string(r.Event.Node)+"/"+r.Event.Detail.Signature] = r.Relation
	}
	if rel, ok := sigs["node-0042/kernel.memcg.oom_kill"]; !ok {
		t.Error("the kernel OOM message was not joined to the job; that is the evidence that explains it")
	} else if rel != join.RelNode {
		t.Errorf("kernel OOM joined as %q, want %q", rel, join.RelNode)
	}
	if rel := sigs["node-0042/slurm.sacct.state.OUT_OF_MEMORY"]; rel != join.RelDirect {
		t.Errorf("the sacct row joined as %q, want %q", rel, join.RelDirect)
	}
	if _, ok := sigs["node-0042/slurm.sacct.state.FAILED"]; ok {
		t.Error("another job's event was swept into this job's result")
	}
	if _, ok := sigs["node-9999/kernel.memcg.oom_kill"]; ok {
		t.Error("an event from an unrelated node was joined")
	}
}

// TestStepsAndArrayTasksResolveToOneJob: .batch, .extern, array tasks and
// heterogeneous components all belong to the same job (§6).
func TestStepsAndArrayTasksResolveToOneJob(t *testing.T) {
	var events []schema.Event
	for _, raw := range []string{"918273", "918273.batch", "918273.extern", "918273.0", "918273_7", "918273+1"} {
		events = append(events, ev(t, "2026-03-04T09:14:02.000000000Z", "node-0042", raw,
			schema.SourceSlurm, schema.ClassAppNonzeroExit, "slurm.sacct.state.FAILED."+raw))
	}
	events = append(events, ev(t, "2026-03-04T09:14:02.000000000Z", "node-0042", "918274",
		schema.SourceSlurm, schema.ClassAppNonzeroExit, "other-job"))

	got := join.ForJob(events, jid(t, "918273"), join.Options{})
	if n := len(got.Direct()); n != 6 {
		t.Errorf("got %d direct events, want 6 (every step, array task, and het component)", n)
	}
	for _, e := range got.Events {
		if e.Event.Detail.Signature == "other-job" {
			t.Error("a different base job was included")
		}
	}

	// Asking by a step id must find the whole job, not just that step.
	if n := len(join.ForJob(events, jid(t, "918273.batch"), join.Options{}).Direct()); n != 6 {
		t.Errorf("querying by step id found %d events, want 6", n)
	}
	// And by an array task.
	if n := len(join.ForJob(events, jid(t, "918273_7"), join.Options{}).Direct()); n != 6 {
		t.Errorf("querying by array task found %d events, want 6", n)
	}
}

// TestPrecursorsAreIncluded: the cause precedes the symptom, so a window that
// began with the job's own events would systematically exclude the explanation.
func TestPrecursorsAreIncluded(t *testing.T) {
	events := []schema.Event{
		// Cable flap eight minutes before the job's first recorded event.
		ev(t, "2026-03-04T10:02:11.000000000Z", "node-0045", "", schema.SourceJournal,
			schema.ClassFabricLinkFlap, "mlx5.ib_event.link_down"),
		// Far outside any sensible window.
		ev(t, "2026-03-04T06:00:00.000000000Z", "node-0045", "", schema.SourceJournal,
			schema.ClassFabricLinkFlap, "ancient.flap"),
		ev(t, "2026-03-04T10:10:31.000000000Z", "node-0045", "918633", schema.SourceSlurm,
			schema.ClassAppNonzeroExit, "slurm.sacct.state.FAILED"),
		// Consequence, after the job is already recorded as failed.
		ev(t, "2026-03-04T10:12:00.000000000Z", "node-0045", "", schema.SourceJournal,
			schema.ClassSchedNodeFail, "slurm.slurmctld.node_set_down"),
	}

	got := join.ForJob(events, jid(t, "918633"), join.Options{})
	found := map[string]bool{}
	for _, r := range got.Events {
		found[r.Event.Detail.Signature] = true
	}
	if !found["mlx5.ib_event.link_down"] {
		t.Error("the precursor link flap was excluded; it is the reason the job failed")
	}
	if !found["slurm.slurmctld.node_set_down"] {
		t.Error("the consequent node_set_down was excluded")
	}
	if found["ancient.flap"] {
		t.Error("an event four hours earlier was included; the window is not bounded")
	}
}

// TestJobIDWithoutNode: a job that never started has events with no node. It must
// still be found, and it must not acquire an empty hostname in its node set (§6).
func TestJobIDWithoutNode(t *testing.T) {
	events := []schema.Event{
		ev(t, "2026-03-04T09:14:02.000000000Z", "", "918714", schema.SourceSlurm,
			schema.ClassAppNonzeroExit, "slurm.sacct.state.FAILED"),
		// A node-scoped event that must NOT be joined: the job was never on a node.
		ev(t, "2026-03-04T09:14:02.000000000Z", "node-0046", "", schema.SourceJournal,
			schema.ClassAuthMunge, "munge.expired_credential"),
	}
	got := join.ForJob(events, jid(t, "918714"), join.Options{})
	if len(got.Direct()) != 1 {
		t.Fatalf("got %d direct events, want 1", len(got.Direct()))
	}
	for _, n := range got.Nodes {
		if n == "" {
			t.Error("an empty hostname entered the node set")
		}
	}
	if len(got.Events) != 1 {
		t.Errorf("a node-scoped event was joined to a job that never ran on a node: %+v", got.Events)
	}
}

func TestUnknownJobReturnsNothing(t *testing.T) {
	events := []schema.Event{
		ev(t, "2026-03-04T09:14:02.000000000Z", "node-0042", "", schema.SourceJournal,
			schema.ClassResourceOOM, "kernel.memcg.oom_kill"),
	}
	got := join.ForJob(events, jid(t, "999999"), join.Options{})
	if len(got.Events) != 0 {
		t.Errorf("a job with no direct events returned %d events; with no window and no "+
			"node set every node-scoped event would qualify equally", len(got.Events))
	}
}

// TestSkewWidensTheWindow: an event just outside the nominal window must be
// admitted once measured clock skew is accounted for.
func TestSkewWidensTheWindow(t *testing.T) {
	events := []schema.Event{
		ev(t, "2026-03-04T09:00:00.000000000Z", "node-0042", "918273", schema.SourceSlurm,
			schema.ClassAppNonzeroExit, "slurm.sacct.state.FAILED"),
		// 20 minutes before: outside the 15-minute default.
		ev(t, "2026-03-04T08:40:00.000000000Z", "node-0042", "", schema.SourceJournal,
			schema.ClassGPUXid, "nvidia.xid.79"),
	}
	if n := len(join.ForJob(events, jid(t, "918273"), join.Options{}).Events); n != 1 {
		t.Fatalf("without skew the join found %d events, want 1", n)
	}
	skewed := join.Options{Skew: join.SkewOf([]schema.ClockOffset{
		{Source: schema.SourceJournal, OffsetNanos: int64(10 * time.Minute)},
	})}
	if n := len(join.ForJob(events, jid(t, "918273"), skewed).Events); n != 2 {
		t.Errorf("with 10 minutes of skew the join found %d events, want 2", n)
	}
}

func TestSkewOfTakesLargestMagnitude(t *testing.T) {
	got := join.SkewOf([]schema.ClockOffset{
		{OffsetNanos: int64(2 * time.Second)},
		{OffsetNanos: int64(-9 * time.Second)},
		{OffsetNanos: 0},
	})
	if got != 9*time.Second {
		t.Errorf("SkewOf = %v, want 9s (largest absolute offset, sign ignored)", got)
	}
}

func TestClusterScopedEventsAreOptional(t *testing.T) {
	events := []schema.Event{
		ev(t, "2026-03-04T09:00:00.000000000Z", "node-0042", "918273", schema.SourceSlurm,
			schema.ClassAppNonzeroExit, "slurm.sacct.state.FAILED"),
		ev(t, "2026-03-04T09:00:00.000000000Z", "", "", schema.SourceJournal,
			schema.ClassAuthMunge, "slurm.protocol_auth_error"),
	}
	if n := len(join.ForJob(events, jid(t, "918273"), join.Options{}).Events); n != 1 {
		t.Errorf("cluster-scoped events were included by default; got %d", n)
	}
	opts := join.Options{IncludeCluster: true}
	if n := len(join.ForJob(events, jid(t, "918273"), opts).Events); n != 2 {
		t.Errorf("cluster-scoped events were not included when asked for; got %d", n)
	}
}

// TestOrderingIsTotal: §2.7 applies to the join's output too, and relation must
// break ties so direct evidence precedes inferred evidence at the same instant.
func TestOrderingIsTotal(t *testing.T) {
	events := []schema.Event{
		ev(t, "2026-03-04T09:00:00.000000000Z", "node-0042", "", schema.SourceJournal,
			schema.ClassResourceOOM, "kernel.memcg.oom_kill"),
		ev(t, "2026-03-04T09:00:00.000000000Z", "node-0042", "918273", schema.SourceSlurm,
			schema.ClassResourceOOM, "slurm.sacct.state.OUT_OF_MEMORY"),
	}
	forward := join.ForJob(events, jid(t, "918273"), join.Options{})
	reversed := join.ForJob([]schema.Event{events[1], events[0]}, jid(t, "918273"), join.Options{})

	if len(forward.Events) != len(reversed.Events) {
		t.Fatal("join result depends on input order")
	}
	for i := range forward.Events {
		if forward.Events[i].Event.Detail.Signature != reversed.Events[i].Event.Detail.Signature {
			t.Fatalf("join ordering depends on input order at index %d", i)
		}
	}
	if forward.Events[0].Relation != join.RelDirect {
		t.Errorf("at an identical instant, inferred evidence sorted before direct evidence")
	}
}

// TestOverCorpus runs the join across every fixture, using the job each fixture
// declares. Every fixture must yield its whole expected stream: these are
// single-incident captures, so anything the join drops is evidence lost.
func TestOverCorpus(t *testing.T) {
	fs, err := fixtures.LoadAll("../fixtures")
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for _, f := range fs {
		if f.Meta.Incident.Job == "" {
			continue
		}
		t.Run(f.Meta.ID, func(t *testing.T) {
			job := jid(t, f.Meta.Incident.Job)
			got := join.ForJob(f.Expected, job, join.Options{IncludeCluster: true})

			if len(got.Events) != len(f.Expected) {
				var missing []string
				in := map[string]bool{}
				for _, r := range got.Events {
					in[r.Event.Detail.Signature+"@"+schema.FormatTime(r.Event.TS)] = true
				}
				for _, e := range f.Expected {
					if !in[e.Detail.Signature+"@"+schema.FormatTime(e.TS)] {
						missing = append(missing, e.Detail.Signature)
					}
				}
				t.Errorf("join returned %d of %d events; missing: %v",
					len(got.Events), len(f.Expected), missing)
			}
			if len(got.Direct()) == 0 {
				t.Errorf("no event carried job %s", f.Meta.Incident.Job)
			}
			t.Logf("job %s: %d events (%d direct), nodes=%v",
				f.Meta.Incident.Job, len(got.Events), len(got.Direct()), got.Nodes)
		})
	}
}
