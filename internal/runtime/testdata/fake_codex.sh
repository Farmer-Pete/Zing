#!/usr/bin/env bash
# fake_codex.sh stands in for the real codex binary in internal/runtime's
# tests (design section 4.1, TASK 9). It records its argv, its stdin, and
# the environment it actually received to files under $FAKE_CODEX_DIR -- an
# env var the test sets and passes through RunRequest.Env, so it survives
# Codex's own environment filter -- then self-checks the host-isolation
# flags (D18) before doing anything else, so a Run that ever builds argv
# without them fails loudly here, not just in a Go assertion. It then
# behaves per $FAKE_CODEX_MODE, so one script drives every process-lifecycle
# test, the same way fake_claude.sh does for Claude.
set -u

dir="${FAKE_CODEX_DIR:?FAKE_CODEX_DIR not set}"
mkdir -p "$dir"

printf '%s\n' "$@" > "$dir/argv"
cat > "$dir/stdin"
env > "$dir/env"

# ---- self-check the host-isolation flags (design section 4.1, D18) --------

has_ignore_user_config=false
has_ignore_rules=false
is_resume=false
outfile=""
prev=""
have_sandbox_config=false

for a in "$@"; do
  case "$a" in
  --ignore-user-config) has_ignore_user_config=true ;;
  --ignore-rules) has_ignore_rules=true ;;
  resume) is_resume=true ;;
  esac
  if [ "$prev" = "-o" ]; then
    outfile="$a"
  fi
  if [ "$prev" = "-c" ] && [ "$a" = 'sandbox_mode="read-only"' ]; then
    have_sandbox_config=true
  fi
  prev="$a"
done

if ! $has_ignore_user_config; then
  echo "fake_codex: --ignore-user-config missing from argv" >&2
  exit 9
fi
if ! $has_ignore_rules; then
  echo "fake_codex: --ignore-rules missing from argv" >&2
  exit 9
fi
if $is_resume && ! $have_sandbox_config; then
  echo 'fake_codex: -c sandbox_mode="read-only" missing from resume argv' >&2
  exit 9
fi
if [ -z "$outfile" ]; then
  echo "fake_codex: -o missing from argv" >&2
  exit 9
fi

# Record the -o file's mode, and its containing directory's mode, while the
# process runs, per the test's own stat -- Run must have created the
# directory at 0700 and the file at 0600 before ever starting this script.
stat_mode() {
  stat -f %Lp "$1" 2>/dev/null || stat -c %a "$1" 2>/dev/null
}
printf '%s' "$(stat_mode "$outfile")" > "$dir/o_mode"
printf '%s' "$(stat_mode "$(dirname "$outfile")")" > "$dir/o_dir_mode"

mode_flag="${FAKE_CODEX_MODE:-success}"

# default_events is a valid thread.started + turn.completed stream (design
# section 4.1: the first event carries thread_id), with a deliberately
# different <zing> document embedded in its agent_message item than the
# default -o content below, so a test that never overrides either file can
# still tell the two apart and confirm the -o file's document is the one
# Run returns, not stdout's.
default_events='{"type":"thread.started","thread_id":"fake-codex-default-thread-id"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"<zing job=\"planreview\" outcome=\"ok\"></zing>"}}
{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}'

# default_result is a valid planreview/ok document with one finding, so it is
# never byte-identical to default_events' embedded (zero-finding) document.
default_result='<zing job="planreview" outcome="ok"><finding lens="quality" severity="nit" location="plan/objective"><text>ok</text><fix>ok</fix></finding></zing>'

case "$mode_flag" in
success)
  if [ -n "${FAKE_CODEX_EVENTS_FILE:-}" ]; then
    cat "$FAKE_CODEX_EVENTS_FILE"
  else
    printf '%s\n' "$default_events"
  fi
  if [ -n "${FAKE_CODEX_RESULT_FILE:-}" ]; then
    cat "$FAKE_CODEX_RESULT_FILE" > "$outfile"
  else
    printf '%s' "$default_result" > "$outfile"
  fi
  exit 0
  ;;
exit_nonzero)
  exit "${FAKE_CODEX_EXIT_CODE:-3}"
  ;;
sleep)
  # A grandchild relative to the Go test process: this script (already the
  # direct child exec.CommandContext started) backgrounds sleep, inheriting
  # stdout, so only a whole-process-group kill (configureProcessGroup) frees
  # the stdout pipe fast; killing just this script's own pid would leave
  # sleep holding it open and cmd.Wait would block on WaitDelay instead.
  sleep "${FAKE_CODEX_SLEEP_SECONDS:-5}" &
  child=$!
  wait "$child"
  exit 0
  ;;
big_output)
  printf '%s\n' "$default_events"
  head -c 6291456 /dev/zero | tr '\0' 'a' > "$outfile"
  exit 0
  ;;
no_output_file)
  # Codex exits 0 and streams a normal event log, but the -o file Run
  # pre-created is gone by the time Run reads it back (F026): readCapped's
  # os.Open then fails for a reason other than ErrOutputTooLarge, which Run
  # must map to *InvalidOutputError, not a bare wrapped error. Removing the
  # file, rather than just leaving it empty, is what actually reaches
  # readCapped's error branch -- an empty file already decodes as the
  # typed "no zing element" InvalidOutputError the unmodified code produces.
  printf '%s\n' "$default_events"
  rm -f "$outfile"
  exit 0
  ;;
big_stderr)
  printf '%s\n' "$default_events"
  printf '%s' "$default_result" > "$outfile"
  head -c 8388608 /dev/zero | tr '\0' 'e' >&2
  exit 0
  ;;
signal_kill)
  kill -TERM "$$"
  sleep 5
  ;;
*)
  echo "fake_codex: unknown FAKE_CODEX_MODE $mode_flag" >&2
  exit 9
  ;;
esac
