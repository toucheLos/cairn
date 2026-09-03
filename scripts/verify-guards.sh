#!/usr/bin/env bash
# Prove that each Phase 0 guard actually fails when violated.
#
# Every check below deliberately breaks something and asserts that the relevant
# command exits nonzero. A guard that has never been observed to fail is a guard
# nobody has verified — and all four of these exist to catch mistakes that are
# permanent once pushed.
#
# Every mutation is reverted, including on failure or interrupt.

set -uo pipefail
cd "$(dirname "$0")/.."

BACKUP=$(mktemp -d)
trap 'restore; rm -rf "$BACKUP"' EXIT INT TERM

FILES=()

stash() {
	local f="$1"
	local dest="$BACKUP/$(echo "$f" | tr '/' '_')"
	cp "$f" "$dest"
	FILES+=("$f")
}

restore() {
	local f dest
	for f in "${FILES[@]:-}"; do
		[ -z "$f" ] && continue
		dest="$BACKUP/$(echo "$f" | tr '/' '_')"
		[ -f "$dest" ] && cp "$dest" "$f"
	done
}

pass=0
fail=0

# expect_fail NAME COMMAND...
expect_fail() {
	local name="$1"; shift
	if "$@" >/dev/null 2>&1; then
		printf '  FAIL  %s\n        the guard did NOT fire; it is not protecting anything\n' "$name"
		fail=$((fail + 1))
	else
		printf '  ok    %s\n' "$name"
		pass=$((pass + 1))
	fi
}

echo "Verifying cairn's guards..."
echo

# 1. The class enum is append-only. Removing a member must fail CI, because the
#    wire format is the string and stored bundles depend on it.
stash schema/testdata/classes.golden
sed -i '1d' schema/testdata/classes.golden
expect_fail "class enum removal is rejected" go test ./schema -run TestClassesGolden
restore

# 2. detail must stay bounded. An unregistered key is the first step toward a
#    free-text log field, which would break both §2.3 and redaction.
stash fixtures/001-oom-cgroup/expected/events.json
sed -i 's/"killed_comm":"python3"/"raw_log_line":"kernel: blah"/' fixtures/001-oom-cgroup/expected/events.json
expect_fail "unregistered detail key is rejected" go test ./fixtures -run TestLoadAll
restore

# 3. The redaction scanner must catch unredacted material. This is the guard
#    whose failure is irreversible once a commit is pushed.
stash fixtures/001-oom-cgroup/input/sacct.txt
printf 'NodeName=compute042.hpc.some-university.edu\n' >> fixtures/001-oom-cgroup/input/sacct.txt
expect_fail "unredacted hostname is caught" go run ./redact/scan/cmd/scan-fixtures fixtures
restore

# 4. Canonical form is part of the contract. A golden file that is merely valid
#    JSON would start failing spuriously the first time a collector reorders.
stash fixtures/002-walltime-exceeded/expected/events.json
python3 - <<'PY'
p = 'fixtures/002-walltime-exceeded/expected/events.json'
s = open(p).read().replace('[\n  {', '[{', 1)
open(p, 'w').write(s)
PY
expect_fail "non-canonical golden file is rejected" go test ./fixtures -run TestExpectedFilesAreCanonical
restore

# 5. Synthetic fixtures must not silently become eval data.
stash fixtures/003-gpu-driver-mismatch/meta.yaml
sed -i 's/^synthetic: true$/synthetic: false/' fixtures/003-gpu-driver-mismatch/meta.yaml
expect_fail "unattributed 'real' fixture is rejected" go test ./fixtures -run TestLoadAll
restore

# 6. The redaction boundary. This is the guard whose failure ships a real
#    hostname to someone outside the site, so it is verified rather than assumed:
#    the check plants an identifier and confirms the redactor removes it.
if go test ./redact -run 'TestNoOriginalSurvives|TestRedactedBundlePassesTheScanner' >/dev/null 2>&1; then
	printf '  ok    redaction removes every identifier and passes the scanner\n'
	pass=$((pass + 1))
else
	printf '  FAIL  redaction round trip\n'
	fail=$((fail + 1))
fi

# 7. Invariant §2.5: one static binary. A dynamically linked build works fine on
#    the machine that produced it and fails on the login node it was built for,
#    which is the only place it matters.
BIN=$(mktemp -u)
if CGO_ENABLED=0 go build -trimpath -o "$BIN" ./cmd/cairn 2>/dev/null; then
	if file "$BIN" | grep -q "statically linked"; then
		printf '  ok    the shipped binary is statically linked\n'
		pass=$((pass + 1))
	else
		printf '  FAIL  the binary is not statically linked: %s\n' "$(file "$BIN")"
		fail=$((fail + 1))
	fi
	rm -f "$BIN"
else
	printf '  FAIL  the binary does not build\n'
	fail=$((fail + 1))
fi

# 8. The shipped binary links no third-party code at all.
#
#    Not a stylistic preference. cairn is aimed at sites that will read what they
#    deploy — defense, pharma, air-gapped facilities — and every module in the
#    binary is something they have to review. yaml.v3 is a real dependency of the
#    fixture loader, but that is the test harness and must never reach the
#    binary. This check is what keeps that true.
THIRD_PARTY=$(go list -deps ./cmd/cairn \
	| grep '\.' \
	| grep -v '^github.com/touchelos/cairn' \
	| grep -v '^crypto/internal/' \
	|| true)
if [ -z "$THIRD_PARTY" ]; then
	printf '  ok    the shipped binary links no third-party code\n'
	pass=$((pass + 1))
else
	printf '  FAIL  the binary now links third-party code:\n'
	printf '          %s\n' $THIRD_PARTY
	fail=$((fail + 1))
fi

