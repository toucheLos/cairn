# CLAUDE.md — cairn

> Working name. Swap by editing this line and `cmd/cairn`. Not yet checked against
> npm / PyPI / crates / USPTO.

**One line:** cairn answers "why did this job die" in one command, across every
cluster you run, without root and without shipping your logs anywhere.

---

## 1. The thesis

Every tool in the HPC ecosystem is a **producer** of state — exporters, schedulers,
filesystems, fabric counters, BMCs. Almost nothing **consumes across producers**.
slurmdbd speaks MySQL, Prometheus speaks TSDB, journald speaks text, IB speaks
perfquery, GPFS speaks mmhealth, the BMC speaks Redfish. Six dialects, no join.

cairn is that join. Everything else in this repo is a view over it.

The failure mode to avoid: building an LLM that shells into a login node and greps.
That is already commodity — several free single-file MCP servers do it. The value is
in the substrate underneath, not the chat interface on top.

---

## 2. Invariants

These are not preferences. Violating any one of them kills a market segment.

1. **Works with the LLM switched off.** Defense, pharma, and air-gapped sites will
   forbid external inference. If value evaporates without an API key, those buyers
   are gone. The agent is a *consumer* of the substrate, never the substrate.
2. **Runs unprivileged.** Login node or inside a job. Root buys better data, never
   gates the tool. Assuming root locks us out of the entire NeoCloud tenant market.
3. **No log storage.** We normalize and discard raw text. We are not competing with
   Loki, Splunk, or Elastic, and we will not ask a site to rip out Grafana.
4. **Read-only by default, in every code path.** Actuation arrives late, gated, and
   reversible.
5. **Single static binary.** No daemon, no server, no database, no procurement.
6. **Never hard-fail on an unknown stack.** Unrecognized filesystem, scheduler, or
   fabric? Log it, skip that collector, keep going.
7. **Deterministic output.** Two runs over the same window produce byte-identical
   bundles. Non-determinism breaks diffing and breaks evals.

**Do not build:** log storage, a web dashboard, an alerting system, a scheduler.
Two of those are "later." Two are "never."

---

## 3. IP hygiene

Personal hardware, personal account, outside employment hours. Never commit real
hostnames, usernames, account codes, or raw accounting rows — fixtures are redacted
by hand before they land. Do not describe the taxonomy publicly as "trained on"
any specific institution's data.

---

## 4. Architecture

```
collectors/   capability-gated readers, one per producer
  ├─ slurm/         sacct, sacctmgr, scontrol, slurm.conf
  ├─ journal/       journald, slurmd/slurmctld logs
  ├─ gpu/           nvidia-smi, DCGM
  ├─ fabric/        ibstat, perfquery, NCCL debug output
  ├─ storage/       mounts, lctl, mmhealth
  └─ bmc/           Redfish, IPMI SEL
schema/       the event struct + class enum. Versioned. Change with care.
join/         correlation on (node, jobid, time)
redact/       deterministic pseudonymization, applied before anything leaves
site/         discovery, site.yaml, capability gating
taxonomy/     signature → cause → remediation, with confidence
policy/       default-deny allowlist, dry-run, audit log  (Phase 4)
cmd/cairn/    context | diff | doctor | init
```

**The event schema** is the most important artifact in the project:

```
{ ts, cluster, node, jobid, source, class, detail }
```

`class` is a **closed enum** — the complete set of failure classes we will ever emit.
Adding to it is a schema version bump, not a casual commit. `detail` is bounded and
structured; it is not a dumping ground for raw log lines.

**The collector contract:** declares its capability requirements (binary present,
permission level, daemon reachable), emits events, never throws. `doctor` reports
what each collector can and cannot see, and why.

---

## 5. Current phase

**Phase 0 — foundations.** Nothing is shipped.

