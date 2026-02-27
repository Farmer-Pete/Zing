"""Claude CLI subprocess wrapper.

Provides sync helpers for invoking ``claude --print`` as subprocesses,
collecting output, and retrying on validation failures.
"""

from __future__ import annotations

import contextlib
import logging
import os
import re
import signal
import subprocess
import sys
import threading
import uuid
from collections.abc import Callable, Iterator
from contextlib import AbstractContextManager
from pathlib import Path

import jinja2

from zing_ai.orchestrator.config import CallType, ZingConfig, get_allowed_tools, get_model
from zing_ai.orchestrator.stream_parser import (
    collect_assistant_text,
    extract_session_id,
    format_event,
    parse_event,
)
from zing_ai.orchestrator.xml_parser import ValidationError

logger = logging.getLogger(__name__)

# Regex to capture a session ID from Claude CLI output.
# Claude typically emits a line like: "Session: <uuid>" on stderr.
_SESSION_ID_RE = re.compile(r"Session:\s*(\S+)")

# Instruction appended to the prompt when using a temp output file.
_OUTPUT_FILE_INSTRUCTION = (
    "\n\nIMPORTANT: Write your complete response to the file at {output_file}. "
    "Use the Write tool to create this file with your full response. "
    "Do not include the structured response in your conversation output."
)


def _build_command(
    prompt: str,
    *,
    call_type: CallType,
    config: ZingConfig,
    skip_permissions: bool = False,
    system_prompt: str | None = None,
    resume_session: str | None = None,
    output_file: str | None = None,
) -> list[str]:
    """Build the ``claude`` CLI command list."""
    cmd: list[str] = ["claude", "--print", "--output-format", "stream-json", "--verbose"]

    # Model selection
    model = get_model(config, call_type)
    cmd.extend(["--model", model])

    # Permissions / tools
    if skip_permissions:
        cmd.append("--dangerously-skip-permissions")
    else:
        tools = get_allowed_tools(config, call_type)
        # When output_file is set, ensure "Write" is in the allowed tools
        if output_file is not None and "Write" not in tools:
            tools = [*tools, "Write"]
        if tools:
            cmd.append("--allowedTools")
            cmd.extend(tools)

    # System prompt
    if system_prompt is not None:
        cmd.extend(["--system-prompt", system_prompt])

    # Resume session
    if resume_session is not None:
        cmd.extend(["--resume", resume_session])

    # The prompt itself
    cmd.extend(["--", prompt])

    return cmd


@contextlib.contextmanager
def invoke_claude(
    prompt: str,
    *,
    call_type: CallType,
    config: ZingConfig,
    skip_permissions: bool = False,
    system_prompt: str | None = None,
    resume_session: str | None = None,
) -> Iterator[Iterator[str]]:
    """Invoke ``claude --print`` as a subprocess, yielding a line iterator.

    Use as a context manager to ensure the subprocess is always cleaned up::

        with invoke_claude("hello", call_type=..., config=...) as lines:
            for line in lines:
                print(line)

    On exit (normal completion, ``break``, or exception) the subprocess is
    sent SIGTERM.  If it does not exit within 5 seconds it is sent SIGKILL.

    Parameters
    ----------
    prompt:
        The prompt text to send to Claude.
    call_type:
        Determines which model and allowed-tools set to use.
    config:
        The loaded :class:`ZingConfig`.
    skip_permissions:
        If ``True``, pass ``--dangerously-skip-permissions`` and omit
        ``--allowedTools``.
    system_prompt:
        Optional system prompt to pass via ``--system-prompt``.
    resume_session:
        If provided, pass ``--resume <session-id>`` to continue a
        previous conversation.

    Yields
    ------
    Iterator[str]
        An iterator of formatted human-readable output strings.
    """
    cmd = _build_command(
        prompt,
        call_type=call_type,
        config=config,
        skip_permissions=skip_permissions,
        system_prompt=system_prompt,
        resume_session=resume_session,
    )

    logger.debug("Running command: %s", cmd)

    proc = subprocess.Popen(
        cmd,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )

    assert proc.stdout is not None  # guaranteed by PIPE

    # Drain stderr in a background thread to prevent pipe-buffer deadlock.
    stderr_lines: list[str] = []

    def _drain_stderr() -> None:
        assert proc.stderr is not None
        for raw_line in proc.stderr:
            line = raw_line.decode("utf-8", errors="replace")
            stderr_lines.append(line)
            logger.debug("Claude stderr (live): %s", line.rstrip())

    stderr_thread = threading.Thread(target=_drain_stderr, daemon=True)
    stderr_thread.start()

    def _iter_lines() -> Iterator[str]:
        assert proc.stdout is not None
        for raw_line in proc.stdout:
            line = raw_line.decode("utf-8", errors="replace")
            event = parse_event(line)
            if event is None:
                continue
            formatted = format_event(event)
            if formatted is not None:
                yield formatted

    try:
        yield _iter_lines()
    finally:
        _kill_process_group(proc, signal.SIGTERM)
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            _kill_process_group(proc, signal.SIGKILL)
            proc.wait()
        stderr_thread.join(timeout=5)

        stderr_text = "".join(stderr_lines)
        if stderr_text:
            if proc.returncode != 0:
                logger.warning(
                    "Claude exited with code %d. stderr:\n%s",
                    proc.returncode,
                    stderr_text.rstrip(),
                )
            else:
                logger.debug("Claude stderr: %s", stderr_text)


