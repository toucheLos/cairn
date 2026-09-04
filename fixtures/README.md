# The fixture corpus

The corpus is the test suite and the eval set (CLAUDE.md §0.3), and it is the
part of this project that compounds. The CLI is copyable in a weekend; a corpus
of real, redacted, correctly classified incidents across heterogeneous sites is
not (§9).

It comes in two halves. **This directory is the public half, and it is synthetic
in its entirety.** The observed incidents — the half that is actually the moat —
live in a private corpus that is never committed. See "Where observed incidents
live" below.

> **Status: seven authored fixtures, zero observed ones.**
>
> Every fixture in this directory carries `synthetic: true`, and always will —
> observed incidents are never committed (CLAUDE.md §3). They exist to exercise
> the harness end to end and to serve as per-class templates. **They are not
> evidence**, they are excluded from every accuracy measurement, and no accuracy
> claim can be made from this directory in any state.
>
> Phase 0.3 targets roughly twenty observed incidents, captured into the private
> corpus with `cairn capture`. Adding them is the work.

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

### Naming the captures

`collectors.FixtureEnv` resolves a command to a file by name, so a capture is
named after the command that produced it:

| Command | File |
|---|---|
| `sacct …` | `input/sacct.txt` |
| `scontrol show job …` | `input/scontrol-show-job.txt` |
| `scontrol show node …` | `input/scontrol-show-node.txt` |
| `journalctl …` | `input/journalctl.txt` |
| `nvidia-smi` | `input/nvidia-smi.txt` |
| `ibstat` | `input/ibstat.txt` |
| a file read by path | `input/<basename>` — e.g. `slurmd.log`, `slurm-918273.err` |

A command with no matching file behaves exactly as an absent binary does. That
is deliberate: every fixture that omits a producer exercises the
missing-capability path for free, which is the path invariant §2.6 is about.

Capture what cairn would actually run, not what is quickest to type.
`journalctl -o short-iso` carries a year and a UTC offset; the bare `journalctl`
default carries neither, and a fixture built from it silently makes every
timestamp ambiguous.

`cairn capture` handles all of this: it records what the collectors actually
asked for and derives these names from the same resolver that finds them again,
so the table above is a description of what it produces rather than a checklist
to follow by hand. It also verifies the result replays before it declares
success — the naming is subtle enough that asserting the outcome beats reasoning
about it. Build a fixture by hand only for an authored, synthetic one.

## Where observed incidents live

**Not here.** This directory is the *public*, synthetic corpus and always will
be. Observed incidents go in the private corpus — `corpus/`, gitignored — and
are never committed: not the raw output, not the redacted output, not the event
streams derived from them. See CLAUDE.md §3 for the boundary and the three
guards that hold it, and run `make install-hooks` before you capture anything.

What becomes public is the taxonomy built from those incidents and the accuracy
measured over them. `LoadCorpus` loads both roots, so the harness runs against
your real incidents on the machine that holds them and against the synthetic set
everywhere else — including CI, which has no private corpus and must not.

## Adding an incident

```sh
cairn capture --job 918633 --slug ib-link-flap \
    --title "IB link flap killed an MPI job"
```

Run it on a host that can see the job. It runs the producers the collectors
read, saves exactly what they printed under the names replay expects, pre-fills
`meta.yaml` from your site profile, and reports what it could not capture and
what identifying material the scanner found. Then:

1. **Redact by hand.** The scanner is a backstop, not the process (§3). The
   conventions are below. capture deliberately does not redact for you: a tool
   that did would become the process, and the first thing it failed to recognize
   would land in the corpus unnoticed.
2. **`make scan-fixtures`.** Clean, or every finding accounted for.
3. **Complete `meta.yaml`.** Every `TODO` goes. `expected_root_cause` is the eval
   target and deserves real thought. `incident.job` and `incident.node` are
   already filled in — the replay harness takes the job from here rather than
   from the expected events, so that it is not handed the answer it is meant to
   be finding.
4. **Write `expected/events.json`.** Canonical form; the loader checks.
5. **Set `redacted_by` and `redaction_method`.** Until these are set the fixture
   is treated as *in progress*: skipped with a note, never loaded, and never
   counted toward accuracy. That is what lets twenty half-finished captures sit
   in the corpus for weeks without breaking `go test`.
6. **`make check`.**

`make new-fixture SLUG=... TITLE="..."` still scaffolds an empty directory by
hand, which is what you want for an authored synthetic fixture — for those,
pass `SYNTHETIC=1`.

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
  that carries no jobid at all. `005-ib-link-flap` has exactly one event carrying
  the job id and seven that do not; a join that returns only the first has
  returned the user the one thing they already knew.

---

## Writing the expected stream

Derive it from the **input**, not from the incident.

Every one of these fixtures was originally authored from the incident — what
happened, written down as events — and building the collectors showed several of
them to be wrong in ways that a reader would never have caught:

- `001` expected a `cgroup` attribute that appears nowhere in its captured
  journal, and had the MaxRSS conversion off by one mebibyte.
- `003` expected a driver mismatch from `nvidia-smi`, which cannot show one: the
  "CUDA Version" it prints is the highest runtime the driver supports, not the
  one the job used. Only the job's stderr knows.
- `002` claimed a partition on the batch step row, where sacct leaves it blank.
- `005` expected a `fabric` event from `ibstat` carrying `flap_count: 5` and a
  timestamp. ibstat prints no time at all — not on that capture, not ever — and
  the flap count is a tally of the *journal's* transitions, which the journal
  collector already emits. Building the fabric collector is what surfaced it.

An expected stream must contain only what a correct collector could actually
derive from the files in `input/`. If the incident is explained by something the
captures do not contain, that belongs in `expected_root_cause` and `notes` — not
as an event nobody can produce.

The reverse is also worth stating: an expected stream may contain events from
producers no collector reads yet. The replay test compares per-source and
reports uncovered producers, so the target stays whole while coverage grows
toward it.

And a third case, which `005` now demonstrates: a producer may be readable and
still have nothing it can honestly put on a timeline. `005` keeps `fabric` in
its `producers` list because `ibstat.txt` is still read — the collector reports
whether the port is visible and what state it is in — while contributing no
events. That state is compared against the node's siblings by `cairn diff`,
which needs no timestamp because a node profile carries its own.
