"""Tests for the Claude CLI subprocess wrapper."""

from __future__ import annotations

import asyncio
from unittest.mock import AsyncMock, patch

import jinja2
import pytest

from zing_ai.orchestrator.claude import (
    _build_command,
    _extract_session_id,
    invoke_claude,
    invoke_claude_full,
    invoke_claude_validated,
)
from zing_ai.orchestrator.config import (
    DEFAULT_MCP_TOOLS,
    CallType,
    ZingConfig,
    get_allowed_tools,
)
from zing_ai.orchestrator.xml_parser import ValidationError

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _make_mock_process(
    stdout_lines: list[bytes] | None = None,
    stderr: bytes = b"",
    returncode: int = 0,
) -> AsyncMock:
    """Create a mock asyncio subprocess with configurable stdout/stderr."""
    proc = AsyncMock()
    proc.returncode = returncode

    if stdout_lines is not None:
        stdout_mock = AsyncMock()
        stdout_mock.read = AsyncMock(return_value=b"".join(stdout_lines))

        # Make it async-iterable for `async for line in proc.stdout`
        async def _stdout_iter():  # type: ignore[return]
            for line in stdout_lines:
                yield line

        stdout_mock.__aiter__ = lambda self: _stdout_iter()
        proc.stdout = stdout_mock
    else:
        proc.stdout = None

    stderr_mock = AsyncMock()
    stderr_mock.read = AsyncMock(return_value=stderr)
    proc.stderr = stderr_mock

    # communicate() returns (stdout, stderr)
    stdout_bytes = b"".join(stdout_lines) if stdout_lines else b""
    proc.communicate = AsyncMock(return_value=(stdout_bytes, stderr))

    proc.wait = AsyncMock(return_value=returncode)

    return proc


def _run(coro):  # type: ignore[no-untyped-def]
    """Run an async coroutine synchronously."""
    return asyncio.run(coro)


# ---------------------------------------------------------------------------
# _build_command tests
# ---------------------------------------------------------------------------


