#!/bin/sh
# Counts invocations of the literal `git` name that land on PATH during a
# `go test` run, by putting a counting `git` shim first on PATH and asking
# it to record one line per call. This is not a raw OS-level process/exec
# count:
#
#   - Because this script's shim directory is first on PATH, code under
#     test that calls exec.LookPath("git") (gitbin.Path(), for one) never
#     sees /usr/bin/git here, so on darwin the xcrun trampoline's extra
#     exec is never on the path this script measures either way, and it
#     cannot show task 5's fix for that trampoline.
#   - It cannot see any subprocess git forks internally, for example a
#     git-remote-* helper.
#
# Usage: scripts/git-spawn-count.sh [go test package args...]
#   scripts/git-spawn-count.sh ./internal/job/
#
# On a green run it prints the number of git processes that started, to
# stdout; test output itself goes to stderr. If the tests fail, the script
# exits non-zero and prints no count (set -eu).
set -eu

real_git=$(command -v git)

tmp=$(mktemp -d "${TMPDIR:-/tmp}/git-spawn-count-XXXXXX")
trap 'rm -rf "$tmp"' EXIT

log="$tmp/calls.log"
: >"$log"

cat >"$tmp/git" <<EOF
#!/bin/sh
printf '%s\n' x >>"$log"
exec "$real_git" "\$@"
EOF
chmod +x "$tmp/git"

PATH="$tmp:$PATH" GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false" go test -count=1 "$@" 1>&2

wc -l <"$log" | tr -d ' '
