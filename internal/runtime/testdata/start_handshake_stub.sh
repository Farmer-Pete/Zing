#!/bin/sh
set -eu
dir="${STUB_DIR:?STUB_DIR not set}"
: > "$dir/started"

outfile=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then
    outfile="$arg"
  fi
  prev="$arg"
done

stdin="$(cat; printf x)"
stdin=${stdin%?}
: > "$dir/stdin_done"
if [ -e "$dir/onstart_done" ]; then
  : > "$dir/ordered"
fi
if [ -z "$stdin" ]; then
  : > "$dir/no_work"
  exit 0
fi

if [ -n "$outfile" ]; then
  printf '%s' '<zing job="classify" outcome="bug"><reason>ok</reason></zing>' > "$outfile"
else
  printf '%s' '{"result":"<zing job=\"classify\" outcome=\"bug\"><reason>ok</reason></zing>"}'
fi