def _extract_session_id(text: str) -> str:
    """Extract a session ID from combined output/stderr text.

    Returns an empty string if no session ID is found.
    """
    match = _SESSION_ID_RE.search(text)
    return match.group(1) if match else ""


def _kill_process_group(proc: subprocess.Popen[bytes], sig: int = signal.SIGTERM) -> None:
    """Send *sig* to the process group of *proc*.

    Falls back silently if the process has already exited.
    """
    try:
        os.killpg(os.getpgid(proc.pid), sig)
    except (ProcessLookupError, PermissionError, OSError):
        pass


def invoke_claude_full(
    prompt: str,
    *,
    on_output: Callable[[str], None] | None = None,
    zing_dir: Path | None = None,
    **kwargs: object,
) -> tuple[str, str]:
    """Convenience wrapper that collects all output and returns ``(full_output, session_id)``.

    Parses JSONL events from Claude's stdout, calls ``on_output`` with
    formatted display text, and collects the structured result either from
    a temp file (when *zing_dir* is provided) or from assistant text events.

    Parameters
    ----------
    on_output:
        Optional callback invoked with each formatted output string as it
        arrives.  Useful for streaming output to a terminal or TUI widget.
    zing_dir:
        When provided, creates a temp file in this directory and instructs
        Claude to write its structured response there.  When ``None``,
        the structured result is collected from assistant text events in
        the JSONL stream.
    **kwargs:
        Forwarded to :func:`_build_command`.
    """
    # Set up temp file if zing_dir is provided
    temp_path: Path | None = None
    output_file: str | None = None
    if zing_dir is not None:
        temp_path = zing_dir / f".tmp_{uuid.uuid4().hex}.txt"
        output_file = str(temp_path)
        prompt = prompt + _OUTPUT_FILE_INSTRUCTION.format(output_file=output_file)

    cmd = _build_command(prompt, output_file=output_file, **kwargs)  # type: ignore[arg-type]

    logger.debug("Running command (full): %s", cmd)

    proc = subprocess.Popen(
        cmd,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )

    # Install a SIGINT handler that kills the child's process group.
    # signal.signal() raises ValueError when called from a non-main thread
    # (e.g. Textual TUI worker threads), so we guard with try/except.
    interrupted = False
    original_handler: signal.Handlers | None = None
    try:

        def _sigint_handler(signum: int, frame: object) -> None:
            nonlocal interrupted
            interrupted = True
            _kill_process_group(proc, signal.SIGTERM)

            # Escalate to SIGKILL after 3 s if the group is still alive.
            def _escalate() -> None:
                try:
                    proc.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    _kill_process_group(proc, signal.SIGKILL)

            threading.Thread(target=_escalate, daemon=True).start()

        original_handler = signal.signal(signal.SIGINT, _sigint_handler)
    except ValueError:
        pass  # Not in main thread -- skip SIGINT handling

    exc_occurred = False
    try:
        assert proc.stdout is not None  # guaranteed by PIPE

        # Drain stderr in a background thread to prevent pipe-buffer deadlock.
        stderr_lines: list[str] = []

        def _drain_stderr() -> None:
            assert proc.stderr is not None
            for raw_line in proc.stderr:
                line = raw_line.decode("utf-8", errors="replace")
                stderr_lines.append(line)
                logger.debug("Claude stderr (live): %s", line.rstrip())

        stderr_thread = threading.Thread(target=_drain_stderr, daemon=True)
        stderr_thread.start()

        session_id = ""
        text_parts: list[str] = []

        for raw_line in proc.stdout:
            line = raw_line.decode("utf-8", errors="replace")
            event = parse_event(line)
            if event is None:
                continue

            # Extract session ID from init event
            sid = extract_session_id(event)
            if sid:
                session_id = sid

            # Collect assistant text for fallback output collection
            if temp_path is None:
                assistant_text = collect_assistant_text(event)
                if assistant_text:
                    text_parts.append(assistant_text)

            # Format and send to callback
            formatted = format_event(event)
            if formatted is not None:
                if on_output is not None:
                    on_output(formatted)
                else:
                    sys.stdout.write(formatted)
                    sys.stdout.flush()

        proc.wait()
        stderr_thread.join(timeout=5)
    except KeyboardInterrupt:
        interrupted = True
        exc_occurred = True
    except BaseException:
        exc_occurred = True
        raise
    finally:
        # Ensure subprocess is terminated on any exception
        if proc.poll() is None:
            _kill_process_group(proc, signal.SIGTERM)
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                _kill_process_group(proc, signal.SIGKILL)
                proc.wait()

        # Restore original SIGINT handler
        if original_handler is not None:
            with contextlib.suppress(ValueError):
                signal.signal(signal.SIGINT, original_handler)

        # Clean up temp file on any exception
        if exc_occurred and temp_path is not None:
            temp_path.unlink(missing_ok=True)

    if interrupted:
        raise KeyboardInterrupt

    # Collect the structured output
    if temp_path is not None:
        # Read from temp file
        if temp_path.is_file():
            full_output = temp_path.read_text(encoding="utf-8")
        else:
            logger.warning("Temp output file not found: %s", temp_path)
            full_output = ""
        temp_path.unlink(missing_ok=True)
    else:
        # Fallback: collect from assistant text events
        full_output = "".join(text_parts)

    # Fallback session ID extraction from stderr
    stderr_text = "".join(stderr_lines)
    if stderr_text:
        if proc.returncode != 0:
            logger.warning(
                "Claude exited with code %d. stderr:\n%s",
                proc.returncode,
                stderr_text.rstrip(),
            )
        else:
            logger.debug("Claude stderr: %s", stderr_text)

    if not session_id:
        session_id = _extract_session_id(stderr_text)
    if not session_id:
        session_id = _extract_session_id(full_output)

    return full_output, session_id


