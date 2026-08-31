// Package collectors defines the contract every producer reader satisfies.
//
// Phase 1. Nothing is implemented yet; this file records the contract so that
// the first collector written has something to conform to rather than something
// to establish.
//
// From CLAUDE.md §4, a collector:
//
//   - Declares its capability requirements — binary present, permission level,
//     daemon reachable — before it runs, so `doctor` can report what it can and
//     cannot see, and why.
//   - Emits events.
//   - Never throws. Invariant §2.6: an unrecognized filesystem, scheduler, or
//     fabric is logged and skipped, never fatal. A site running something we have
//     not seen must still get everything else.
//
// Two consequences that are easy to miss:
//
// A collector that cannot run is not an error. It is a capability report, and
// `doctor` exists to render it. "nvidia-smi absent" is the correct, complete
// answer on a CPU-only node.
//
// A collector emits observations, never conclusions. See schema/DESIGN.md §1.
// The temptation to have the slurm collector decide *why* a job died is the
// single most likely way this design gets eroded, because it always looks like a
// small local improvement.
package collectors