class TestBuildCommand:
    """Tests for the internal _build_command function."""

    def test_basic_command_structure(self) -> None:
        config = ZingConfig()
        cmd = _build_command("hello", call_type=CallType.BUILD, config=config)
        assert cmd[0] == "claude"
        assert cmd[1] == "--print"
        assert "--prompt" in cmd
        assert cmd[cmd.index("--prompt") + 1] == "hello"

    def test_model_flag_from_config(self) -> None:
        config = ZingConfig()
        cmd = _build_command("test", call_type=CallType.BUILD, config=config)
        assert "--model" in cmd
        idx = cmd.index("--model")
        assert cmd[idx + 1] == "sonnet"

    def test_model_flag_investigate(self) -> None:
        config = ZingConfig()
        cmd = _build_command("test", call_type=CallType.INVESTIGATE, config=config)
        idx = cmd.index("--model")
        assert cmd[idx + 1] == "opus"

    def test_model_flag_custom(self) -> None:
        config = ZingConfig()
        config.models[CallType.BUILD] = "haiku"
        cmd = _build_command("test", call_type=CallType.BUILD, config=config)
        idx = cmd.index("--model")
        assert cmd[idx + 1] == "haiku"

    def test_allowed_tools_for_build(self) -> None:
        config = ZingConfig()
        cmd = _build_command("test", call_type=CallType.BUILD, config=config)
        assert "--allowedTools" in cmd
        idx = cmd.index("--allowedTools")
        # Everything between --allowedTools and the next flag (--prompt) is a tool name
        prompt_idx = cmd.index("--prompt")
        tools_in_cmd = cmd[idx + 1 : prompt_idx]
        expected = get_allowed_tools(config, CallType.BUILD)
        assert tools_in_cmd == expected

    def test_allowed_tools_for_investigate(self) -> None:
        """investigate has no extra_tools by default, but still has mcp_tools."""
        config = ZingConfig()
        cmd = _build_command("test", call_type=CallType.INVESTIGATE, config=config)
        assert "--allowedTools" in cmd
        idx = cmd.index("--allowedTools")
        prompt_idx = cmd.index("--prompt")
        tools_in_cmd = cmd[idx + 1 : prompt_idx]
        assert tools_in_cmd == list(DEFAULT_MCP_TOOLS)

    def test_allowed_tools_merged_correctly(self) -> None:
        """MCP tools and extra tools are merged into a flat list."""
        config = ZingConfig(mcp_tools=["mcp__custom__*"])
        config.extra_tools[CallType.AUDIT] = ["Read", "Grep"]
        cmd = _build_command("test", call_type=CallType.AUDIT, config=config)
        idx = cmd.index("--allowedTools")
        prompt_idx = cmd.index("--prompt")
        tools_in_cmd = cmd[idx + 1 : prompt_idx]
        assert tools_in_cmd == ["mcp__custom__*", "Read", "Grep"]

    def test_skip_permissions_flag(self) -> None:
        config = ZingConfig()
        cmd = _build_command(
            "test", call_type=CallType.BUILD, config=config, skip_permissions=True
        )
        assert "--dangerously-skip-permissions" in cmd

    def test_skip_permissions_omits_allowed_tools(self) -> None:
        config = ZingConfig()
        cmd = _build_command(
            "test", call_type=CallType.BUILD, config=config, skip_permissions=True
        )
        assert "--allowedTools" not in cmd

    def test_no_skip_permissions_by_default(self) -> None:
        config = ZingConfig()
        cmd = _build_command("test", call_type=CallType.BUILD, config=config)
        assert "--dangerously-skip-permissions" not in cmd
        assert "--allowedTools" in cmd

    def test_system_prompt_flag(self) -> None:
        config = ZingConfig()
        cmd = _build_command(
            "test",
            call_type=CallType.BUILD,
            config=config,
            system_prompt="You are helpful.",
        )
        assert "--system-prompt" in cmd
        idx = cmd.index("--system-prompt")
        assert cmd[idx + 1] == "You are helpful."

    def test_no_system_prompt_by_default(self) -> None:
        config = ZingConfig()
        cmd = _build_command("test", call_type=CallType.BUILD, config=config)
        assert "--system-prompt" not in cmd

    def test_resume_session_flag(self) -> None:
        config = ZingConfig()
        cmd = _build_command(
            "test",
            call_type=CallType.BUILD,
            config=config,
            resume_session="abc-123",
        )
        assert "--resume" in cmd
        idx = cmd.index("--resume")
        assert cmd[idx + 1] == "abc-123"

    def test_no_resume_by_default(self) -> None:
        config = ZingConfig()
        cmd = _build_command("test", call_type=CallType.BUILD, config=config)
        assert "--resume" not in cmd

    def test_all_call_types_produce_valid_command(self) -> None:
        """Every call type should produce a command with claude, --print, --model, --prompt."""
        config = ZingConfig()
        for ct in CallType:
            cmd = _build_command("test", call_type=ct, config=config)
            assert cmd[0] == "claude"
            assert "--print" in cmd
            assert "--model" in cmd
            assert "--prompt" in cmd
            assert "--allowedTools" in cmd


# ---------------------------------------------------------------------------
# _extract_session_id tests
# ---------------------------------------------------------------------------


class TestExtractSessionId:
    """Tests for session ID extraction from Claude output."""

    def test_extract_from_stderr(self) -> None:
        text = "Session: abc-def-123\nSome other output"
        assert _extract_session_id(text) == "abc-def-123"

    def test_extract_uuid_style(self) -> None:
        text = "Session: 550e8400-e29b-41d4-a716-446655440000\n"
        assert _extract_session_id(text) == "550e8400-e29b-41d4-a716-446655440000"

    def test_no_session_id(self) -> None:
        assert _extract_session_id("no session here") == ""

    def test_empty_string(self) -> None:
        assert _extract_session_id("") == ""


# ---------------------------------------------------------------------------
# invoke_claude tests
# ---------------------------------------------------------------------------


