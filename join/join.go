// Package join correlates events across producers on (node, jobid, time).
//
// CLAUDE.md §1: six dialects, no join. This is the join, and everything else in
// the repository is a view over it.
//
// The correctness target from §6 is "given a jobid, return every event from
// every producer that bears on it". "Every" is the hard word. The events that
// actually explain a failure are usually the ones carrying no job id at all —
// the kernel's OOM message, the fabric link going down, the controller marking a
// node DOWN. A join that returns only events stamped with the job id returns
// exactly the evidence the user already had.
package join

import (
	"sort"
	"time"

	"github.com/touchelos/cairn/schema"
)

// Relation says why an event was included.
//
// It is returned rather than kept internal because the difference matters to
// whoever reads the result. A direct event is a fact about the job; an inferred
// one is a fact about a node during a window, which the join is *proposing* is
// related. Presenting the two identically would invite a confident conclusion
// from a coincidence.
type Relation string

const (
	// RelDirect: the event carries a job id belonging to this job — the same
	// base job, including its steps and array tasks.
	RelDirect Relation = "direct"

	// RelNode: the event carries no job id, but occurred on a node the job was
	// running on, inside the window. This is the node-without-jobid case from §6.
	RelNode Relation = "node"

	// RelCluster: the event carries neither job id nor node, but occurred inside
	// the window. Controller-side events reach the result this way. Weakest
	// relation, and last in the ordering for that reason.
	RelCluster Relation = "cluster"
)

// Related is one event and why it is here.
type Related struct {
	Event    schema.Event
	Relation Relation
}

// Options tunes the join.
type Options struct {
	// Before extends the window backwards from the job's first known event.
	//
	// The cause almost always precedes the symptom, often by minutes: a cable
	// starts flapping, and the MPI job dies some time later. A window that starts
	// when the job's own events start would systematically exclude the evidence
	// that explains them. Defaults to DefaultBefore.
	Before time.Duration

	// After extends the window forwards past the job's last known event.
	//
	// Consequences lag: the controller marks a node DOWN some seconds after the
	// job is already recorded as failed. Defaults to DefaultAfter.
	After time.Duration

	// Skew widens the window at both ends to absorb clock disagreement between
	// producers. Populate from the bundle's ClockOffsets — SkewOf does it.
	Skew time.Duration

	// IncludeCluster admits events with neither node nor job id. Off by default:
	// on a busy controller these are numerous and mostly unrelated, and a result
	// padded with them is harder to read, not more complete.
	IncludeCluster bool
}

// Defaults chosen to be generous rather than precise. Phase 2 dogfooding is what
// should tune them, against a miss log rather than intuition (CLAUDE.md §6).
const (
	DefaultBefore = 15 * time.Minute
	DefaultAfter  = 5 * time.Minute
)

func (o Options) withDefaults() Options {
	if o.Before == 0 {
		o.Before = DefaultBefore
	}
	if o.After == 0 {
		o.After = DefaultAfter
	}
	return o
}

// SkewOf returns the largest absolute clock offset in a bundle.
//
// The join widens its window by this much, rather than trying to correct each
// event. Correcting would require trusting the offsets to be right; widening only
// requires them to be roughly the right size, which is a much weaker claim and
// the only one a measured offset actually supports.
func SkewOf(clocks []schema.ClockOffset) time.Duration {
	var max time.Duration
	for _, c := range clocks {
		d := time.Duration(c.OffsetNanos)
		if d < 0 {
			d = -d
		}
		if d > max {
			max = d
		}
	}
	return max
}

// Result is everything bearing on one job.
type Result struct {
	Job    *schema.JobID
	Nodes  []schema.Hostname
	Window schema.Window
	Events []Related
}

// Direct returns just the events carrying the job's own id.
func (r Result) Direct() []schema.Event {
	var out []schema.Event
	for _, e := range r.Events {
		if e.Relation == RelDirect {
			out = append(out, e.Event)
		}
	}
	return out
}

// Plain returns the events without their relations, in canonical order.
func (r Result) Plain() []schema.Event {
	out := make([]schema.Event, 0, len(r.Events))
	for _, e := range r.Events {
		out = append(out, e.Event)
	}
	schema.SortEvents(out)
	return out
}