# 9. site.yaml decoding rejects a key nobody registered.
#
#    This file is hand-edited by design (CLAUDE.md §6: admins correct a generated
#    file), so a typo must fail loudly. Ignoring `partitons:` would leave the file
#    saying one thing and the tool doing another — which is the exact failure the
#    site profile exists to prevent, reintroduced one silent line at a time.
stash site/testdata/slurm-lmod-gpu.golden.yaml
sed -i 's/^  partitions:/  partitons:/' site/testdata/slurm-lmod-gpu.golden.yaml
expect_fail "site.yaml rejects an unknown key" \
	go test ./site -run TestGoldensDecode
restore

# 10. A site profile from a newer cairn is refused, not half-read.
#
#     Same argument as schema/DESIGN.md §8 makes for bundles: a version mismatch
#     must be loud. A profile read with fields silently missing would produce a
#     context header that is confidently wrong about the scheduler.
stash site/testdata/cpu-only.golden.yaml
sed -i 's/^version: 1/version: 99/' site/testdata/cpu-only.golden.yaml
expect_fail "a future site profile version is refused" \
	go test ./site -run TestGoldensDecode
restore

# 11. An observed incident must never be committable.
#
#     CLAUDE.md §3, and the one guard here whose failure cannot be undone by a
#     follow-up commit: once pushed, the material is on GitHub's servers and in
#     the history. Three layers protect it and this exercises the outermost one,
#     which is the layer that still works after `git add -f` and `--no-verify`.
mkdir -p fixtures/999-guard-probe
printf 'id: 999-guard-probe\nsynthetic: false\n' > fixtures/999-guard-probe/meta.yaml
git add -f fixtures/999-guard-probe/meta.yaml >/dev/null 2>&1
expect_fail "an observed fixture in the repo is caught" ./scripts/check-boundary.sh
git rm -q --cached fixtures/999-guard-probe/meta.yaml >/dev/null 2>&1 || true
rm -rf fixtures/999-guard-probe

# 12. The public corpus refuses to load an observed fixture.
#
#     Belt to guard 11's braces, and at a different layer: this one fails the
#     test run rather than the commit, so a real incident dropped into fixtures/
#     is caught by anyone who types `go test` before they ever reach git.
#     The redaction provenance is filled in as well as the flag flipped. Without
#     that this only re-tests guard 5: Validate rejects an observed fixture with
#     no redacted_by long before the boundary rule is reached, so the guard would
#     pass while proving nothing about the rule it names.
stash fixtures/003-gpu-driver-mismatch/meta.yaml
sed -i -e 's/^synthetic: true$/synthetic: false/' \
	-e 's/^redacted_by: ""$/redacted_by: "guard probe"/' \
	-e 's/^redaction_method: ""$/redaction_method: "guard probe"/' \
	fixtures/003-gpu-driver-mismatch/meta.yaml
expect_fail "the public corpus refuses an observed fixture" \
	go test ./fixtures -run TestPublicCorpusIsSynthetic
restore

# 13. The shipped binary can perform no actuation at all.
#
#     CLAUDE.md §6: the policy engine is built and tested against a null action
#     set, and only once it is proven correct do the three reversible actuations
#     exist. policy/doc.go calls that ordering "easy to get backwards", so this
#     makes it a fact about the binary rather than a claim in a comment. Adding
#     an actuation must fail here until someone deliberately changes this guard.
stash policy/action.go
sed -i 's|^var shippedActions = \[\]ActionSpec{}$|var shippedActions = []ActionSpec{{Kind: "guard.probe", Undo: "n/a"}}|' policy/action.go
expect_fail "the shipped action set is empty" go test ./policy -run TestNullActionSet
restore

# 14. Default-deny survives.
#
#     Deliberately exercised against an action the engine *does* implement, so it
#     cannot pass merely because nothing exists — that is the failure mode of a
#     test written against a null set.
stash policy/engine.go
sed -i 's|if !e.policy.Allows(req.Kind) {|if false {|' policy/engine.go
expect_fail "an unlisted action is denied" go test ./policy -run TestDefaultDenyWithKnownAction
restore

# 15. No audit record, no action.
#
#     The one ordering here that cannot be retrofitted: an actuation that
#     happened with no record of it is only discoverable by noticing the damage.
stash policy/engine.go
sed -i 's|if err := e.audit.Record(d); err != nil {|if err := e.audit.Record(d); false {|' policy/engine.go
expect_fail "an unwritable audit log denies the action" go test ./policy -run TestAuditFailureDenies
restore

# 16. An empty scope is not every target.
#
#     One character between "this policy names no nodes" and "this policy names
#     all of them".
stash policy/config.go
sed -i 's|^func matchAny(patterns \[\]string, v string) bool {|func matchAny(patterns []string, v string) bool {\n\tif len(patterns) == 0 { return true }|' policy/config.go
expect_fail "an empty scope authorizes nothing" go test ./policy -run TestEmptyScopeIsNotEveryTarget
restore

# 17. The policy package has no route to the cluster.
#
#     With no actuations there is nothing to execute, and keeping the dependency
#     absent means the absence is checkable rather than asserted. If policy ever
#     imports collectors, that is the commit to look at.
POLICY_DEPS=$(go list -deps ./policy | grep -E 'touchelos/cairn/(collectors|site|join)' || true)
if [ -z "$POLICY_DEPS" ]; then
	printf '  ok    the policy package cannot reach the cluster\n'
	pass=$((pass + 1))
else
	printf '  FAIL  the policy package now depends on:\n'
	printf '          %s\n' $POLICY_DEPS
	fail=$((fail + 1))
fi

echo
if [ "$fail" -ne 0 ]; then
	echo "$fail guard(s) did not fire. Fix them before trusting any of this."
	exit 1
fi
echo "All $pass guards fire correctly."