class TestInvokeClaude:
    """Tests for the async streaming invoke_claude function."""

    def test_yields_stdout_lines(self) -> None:
        async def _test() -> list[str]:
            mock_proc = _make_mock_process(
                stdout_lines=[b"line 1\n", b"line 2\n"],
                stderr=b"Session: sess-001\n",
            )

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.return_value = mock_proc
                config = ZingConfig()

                lines: list[str] = []
                async for line in invoke_claude(
                    "hello", call_type=CallType.BUILD, config=config
                ):
                    lines.append(line)

            return lines

        assert _run(_test()) == ["line 1\n", "line 2\n"]

    def test_passes_correct_command(self) -> None:
        async def _test() -> list[object]:
            mock_proc = _make_mock_process(stdout_lines=[], stderr=b"")

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.return_value = mock_proc
                config = ZingConfig()

                async for _ in invoke_claude(
                    "test prompt", call_type=CallType.INVESTIGATE, config=config
                ):
                    pass

                return list(mock_exec.call_args[0])

        cmd = _run(_test())
        assert cmd[0] == "claude"
        assert "--print" in cmd
        assert "--model" in cmd
        assert "--prompt" in cmd
        assert "test prompt" in cmd

    def test_skip_permissions(self) -> None:
        async def _test() -> list[object]:
            mock_proc = _make_mock_process(stdout_lines=[], stderr=b"")

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.return_value = mock_proc
                config = ZingConfig()

                async for _ in invoke_claude(
                    "test",
                    call_type=CallType.BUILD,
                    config=config,
                    skip_permissions=True,
                ):
                    pass

                return list(mock_exec.call_args[0])

        cmd = _run(_test())
        assert "--dangerously-skip-permissions" in cmd
        assert "--allowedTools" not in cmd

    def test_resume_session(self) -> None:
        async def _test() -> list[object]:
            mock_proc = _make_mock_process(stdout_lines=[], stderr=b"")

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.return_value = mock_proc
                config = ZingConfig()

                async for _ in invoke_claude(
                    "continue",
                    call_type=CallType.BUILD,
                    config=config,
                    resume_session="sess-xyz",
                ):
                    pass

                return list(mock_exec.call_args[0])

        cmd = _run(_test())
        assert "--resume" in cmd
        idx = cmd.index("--resume")
        assert cmd[idx + 1] == "sess-xyz"


# ---------------------------------------------------------------------------
# invoke_claude_full tests
# ---------------------------------------------------------------------------


class TestInvokeClaudeFull:
    """Tests for the convenience invoke_claude_full wrapper."""

    def test_returns_full_output(self) -> None:
        async def _test() -> tuple[str, str]:
            mock_proc = _make_mock_process(
                stdout_lines=[b"Hello ", b"world\n"],
                stderr=b"",
            )

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.return_value = mock_proc

                return await invoke_claude_full(
                    "test", call_type=CallType.BUILD, config=ZingConfig()
                )

        output, session_id = _run(_test())
        assert output == "Hello world\n"

    def test_returns_session_id_from_stderr(self) -> None:
        async def _test() -> tuple[str, str]:
            mock_proc = _make_mock_process(
                stdout_lines=[b"output\n"],
                stderr=b"Session: sess-abc-123\n",
            )

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.return_value = mock_proc

                return await invoke_claude_full(
                    "test", call_type=CallType.BUILD, config=ZingConfig()
                )

        output, session_id = _run(_test())
        assert session_id == "sess-abc-123"

    def test_returns_session_id_from_stdout(self) -> None:
        async def _test() -> tuple[str, str]:
            mock_proc = _make_mock_process(
                stdout_lines=[b"Session: sess-in-stdout\nother output\n"],
                stderr=b"",
            )

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.return_value = mock_proc

                return await invoke_claude_full(
                    "test", call_type=CallType.BUILD, config=ZingConfig()
                )

        output, session_id = _run(_test())
        assert session_id == "sess-in-stdout"

    def test_empty_session_id_when_not_present(self) -> None:
        async def _test() -> tuple[str, str]:
            mock_proc = _make_mock_process(
                stdout_lines=[b"just output\n"],
                stderr=b"no session here\n",
            )

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.return_value = mock_proc

                return await invoke_claude_full(
                    "test", call_type=CallType.BUILD, config=ZingConfig()
                )

        output, session_id = _run(_test())
        assert session_id == ""

    def test_passes_kwargs_to_build_command(self) -> None:
        async def _test() -> list[object]:
            mock_proc = _make_mock_process(stdout_lines=[], stderr=b"")

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.return_value = mock_proc

                await invoke_claude_full(
                    "test",
                    call_type=CallType.AUDIT,
                    config=ZingConfig(),
                    skip_permissions=True,
                    system_prompt="be helpful",
                    resume_session="sess-999",
                )

                return list(mock_exec.call_args[0])

        cmd = _run(_test())
        assert "--dangerously-skip-permissions" in cmd
        assert "--allowedTools" not in cmd
        assert "--system-prompt" in cmd
        assert "--resume" in cmd
        idx = cmd.index("--resume")
        assert cmd[idx + 1] == "sess-999"


