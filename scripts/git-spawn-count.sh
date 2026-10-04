#!/bin/sh
# Counts the git processes a `go test` run starts, by putting a counting
# `git` shim first on PATH and asking it to record one line per call.
#
# Usage: scripts/git-spawn-count.sh [go test package args...]
#   scripts/git-spawn-count.sh ./internal/job/
#
# On a green run it prints the number of git processes that started, to
# stdout; test output itself goes to stderr. If the tests fail, the script
# exits non-zero and prints no count (set -eu).
set -eu

real_git=$(command -v git)

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

log="$tmp/calls.log"
: >"$log"

cat >"$tmp/git" <<EOF
#!/bin/sh
echo "\$0 \$*" >>"$log"
exec "$real_git" "\$@"
EOF
chmod +x "$tmp/git"

PATH="$tmp:$PATH" GOFLAGS=-buildvcs=false go test -count=1 "$@" 1>&2

wc -l <"$log" | tr -d ' '
