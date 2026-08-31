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

echo "Verifying Phase 0 guards..."
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

echo
if [ "$fail" -ne 0 ]; then
	echo "$fail guard(s) did not fire. Fix them before trusting any of this."
	exit 1
fi
echo "All $pass guards fire correctly."
