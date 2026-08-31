# The fixture corpus

This directory is the test suite and the eval set (CLAUDE.md §0.3), and it is
the part of this project that compounds. The CLI is copyable in a weekend; a
corpus of real, redacted, correctly classified incidents across heterogeneous
sites is not (§9).

> **Status: seven authored fixtures, zero observed ones.**
>
> Every fixture currently here carries `synthetic: true`. They exist to exercise
> the harness end to end and to serve as per-class templates. **They are not
> evidence**, they are excluded from every accuracy measurement, and no accuracy
> claim can be made from this directory in its present state.
>
> Phase 0.3 targets roughly twenty observed incidents. Adding them is the work.

---

## Layout

```
NNN-short-slug/
  meta.yaml             provenance, expected classification, expected root cause
  input/                redacted producer output, exactly as the tool printed it
    .redaction-ok       optional: reviewed scanner exceptions for this directory
  expected/
    events.json         the canonical event stream a correct collector must produce
```

## Adding an incident

```sh
make new-fixture SLUG=ib-link-flap TITLE="IB link flap killed an MPI job"
```

That scaffolds the directory and prints the full checklist. In outline:

1. **Capture.** Put the raw producer output in `input/`. Keep it byte-realistic —
   Phase 1 collectors have to parse these files, so do not annotate them.
2. **Redact by hand.** The scanner is a backstop, not the process (§3). The
   conventions are below.
3. **`make scan-fixtures`.** Clean, or every finding accounted for.
4. **Fill in `meta.yaml`.** Every `TODO` goes. `expected_root_cause` is the eval
   target and deserves real thought.
5. **Write `expected/events.json`.** Canonical form; the loader checks.
6. **`make check`.**

### If the incident needs a class that does not exist

Stop and make it a deliberate decision. Adding a member is a schema version bump
(§4): add it to `schema/class.go`, register its detail keys in `schema/attrs.go`,
record *why* in `schema/CHANGELOG.md`, and regenerate goldens with `make golden`.

This has already happened once. Fixture `006-munge-auth-failure` is a Munge
failure whose actual cause is a 312-second clock error, and there was no class
for a wrong clock — so `config.clock_skew` was added. That is the intended
direction of travel, and it is why §0.3 puts the corpus before the code.

Do not reach for `unknown` to avoid the conversation. But do use it, deliberately,
when the honest answer is that cairn has no class for what was observed — see
`007-nccl-hang`, which records a GPU holding 73 GiB at 0% utilization as
`unknown` because that gap is real and hiding it would help nobody.

---

## Redaction conventions

Consistency matters as much as removal: the *same* original must map to the
*same* placeholder everywhere it appears, or the topology and the join
relationships in the fixture are destroyed along with the identifying data.

| Original | Becomes |
|---|---|
| hostnames | `node-0001`, `node-0002`, … |
| cluster names | `cluster-a` |
| user names | `user-01` |
| uids / gids | `90001`, `90002`, … (the 90000+ band reads as a placeholder and still parses as an integer) |
| account / project codes | `acct-01` |
| IPv4 | `192.0.2.x`, `198.51.100.x`, `203.0.113.x` (RFC 5737) |
| InfiniBand GUIDs | `0x0000000000000001`, `0x0000000000000002`, … |
| domains | removed entirely — a bare pseudonym host is enough |
| home paths | `/home/user-01/…` |
| MAC addresses | removed |
| key material | **deleted, never redacted** |

**Keep the evidence.** Job ids, timestamps, error strings, counters, versions,
device names, Xid numbers, exit codes. Redacting those leaves a fixture that
tests nothing.

### Reviewed exceptions

Where a scanner rule fires on something genuinely safe — a hardware model number
that looks like an allocation code, say — record it in that directory's
`.redaction-ok`:

```
account-code MT4123 # Mellanox/NVIDIA HCA model designator, not an allocation code
```

A reason is required, and suppressions do not apply to subdirectories. Do not
silence a rule in `scan.go` to make one fixture pass.

---

## `synthetic` is load-bearing

`Real()` in `fixtures.go` filters on it, and accuracy is reported over observed
fixtures only. A synthetic fixture that lost its flag would start counting toward
an accuracy number — which would make the eval harness worse than not having one,
since §9 lists it as a credibility asset precisely because the distinction holds.

Observed fixtures additionally require `redacted_by` and `redaction_method`. An
unattributed redaction is an unreviewed one.

---

## What makes a fixture good

- **It has a defensible expected root cause.** Not just the right classes — the
  right *answer*. A run that produces every correct class and the wrong
  conclusion has still failed.
- **It is honest about what the evidence cannot settle.** `004-node-not-responding`
  is entirely the controller's view and cannot say why the node went quiet. That
  is what makes it valuable: it catches a classifier that overclaims.
- **It preserves the awkward parts.** Missing timestamps, tool output that
  contradicts itself, a down port reporting a meaningless rate. Collectors have
  to survive these, and a tidied fixture does not test that they do.
- **It exercises the join.** Events from more than one producer, ideally with one
  that carries no jobid at all.