// ForJob returns every event bearing on job.
//
// Three passes, because the second and third depend on what the first learns:
//
//  1. Direct events — those whose job id shares a base with the target. This is
//     where array tasks and heterogeneous components are absorbed, and where the
//     .batch and .extern steps join their parent (schema.JobID.SameJob).
//  2. From those, derive the node set and the time window. A job's own events
//     are the only evidence of where and when it ran; nothing else in a bundle
//     says so.
//  3. Node-scoped and cluster-scoped events falling inside the widened window.
//
// Returns a zero Result when nothing carries the job id: with no window and no
// node set, every node-scoped event in the bundle would qualify equally, and
// returning all of them would be worse than returning nothing.
func ForJob(events []schema.Event, job *schema.JobID, opts Options) Result {
	opts = opts.withDefaults()
	if job == nil {
		return Result{}
	}

	res := Result{Job: job}
	nodes := map[schema.Hostname]bool{}
	var first, last time.Time

	for _, e := range events {
		if !e.JobID.SameJob(job) {
			continue
		}
		res.Events = append(res.Events, Related{Event: e, Relation: RelDirect})
		// A direct event may carry no node — a job that never started, or a
		// controller-side record. That is the jobid-without-node case from §6,
		// and it must not contribute an empty hostname to the node set.
		if e.Node != "" {
			nodes[e.Node] = true
		}
		if first.IsZero() || e.TS.Before(first) {
			first = e.TS
		}
		if e.TS.After(last) {
			last = e.TS
		}
	}

	if len(res.Events) == 0 {
		return Result{Job: job}
	}

	res.Window = schema.Window{
		Start: first.Add(-opts.Before - opts.Skew),
		End:   last.Add(opts.After + opts.Skew),
	}
	for n := range nodes {
		res.Nodes = append(res.Nodes, n)
	}
	sort.Slice(res.Nodes, func(i, j int) bool { return res.Nodes[i] < res.Nodes[j] })

	for _, e := range events {
		// An event belonging to a different job is that job's business. Without
		// this, a busy node's every neighbour would be swept in and the result
		// would be a node timeline rather than an answer about one job.
		if e.JobID != nil {
			continue
		}
		if e.TS.Before(res.Window.Start) || e.TS.After(res.Window.End) {
			continue
		}
		switch {
		case e.Node != "" && nodes[e.Node]:
			res.Events = append(res.Events, Related{Event: e, Relation: RelNode})
		case e.Node == "" && opts.IncludeCluster:
			res.Events = append(res.Events, Related{Event: e, Relation: RelCluster})
		}
	}

	sortRelated(res.Events)
	return res
}

// ForNode returns every event on a node within a window, whether or not it
// belongs to a job.
//
// This is the other direction, and it is what fleet-relative health checking
// (CLAUDE.md §7) and `cairn diff` are built on: a node is unhealthy when it
// diverges from its siblings, which is a question about a node, not about a job.
func ForNode(events []schema.Event, node schema.Hostname, window schema.Window) []schema.Event {
	var out []schema.Event
	for _, e := range events {
		if e.Node != node {
			continue
		}
		if !window.Start.IsZero() && e.TS.Before(window.Start) {
			continue
		}
		if !window.End.IsZero() && e.TS.After(window.End) {
			continue
		}
		out = append(out, e)
	}
	schema.SortEvents(out)
	return out
}

// Jobs returns every distinct base job in the event stream, ascending.
func Jobs(events []schema.Event) []uint64 {
	seen := map[uint64]bool{}
	for _, e := range events {
		if e.JobID != nil {
			seen[e.JobID.Base] = true
		}
	}
	out := make([]uint64, 0, len(seen))
	for b := range seen {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// sortRelated orders the result: canonical event order, with relation as the
// final tiebreaker so direct evidence precedes inferred evidence at the same
// instant. Total, because §2.7 applies to the join's output too.
func sortRelated(rs []Related) {
	rank := map[Relation]int{RelDirect: 0, RelNode: 1, RelCluster: 2}
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i].Event, rs[j].Event
		if !a.TS.Equal(b.TS) {
			return a.TS.Before(b.TS)
		}
		if rank[rs[i].Relation] != rank[rs[j].Relation] {
			return rank[rs[i].Relation] < rank[rs[j].Relation]
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		if a.JobID.RawOrEmpty() != b.JobID.RawOrEmpty() {
			return a.JobID.RawOrEmpty() < b.JobID.RawOrEmpty()
		}
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		return a.Detail.Signature < b.Detail.Signature
	})
}
