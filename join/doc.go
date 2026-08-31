// Package join correlates events across producers on (node, jobid, time).
//
// Phase 1. Not implemented.
//
// This is the package the whole project exists for. CLAUDE.md §1: six dialects,
// no join — everything else in the repo is a view over this.
//
// The cases that make it hard, all named in §6 and all already representable in
// the schema:
//
//   - Clock skew between producers. Offsets are measured once per (source, node)
//     and recorded on the bundle header; see schema/bundle.go.
//   - node-without-jobid. Node health, fabric events, and config drift belong to
//     no job. schema.Event.JobID is nil for these, and they still have to reach
//     the right job by node and time window.
//   - jobid-without-node. A job that never started, or a controller-side event.
//     schema.Event.Node is empty.
//   - Array and heterogeneous jobs. schema.JobID.SameJob compares base only, so
//     12345.batch, 12345.extern, and 12345_7 all resolve to one job.
//
// The correctness target from §6: given a jobid, return every event from every
// producer that bears on it. "Every" is the hard word — a join that quietly drops
// the kernel's OOM message because it carried no jobid has failed at exactly the
// case the tool exists for.
package join
