# Event schema — design decisions

Schema version: **1**

This file records decisions that are cheap now and expensive later. It is not an
API reference — read the code for that. It exists so that a future change knows
what it is overturning.

---

## 1. `class` names an observation, not a diagnosis

CLAUDE.md §4 puts `class` on the event. There are two things that could mean, and
they pull in opposite directions:

- **Observation.** "This event evidences an Xid 79 on GPU 3."
- **Diagnosis.** "This job died because the GPU fell off the bus."

**v1 chooses observation.** A class answers *what did a producer show us*, never
*what killed the job*.

The reason is that §4 also requires the enum to be **closed**. Observations are a
bounded set: producers emit a finite vocabulary, and it changes only when a
vendor ships a new error category. Diagnoses are not bounded — every combination
of observations is a potential cause, and the set grows with the taxonomy
forever. An enum of diagnoses could never be closed, and the requirement that it
be closed would quietly become a lie.

Mapping sets of classed events onto a cause is the taxonomy's job (Phase 4).
That is a **derivation over a bundle**, not a field on an event.

The practical consequence, worth stating because it looks like a bug otherwise:
**one incident normally produces several events, and their classes may disagree.**
A node that lost a GPU emits `gpu.xid`, `gpu.fallen_off_bus`, and
`sched.node_fail` — three observations, one cause. The taxonomy resolves them.
An event stream that shows only the "right" class has already thrown away the
evidence that made the conclusion checkable.

## 2. `detail` is bounded by construction, not by convention

§4: detail "is not a dumping ground for raw log lines." The mechanism:

- `Attrs` is capped at 16 keys, values at 256 bytes, signature at 128.
- Every key must be registered for the event's class in `attrs.go`. An
  unregistered key is a validation **error**.
- `Validate` runs inside `EncodeEvent`, so an event that breaks a bound is not
  serializable at all.

There is deliberately **no free-text field**. Two invariants depend on this:

- §2.3 (no log storage). A `raw_line` field would be filled within a month, and
  cairn would be a log store with extra steps.
- §10 (redaction at the boundary). Free text is where an unredacted hostname
  hides. If every value is an individually registered field, the redactor can
  reason about all of them; if one of them is a log line, it cannot.

`Signature` names the **pattern** that matched — `nvidia.xid.79` — never the line
that matched it.

## 3. `detail.attrs` keys are marked for PII at registration

`AttrSpec.PII` is what lets redaction happen at the boundary rather than at the
call site. The redactor pseudonymizes every value whose key is marked, without
knowing which collector produced it.

`AttrIsPII` returns **true for unregistered keys**. The safe default for data
nobody has assessed is that it identifies someone.

## 4. Identifying fields are distinct types

`ClusterName`, `Hostname`, `Username`, `AccountCode` are named types, not
`string`. §10 says an unredacted hostname on an outbound path is a bug rather
than a configuration choice — this gives that rule something the compiler can
see. A function taking `Hostname` is visibly handling identifying data.

## 5. `JobID` is structured

Slurm identifiers are not integers: `12345.batch`, `12345_7`, `12345_[8-20%4]`,
`12345+0`. §6 lists array and heterogeneous jobs as Phase 1 join work, so the
schema carries them from the start.

The join's core predicate is `SameJob`, which compares **base** only. "Every event
bearing on job 12345" must return the batch step, the extern step, and every
array task. An opaque string cannot answer that question.

`ArrayRange` holds masks like `[8-20%4]` **unexpanded**. Expanding them would
invent task identifiers for jobs that may never run.

`ParseJobID` is strict — it errors rather than guessing. §2.6 ("never hard-fail on
an unknown stack") governs collectors, which should log and skip a record they
cannot parse. It does not license the schema to emit a `JobID` whose `Base` is
wrong, because that corrupts the join rather than degrading it.

## 6. `ts` is one instant; clock skew lives on the bundle header

Every timestamp is UTC, fixed-width, nanosecond precision.

Clock skew is real and is recorded **once per (source, node)** on the bundle, not
per event. Two reasons:

1. It widens the hottest struct in the project to carry a value that changes a
   handful of times per run.
2. A per-event skew field invites collectors to store an unnormalized timestamp
   "just in case", after which the join has two notions of when things happened
   and no rule for which wins.

`ClockOffset.Method` records how the offset was derived, including
`assumed_zero`. Assuming zero is usually right; leaving the assumption implicit
is not, because a wrong one silently misorders the join.

## 7. Determinism is enforced in one file

Invariant §2.7 requires byte-identical output across runs. The threats are
mundane and each has a specific answer in `encode.go`:

| Threat | Answer |
|---|---|
| Map iteration order | Keys sorted explicitly, not left to `encoding/json` |
| Struct field order | Objects assembled field by field in declared order |
| Variable-width timestamps | Fixed layout; `RFC3339Nano` trims trailing zeros and is not used |
| HTML escaping | Disabled; `<`, `>`, `&` are not special outside a browser |
| Float formatting | No floats in the schema at all |
| Caller's slice order | `EncodeEvents` sorts a copy; canonical order is part of canonical form |

`SortEvents` is a **total** order — `(ts, source, node, jobid.raw, class,
signature)`. Timestamp ties are constant in practice (journald and sacct
routinely land in the same second), so every tiebreaker is load-bearing.

The bundle header carries **no `generated_at`**. A wall-clock stamp would make
every bundle differ from every other bundle covering the same window, which is
exactly what §2.7 forbids and what `cairn diff` and the eval harness depend on.
`TestBundleHasNoWallClockStamp` enforces this.

## 8. Decoding rejects unknown fields

A bundle written by a newer schema version fails loudly rather than being
silently truncated. A version mismatch that degrades into wrong answers is worse
than one that errors.

Note the asymmetry with `ClassUnknown`: an unrecognized *observation* is expected
and gets a home in the enum (§2.6). An unrecognized *wire field* is a version
mismatch and is fatal. `ParseClass` therefore never falls back to
`ClassUnknown` — a collector that cannot classify something chooses
`ClassUnknown` deliberately, upstream.

---

## Changing this schema

1. Any change to the event shape, or any non-additive change to either enum, is a
   `Version` bump plus an entry in `CHANGELOG.md` (§10: never a silent edit).
2. Adding a class appends a line to `testdata/classes.golden`. Renaming or
   removing one **changes** a line — which is the point: it surfaces in review as
   a breaking change to a wire format that stored bundles depend on.
3. Regenerate golden files with `go test ./schema -update` and read the diff. The
   diff is the change; if it is larger than expected, the change was larger than
   intended.
