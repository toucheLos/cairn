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

echo
if [ "$fail" -ne 0 ]; then
	echo "$fail guard(s) did not fire. Fix them before trusting any of this."
	exit 1
fi
echo "All $pass guards fire correctly."