def print_line(line: str) -> None:
    """Print a line to stdout (no extra newline). Convenience ``on_output`` callback."""
    print(line, end="", flush=True)


def invoke_claude_validated[T](
    prompt: str,
    validator: Callable[[str], T],
    retry_prompt_template: jinja2.Template,
    *,
    max_retries: int = 3,
    on_retry: Callable[[int, str], None] | None = None,
    zing_dir: Path | None = None,
    **kwargs: object,
) -> T:
    """Invoke Claude, validate the output, and retry on :class:`ValidationError`.

    Parameters
    ----------
    prompt:
        The initial prompt text.
    validator:
        A callable that accepts the Claude output string and returns a
        parsed result of type *T*.  Should raise :class:`ValidationError`
        on invalid output.
    retry_prompt_template:
        A Jinja2 :class:`~jinja2.Template` rendered with ``error`` (the
        exception message) to produce the retry prompt.
    max_retries:
        Maximum number of retry attempts (default 3).
    on_retry:
        Optional sync callback invoked as ``on_retry(attempt, error_message)``
        before each retry (e.g. to send progress updates).
    zing_dir:
        When provided, passed to :func:`invoke_claude_full` to use temp
        file output collection.
    **kwargs:
        Forwarded to :func:`invoke_claude_full`.

    Returns
    -------
    T
        The validated result from *validator*.

    Raises
    ------
    ValidationError
        If validation still fails after *max_retries* attempts.
    """
    output, session_id = invoke_claude_full(prompt, zing_dir=zing_dir, **kwargs)

    for attempt in range(1, max_retries + 1):
        try:
            return validator(output)
        except ValidationError as exc:
            error_message = str(exc)
            logger.warning(
                "Validation failed (attempt %d/%d): %s",
                attempt,
                max_retries,
                error_message,
            )

            if attempt >= max_retries:
                raise

            if on_retry is not None:
                on_retry(attempt, error_message)

            retry_prompt = retry_prompt_template.render(error=error_message)

            # Resume the session so Claude has context of its previous response
            output, session_id = invoke_claude_full(
                retry_prompt,
                zing_dir=zing_dir,
                **{**kwargs, "resume_session": session_id},  # type: ignore[arg-type]
            )

    # This should not be reached -- the loop either returns or raises
    return validator(output)  # pragma: no cover
