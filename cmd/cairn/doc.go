// Command cairn is the single static binary: context, diff, doctor, init.
//
// Phase 2 and later. Not implemented — there is no main() here yet, and Phase 0
// deliberately ships nothing runnable.
//
// Planned surface (CLAUDE.md §6):
//
//	cairn context --job <id>   token-budgeted, deterministically ordered,
//	                           redactable evidence for one job
//	cairn doctor               what each collector can and cannot see, and why
//	cairn diff <node>          compare a node against its fleet siblings
//	cairn init                 probe the stack, emit a reviewable site.yaml
//
// Invariant §2.5: one static binary. No daemon, no server, no database, no
// procurement. Invariant §2.1: every one of these must work with inference
// switched off.
package main
