#!/bin/sh
# Runs `go test -race` over the whole module, with internal/job split
# across several concurrent `go test` processes ("shards").
#
# Why: internal/job's tests spawn tens of thousands of git processes, and
# a race-instrumented binary forks slowly (each fork copies its large
# shadow address space). Go serializes forks within one process
# (syscall.ForkLock), so one internal/job process is bound by that lock no
# matter how many tests run in parallel. Each shard has its own lock.
# Every test still runs, under -race, exactly once.
#
#   go test -race -list ──► test names ──► round-robin ──► shard regexes
#        │
#   check: each name matches exactly one shard (go's own -list)
#        │
#   go test -race <every other package>          (alone, on a quiet machine)
#        │
#   shard 0 ─┐
#   shard 1 ─┼─ concurrent go test -race -run '^(A|B|...)$' ./internal/job
#   ...     ─┘
#
# The other packages run before the shards, not beside them: their timing
# tests (for example internal/response's 5 s bound on ExtractAll) are not
# written for a machine the shards saturate, and one failed that way.
#
# Usage: scripts/test-race.sh SHARDS TIMEOUT
set -eu

shards=$1
timeout=$2
pkg=zing/internal/job

tmp=$(mktemp -d "${TMPDIR:-/tmp}/test-race-XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# Every top-level test, example, and fuzz target `go test` runs by default.
# Benchmarks are left out: go test only runs them under -bench.
go test -race -list '.*' "$pkg" | grep -E '^(Test|Example|Fuzz)' >"$tmp/all"
if [ ! -s "$tmp/all" ]; then
	echo "test-race: no tests listed in $pkg" >&2
	exit 1
fi

# Shard i gets the names at positions i, i+shards, i+2*shards, ... Source
# order groups a file's heavy tests together, so round-robin spreads them.
awk -v n="$shards" -v dir="$tmp" '
	{ names[NR % n] = names[NR % n] (names[NR % n] == "" ? "" : "|") $0 }
	END { for (i = 0; i < n; i++) print "^(" names[i] ")$" > (dir "/regex." i) }
' "$tmp/all"

# Check, with go's own regexp matching: every listed name lands in exactly
# one shard. A name in no shard would silently never run; a name in two
# would run twice.
i=0
while [ "$i" -lt "$shards" ]; do
	go test -race -list "$(cat "$tmp/regex.$i")" "$pkg" | grep -E '^(Test|Example|Fuzz)' || true
	i=$((i + 1))
done | sort | uniq -c | awk '{ print $2, $1 }' >"$tmp/counts"
sort "$tmp/all" | while read -r name; do
	count=$(awk -v n="$name" '$1 == n { print $2 }' "$tmp/counts")
	if [ "${count:-0}" -ne 1 ]; then
		echo "test-race: $name matches ${count:-0} shards, want exactly 1" >&2
		exit 1
	fi
done

echo "=== every package but $pkg"
# shellcheck disable=SC2046 # word splitting on the package list is intended
go test -race -timeout "$timeout" $(go list ./... | grep -vx "$pkg")

# Run the shards concurrently, then report each one's output in turn so
# the logs never interleave.
pids=""
i=0
while [ "$i" -lt "$shards" ]; do
	go test -race -timeout "$timeout" -run "$(cat "$tmp/regex.$i")" "$pkg" >"$tmp/out.$i" 2>&1 &
	pids="$pids $!"
	i=$((i + 1))
done

failed=0
for pid in $pids; do
	wait "$pid" || failed=1
done

i=0
while [ "$i" -lt "$shards" ]; do
	echo "=== $pkg shard $((i + 1))/$shards"
	cat "$tmp/out.$i"
	i=$((i + 1))
done
exit "$failed"
