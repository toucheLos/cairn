# cairn

**cairn answers "why did this job die" in one command, across every cluster you run,
without root and without shipping your logs anywhere.**

> **Status: Phase 3 — `init`, `context`, `doctor`, `profile` and `diff` run.**
> The binary builds and works, against a live host or a replayed fixture. It has
> been exercised against a seven-incident corpus, three site fixtures, and one
> ordinary Linux box — not against a production cluster. Treat it as early: the
> failure corpus is still synthetic, so no accuracy claim is made or implied,
> and the four weeks of dogfooding Phase 2 calls for have not happened.

---

## The problem

Every tool in the HPC ecosystem is a *producer* of state — schedulers, exporters,
filesystems, fabric counters, BMCs. Almost nothing *consumes across producers*.
slurmdbd speaks MySQL, Prometheus speaks TSDB, journald speaks text, InfiniBand
speaks perfquery, GPFS speaks mmhealth, the BMC speaks Redfish.

Six dialects, no join. cairn is that join.

## Invariants

These are load-bearing. Each one exists because violating it forecloses a class of
deployment:

1. **Works with the LLM switched off.** Air-gapped sites forbid external inference.
   If value evaporates without an API key, those sites cannot use this at all.
2. **Runs unprivileged.** Login node or inside a job. Root buys better data; it never
   gates the tool.
3. **No log storage.** Raw text is normalized and discarded. This does not compete
   with Loki, Splunk, or Elastic, and it will not ask you to rip out Grafana.
4. **Read-only by default, in every code path.** Actuation arrives late, gated, and
   reversible.
5. **Single static binary.** No daemon, no server, no database, no procurement.
6. **Never hard-fails on an unknown stack.** Unrecognized scheduler, filesystem, or
   fabric is logged and skipped, not fatal.
7. **Deterministic output.** Two runs over the same window produce byte-identical
   bundles. Non-determinism breaks both diffing and evaluation.

## What this is not

Not log storage. Not a dashboard. Not an alerting system. Not a scheduler.

## Try it

```sh
make cairn                 # a static binary, no third-party code linked in
./cairn doctor             # what can this host see, and what does each gap cost?
./cairn init               # probe the stack into a reviewable site.yaml
./cairn context --job 12345
```

Nothing to configure and nothing to install. `doctor` is the honest place to
start: it reports what cairn cannot see and what each gap costs you, which on
most hosts is more informative than what it can.

To see the output shape without a cluster, replay a fixture:

```sh
make demo        # cairn context, on a Munge failure
make demo-site   # cairn init and cairn diff, on a fixture site
```

```
cairn context — job 918714 on cluster-a
schema v2 · 6 events (1 carry this job's id) · 1 node(s)
window 2026-03-04 10:16:03 .. 2026-03-04 10:36:03 (UTC)
redaction: none — this bundle contains real host and account names
nodes: node-0046

SITE       from site.yaml
  scheduler  slurm 23.02.7
  modules    lmod
  os         rocky 9.3, kernel 5.14.0-362.18.1.el9_3.x86_64, glibc 2.34
  fabric     infiniband 200
  gpu        nvidia driver 550.54.14, CUDA 12.4 (NVIDIA H100 80GB HBM3)

TIMELINE   * = carries this job's id · blank = node-scoped evidence · ~ = cluster-scoped
  [2026-03-04]
  10:31:02   journal auth.munge        munge.expired_credential daemon=munged ...
  10:31:02   journal config.clock_skew chrony.system_clock_wrong skew_sec=312 ...
  10:31:03 * slurm   app.nonzero_exit  slurm.sacct.state.FAILED job=918714 ...

WHAT CAIRN COULD NOT SEE
  slurm/job-stderr (unprivileged) — scontrol could not locate the job
      lost: CUDA driver/runtime mismatches and NCCL collective timeouts
```

One event carries the job id. The clock skew that actually caused the failure
does not, and never would have — which is the entire reason the join exists.

The SITE block comes from `site.yaml`, and it is reserved out of the token
budget: the line that says "this is Slurm 23.02" prevents a wrong answer, so it
is never dropped to fit two more events. With no profile, cairn says so there
rather than staying silent and letting a reader assume a stack.

