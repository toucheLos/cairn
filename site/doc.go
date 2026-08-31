// Package site handles discovery, site.yaml, and capability gating.
//
// Phase 3. Not implemented.
//
// CLAUDE.md §6: `cairn init` probes the stack and emits a reviewable,
// git-committable site.yaml. Onboarding is discovery-first — admins correct a
// generated file, they do not fill in a form.
//
// The generated profile becomes the context header, and that is what stops a
// model suggesting PBS syntax to a Slurm site.
package site
