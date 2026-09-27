#!/usr/bin/env bash
# fake_claude.sh stands in for the real claude binary in internal/runtime's
# tests (design section 4.1, TASK 5). It records its argv, its stdin, and
# the environment it actually received to files under $FAKE_CLAUDE_DIR --
# an env var the test sets and passes through RunRequest.Env, so it
# survives Claude's own environment filter -- then behaves per
# $FAKE_CLAUDE_MODE, so one script drives every process-lifecycle test.
set -u

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
sleep)
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
*)
  echo "fake_claude: unknown FAKE_CLAUDE_MODE $mode" >&2
  exit 9
  ;;
esac
