#!/usr/bin/env bash
# fake_claude_stop_hook.sh stands in for the real claude binary in
# TestStopHook_FakeClaudeBlocksThenAccepts. Like internal/runtime's own
# fake_claude.sh, it records its argv, its stdin, and its environment under
# $FAKE_CLAUDE_DIR, then it extracts the Stop hook command from its own
# --settings argument -- the exact JSON Claude.Command builds -- and runs
# that command, through a shell, once per $FAKE_CLAUDE_DIR/hook_in_N.json
# fixture, in order starting at N=1, feeding each one to the hook on
# stdin and saving its stdout to hook_out_N.
#
# At the first hook_out_N that does not contain "decision":"block", the
# hook let the turn end: this script prints result_N.json (the claude
# result JSON for that turn) and exits 0, simulating what the real CLI
# would send after a Stop hook stops blocking. If every fixture blocks, it
# exits 8, so the test can tell that case apart from a successful run.
set -euo pipefail

dir="${FAKE_CLAUDE_DIR:?FAKE_CLAUDE_DIR not set}"
mkdir -p "$dir"

printf '%s\n' "$@" > "$dir/argv"
cat > "$dir/stdin"
env > "$dir/env"

settings=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--settings" ]; then
    settings="$arg"
  fi
  prev="$arg"
done

# The hook command sits between "command":" and the next double quote.
# stopHookSettings shell-quotes every value inside it with single quotes,
# so the command text itself never contains a literal double quote.
rest="${settings#*\"command\":\"}"
cmd="${rest%%\"*}"

i=1
while [ -f "$dir/hook_in_$i.json" ]; do
  out=$(sh -c "$cmd" < "$dir/hook_in_$i.json")
  printf '%s' "$out" > "$dir/hook_out_$i"
  case "$out" in
  *'"decision":"block"'*) ;;
  *)
    cat "$dir/result_$i.json"
    exit 0
    ;;
  esac
  i=$((i + 1))
done

exit 8