`--redact` pseudonymizes hosts, users and accounts before anything is printed,
deterministically, so two bundles from the same site stay comparable.
`--format json` emits the canonical bundle for archival or replay.

## Knowing which cluster you are on

`cairn init` probes the stack — scheduler and version, module system, Spack and
EasyBuild roots, distro and kernel, mounts, fabric, GPUs — and writes a
`site.yaml` meant to be read, corrected, and committed:

```yaml
# The batch system. This is the single most load-bearing value in the file:
# it is what stops a model answering a Slurm question with PBS syntax.
scheduler:
  kind: slurm
  version: 23.02.7
  partitions:
    - batch
    - gpu
```

cairn guesses, and a guess nobody has checked is not a site profile — so `init`
will not overwrite your corrections without `--force`, and shows you what it
would change instead. The profile becomes the header on every `cairn context`.

The file also records what cairn *could not* probe, and what each gap costs. A
profile with no `fabric` section would otherwise be ambiguous between "no
InfiniBand here" and "nobody looked".

## Fleet-relative health checking

A node is unhealthy when it diverges from its 47 siblings, not when it crosses a
number someone guessed in 2014. `cairn profile` captures one node's drift keys;
`cairn diff` compares one against the rest.

```sh
srun -w node[001-048] --ntasks-per-node=1 \
    sh -c 'cairn profile > profiles/$(hostname -s).json'
cairn diff node-0046
```

```
cairn diff — node-0046 on cluster cluster-a
4 sibling(s): node-0045 node-0047 node-0048 node-0049

DIVERGENCE FROM SIBLING MAJORITY
  munge.key_mtime        this node: 2025-08-11
                         4 of 4 siblings: 2026-02-01
  nvidia.driver_version  this node: 535.104.05
                         4 of 4 siblings: 550.54.14

Which side is correct is not something cairn can tell you. A node that
differs from its siblings may be the only one that got the patch.
```

That last paragraph is the design, not a disclaimer. cairn reports divergence
and leaves the verdict to you. Below three siblings it refuses to compare at
all, because a majority of two is not a fleet norm.

cairn implements no fan-out of its own — `srun`, `pdsh` and `clush` already
exist, and shipping remote execution would mean an ssh dependency and a second
read-only boundary to get right.

## Layout

```
schema/       the event struct and the closed class enum   <- the important part
fixtures/     redacted, pre-classified incidents           <- tests AND eval set
redact/       deterministic pseudonymization + the pre-commit scanner
collectors/   capability-gated readers: slurm, journal, gpu
join/         correlation on (node, jobid, time)
site/         discovery, site.yaml, fleet-relative drift
cmd/cairn/    init · context · doctor · profile · diff · miss
taxonomy/     signature -> cause -> remediation            (Phase 4)
policy/       default-deny allowlist, dry-run, audit log   (Phase 4)
```

`fabric`, `storage` and `bmc` have no collector yet. `cairn doctor` says so
rather than leaving their absence to be read as a clean bill of health.

`schema/` is the most important artifact here. The `class` enum is closed: adding a
member is a schema version bump, not a casual commit. See `schema/DESIGN.md`.

## Development

Requires Go 1.27 or later.

```sh
make check          # build, vet, test, verify enum, validate fixtures, scan redaction
make verify-guards  # deliberately break each guard and confirm it fires
make new-fixture    # scaffold a new incident fixture
make scan-fixtures  # check fixtures for unredacted material before committing
```

`make verify-guards` matters more than it looks. A guard nobody has watched fail
is a guard nobody has verified, and several of these protect things that cannot
be undone once pushed.

Install the pre-commit hook so unredacted material cannot land by accident:

```sh
make install-hooks
```

## Contributing a fixture

The corpus is the point. See `fixtures/README.md` for the intake procedure and the
redaction checklist. Incidents are hand-redacted before they land; the scanner is a
backstop, not a substitute.

## License

Source code is Apache-2.0. `fixtures/` (excluding fixtures marked `synthetic: true`)
and `taxonomy/` are reserved pending a separate licensing decision — see `NOTICE`.
