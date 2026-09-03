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

Personal hardware, personal account, outside employment hours.

**Observed incidents never reach this repository.** Not the raw producer output,
not the hand-redacted output, and not the event streams derived from them. They
live in a private corpus — `corpus/`, gitignored — and what is published is the
taxonomy built from them and the accuracy measured over them.

| Artifact | Where it lives |
|---|---|
| Raw producer output | private corpus only |
| Hand-redacted output | private corpus only |
| Event streams (`expected/events.json`) | private corpus only |
| Signature names (`mlx5.ib_event.link_down`) | may be published |
| Taxonomy rules (signature → cause → remediation) | may be published |
| Aggregate accuracy numbers | may be published |

The line falls there because a signature describes *vendor* output — a Mellanox
driver message, `sacct` printing `OUT_OF_MEMORY` — rather than any site's data.
That is what makes §9's "publish the methodology, not the data" achievable
rather than aspirational: the moat is buildable while the corpus stays home.

Three guards enforce this, because each one alone can be stepped over:
`.gitignore` (bypassed by `git add -f`), `scripts/pre-commit` (bypassed by
`--no-verify`), and `scripts/check-boundary.sh`, which runs in CI — on GitHub —
and so catches a bypass of the other two. `make install-hooks` installs the
second.

The public corpus in `fixtures/` is synthetic and always will be. `LoadCorpus`
refuses to load an observed fixture from it.

One residual risk no code can remove: the taxonomy's *shape* leaks stack
composition — a burst of GPFS signatures implies a GPFS site. Do not describe
the taxonomy publicly as "trained on" any specific institution's data.

If an observed incident ever does land here, deleting it in a later commit does
not fix it. The content is in the history and, once pushed, on GitHub's servers.
Treat it as a disclosure.

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
site/         discovery, site.yaml, capability gating, fleet-relative drift
taxonomy/     signature → cause → remediation, with confidence
policy/       default-deny allowlist, dry-run, audit log. Null action set.
yamlsub/      the bounded YAML subset site.yaml and policy.yaml are written in
cmd/cairn/    init | context | doctor | capture | profile | diff | policy | miss
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

**Phase 4 is current**, entered from its far end: the policy engine is built
against a null action set, because that is the one piece of Phase 4 that needs
neither the corpus nor a cluster.

Everything else that remains is gated on evidence — the ~20 real incidents and
Phase 2's four weeks of dogfooding, neither of which has started. Phases 0–3
are below with their status.

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
      - [x] `cairn capture` — intake is one command. It runs the producers the
            collectors read, saves exactly what they printed under the names
            replay expects, pre-fills `meta.yaml` from the site profile, and
            runs the redaction scanner. It never redacts (§3).
      - [ ] **The ~20 real incidents.** This is the remaining Phase 0 work and it
            cannot be delegated: it needs cluster access, and inventing incidents
            would corrupt the eval set at its root.

            They are captured into the **private corpus**, never here (§3). The
            box is ticked when `corpus/` holds twenty redacted, classified
            incidents — a state only the machine holding them can observe, which
            is the point. What becomes public is the accuracy measured over them.

Phase 0.3 is the test suite *and* the eval set. Build it before the code it tests.

Do not treat the authored fixtures as evidence. They prove the harness runs and
the collectors parse; they prove nothing about whether cairn classifies real
incidents correctly, and a collector tuned to pass them is tuned to pass our own
guesses.

**Phase 1 — substrate.** In progress.

- [x] Collector contract, capability model, and the Env abstraction that lets
      every collector be replayed against the corpus without a cluster
- [x] slurm collector — sacct (parsable and aligned), scontrol, job stderr
- [x] journal collector — journald and the slurmd/slurmctld logs
- [x] gpu collector — nvidia-smi
- [x] Redaction layer, built now rather than retrofitted
- [x] The join — clock skew, node-without-jobid, jobid-without-node, array and
      heterogeneous jobs
- [ ] fabric, storage, and bmc collectors (not in the §6 scope for Phase 1;
      fixture 005 still carries one uncovered `fabric` event)

Building the collectors corrected three fixtures whose expected streams had been
authored from the incident rather than from the captured input. See
`fixtures/README.md`, "Writing the expected stream".

**Phase 2 — first shippable artifact.** The commands run.

- [x] `cairn context --job <id>` — token-budgeted, deterministically ordered,
      redactable. `--format json` emits the canonical bundle.
- [x] `cairn doctor` — what each collector can and cannot see, and what each gap
      costs. Names the producers with no collector at all, so a clean report is
      not mistaken for a clean fabric.
- [x] `cairn miss` — the log §6 makes the input to Phase 3.
- [x] Static binary, no third-party code linked in. Guarded, not assumed.
- [ ] **Four weeks of dogfooding across every cluster we admin, logging every
      miss.** Not started. This is the remaining Phase 2 work and it is the part
      that decides Phase 3.

