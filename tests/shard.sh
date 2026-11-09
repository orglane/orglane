#!/bin/sh
# Runs one shard of a Go test package: every count-th test, sorted by name.
#
#   tests/shard.sh <package> <shard> <count> [go test flags...]
#
# `go test` has no shard flag (and the tests must keep their -race and -skip
# flags per tier), so this lists the package's tests with the same build tags
# the run uses and selects one shard with tests/ci (which also has the
# regression tests for the selection). A shard that would run zero tests is a
# failure, never a green no-op: names are filtered by the -skip expression
# before sharding, and the execution is checked for at least one `=== RUN`.
#
# GO_TEST_ONLY narrows the shard to a regex of test names (the heavy Docker
# tests of a package, for example), so both tiers shard their own tests.
set -eu

if [ "$#" -lt 3 ]; then
	echo "usage: $0 <package> <shard> <count> [go test flags...]" >&2
	exit 2
fi

pkg=$1
shard=$2
count=$3
shift 3

case $shard in
'' | *[!0-9]*) echo "error: shard $shard is not a number" >&2; exit 2 ;;
esac
case $count in
'' | *[!0-9]*) echo "error: count $count is not a number" >&2; exit 2 ;;
esac

# Work from the repository root: the package and helper paths are relative.
root=$(unset CDPATH; cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

# Discovery must use the same build tags as execution; a tag mismatch would
# otherwise select tests the run cannot see and go test would exit 0 with
# "no tests to run". -skip is applied to the discovered names for the same
# reason: a shard whose every test the caller excludes must fail.
tags=${GO_TEST_TAGS:-integration}
skip=
prev=
for arg in "$@"; do
	case $arg in
	-tags=*) tags=${arg#-tags=} ;;
	-skip=*) skip=${arg#-skip=} ;;
	-test.skip=*) skip=${arg#-test.skip=} ;;
	esac
	case $prev in
	-tags) tags=$arg ;;
	-skip | -test.skip) skip=$arg ;;
	esac
	prev=$arg
done

# Check the listing itself: a compile error must not look like "no tests",
# and its output is the useful part of the failure.
listing=$(go test -tags "$tags" -list '^Test' "$pkg") || {
	echo "error: listing the tests of $pkg failed" >&2
	exit 1
}
names=$(printf '%s\n' "$listing" | grep '^Test' || true)
if [ -z "$names" ]; then
	echo "error: $pkg has no tests to shard" >&2
	exit 1
fi

selected=$(printf '%s\n' "$names" | go run ./tests/ci shard -shard "$shard" -shards "$count" -only "${GO_TEST_ONLY:-}" -skip "$skip")
if [ -z "$selected" ]; then
	echo "error: shard $shard of $count selected no tests in $pkg" >&2
	exit 1
fi

pattern=$(printf '%s\n' "$selected" | paste -sd'|' -)
log=$(mktemp)
status_file=$(mktemp)
trap 'rm -f "$log" "$status_file"' EXIT INT TERM

# -v is required to see the execution events: go test exits 0 when -run
# matches nothing, which must not pass as a shard. The status is captured
# from go test itself (the pipeline's status is tee's).
set +e
{ go test -v "$@" -run "^($pattern)\$" "$pkg" 2>&1; echo $? > "$status_file"; } | tee "$log"
set -e
status=$(cat "$status_file" 2>/dev/null || echo 1)
if [ "$status" -ne 0 ]; then
	exit "$status"
fi
if ! grep -q '^=== RUN   Test' "$log"; then
	echo "error: shard $shard of $count ran no tests in $pkg" >&2
	exit 1
fi
