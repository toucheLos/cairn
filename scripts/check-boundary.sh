#!/usr/bin/env bash
# Assert that no observed incident is present in this repository.
#
# CLAUDE.md §3: raw producer output, hand-redacted output, and the event streams
# derived from them never reach GitHub. Only the taxonomy built from them and the
# accuracy measured over them are published — §9's "publish the methodology, not
# the data".
#
# This is the third of three guards, and the only one that runs *on GitHub*:
#
#   .gitignore          covers the ordinary case; `git add -f` steps over it
#   scripts/pre-commit  covers a forced add; `--no-verify` steps over it
#   this                covers both, after the fact, where it cannot be skipped
#
# It inspects tracked files rather than the working tree. What matters is what
# landed, and an untracked private corpus sitting beside the checkout is the
# expected state on a machine doing the capture work.

set -uo pipefail
cd "$(dirname "$0")/.."

fail=0

# 1. The private corpus directory must not be tracked.
tracked_corpus=$(git ls-files -- 'corpus/*' 'corpus' 2>/dev/null || true)
if [ -n "$tracked_corpus" ]; then
	printf '  FAIL  the private corpus is committed to this repository:\n'
	printf '          %s\n' $tracked_corpus
	fail=1
else
	printf '  ok    the private corpus is not tracked\n'
fi

# 2. No tracked fixture may declare itself observed.
#
# Checked by content rather than by location: a real incident committed under
# some directory nobody thought to exclude is the case a path-based rule misses.
observed=""
for meta in $(git ls-files -- '*meta.yaml' 2>/dev/null || true); do
	[ -f "$meta" ] || continue
	if grep -qE '^synthetic:[[:space:]]*false' "$meta"; then
		observed="$observed $meta"
	fi
done
if [ -n "$observed" ]; then
	printf '  FAIL  observed fixtures are committed to this repository:\n'
	printf '          %s\n' $observed
	fail=1
else
	printf '  ok    no tracked fixture declares itself observed\n'
fi

if [ "$fail" -ne 0 ]; then
	cat >&2 <<'MSG'

An observed incident has reached this repository.

This is not fixable by deleting the file in a new commit: the content is in the
history and, if it has been pushed, on GitHub's servers. Treat it as a
disclosure — rewrite the history, force-push, and assume the material was seen.

Observed incidents belong in corpus/, which is gitignored and refused by
scripts/pre-commit. Install that hook with `make install-hooks`.
MSG
	exit 1
fi
