#!/usr/bin/env bash
# fake_codex.sh stands in for the real codex binary in internal/runtime's
# tests (design section 4.1, TASK 9). It records its argv, its stdin, and
# the environment it actually received to files under $FAKE_CODEX_DIR -- an
# env var the test sets and passes through RunRequest.Env, so it survives
# Codex's own environment filter -- then self-checks the host-isolation
# flags (D18) before doing anything else, so a Run that ever builds argv
# without them fails loudly here, not just in a Go assertion. The sandbox
# flag check accepts either read-only or danger-full-access (PKG9-PLAN.md
# section 4.6, D20: the judge job's own full-access mode), not just
# read-only. It then behaves per $FAKE_CODEX_MODE, so one script drives
# every process-lifecycle test, the same way fake_claude.sh does for
# Claude.
set -u

dir="${FAKE_CODEX_DIR:?FAKE_CODEX_DIR not set}"
mkdir -p "$dir"

printf '%s\n' "$@" > "$dir/argv"
cat > "$dir/stdin"
env > "$dir/env"

# One line appended per invocation, in every mode, so a test driving a
# retry (job.retryTransient calling rt.Run a second time on the same run)
# can count how many times this script actually ran, and transient_once
# below can tell its first call from its second.
calls_file="$dir/calls"
call_n=1
if [ -f "$calls_file" ]; then
  call_n=$(($(wc -l < "$calls_file") + 1))
fi
echo "$call_n" >> "$calls_file"

# ---- self-check the host-isolation flags (design section 4.1, D18) --------

has_ignore_user_config=false
has_ignore_rules=false
is_resume=false
outfile=""
prev=""
have_sandbox_config=false
have_sandbox_flag=false

for a in "$@"; do
  case "$a" in
  --ignore-user-config) has_ignore_user_config=true ;;
  --ignore-rules) has_ignore_rules=true ;;
  resume) is_resume=true ;;
  esac
  if [ "$prev" = "-o" ]; then
    outfile="$a"
  fi
  if [ "$prev" = "-c" ] && { [ "$a" = 'sandbox_mode="read-only"' ] || [ "$a" = 'sandbox_mode="danger-full-access"' ]; }; then
    have_sandbox_config=true
  fi
  if [ "$prev" = "-s" ] && { [ "$a" = "read-only" ] || [ "$a" = "danger-full-access" ]; }; then
    have_sandbox_flag=true
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
  echo 'fake_codex: -c sandbox_mode="read-only" or "danger-full-access" missing from resume argv' >&2
  exit 9
fi
if ! $is_resume && ! $have_sandbox_flag; then
  echo "fake_codex: -s read-only or danger-full-access missing from first-turn argv" >&2
  exit 9
fi
if [ -z "$outfile" ]; then
  echo "fake_codex: -o missing from argv" >&2
  exit 9
fi

# Record the -o file's mode, and its containing directory's mode, while the
# process runs, per the test's own stat -- Run must have created the
# directory at 0700 and the file at 0600 before ever starting this script.
# GNU stat (-c) goes first: on Linux, BSD's -f flag also succeeds but prints
# filesystem status, not the mode. On macOS -c fails and -f takes over.
stat_mode() {
  stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1" 2>/dev/null
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
  if [ -n "${FAKE_CODEX_STDERR_FILE:-}" ]; then
    cat "$FAKE_CODEX_STDERR_FILE" >&2
  fi
  exit 0
  ;;
exit_nonzero)
  exit "${FAKE_CODEX_EXIT_CODE:-3}"
  ;;
error_event)
  # A run that dies within seconds with an error event on stdout, no
  # stderr, and no -o content (the ticket's repro, "Codex runs that exit 1
  # within seconds leave no stderr, transcript, or cause"): thread.started
  # so a session id is still known, then an error event and a matching
  # turn.failed carrying the same message in its error.message, the two
  # shapes codexFailureDetail reads. The -o file is left exactly as Run
  # created it (empty), so FinalMessage stays "".
  msg="${FAKE_CODEX_ERROR_MESSAGE:-unexpected status 400 Bad Request: model not supported}"
  printf '{"type":"thread.started","thread_id":"fake-codex-error-thread-id"}\n'
  printf '{"type":"error","message":"%s"}\n' "$msg"
  printf '{"type":"turn.failed","error":{"message":"%s"}}\n' "$msg"
  exit 1
  ;;
transient_once)
  # The retry repro (design goal: a Codex failure matching a transient
  # pattern is retried once on the same run): the first call sleeps 1s --
  # long enough that a test can tell it apart from the second call's own
  # near-instant return, proving retryTransient actually re-ran the
  # process rather than just reusing the first result -- then fails with a
  # 503 event, no stderr, and no -o content, the same repro shape
  # error_event uses. Every call after the first succeeds normally.
  if [ "$call_n" = "1" ]; then
    sleep 1
    printf '{"type":"thread.started","thread_id":"fake-codex-transient-thread-id"}\n'
    printf '{"type":"error","message":"unexpected status 503 Service Unavailable"}\n'
    printf '{"type":"turn.failed","error":{"message":"unexpected status 503 Service Unavailable"}}\n'
    exit 1
  fi
  printf '%s\n' "$default_events"
  printf '%s' "$default_result" > "$outfile"
  exit 0
  ;;
plain_stdout_error)
  # A run that exits 1 with plain text on stdout -- no JSON event at all --
  # so codexFailureDetail falls back to its last-lines reading (fromEvent
  # false) even when that text itself contains a transient-looking number
  # such as 503: Codex.run must only call codexTransientMatch when
  # codexFailureDetail's match came from an actual error or turn.failed
  # event, never from this fallback.
  msg="${FAKE_CODEX_ERROR_MESSAGE:-plain line mentioning 503 with no event}"
  printf '%s\n' "$msg"
  exit 1
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
command_rejected)
  # Codex's command policy refuses a command (#94): a declined
  # command_execution item carrying "are not permitted" in its aggregated
  # output, then a turn.failed with its own, different message. The -o
  # file is left exactly as Run created it (empty), so FinalMessage stays
  # "".
  printf '{"type":"thread.started","thread_id":"fake-codex-rejected-thread-id"}\n'
  printf '{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"rm -f s2.json","aggregated_output":"rm -f style commands are not permitted. Use a safer approach","exit_code":1,"status":"declined"}}\n'
  printf '{"type":"turn.failed","error":{"message":"judge could not run its checks"}}\n'
  exit 1
  ;;
command_rejected_long_stream)
  # Same rejection as command_rejected, but followed by more than 64 KiB of
  # filler lines before turn.failed: the declined item falls out of
  # RunResult.Stdout (maxCodexStdoutBytes, the last 64 KiB) while
  # codexCommandRejection still finds it because Codex.run passes it the
  # head capWriter (up to 4 MiB), not the tail (#94 review finding r2f2).
  printf '{"type":"thread.started","thread_id":"fake-codex-rejected-thread-id"}\n'
  printf '{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"rm -f s2.json","aggregated_output":"rm -f style commands are not permitted. Use a safer approach","exit_code":1,"status":"declined"}}\n'
  filler=$(head -c 70000 /dev/zero | tr '\0' 'x')
  printf '{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"%s"}}\n' "$filler"
  printf '{"type":"turn.failed","error":{"message":"judge could not run its checks"}}\n'
  exit 1
  ;;
*)
  echo "fake_codex: unknown FAKE_CODEX_MODE $mode_flag" >&2
  exit 9
  ;;
esac
