# Schema changelog

Every entry records a version, what changed, and what a consumer must do about
it. CLAUDE.md §10: schema changes require a version bump and a migration note,
never a silent edit.

## Version 1 — unreleased (Phase 0)

Initial schema. Nothing has shipped, so nothing needs migrating.

**Event:** `{ ts, cluster, node, jobid, source, class, detail }`, exactly as
specified in CLAUDE.md §4.

- `ts` — UTC, fixed-width nanosecond precision.
- `cluster` — required.
- `node` — optional; omitted for jobid-without-node.
- `jobid` — nullable; `null` for node-without-jobid. Structured: base, array
  task, array mask, heterogeneous offset, step.
- `source` — closed enum: `bmc`, `fabric`, `gpu`, `journal`, `slurm`, `storage`.
- `class` — closed enum, 25 members. See `testdata/classes.golden`.
- `detail` — `{ signature, attrs }`. Attrs capped at 16 keys / 256-byte values,
  keys registered per class in `attrs.go`.

**Bundle header:** `{ schema_version, cluster, window, clocks, redaction, events }`.
Deliberately contains no wall-clock generation timestamp — see DESIGN.md §7.

### Changes made during Phase 0, before v1 was released

- **Added `config.clock_skew`.** Building fixture `006-munge-auth-failure`
  surfaced a gap: the incident's actual cause is a host clock 312 seconds fast,
  and there was no class for it. Without a member, the causal observation would
  have been recorded as `unknown` while three `auth.munge` events sat above it
  looking like the answer — sending an admin to redistribute the munge key, which
  would have changed nothing.

  It earns a member rather than an attr because clock skew breaks three unrelated
  things — Munge authentication, the (node, jobid, time) join, and scheduler
  bookkeeping — and its remediation is unlike any other member's.

  This is the process CLAUDE.md §0.3 intends, working as designed: the corpus
  argued with the enum while arguing was still free. After v1 ships, the same
  argument costs a version bump.

### Open questions deferred past v1

Recording these so a future version knows they were considered rather than
overlooked:

- **Per-event confidence.** The taxonomy (Phase 4) attaches confidence to a
  *cause*. Whether an observation also needs one is unresolved — a partially
  parsed sacct row is arguably a low-confidence observation. Deferred until the
  fixture corpus shows real cases.
- **Event-to-event references.** Deduplicating "the same Xid seen by both the
  journal and DCGM" currently has no expression in the schema. The join may need
  it. Deferred until the join exists and the need is demonstrated rather than
  anticipated.
- **Sub-cluster scoping.** Partition or rack is currently an attr, not a field.
  Fleet-relative health checking (§7) may want it structured. Deferred.
- **"Allocated but idle".** Fixture `007-nccl-hang` records a GPU holding 73 GiB
  at 0% utilization as `unknown`, because that is the clearest single signature of
  a collective hang and there is no class for it. Whether it earns a member is
  deliberately deferred until a real incident argues for it: one authored fixture
  is not enough evidence to close an enum member around, and `unknown` keeps the
  gap visible in the meantime.