# ---------------------------------------------------------------------------
# invoke_claude_validated tests
# ---------------------------------------------------------------------------


class TestInvokeClaudeValidated:
    """Tests for the retry-on-validation invoke_claude_validated wrapper."""

    def test_returns_on_first_success(self) -> None:
        async def _test() -> str:
            mock_proc = _make_mock_process(
                stdout_lines=[b"valid output\n"],
                stderr=b"Session: sess-001\n",
            )

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.return_value = mock_proc

                return await invoke_claude_validated(
                    "test",
                    validator=lambda x: x.strip().upper(),
                    retry_prompt_template=jinja2.Template("Fix: {{ error }}"),
                    call_type=CallType.BUILD,
                    config=ZingConfig(),
                )

        assert _run(_test()) == "VALID OUTPUT"

    def test_retries_on_validation_error(self) -> None:
        async def _test() -> tuple[str, int]:
            mock_proc_bad = _make_mock_process(
                stdout_lines=[b"bad output\n"],
                stderr=b"Session: sess-001\n",
            )
            mock_proc_good = _make_mock_process(
                stdout_lines=[b"good output\n"],
                stderr=b"Session: sess-002\n",
            )

            def validator(text: str) -> str:
                if "bad" in text:
                    raise ValidationError("output was bad")
                return text.strip()

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.side_effect = [mock_proc_bad, mock_proc_good]

                result = await invoke_claude_validated(
                    "test",
                    validator=validator,
                    retry_prompt_template=jinja2.Template(
                        "Fix this error: {{ error }}"
                    ),
                    call_type=CallType.BUILD,
                    config=ZingConfig(),
                )

                return result, mock_exec.call_count

        result, call_count = _run(_test())
        assert result == "good output"
        assert call_count == 2

    def test_raises_after_max_retries(self) -> None:
        async def _test() -> int:
            mock_proc = _make_mock_process(
                stdout_lines=[b"always bad\n"],
                stderr=b"Session: sess-001\n",
            )

            def validator(text: str) -> str:
                raise ValidationError("still bad")

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.return_value = mock_proc

                with pytest.raises(ValidationError, match="still bad"):
                    await invoke_claude_validated(
                        "test",
                        validator=validator,
                        retry_prompt_template=jinja2.Template("Retry: {{ error }}"),
                        max_retries=3,
                        call_type=CallType.BUILD,
                        config=ZingConfig(),
                    )

                return mock_exec.call_count

        # 1 initial call + 2 retry calls = 3 total
        assert _run(_test()) == 3

    def test_calls_on_retry_callback(self) -> None:
        async def _test() -> tuple[str, AsyncMock]:
            mock_proc = _make_mock_process(
                stdout_lines=[b"bad\n"],
                stderr=b"Session: sess-001\n",
            )
            mock_proc_good = _make_mock_process(
                stdout_lines=[b"good\n"],
                stderr=b"Session: sess-002\n",
            )

            call_count = 0

            def validator(text: str) -> str:
                nonlocal call_count
                call_count += 1
                if call_count <= 1:
                    raise ValidationError("bad output")
                return text.strip()

            on_retry = AsyncMock()

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.side_effect = [mock_proc, mock_proc_good]

                result = await invoke_claude_validated(
                    "test",
                    validator=validator,
                    retry_prompt_template=jinja2.Template("Fix: {{ error }}"),
                    on_retry=on_retry,
                    call_type=CallType.BUILD,
                    config=ZingConfig(),
                )

            return result, on_retry

        result, on_retry = _run(_test())
        assert result == "good"
        on_retry.assert_called_once_with(1, "bad output")

    def test_retry_uses_resume_session(self) -> None:
        """The retry call should pass --resume with the session ID from the first call."""

        async def _test() -> list[list[object]]:
            mock_proc_bad = _make_mock_process(
                stdout_lines=[b"bad\n"],
                stderr=b"Session: sess-original\n",
            )
            mock_proc_good = _make_mock_process(
                stdout_lines=[b"good\n"],
                stderr=b"Session: sess-retry\n",
            )

            call_count = 0

            def validator(text: str) -> str:
                nonlocal call_count
                call_count += 1
                if call_count <= 1:
                    raise ValidationError("bad")
                return text.strip()

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.side_effect = [mock_proc_bad, mock_proc_good]

                await invoke_claude_validated(
                    "test",
                    validator=validator,
                    retry_prompt_template=jinja2.Template("Fix: {{ error }}"),
                    call_type=CallType.BUILD,
                    config=ZingConfig(),
                )

                return [list(call[0]) for call in mock_exec.call_args_list]

        call_cmds = _run(_test())
        # Second call should have --resume flag with the session from first call
        second_call_cmd = call_cmds[1]
        assert "--resume" in second_call_cmd
        idx = second_call_cmd.index("--resume")
        assert second_call_cmd[idx + 1] == "sess-original"

    def test_retry_prompt_rendered_with_error(self) -> None:
        """The retry prompt should be rendered with the error message."""

        async def _test() -> list[list[object]]:
            mock_proc_bad = _make_mock_process(
                stdout_lines=[b"bad\n"],
                stderr=b"Session: sess-001\n",
            )
            mock_proc_good = _make_mock_process(
                stdout_lines=[b"good\n"],
                stderr=b"Session: sess-002\n",
            )

            call_count = 0

            def validator(text: str) -> str:
                nonlocal call_count
                call_count += 1
                if call_count <= 1:
                    raise ValidationError("missing <plan> element")
                return text.strip()

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.side_effect = [mock_proc_bad, mock_proc_good]

                await invoke_claude_validated(
                    "test",
                    validator=validator,
                    retry_prompt_template=jinja2.Template(
                        "Your output had an error: {{ error }}. Please fix it."
                    ),
                    call_type=CallType.BUILD,
                    config=ZingConfig(),
                )

                return [list(call[0]) for call in mock_exec.call_args_list]

        call_cmds = _run(_test())
        second_call_cmd = call_cmds[1]
        prompt_idx = second_call_cmd.index("--prompt")
        retry_prompt = second_call_cmd[prompt_idx + 1]
        assert "missing <plan> element" in retry_prompt
        assert "Please fix it." in retry_prompt

    def test_max_retries_one(self) -> None:
        """With max_retries=1, should fail immediately on first validation error."""

        async def _test() -> int:
            mock_proc = _make_mock_process(
                stdout_lines=[b"bad\n"],
                stderr=b"Session: sess-001\n",
            )

            def validator(text: str) -> str:
                raise ValidationError("always fails")

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.return_value = mock_proc

                with pytest.raises(ValidationError, match="always fails"):
                    await invoke_claude_validated(
                        "test",
                        validator=validator,
                        retry_prompt_template=jinja2.Template("Fix: {{ error }}"),
                        max_retries=1,
                        call_type=CallType.BUILD,
                        config=ZingConfig(),
                    )

                return mock_exec.call_count

        # Only one call — no retries with max_retries=1
        assert _run(_test()) == 1

    def test_no_on_retry_callback(self) -> None:
        """When on_retry is None, retries still work without callback."""

        async def _test() -> str:
            mock_proc_bad = _make_mock_process(
                stdout_lines=[b"bad\n"],
                stderr=b"Session: sess-001\n",
            )
            mock_proc_good = _make_mock_process(
                stdout_lines=[b"good\n"],
                stderr=b"Session: sess-002\n",
            )

            call_count = 0

            def validator(text: str) -> str:
                nonlocal call_count
                call_count += 1
                if call_count <= 1:
                    raise ValidationError("bad")
                return text.strip()

            with patch(
                "zing_ai.orchestrator.claude.asyncio.create_subprocess_exec"
            ) as mock_exec:
                mock_exec.side_effect = [mock_proc_bad, mock_proc_good]

                return await invoke_claude_validated(
                    "test",
                    validator=validator,
                    retry_prompt_template=jinja2.Template("Fix: {{ error }}"),
                    on_retry=None,
                    call_type=CallType.BUILD,
                    config=ZingConfig(),
                )

        assert _run(_test()) == "good"
