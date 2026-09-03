// Package policy is the default-deny allowlist, dry-run, and audit log.
//
// Invariant §2.4: read-only by default, in every code path. Actuation arrives
// late, gated, and reversible.
//
// **This build ships no actions.** The engine exists; the null action set is
// what it is built against, and `ShippedActions()` returns nothing. That is not
// an unfinished state, it is the deliverable — CLAUDE.md §6 puts the gate before
// anything that could pass through it, and doc comments are a poor place to
// keep a promise, so scripts/verify-guards.sh asserts the emptiness instead.
//
// The sequencing is not negotiable and is easy to get backwards: the policy
// engine is built and tested against a null action set first. Only once it is
// proven correct do the three permitted actuations exist — drain node, requeue
// job, rerun health check. All three are reversible. No config edits, ever.
//
// Policy proven correct before anything can execute. Never the other way around.
//
// # What is proven
//
// Each of these is a test, and each is a guard that has been watched to fail:
//
//   - The shipped action set is empty, so nothing can execute at all.
//   - An action this build implements is still denied unless a policy lists it.
//   - An empty scope authorizes no targets — never every target.
//   - An action that cannot say how it is undone does not run.
//   - A policy written for one cluster does not authorize another.
//   - If the audit log cannot be written, the action is refused. No record, no
//     action; that ordering is the one thing here that cannot be retrofitted.
//
// # Two structural choices worth knowing
//
// The action set is a field on Engine, not a package-level registry, and there
// is no exported way to add to it. Tests build an engine with a fake action;
// nothing outside this package can give one to a production engine.
//
// This package does not import collectors, site or join, so there is provably no
// route from here to a cluster in this build. Guard 17 checks it, and if that
// import ever appears, the commit that added it is the one to read.
package policy
