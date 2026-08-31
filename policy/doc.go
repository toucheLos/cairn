// Package policy is the default-deny allowlist, dry-run, and audit log.
//
// Phase 4. Not implemented.
//
// Invariant §2.4: read-only by default, in every code path. Actuation arrives
// late, gated, and reversible.
//
// The sequencing in CLAUDE.md §6 is not negotiable and is easy to get backwards:
// the policy engine is built and tested against a **null action set** first.
// Only once it is proven correct do the three permitted actuations exist —
// drain node, requeue job, rerun health check. All three are reversible.
// No config edits, ever.
//
// Policy proven correct before anything can execute. Never the other way around.
package policy
