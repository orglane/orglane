#!/bin/sh
# Runs one shard of a Go test package: every count-th test, sorted by name.
#
#   tests/shard.sh <package> <shard> <count> [go test flags...]
#
# `go test` has no shard flag (and the tests must keep their -race and -skip
# flags per tier), so this lists the package's tests and selects them with
# -run. The list comes from the package itself: a new test joins a shard
# automatically instead of silently never running. Shards are 1-based and
# deterministic; sorted round-robin balances the test count, not measured
# durations.
#
# GO_TEST_TAGS (default: integration) is the tag used to list the package's
# tests; pass the same -tags value in the go test flags.
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
if [ "$shard" -lt 1 ] || [ "$shard" -gt "$count" ]; then
	echo "error: shard $shard is not in 1..$count" >&2
	exit 2
fi

tags=${GO_TEST_TAGS:-integration}
# Check the listing itself: a compile error must not look like "no tests",
# and its output is the useful part of the failure.
listing=$(go test -tags "$tags" -list '^Test' "$pkg") || {
	echo "error: listing the tests of $pkg failed" >&2
	exit 1
}
names=$(printf '%s\n' "$listing" | grep '^Test' | sort || true)
# GO_TEST_ONLY narrows the shard to a regex of test names (the heavy Docker
# tests of a package, for example), so both tiers shard their own tests.
if [ -n "${GO_TEST_ONLY:-}" ]; then
	names=$(printf '%s\n' "$names" | grep -E "$GO_TEST_ONLY" || true)
fi
if [ -z "$names" ]; then
	echo "error: $pkg has no tests to shard" >&2
	exit 1
fi

selected=$(printf '%s\n' "$names" | awk -v n="$count" -v i="$shard" '(NR - 1) % n == (i - 1) % n')
if [ -z "$selected" ]; then
	# An empty shard must not pass silently: the caller expects coverage.
	echo "error: shard $shard of $count selected no tests in $pkg" >&2
	exit 1
fi

pattern=$(printf '%s\n' "$selected" | paste -sd'|' -)
exec go test "$@" -run "^($pattern)\$" "$pkg"
