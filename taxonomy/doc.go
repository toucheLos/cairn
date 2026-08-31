// Package taxonomy maps signatures to causes and remediations, with confidence.
//
// Phase 4. Not implemented.
//
// This is where diagnosis lives, and it is deliberately not in the schema.
// schema/DESIGN.md §1: an event's class names what a producer showed us; a cause
// is a derivation over a whole bundle. Keeping them apart is what allows the
// class enum to be closed at all.
//
// Rules and signatures, not ML, until the corpus is large enough to justify
// otherwise and we can measure the difference (CLAUDE.md §10). The corpus is the
// moat (§9); a model trained too early on too little of it would be neither
// defensible nor checkable.
//
// NOTE ON LICENSING: this package and the non-synthetic fixtures are excluded
// from the repository's Apache-2.0 grant. See NOTICE.
package taxonomy
