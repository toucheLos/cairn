# cairn

**cairn answers "why did this job die" in one command, across every cluster you run,
without root and without shipping your logs anywhere.**

> **Status: Phase 0 — foundations. Nothing is shipped.**
> There is no working `cairn` binary yet. This repository currently contains the
> frozen event schema, the fixture corpus format, and the test harness. It is not
> usable as a tool. Do not deploy it.

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

## Layout

```
schema/       the event struct and the closed class enum   <- the important part
fixtures/     redacted, pre-classified incidents           <- tests AND eval set
redact/       deterministic pseudonymization + the pre-commit scanner
collectors/   capability-gated readers, one per producer   (Phase 1)
join/         correlation on (node, jobid, time)           (Phase 1)
site/         discovery, site.yaml, capability gating      (Phase 3)
taxonomy/     signature -> cause -> remediation            (Phase 4)
policy/       default-deny allowlist, dry-run, audit log   (Phase 4)
cmd/cairn/    context | diff | doctor | init               (Phase 2+)
```

`schema/` is the most important artifact here. The `class` enum is closed: adding a
member is a schema version bump, not a casual commit. See `schema/DESIGN.md`.

## Development

Requires Go 1.27 or later.

```sh
make check          # build, vet, test, verify enum, validate fixtures, scan redaction
make new-fixture    # scaffold a new incident fixture
make scan-fixtures  # check fixtures for unredacted material before committing
```

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
