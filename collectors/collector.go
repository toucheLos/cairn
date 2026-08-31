package collectors

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/touchelos/cairn/schema"
)

// Request is what a collector is asked to gather.
//
// Fields may be zero. A collector must cope with an unset window (collect what
// is available), a nil job (node-scoped collection only), and an empty node list
// (whatever the local host can see). §6 requires node-without-jobid and
// jobid-without-node to be representable at every layer, and that starts here.
type Request struct {
	Cluster schema.ClusterName
	Job     *schema.JobID
	Window  schema.Window
	Nodes   []schema.Hostname
}

// Level is how much access a capability requires.
type Level int

const (
	// LevelUnprivileged is satisfiable by any user on a login node or inside a
	// job. Invariant §2.2: this is the level cairn must work at.
	LevelUnprivileged Level = iota
	// LevelPrivileged needs root or a specific group. Root buys better data; it
	// never gates the tool.
	LevelPrivileged
)

func (l Level) String() string {
	if l == LevelPrivileged {
		return "privileged"
	}
	return "unprivileged"
}

// Capability is one thing a collector needs in order to see something.
type Capability struct {
	// Name identifies the requirement, e.g. "sacct" or "journal:kernel".
	Name string
	// Level is the access it requires.
	Level Level
	// Available reports whether it was satisfied.
	Available bool
	// Detail says why not, when it was not. This is what `doctor` prints, so it
	// is written for an admin: "nvidia-smi not on PATH", not "exec: not found".
	Detail string
	// Reveals describes what is lost without it, so an admin can judge whether
	// to care. A capability report that lists what is missing without saying
	// what it costs cannot be acted on.
	Reveals string
}

// Result is what a collector returns. It never returns an error.
//
// Invariant §2.6: never hard-fail on an unknown stack. A collector that cannot
// run reports capabilities it could not satisfy and returns no events. A
// collector that ran but could not parse some input reports Warnings and returns
// the events it did understand. Neither is fatal, and neither is silent.
type Result struct {
	Source       schema.Source
	Events       []schema.Event
	Capabilities []Capability

	// Warnings are lines or records the collector could not interpret. These
	// drive the Phase 2 miss log — CLAUDE.md §6 says the miss log drives Phase 3,
	// not our intuition, and this is where the misses are recorded.
	Warnings []string
}

// OK reports whether every capability was satisfied.
func (r Result) OK() bool {
	for _, c := range r.Capabilities {
		if !c.Available {
			return false
		}
	}
	return true
}

// Missing returns the unsatisfied capabilities, sorted by name.
func (r Result) Missing() []Capability {
	var out []Capability
	for _, c := range r.Capabilities {
		if !c.Available {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Collector reads one producer.
//
// The contract, from CLAUDE.md §4:
//
//   - Declares its capability requirements, so `doctor` can report what each
//     collector can and cannot see, and why.
//   - Emits events.
//   - Never throws.
//
// Collect returns a Result and no error. That signature is the contract, not a
// stylistic choice: an `error` return would eventually be propagated by some
// caller, and one unreadable log file on one node would abort collection for a
// whole cluster.
//
// Collectors emit observations, never conclusions. See schema/DESIGN.md §1. The
// temptation to have the slurm collector decide *why* a job died always looks
// like a small local improvement, and it is how this design would erode.
type Collector interface {
	// Source is the producer this collector reads. It is the Source stamped on
	// every event it emits.
	Source() schema.Source

	// Collect gathers events. It must not panic and must not return an error.
	Collect(ctx context.Context, env Env, req Request) Result
}

// Registry is an ordered set of collectors.
type Registry []Collector

// Collect runs every collector and merges the results.
//
// Collectors run in registry order rather than concurrently. Concurrency here
// would buy very little — these are a handful of short-lived reads — and would
// cost determinism in the warning order, which invariant §2.7 makes load-bearing
// for diffing.
func (r Registry) Collect(ctx context.Context, env Env, req Request) []Result {
	out := make([]Result, 0, len(r))
	for _, c := range r {
		out = append(out, safeCollect(ctx, c, env, req))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// safeCollect enforces "never throws" even against a collector that does.
//
// The interface says a collector must not panic. This makes that true rather
// than merely stated: a panic in the fabric collector must not cost an admin the
// slurm and journal events that would have answered their question.
func safeCollect(ctx context.Context, c Collector, env Env, req Request) (res Result) {
	defer func() {
		if p := recover(); p != nil {
			res = Result{
				Source: c.Source(),
				Warnings: []string{fmt.Sprintf(
					"collector %s panicked: %v; its events are missing from this bundle", c.Source(), p)},
				Capabilities: []Capability{{
					Name:    string(c.Source()) + ":collector",
					Detail:  fmt.Sprintf("panicked: %v", p),
					Reveals: "every event this producer would have contributed",
				}},
			}
		}
	}()
	res = c.Collect(ctx, env, req)
	if res.Source == "" {
		res.Source = c.Source()
	}
	return res
}

// MergeEvents flattens results into one canonically ordered stream.
func MergeEvents(results []Result) []schema.Event {
	var out []schema.Event
	for _, r := range results {
		out = append(out, r.Events...)
	}
	schema.SortEvents(out)
	return out
}

// Report renders a capability summary: the body of `cairn doctor`.
//
// It reports what each collector could and could not see, and what the gaps
// cost. An admin reading this should be able to decide whether a missing
// capability matters to them without reading any code.
func Report(results []Result) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%s\n", r.Source)
		if len(r.Capabilities) == 0 {
			b.WriteString("  (declares no capabilities)\n")
		}
		caps := append([]Capability(nil), r.Capabilities...)
		sort.SliceStable(caps, func(i, j int) bool { return caps[i].Name < caps[j].Name })
		for _, c := range caps {
			mark := "ok  "
			if !c.Available {
				mark = "MISS"
			}
			fmt.Fprintf(&b, "  %s %-24s (%s)", mark, c.Name, c.Level)
			if c.Detail != "" {
				fmt.Fprintf(&b, " %s", c.Detail)
			}
			b.WriteString("\n")
			if !c.Available && c.Reveals != "" {
				fmt.Fprintf(&b, "       without it: %s\n", c.Reveals)
			}
		}
		fmt.Fprintf(&b, "  %d event(s)", len(r.Events))
		if n := len(r.Warnings); n > 0 {
			fmt.Fprintf(&b, ", %d warning(s)", n)
		}
		b.WriteString("\n")
	}
	return b.String()
}
