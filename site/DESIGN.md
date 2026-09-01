# Site profiles — design decisions

`site/` answers two questions that the collectors cannot: *what kind of cluster
is this*, and *how does this node differ from its siblings*. Both are state
rather than observation, which is why they live outside `schema/`.

---

## 1. Discovery is a generated draft, not an answer

CLAUDE.md §6: "Onboarding is discovery-first. Admins correct a generated file;
they do not fill in a form."

Everything about `site.yaml` follows from taking that literally.

- The file carries **inline comments** explaining what each value is for. That
  is the whole reason it is YAML and not JSON — a file nobody can read is a file
  nobody will correct.
- `cairn init` **refuses to overwrite** without `--force`, and prints the
  difference between what is recorded and what it just probed. The corrections
  are the value of the file; silently clobbering them defeats the feature. A
  bare "file exists, use --force" would push an operator toward `--force`
  without ever showing them what they were about to lose.
- Decoding **rejects unknown keys**. This is schema/DESIGN.md §8 applied to a
  file people hand-edit, which is where it matters most: `partitons:` has to be
  an error, or the file says one thing and the tool does another.

## 2. Probes report what they could not see, and what it costs

`site.Probe` mirrors `collectors.Capability` field for field, including
`Reveals`. The reasoning is the same one `doctor` is built on: a report of what
was found is not usable without a report of what was looked for and missed.

A `site.yaml` with no `fabric` section is ambiguous between "no InfiniBand here"
and "nobody looked", and those need opposite responses from an admin. The `gaps`
section is what disambiguates them.

Only *unavailable* probes are written to the file. A probe that succeeded has
already said what it found in the sections above, and repeating it doubled the
length of a file whose entire purpose is to be read.

`Reveals` is not stored. It is static text belonging to the probe rather than a
fact about this site, so it is supplied by cairn when reporting and left out of
the artifact.

## 3. Discovery reads only through `collectors.Env`

`site/` touches neither `os` nor `exec` directly. Two things fall out of that:

- Every probe **replays against a fixture directory**, so `cairn init` is
  testable without a cluster. The corpus is the eval set (CLAUDE.md §0.3); a
  probe exercisable only on real hardware cannot be evaluated at all.
- The read-only boundary stays reviewable at **one interface**. There is no
  write, create, or remove on `Env`, and adding one would show up in review as a
  change to that file rather than buried in a probe.

Phase 3 added two methods, both reads:

- `Getenv` — Lmod announces itself through `$LMOD_CMD`, Spack through
  `$SPACK_ROOT`. Probing well-known paths instead would find an installation the
  site has not activated and report a module system no job of theirs can use.
- `Stat` — the munge key's mtime is a §7 drift key, and
  `/etc/munge/munge.key` is `0400 munge:munge`. cairn can stat it unprivileged
  and must never read it. `Stat` returns a `time.Time` rather than an
  `fs.FileInfo` so it cannot be widened into a way to reach the contents.

`FixtureEnv` serves `Stat` from an explicit `input/mtimes.txt` rather than from
the replayed file on disk. A checkout's mtimes are whenever git wrote them, so
reading them would make the same fixture produce a different profile on every
machine — a §2.7 violation that only appears when two people compare results.

## 4. YAML is hand-rolled

`scripts/verify-guards.sh` §8 asserts the shipped binary links no third-party
code. `gopkg.in/yaml.v3` is a real dependency of the fixture loader and must not
reach `cmd/cairn`.

So `yaml.go` implements a writer and reader for exactly the subset `site.yaml`
uses, and rejects everything else — anchors, flow collections, block scalars,
tabs, multiple documents — with an error naming the line. This is the same trade
`schema/encode.go` makes for canonical JSON. A bounded format we emit and
consume ourselves is cheaper for a site to review than a general parser, and it
cannot surprise us.

Node profiles go the other way and use `encoding/json`, matching
`schema/decode.go`: hand-rolled writer for canonical byte order, stdlib reader
with `DisallowUnknownFields`. Both are stdlib, so the guard is satisfied either
way, and matching the existing split keeps one convention in the repo.

## 5. Profiles are state, and carry their own version