- [x] 0.1 Clean repo, LICENSE, NOTICE
- [x] 0.2 Freeze event schema + class enum — v1, 25 classes. See `schema/DESIGN.md`.
- [~] 0.3 Fixture corpus: ~20 real, hand-redacted, pre-classified incidents
      (OOM, walltime, driver-generation mismatch, node-not-responding, IB link flap,
      Munge auth failure, NCCL hang)
      - [x] Format, loader, validator, golden harness, `make new-fixture`
      - [x] Redaction scanner + pre-commit hook (`redact/scan/`)
      - [x] Seven authored fixtures, one per mode above — all `synthetic: true`,
            excluded from every accuracy measurement. Templates, not evidence.
      - [ ] **The ~20 real incidents.** This is the remaining Phase 0 work and it
            cannot be delegated: it needs cluster access, and inventing incidents
            would corrupt the eval set at its root. Intake procedure is in
            `fixtures/README.md`.

Phase 0.3 is the test suite *and* the eval set. Build it before the code it tests.

Do not start Phase 1 on the strength of the authored fixtures. They prove the
harness runs; they prove nothing about whether cairn classifies real incidents
correctly, and a collector tuned to pass them is tuned to pass our own guesses.

---

## 6. Roadmap

**Phase 1 — substrate.** Collector interface + slurm/journal/gpu collectors. The join,
including clock skew, node-without-jobid, jobid-without-node, array and
heterogeneous jobs. Redaction layer built *now*, not retrofitted.
→ *Given a jobid, return every event from every producer that bears on it.*

**Phase 2 — first shippable artifact.** `cairn context --job <id>`: token-budgeted,
deterministically ordered, redactable. `cairn doctor`. Then four weeks of dogfooding
across every cluster we admin, logging every miss.
→ *One command, paste into an LLM, get a correct answer. The miss log drives Phase 3,
not our intuition.*

**Phase 3 — site awareness.** `cairn init` probes the stack (scheduler + version,
module system, Spack/EasyBuild roots, mounts, ibstat, DCGM, distro/kernel, Redfish,
existing Prometheus/Ganglia) and emits a reviewable, git-committable `site.yaml`.
The profile becomes the context header — this is what stops a model suggesting PBS
syntax to a Slurm site. `cairn diff <node>` compares a node to its fleet siblings.
Multi-cluster config: one invocation, N clusters.
→ *Onboarding is discovery-first. Admins correct a generated file; they do not fill
in a form.*

**Phase 4 — the compounding asset.** Taxonomy v1 (rules and signatures, not ML).
Read-only MCP surface. Policy engine built and tested against a **null action set**.
Then exactly three reversible actuations: drain node, requeue job, rerun health check.
No config edits, ever.
→ *Policy proven correct before anything can execute. Never the other way around.*

**Phase 5 — revenue surface.** Burn accounting and attribution. See §8.

---

## 7. Track A — academic and national labs

This segment **adopts and does not pay**. Their budget is capital, not opex.
Treat them as distribution, corpus, and credibility — not revenue. They are also the
population most likely to clone a thin product in a weekend, so what we give them
must be the part that compounds rather than the part that's copyable.

- **Fleet-relative health checking.** LBNL NHC is a bash script from ~2011 and it's
  still what most sites wire into Slurm's `HealthCheckProgram`. Replace threshold
  checks with *sibling comparison*: a node is unhealthy when it diverges from its
  47 peers, not when it crosses a number someone guessed in 2014.
- **Configuration drift detection.** Ansible enforces intent; nothing measures
  divergence. Driver generation, kernel cmdline, glibc, module tree, mount set,
  munge key mtime. This is the class of failure that stays invisible until jobs die.
- **Shareable incident bundles.** A redacted, deterministic artifact a user attaches
  to a ticket and an admin replays offline. If this becomes the lingua franca for
  "here's what happened," we own the interchange format.
- **Submission-script analysis against actual site config** — not generic linting.
  Partition limits, QOS, module availability, GPU:CPU ratios, the real ones.
- **Tier-1 deflection for new users.** REU students, first-year grads, wet-lab
  researchers running GROMACS. "Explain my failure" in terms of *this* cluster.