Half a day of running `doctor` on one ordinary Linux box already found two real
bugs: the journal collector accepted only the `+0000` offset form while systemd
emits `-04:00`, so every live journald line failed to parse while the whole
corpus passed; and unmatched lines were warned about individually, producing
353,526 warnings on a single host. Both are fixed and both are now covered. The
lesson is the one §6 already states — the miss log, not our intuition.

Do not skip the dogfooding. Everything above is validated against seven authored
fixtures and one laptop.

**Phase 3 — site awareness.** The commands run.

- [x] `cairn init` — probes scheduler + version, module system, Spack/EasyBuild
      roots, distro/kernel/glibc, mounts, fabric, GPU, BMC and existing
      Prometheus/Ganglia into a reviewable, git-committable `site.yaml`.
      Refuses to overwrite an admin's corrections without `--force`, and shows
      the diff instead. Records what it could *not* probe and what that costs.
- [x] The profile is the context header, reserved out of the token budget for
      the same reason the capability section is. Absent profile is stated, not
      left silent.
- [x] `cairn profile` and `cairn diff <node>` — fleet-relative drift on the §7
      keys. Refuses below three siblings, requires a strict majority, and
      reports divergence rather than a verdict.
- [x] Multi-cluster config: a `site.Set` loads N profiles; `doctor -A` and
      `diff` work across all of them in one invocation. `context` needs a live
      scheduler and says it only reached the local one.
- [ ] **Phase 2's four weeks of dogfooding still have not happened.** Phase 3
      was therefore built from §6's specification rather than from the miss log,
      which is what §6 says should drive it. The shape was specified in enough
      detail for that to be defensible; the priorities *inside* it are still
      intuition, and the miss log is what would correct them.

Two things this phase changed that were not planned, recorded because both were
the corpus arguing with the design rather than the other way around:

- **Schema v2 adds the `site` producer.** `config.drift` was registered in v1
  with `peer_count` and `peer_majority` — the class was there, only the producer
  was missing, and none of the six existing ones fits a fact cairn derives by
  comparing profiles it captured itself. See `schema/CHANGELOG.md`.
- **Redaction leaked storage addresses.** `redact.text` only substitutes
  identifiers learned from a structured field, and a filesystem server's address
  never appears in one — only inside a mount source, which is exactly where
  every Lustre and NFS mount puts it. `cairn diff --redact` shipped
  `192.0.2.10@o2ib:/lustre` straight through the boundary. Addresses are now
  pseudonymized structurally. This was in every code path that emits a mount
  value, not just the new one.

A known gap, found by running `doctor` on this laptop and deliberately left:
`doctor` reports `N warning(s)` but has no `-v` to show them, while `context`
has one. The warnings are the miss log.

**Phase 4 — the compounding asset.** Started, from the far end.

- [x] **Policy engine, against a null action set.** The gate is built and proven
      while the blast radius is provably zero. `ShippedActions()` returns
      nothing, and a guard asserts it — so "cairn cannot act" is a fact about
      the binary rather than a claim in a comment.

      Six properties, each a test and each a guard watched to fail: the action
      set is empty; an implemented action is still denied unless a policy lists
      it; an empty scope authorizes no targets rather than all of them; an
      action that cannot say how it is undone does not run; a policy written for
      one cluster does not authorize another; and if the audit log cannot be
      written the action is refused.

      That last one is the ordering that cannot be retrofitted. An actuation
      that happened with no record of it is only discoverable by noticing the
      damage.

      Two structural choices carry more weight than they look: the action set is
      a field on `Engine` with no exported way to add to it, so nothing outside
      the package can hand an actuation to a production engine; and `policy/`
      imports neither `collectors`, `site` nor `join`, so there is provably no
      route from it to a cluster in this build.

- [ ] **The three actuations** — drain node, requeue job, rerun health check.
      Deliberately absent. §6 puts them after the gate is proven, and
      `policy/doc.go` calls that ordering easy to get backwards.
- [ ] **Taxonomy v1.** Blocked on the corpus, and that is the right kind of
      blocked: built on seven fixtures we authored ourselves it would encode our
      guesses as the moat.
- [ ] **Read-only MCP surface.**

`yamlsub/` was extracted from `site/` so `policy.yaml` could share one parser
rather than growing a second. The site goldens are byte-identical across the
move, which is what says it was clean.

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

  That sentence is now an arrangement of files rather than an intention. The
  incidents live in a private corpus and cannot be committed; the harness, the
  synthetic fixtures, the taxonomy and the accuracy numbers are public. See §3
  for the boundary and the three guards that hold it. This also turns out to be
  the only shape that works for a contributor who can run cairn on a cluster but
  cannot export anything derived from its logs — which is most people who have
  the access worth having.

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