`site.ProfileVersion` is separate from `schema.Version`. An event is an
observation; a profile is what a machine currently is. Tying them together would
mean that discovering a new kind of mount became a migration for consumers who
only ever read events.

The site profile is deliberately **not** part of `schema.Bundle`. Putting it
there would be a schema migration, and the header is a rendering concern. The
question of whether a shared incident bundle should carry its site profile is
recorded in `schema/CHANGELOG.md` under deferred questions.

A node profile *does* carry a `captured_at`, unlike a bundle. A bundle describes
a fixed past window, so a generation stamp would be noise that breaks
byte-comparison. A profile describes a node *now*, and a fleet comparison across
profiles captured weeks apart is not a fleet comparison — so the staleness has
to be visible, and `diff` reports the spread.

## 6. Drift is an observation, never a verdict

This is the same rule schema/DESIGN.md §1 applies to collectors, and it is
easier to violate here because the output looks so much like a judgement.

`cairn diff` reports that a node diverges from its siblings. It does **not**
report that the node is unhealthy, and every layer preserves that:

- The comparison **refuses below `MinPeers` (3) siblings** and says why. A
  majority of two is not a fleet norm, and a confident-looking claim derived
  from a coin flip is worse than no claim.
- It requires a **strict majority**, not a plurality. A fleet split 40/35/25 has
  no norm to diverge from; naming the 40% "expected" would manufacture one. Such
  keys are reported as undecided, which is itself a finding about the fleet.
- The text output says outright that cairn cannot tell which side is correct. A
  node diverging from 47 siblings may be the only one that got the patch.

An **absent** key is drift, not missing data. The node that lost `/scratch` is
exactly the node whose jobs are failing, so absence is compared as a value.

`kernel.cmdline` drops `root=`, `resume=` and `rd.*` before comparing. Those
legitimately differ on every machine, and keeping them would report drift on
every node against every other node and bury the real findings.

## 7. cairn does not fan out

`cairn profile` captures one node and writes JSON to stdout. Gathering a fleet
uses whatever the site already runs — `srun -w`, `pdsh`, `clush`.

Shipping remote execution would mean an ssh dependency and a second read-only
boundary to get right, for something every site already has. Running an
operator-supplied command template through `Env` would punch a hole in the one
place invariant §2.4 is enforced.

The same reasoning bounds what "multi-cluster" means (CLAUDE.md §6, "one
invocation, N clusters"). A `site.Set` loads every cluster's profile at once, so
everything that reads stored artifacts — `doctor`, `diff`, the context header —
works across all of them in one invocation. `context` needs a live scheduler and
so runs against the local cluster, and says so, rather than appearing to have
checked clusters it never touched. Reaching N schedulers live needs a daemon or
an ssh dependency, and §2.5 forecloses both.

## 8. The header is reserved out of the token budget

`cairn context` renders the profile above the timeline, and `renderText`
reserves it from the budget exactly as it reserves the capability section.

Both are the part that prevents a *wrong* answer rather than adding another
right one. Dropping the line that says "this is Slurm 23.02" to fit two more
journal events is a bad trade at any budget — and a report with no header
invites a reader to assume a stack, which is the failure the profile exists to
prevent.

For the same reason, a missing profile is stated rather than left silent, and a
profile whose scheduler probe never succeeded says so where the reader is
already looking.

## 9. Redaction is applied to profiles at the boundary

`redact.Redactor.Profile` exists so `cairn context --redact` cannot put back what
the bundle just removed. Cluster names are site-assigned, module and Spack roots
embed project names, and mount sources carry storage server addresses.

Building this surfaced a real gap in the redactor. `text()` only substitutes
identifiers it *learned* from a structured field, and a filesystem server's
address never appears in one — it appears only inside a free-form value, which
is precisely where every Lustre and NFS mount source puts it. So `--redact` on a
mount drift shipped `192.0.2.10@o2ib:/lustre` straight through. Addresses are
now pseudonymized structurally, the way home paths already were, with an octet
range check so a four-part driver version is not mistaken for a dotted quad.

Not redacted, deliberately: scheduler kind and version, distro, kernel, glibc,
driver and CUDA versions, GPU models, fabric rates. None identifies a site, and
all of them are the reason the header exists — pseudonymizing "slurm 23.02.7"
would leave a bundle nobody can reason about.
