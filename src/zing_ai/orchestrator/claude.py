"""Claude CLI subprocess wrapper.

Provides sync helpers for invoking ``claude --print`` as subprocesses,
collecting output, and retrying on validation failures.
"""

from __future__ import annotations

import logging
import re
import subprocess
from collections.abc import Callable, Iterator

import jinja2

from zing_ai.orchestrator.config import CallType, ZingConfig, get_allowed_tools, get_model
from zing_ai.orchestrator.xml_parser import ValidationError

logger = logging.getLogger(__name__)

# Regex to capture a session ID from Claude CLI output.
# Claude typically emits a line like: "Session: <uuid>" on stderr.
_SESSION_ID_RE = re.compile(r"Session:\s*(\S+)")


def _build_command(
    prompt: str,
    *,
    call_type: CallType,
    config: ZingConfig,
    skip_permissions: bool = False,
    system_prompt: str | None = None,
    resume_session: str | None = None,
) -> list[str]:
    """Build the ``claude`` CLI command list."""
    cmd: list[str] = ["claude", "--print"]

    # Model selection
    model = get_model(config, call_type)
    cmd.extend(["--model", model])

    # Permissions / tools
    if skip_permissions:
        cmd.append("--dangerously-skip-permissions")
    else:
        tools = get_allowed_tools(config, call_type)
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
    cmd.extend(["--prompt", prompt])

    return cmd


def invoke_claude(
    prompt: str,
    *,
    call_type: CallType,
    config: ZingConfig,
    skip_permissions: bool = False,
    system_prompt: str | None = None,
    resume_session: str | None = None,
) -> Iterator[str]:
    """Invoke ``claude --print`` as a subprocess and yield stdout lines.

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
    str
        Lines of stdout output as they arrive.
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
    )

    assert proc.stdout is not None  # guaranteed by PIPE

    for raw_line in proc.stdout:
        line = raw_line.decode("utf-8", errors="replace")
        yield line

    # Wait for the process to finish and capture stderr for session ID
    proc.wait()

    if proc.stderr is not None:
        stderr_data = proc.stderr.read()
        stderr_text = stderr_data.decode("utf-8", errors="replace")
        if stderr_text:
            logger.debug("Claude stderr: %s", stderr_text)


def _extract_session_id(text: str) -> str:
    """Extract a session ID from combined output/stderr text.

    Returns an empty string if no session ID is found.
    """
    match = _SESSION_ID_RE.search(text)
    return match.group(1) if match else ""


def invoke_claude_full(
    prompt: str,
    **kwargs: object,
) -> tuple[str, str]:
    """Convenience wrapper that collects all output and returns ``(full_output, session_id)``.

    Accepts the same keyword arguments as :func:`invoke_claude`.
    """
    cmd = _build_command(prompt, **kwargs)  # type: ignore[arg-type]

    logger.debug("Running command (full): %s", cmd)

    result = subprocess.run(
        cmd,
        capture_output=True,
    )

    full_output = result.stdout.decode("utf-8", errors="replace")
    stderr_text = result.stderr.decode("utf-8", errors="replace")

    if stderr_text:
        logger.debug("Claude stderr: %s", stderr_text)

    session_id = _extract_session_id(stderr_text)
    if not session_id:
        # Also check stdout for session ID
        session_id = _extract_session_id(full_output)

    return full_output, session_id


def invoke_claude_validated[T](
    prompt: str,
    validator: Callable[[str], T],
    retry_prompt_template: jinja2.Template,
    *,
    max_retries: int = 3,
    on_retry: Callable[[int, str], None] | None = None,
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
    output, session_id = invoke_claude_full(prompt, **kwargs)

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
                **{**kwargs, "resume_session": session_id},  # type: ignore[arg-type]
            )

    # This should not be reached — the loop either returns or raises
    return validator(output)  # pragma: no cover