- **Air-gapped operation.** Offline taxonomy bundle, no egress, reviewable config.
- **Interop, not replacement.** Read from Prometheus/Ganglia where present; export to
  XDMoD and ColdFront; render inside OpenOnDemand. Meet sites where they are.
- **Federated failure signatures (the long game).** Opt-in, anonymized signature
  contribution across sites. There is no CVE-equivalent for HPC hardware and
  software failure modes; every site's knowledge dies with its senior admin. This is
  the single highest-value thing we could give the ecosystem, and it is the one asset
  a competitor cannot clone — it accretes.

---

## 8. Track B — NeoCloud tenants

This segment **pays**. ML infra leads on rented GPUs: highest pain, lowest HPC
competence, real budget, and a meter running. Value is denominated in GPU-hours,
not admin-hours — which is why it's collectible here and not in Track A.

- **Attribution.** My code, my container, or their fabric? The tenant is blind (no
  root, no node telemetry) and the provider is structurally conflicted. Nobody can
  answer this today. Requires exactly the fabric and NCCL diagnosis skill that is
  scarce in this market.
- **SLA credit substantiation.** Providers issue credits when degradation is proven.
  Most tenants cannot prove it. A timestamped, defensible incident record pays for
  the tool out of one bad week.
- **Burn accounting.** DCGM occupancy joined to Slurm allocation joined to the rate
  card. "You reserved 64 GPUs for 300 hours at 11% average occupancy" is a number
  with a dollar sign, producible during a trial.
- **Pre-flight fabric validation.** Validate the allocation *before* committing a
  512-GPU run, not at hour 40.
- **Early-abort signal on doomed runs.** Detect a NCCL hang or a degraded rail at
  hour 2. The economics of catching it early are the entire pitch.
- **Cross-provider normalization.** The same workload's failure and efficiency
  profile across providers, on one schema. No provider will ever build this. It
  requires sitting outside all of them, and it gets more valuable with each provider
  and tenant added.

**Do not sell to the providers first.** Their tier-1 queue is full of ML engineers
who have never used Slurm, so there is a real headcount-savings contract there — but
it's a long enterprise cycle and the wrong opening move. Go bottom-up through
tenants; providers notice when their customers already run it.

---

## 9. What is and isn't defensible

Be honest about this internally, so effort goes to the right place.

**Copyable in a weekend:** the CLI, the MCP tool surface, sacct parsing, any
dashboard. Assume these get cloned. Give them away; they're distribution.

**Not copyable:**

- **The failure corpus.** Signatures accumulated across heterogeneous sites and
  providers. Requires access, time, and trust. Compounds. This is the moat.
- **The domain judgment encoded in the taxonomy.** InfiniBand fabric debugging,
  Slurm internals, NCCL failure semantics, GPU driver-generation interactions. Very
  few people have run enough of these to encode them, and a model can't infer them
  from documentation.
- **Heterogeneity handling.** Getting the join right across differing schedulers,
  fabrics, filesystems, and permission levels requires many real sites. Cannot be
  built from one cluster.
- **The tenant-side vantage point.** Attribution across the tenant/provider boundary
  is structurally unavailable to the provider and technically unavailable to the
  tenant working alone.
- **The eval harness.** A labeled set of real incidents with known-correct
  classifications is the only way to make defensible accuracy claims — and it's a
  credibility asset in its own right. Publish the methodology, not the data.

---

## 10. Conventions for working in this repo

- Every feature must work unprivileged and with inference disabled. If it can't,
  it's the wrong feature or it belongs behind a capability gate.
- Fixtures first. New collector or classifier → add the redacted fixture and the
  expected classification before the implementation.
- Schema changes require a version bump and a migration note. Never a silent edit.
- Redaction is applied at the boundary, not by the caller. If a code path can emit
  an unredacted hostname, that's a bug, not a configuration choice.
- Prefer rules and signatures over ML until the corpus is large enough to justify
  otherwise, and until we can measure the difference.
- Keep the diff between the on-prem and tenant code paths as close to zero as
  possible. Same binary, thinner site profile.
