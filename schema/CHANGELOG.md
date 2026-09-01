# Schema changelog

Every entry records a version, what changed, and what a consumer must do about
it. CLAUDE.md §10: schema changes require a version bump and a migration note,
never a silent edit.

## Version 2 — unreleased (Phase 3)

**Added the `site` producer.** `cairn diff` compares a node's captured profile
against its fleet siblings and emits `config.drift` for each key that diverges.
Those events needed a `source`, and none of the six existing producers is one:
nothing on a node emits a drift event. cairn derives it by comparing profiles it
captured itself, and kernel release, glibc version, munge-key mtime and module
roots have no producer to attribute them to at all.

`config.drift` and its attrs — `key`, `observed`, `expected`, `peer_count`,
`peer_majority` — were registered in v1 with sibling-majority semantics already
in mind. The class was there; only the producer was missing.

**Why this is a version bump and not an additive change.** `DecodeEvents`
rejects an unknown source, and `Event.Validate` rejects one on the way in. So a
v1 reader handed a bundle containing site events fails outright rather than
skipping them. That is the intended behavior — schema/DESIGN.md §8 argues a
version mismatch must be loud — but it makes adding a producer a breaking change
in practice, whatever it looks like on paper.

**Migration.** Nothing has shipped, so no stored bundle needs rewriting.
A consumer pinned to v1 must either add `site` to its source enum or reject v2
bundles; it must not silently drop the events, because a dropped drift event
reads as a clean fleet.

`site` is excluded from the collector registry and from `doctor`'s
"not yet implemented" list, via `Source.CollectorBacked`. Listing it there would
claim a gap that no amount of work could ever close.

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
