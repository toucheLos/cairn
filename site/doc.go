// Package site handles discovery, site.yaml, and capability gating.
//
// CLAUDE.md §6: `cairn init` probes the stack and emits a reviewable,
// git-committable site.yaml. Onboarding is discovery-first — admins correct a
// generated file, they do not fill in a form.
//
// The generated profile becomes the context header, and that is what stops a
// model suggesting PBS syntax to a Slurm site.
//
// This package also holds the fleet-relative half of §7: `cairn profile`
// captures a node's configuration drift keys, and `cairn diff` compares one
// node against its siblings. A node is interesting when it diverges from its
// 47 peers, not when it crosses a threshold someone guessed in 2014.
//
// Two rules run through the whole package, and both are easy to erode:
//
//   - Discovery produces a draft for a human to correct, never an answer. The
//     file is commented, it is not overwritten without --force, and unknown keys
//     in it are an error rather than being ignored.
//   - Drift is an observation, never a verdict. cairn reports that a node
//     differs from its siblings; it does not report that the node is wrong,
//     because the divergent node may be the only one that got the patch.
//
// See DESIGN.md for the reasoning behind both, and for why the YAML is
// hand-rolled rather than imported.
package site
