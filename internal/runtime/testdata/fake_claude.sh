#!/usr/bin/env bash
# fake_claude.sh stands in for the real claude binary in internal/runtime's
# tests (design section 4.1, TASK 5). It records its argv, its stdin, and
# the environment it actually received to files under $FAKE_CLAUDE_DIR --
# an env var the test sets and passes through RunRequest.Env, so it
# survives Claude's own environment filter -- then behaves per
# $FAKE_CLAUDE_MODE, so one script drives every process-lifecycle test.
#
# set -euo pipefail so a failed setup step (a missing FAKE_CLAUDE_RESULT_FILE,
# a mkdir or a recording write that fails) surfaces as a nonzero exit instead
# of a silent exit 0 that leaves Go's Run reporting a misleading empty-output
# error. The big_stdout/big_stderr branches keep their explicit exit 0: Go
# fully drains both pipes (capWriter never short-reads), so their head|tr
# pipelines complete cleanly and never trip pipefail.
set -euo pipefail

dir="${FAKE_CLAUDE_DIR:?FAKE_CLAUDE_DIR not set}"
mkdir -p "$dir"

printf '%s\n' "$@" > "$dir/argv"
cat > "$dir/stdin"
env > "$dir/env"

mode="${FAKE_CLAUDE_MODE:-success}"

# default_result is a valid classify/bug document, so a test that does not
# care about final-message parsing (it is only checking argv, stdin, or
# env) still gets a clean Run with no error to thread through.
default_result='{"type":"result","subtype":"success","is_error":false,"result":"<zing job=\"classify\" outcome=\"bug\"><reason>ok</reason></zing>","session_id":"unused","num_turns":1,"duration_ms":1}'

case "$mode" in
success)
  if [ -n "${FAKE_CLAUDE_RESULT_FILE:-}" ]; then
    cat "$FAKE_CLAUDE_RESULT_FILE"
  else
    printf '%s' "$default_result"
  fi
  exit 0
  ;;
exit_nonzero)
  exit "${FAKE_CLAUDE_EXIT_CODE:-3}"
  ;;
result_then_exit)
  if [ -n "${FAKE_CLAUDE_RESULT_FILE:-}" ]; then
    cat "$FAKE_CLAUDE_RESULT_FILE"
  else
    printf '%s' "$default_result"
  fi
  exit "${FAKE_CLAUDE_EXIT_CODE:-1}"
  ;;
sleep)
  sleep "${FAKE_CLAUDE_SLEEP_SECONDS:-5}"
  exit 0
  ;;
event_then_sleep)
  transcript="${FAKE_CLAUDE_TRANSCRIPT:?FAKE_CLAUDE_TRANSCRIPT not set}"
  mkdir -p "$(dirname "$transcript")"
  printf '%s\n' '{"type":"assistant"}' >> "$transcript"
  sleep "${FAKE_CLAUDE_SLEEP_SECONDS:-30}"
  exit 0
  ;;
events_then_result)
  transcript="${FAKE_CLAUDE_TRANSCRIPT:?FAKE_CLAUDE_TRANSCRIPT not set}"
  mkdir -p "$(dirname "$transcript")"
  events="${FAKE_CLAUDE_EVENTS:-8}"
  gap="${FAKE_CLAUDE_EVENT_GAP_SECONDS:-0.3}"
  i=0
  while [ "$i" -lt "$events" ]; do
    printf '%s\n' '{"type":"assistant"}' >> "$transcript"
    sleep "$gap"
    i=$((i + 1))
  done
  printf '%s' "$default_result"
  exit 0
  ;;
partial_then_sleep)
  printf '%s' "${FAKE_CLAUDE_PARTIAL_OUTPUT:-partial output}"
  sleep "${FAKE_CLAUDE_SLEEP_SECONDS:-5}"
  exit 0
  ;;
big_stdout)
  head -c 6291456 /dev/zero | tr '\0' 'a'
  exit 0
  ;;
big_stderr)
  printf '%s' "$default_result"
  head -c 8388608 /dev/zero | tr '\0' 'e' >&2
  exit 0
  ;;
signal_kill)
  kill -TERM "$$"
  sleep 5
  ;;
fork_delay_write)
  # Forks a grandchild, in this same process group, that would write the
  # canary file two seconds from now, then exits immediately with a clean
  # result. The grandchild's own stdout/stderr are redirected to /dev/null
  # rather than left inherited: Go's os/exec Wait already waits for its own
  # stdout/stderr pipes to reach EOF, so an inherited copy of that pipe's
  # write end would make Wait block on the grandchild itself, defeating the
  # very race this test means to prove (Run's kill runs after Wait returns,
  # not instead of it). TestClaudeKillsGroupAfterExit proves that kill reaps
  # this grandchild before it ever runs.
  canary="${FAKE_CLAUDE_CANARY:?FAKE_CLAUDE_CANARY not set}"
  ( sleep 2; touch "$canary" ) >/dev/null 2>&1 &
  # The grandchild's pid, so the test can poll for its death instead of
  # waiting a fixed time (review F031).
  printf '%s' "$!" > "$dir/grandchild_pid"
  disown
  printf '%s' "$default_result"
  exit 0
  ;;
*)
  echo "fake_claude: unknown FAKE_CLAUDE_MODE $mode" >&2
  exit 9
  ;;
esac
