"""Tests for the Claude CLI subprocess wrapper."""

from __future__ import annotations

import json
import signal
from pathlib import Path
from unittest.mock import MagicMock, patch

import jinja2
import pytest

from zing_ai.orchestrator.claude import (
    _build_command,
    _extract_session_id,
    invoke_claude,
    invoke_claude_full,
    invoke_claude_validated,
    print_line,
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


def _jsonl_line(event: dict) -> bytes:
    """Encode an event dict as a JSONL line (bytes with trailing newline)."""
    return (json.dumps(event) + "\n").encode()


def _make_init_event(session_id: str = "sess-001") -> dict:
    """Create a system/init event."""
    return {"type": "system", "subtype": "init", "session_id": session_id}


def _make_text_event(text: str) -> dict:
    """Create an assistant text event."""
    return {
        "type": "assistant",
        "message": {"content": [{"type": "text", "text": text}]},
    }


def _make_tool_use_event(name: str) -> dict:
    """Create an assistant tool_use event."""
    return {
        "type": "assistant",
        "message": {"content": [{"type": "tool_use", "name": name, "input": {}}]},
    }


def _make_result_event(duration: float = 5.0, cost: float = 0.01) -> dict:
    """Create a result/success event."""
    return {
        "type": "result",
        "subtype": "success",
        "duration_seconds": duration,
        "total_cost_usd": cost,
    }


def _make_mock_popen(
    stdout_lines: list[bytes] | None = None,
    stderr: bytes = b"",
    returncode: int = 0,
) -> MagicMock:
    """Create a mock subprocess.Popen with configurable stdout/stderr."""
    proc = MagicMock()
    proc.returncode = returncode
    proc.pid = 12345

    if stdout_lines is not None:
        proc.stdout = iter(stdout_lines)
    else:
        proc.stdout = iter([])

    # stderr must be iterable (line-by-line) for the background drain thread,
    # and also support .read() for any legacy code paths.
    stderr_lines = stderr.split(b"\n")
    # Reconstruct lines with trailing newlines (matching real pipe behavior),
    # dropping the empty trailing element from split.
    stderr_line_list = [line + b"\n" for line in stderr_lines if line]
    stderr_mock = MagicMock()
    stderr_mock.read.return_value = stderr
    stderr_mock.__iter__ = lambda self: iter(stderr_line_list)
    proc.stderr = stderr_mock

    proc.wait.return_value = returncode

    return proc


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
        assert "--output-format" in cmd
        idx = cmd.index("--output-format")
        assert cmd[idx + 1] == "stream-json"
        assert "--verbose" in cmd
        assert "--" in cmd
        assert cmd[-1] == "hello"

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
        # Everything between --allowedTools and the -- separator is a tool name
        prompt_idx = cmd.index("--")
        tools_in_cmd = cmd[idx + 1 : prompt_idx]
        expected = get_allowed_tools(config, CallType.BUILD)
        assert tools_in_cmd == expected

    def test_allowed_tools_for_investigate(self) -> None:
        """investigate has no extra_tools by default, but still has mcp_tools."""
        config = ZingConfig()
        cmd = _build_command("test", call_type=CallType.INVESTIGATE, config=config)
        assert "--allowedTools" in cmd
        idx = cmd.index("--allowedTools")
        separator_idx = cmd.index("--")
        tools_in_cmd = cmd[idx + 1 : separator_idx]
        assert tools_in_cmd == list(DEFAULT_MCP_TOOLS)

    def test_allowed_tools_merged_correctly(self) -> None:
        """MCP tools and extra tools are merged into a flat list."""
        config = ZingConfig(mcp_tools=["mcp__custom__*"])
        config.extra_tools[CallType.AUDIT] = ["Read", "Grep"]
        cmd = _build_command("test", call_type=CallType.AUDIT, config=config)
        idx = cmd.index("--allowedTools")
        separator_idx = cmd.index("--")
        tools_in_cmd = cmd[idx + 1 : separator_idx]
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
        """Every call type should produce a command with claude, --print, --model, -- separator."""
        config = ZingConfig()
        for ct in CallType:
            cmd = _build_command("test", call_type=ct, config=config)
            assert cmd[0] == "claude"
            assert "--print" in cmd
            assert "--output-format" in cmd
            assert "--verbose" in cmd
            assert "--model" in cmd
            assert "--" in cmd
            assert cmd[-1] == "test"
            assert "--allowedTools" in cmd

    def test_output_file_adds_write_tool(self) -> None:
        """When output_file is set, Write should be added to allowed tools."""
        config = ZingConfig()
        # INVESTIGATE has no extra_tools by default, so Write is not present
        cmd = _build_command(
            "test",
            call_type=CallType.INVESTIGATE,
            config=config,
            output_file="/tmp/out.txt",
        )
        idx = cmd.index("--allowedTools")
        separator_idx = cmd.index("--")
        tools_in_cmd = cmd[idx + 1 : separator_idx]
        assert "Write" in tools_in_cmd

    def test_output_file_does_not_duplicate_write(self) -> None:
        """When output_file is set but Write is already in tools, don't duplicate."""
        config = ZingConfig()
        # BUILD already has Write in extra_tools
        cmd = _build_command(
            "test",
            call_type=CallType.BUILD,
            config=config,
            output_file="/tmp/out.txt",
        )
        idx = cmd.index("--allowedTools")
        separator_idx = cmd.index("--")
        tools_in_cmd = cmd[idx + 1 : separator_idx]
        assert tools_in_cmd.count("Write") == 1

    def test_output_file_skip_permissions_no_tools(self) -> None:
        """When skip_permissions is True, output_file doesn't add allowed tools."""
        config = ZingConfig()
        cmd = _build_command(
            "test",
            call_type=CallType.BUILD,
            config=config,
            skip_permissions=True,
            output_file="/tmp/out.txt",
        )
        assert "--allowedTools" not in cmd

    def test_output_file_none_no_write(self) -> None:
        """When output_file is None, Write is not injected for call types without it."""
        config = ZingConfig()
        cmd = _build_command(
            "test",
            call_type=CallType.INVESTIGATE,
            config=config,
        )
        idx = cmd.index("--allowedTools")
        separator_idx = cmd.index("--")
        tools_in_cmd = cmd[idx + 1 : separator_idx]
        assert "Write" not in tools_in_cmd


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
    """Tests for the sync streaming invoke_claude function."""

    def test_yields_formatted_output(self) -> None:
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event()),
                _jsonl_line(_make_text_event("line 1\n")),
                _jsonl_line(_make_text_event("line 2\n")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc
            config = ZingConfig()

            lines: list[str] = []
            with invoke_claude(
                "hello", call_type=CallType.BUILD, config=config
            ) as line_iter:
                for line in line_iter:
                    lines.append(line)

        assert lines == ["line 1\n", "line 2\n"]

    def test_yields_tool_use_events(self) -> None:
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_tool_use_event("Read")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc
            config = ZingConfig()

            lines: list[str] = []
            with invoke_claude(
                "hello", call_type=CallType.BUILD, config=config
            ) as line_iter:
                for line in line_iter:
                    lines.append(line)

        assert lines == ["Tool: Read\n"]

    def test_skips_init_and_user_events(self) -> None:
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event()),
                _jsonl_line({"type": "user", "message": {"content": []}}),
                _jsonl_line(_make_text_event("visible")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc
            config = ZingConfig()

            lines: list[str] = []
            with invoke_claude(
                "hello", call_type=CallType.BUILD, config=config
            ) as line_iter:
                for line in line_iter:
                    lines.append(line)

        assert lines == ["visible"]

    def test_passes_correct_command(self) -> None:
        mock_proc = _make_mock_popen(stdout_lines=[], stderr=b"")

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc
            config = ZingConfig()

            with invoke_claude(
                "test prompt", call_type=CallType.INVESTIGATE, config=config
            ) as line_iter:
                for _ in line_iter:
                    pass

            cmd = mock_popen.call_args[0][0]

        assert cmd[0] == "claude"
        assert "--print" in cmd
        assert "--output-format" in cmd
        assert "--verbose" in cmd
        assert "--model" in cmd
        assert "--" in cmd
        assert cmd[-1] == "test prompt"

    def test_skip_permissions(self) -> None:
        mock_proc = _make_mock_popen(stdout_lines=[], stderr=b"")

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc
            config = ZingConfig()

            with invoke_claude(
                "test",
                call_type=CallType.BUILD,
                config=config,
                skip_permissions=True,
            ) as line_iter:
                for _ in line_iter:
                    pass

            cmd = mock_popen.call_args[0][0]

        assert "--dangerously-skip-permissions" in cmd
        assert "--allowedTools" not in cmd

    def test_resume_session(self) -> None:
        mock_proc = _make_mock_popen(stdout_lines=[], stderr=b"")

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc
            config = ZingConfig()

            with invoke_claude(
                "continue",
                call_type=CallType.BUILD,
                config=config,
                resume_session="sess-xyz",
            ) as line_iter:
                for _ in line_iter:
                    pass

            cmd = mock_popen.call_args[0][0]

        assert "--resume" in cmd
        idx = cmd.index("--resume")
        assert cmd[idx + 1] == "sess-xyz"

    def test_popen_uses_start_new_session(self) -> None:
        """Popen should use start_new_session=True for process group management."""
        mock_proc = _make_mock_popen(stdout_lines=[], stderr=b"")

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc
            config = ZingConfig()

            with invoke_claude(
                "test", call_type=CallType.BUILD, config=config
            ) as line_iter:
                for _ in line_iter:
                    pass

        _, kwargs = mock_popen.call_args
        assert kwargs.get("start_new_session") is True

    def test_context_manager_normal_completion(self) -> None:
        """Normal completion yields all lines and cleans up the subprocess."""
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_text_event("a\n")),
                _jsonl_line(_make_text_event("b\n")),
                _jsonl_line(_make_text_event("c\n")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen, patch(
            "zing_ai.orchestrator.claude._kill_process_group"
        ) as mock_kill:
            mock_popen.return_value = mock_proc
            config = ZingConfig()

            collected: list[str] = []
            with invoke_claude(
                "hello", call_type=CallType.BUILD, config=config
            ) as line_iter:
                for line in line_iter:
                    collected.append(line)

        assert collected == ["a\n", "b\n", "c\n"]
        # Subprocess cleanup must happen even on normal exit
        mock_kill.assert_any_call(mock_proc, signal.SIGTERM)
        mock_proc.wait.assert_called()

    def test_context_manager_early_break_kills_subprocess(self) -> None:
        """Breaking out of the with block kills the subprocess."""
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_text_event("first\n")),
                _jsonl_line(_make_text_event("second\n")),
                _jsonl_line(_make_text_event("third\n")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen, patch(
            "zing_ai.orchestrator.claude._kill_process_group"
        ) as mock_kill:
            mock_popen.return_value = mock_proc
            config = ZingConfig()

            collected: list[str] = []
            with invoke_claude(
                "hello", call_type=CallType.BUILD, config=config
            ) as line_iter:
                for line in line_iter:
                    collected.append(line)
                    break  # exit early after first line

        # Only consumed the first line
        assert collected == ["first\n"]
        # Subprocess must still be killed
        mock_kill.assert_any_call(mock_proc, signal.SIGTERM)
        mock_proc.wait.assert_called()

    def test_context_manager_exception_kills_subprocess(self) -> None:
        """An exception inside the with block kills the subprocess."""
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_text_event("line\n")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen, patch(
            "zing_ai.orchestrator.claude._kill_process_group"
        ) as mock_kill:
            mock_popen.return_value = mock_proc
            config = ZingConfig()

            with pytest.raises(RuntimeError, match="boom"):
                with invoke_claude(
                    "hello", call_type=CallType.BUILD, config=config
                ) as line_iter:
                    for line in line_iter:
                        raise RuntimeError("boom")

        # Subprocess must be killed even when an exception occurs
        mock_kill.assert_any_call(mock_proc, signal.SIGTERM)
        mock_proc.wait.assert_called()


# ---------------------------------------------------------------------------
# invoke_claude_full tests
# ---------------------------------------------------------------------------


class TestInvokeClaudeFull:
    """Tests for the convenience invoke_claude_full wrapper."""

    def test_returns_full_output_from_jsonl(self) -> None:
        """Without zing_dir, collects assistant text from JSONL events."""
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-001")),
                _jsonl_line(_make_text_event("Hello world\n")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            output, session_id = invoke_claude_full(
                "test", call_type=CallType.BUILD, config=ZingConfig()
            )

        assert output == "Hello world\n"

    def test_returns_session_id_from_jsonl(self) -> None:
        """Session ID is extracted from system/init event."""
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-abc-123")),
                _jsonl_line(_make_text_event("output\n")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            output, session_id = invoke_claude_full(
                "test", call_type=CallType.BUILD, config=ZingConfig()
            )

        assert session_id == "sess-abc-123"

    def test_session_id_fallback_to_stderr(self) -> None:
        """When no init event has session_id, falls back to stderr parsing."""
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_text_event("output\n")),
            ],
            stderr=b"Session: sess-from-stderr\n",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            output, session_id = invoke_claude_full(
                "test", call_type=CallType.BUILD, config=ZingConfig()
            )

        assert session_id == "sess-from-stderr"

    def test_empty_session_id_when_not_present(self) -> None:
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_text_event("just output\n")),
            ],
            stderr=b"no session here\n",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            output, session_id = invoke_claude_full(
                "test", call_type=CallType.BUILD, config=ZingConfig()
            )

        assert session_id == ""

    def test_passes_kwargs_to_build_command(self) -> None:
        mock_proc = _make_mock_popen(stdout_lines=[], stderr=b"")

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            invoke_claude_full(
                "test",
                call_type=CallType.AUDIT,
                config=ZingConfig(),
                skip_permissions=True,
                system_prompt="be helpful",
                resume_session="sess-999",
            )

            cmd = mock_popen.call_args[0][0]

        assert "--dangerously-skip-permissions" in cmd
        assert "--allowedTools" not in cmd
        assert "--system-prompt" in cmd
        assert "--resume" in cmd
        idx = cmd.index("--resume")
        assert cmd[idx + 1] == "sess-999"

    def test_on_output_callback_called_with_formatted_events(self) -> None:
        """The on_output callback receives formatted event strings."""
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event()),
                _jsonl_line(_make_text_event("line 1\n")),
                _jsonl_line(_make_text_event("line 2\n")),
                _jsonl_line(_make_tool_use_event("Grep")),
            ],
            stderr=b"",
        )

        callback = MagicMock()

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            output, _ = invoke_claude_full(
                "test",
                on_output=callback,
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

        # init is silent, so callback should be called for text + tool_use only
        assert callback.call_count == 3
        callback.assert_any_call("line 1\n")
        callback.assert_any_call("line 2\n")
        callback.assert_any_call("Tool: Grep\n")

    def test_on_output_none_still_collects(self) -> None:
        """When on_output is None, output is still accumulated from JSONL."""
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_text_event("hello\n")),
                _jsonl_line(_make_text_event("world\n")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            output, _ = invoke_claude_full(
                "test",
                on_output=None,
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

        assert output == "hello\nworld\n"

    def test_collects_multiple_text_blocks(self) -> None:
        """Multiple assistant text events are concatenated."""
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_text_event("part1")),
                _jsonl_line(_make_text_event("part2")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            output, _ = invoke_claude_full(
                "test",
                on_output=MagicMock(),
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

        assert output == "part1part2"

    def test_tool_use_not_in_output(self) -> None:
        """Tool use events are formatted for display but not in collected output."""
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_text_event("text before\n")),
                _jsonl_line(_make_tool_use_event("Read")),
                _jsonl_line(_make_text_event("text after\n")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            output, _ = invoke_claude_full(
                "test",
                on_output=MagicMock(),
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

        assert output == "text before\ntext after\n"

    def test_zing_dir_reads_temp_file(self, tmp_path: Path) -> None:
        """When zing_dir is provided, output is read from the temp file."""
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()

        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-tmp")),
                _jsonl_line(_make_text_event("streaming text\n")),
            ],
            stderr=b"",
        )

        # We need to intercept the temp file path and write to it
        written_temp_path = None

        original_build_command = _build_command

        def mock_build_command(prompt, **kw):
            nonlocal written_temp_path
            output_file = kw.get("output_file")
            if output_file:
                written_temp_path = Path(output_file)
                # Simulate Claude writing the file
                written_temp_path.write_text("structured result from file")
            return original_build_command(prompt, **kw)

        with (
            patch(
                "zing_ai.orchestrator.claude.subprocess.Popen"
            ) as mock_popen,
            patch(
                "zing_ai.orchestrator.claude._build_command",
                side_effect=mock_build_command,
            ),
        ):
            mock_popen.return_value = mock_proc

            output, session_id = invoke_claude_full(
                "test",
                on_output=MagicMock(),
                zing_dir=zing_dir,
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

        assert output == "structured result from file"
        assert session_id == "sess-tmp"
        # Temp file should be cleaned up
        assert written_temp_path is not None
        assert not written_temp_path.exists()

    def test_zing_dir_missing_temp_file(self, tmp_path: Path) -> None:
        """When temp file doesn't exist after Claude runs, output is empty."""
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()

        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event()),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            output, _ = invoke_claude_full(
                "test",
                on_output=MagicMock(),
                zing_dir=zing_dir,
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

        assert output == ""

    def test_zing_dir_appends_instruction_to_prompt(self, tmp_path: Path) -> None:
        """When zing_dir is provided, the prompt has a file-write instruction appended."""
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()

        mock_proc = _make_mock_popen(
            stdout_lines=[_jsonl_line(_make_init_event())],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            invoke_claude_full(
                "original prompt",
                on_output=MagicMock(),
                zing_dir=zing_dir,
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

            cmd = mock_popen.call_args[0][0]

        # The prompt (last element after --) should contain the instruction
        separator_idx = cmd.index("--")
        prompt = cmd[separator_idx + 1]
        assert "original prompt" in prompt
        assert "Write your complete response to the file at" in prompt
        assert ".zing/.tmp_" in prompt

    def test_sigint_terminates_child_and_reraises(self) -> None:
        """SIGINT handler should kill the process group and re-raise KeyboardInterrupt."""
        mock_proc = MagicMock()
        mock_proc.returncode = 0
        mock_proc.pid = 12345
        mock_proc.wait.return_value = 0

        stderr_mock = MagicMock()
        stderr_mock.read.return_value = b""
        stderr_mock.__iter__ = lambda self: iter([])
        mock_proc.stderr = stderr_mock

        # Make stdout iteration raise KeyboardInterrupt on the second line
        def _stdout_iter():
            yield _jsonl_line(_make_text_event("line 1\n"))
            raise KeyboardInterrupt

        mock_proc.stdout = _stdout_iter()

        with (
            patch(
                "zing_ai.orchestrator.claude.subprocess.Popen"
            ) as mock_popen,
            patch(
                "zing_ai.orchestrator.claude.os.getpgid", return_value=12345
            ),
            patch(
                "zing_ai.orchestrator.claude.os.killpg"
            ) as mock_killpg,
        ):
            mock_popen.return_value = mock_proc

            with pytest.raises(KeyboardInterrupt):
                invoke_claude_full(
                    "test",
                    call_type=CallType.BUILD,
                    config=ZingConfig(),
                )

        # Should have killed the process group with SIGTERM
        mock_killpg.assert_any_call(12345, signal.SIGTERM)

    def test_popen_uses_start_new_session(self) -> None:
        """Popen should use start_new_session=True for process group management."""
        mock_proc = _make_mock_popen(stdout_lines=[], stderr=b"")

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            invoke_claude_full(
                "test",
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

        _, kwargs = mock_popen.call_args
        assert kwargs.get("start_new_session") is True


# ---------------------------------------------------------------------------
# print_line tests
# ---------------------------------------------------------------------------


class TestPrintLine:
    """Tests for the print_line convenience callback."""

    def test_prints_without_extra_newline(self, capsys: pytest.CaptureFixture[str]) -> None:
        print_line("hello world\n")
        captured = capsys.readouterr()
        assert captured.out == "hello world\n"

    def test_prints_partial_line(self, capsys: pytest.CaptureFixture[str]) -> None:
        print_line("partial")
        captured = capsys.readouterr()
        assert captured.out == "partial"


# ---------------------------------------------------------------------------
# invoke_claude_validated tests
# ---------------------------------------------------------------------------


class TestInvokeClaudeValidated:
    """Tests for the retry-on-validation invoke_claude_validated wrapper."""

    def test_returns_on_first_success(self) -> None:
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-001")),
                _jsonl_line(_make_text_event("valid output\n")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            result = invoke_claude_validated(
                "test",
                validator=lambda x: x.strip().upper(),
                retry_prompt_template=jinja2.Template("Fix: {{ error }}"),
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

        assert result == "VALID OUTPUT"

    def test_retries_on_validation_error(self) -> None:
        mock_proc_bad = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-001")),
                _jsonl_line(_make_text_event("bad output\n")),
            ],
            stderr=b"",
        )
        mock_proc_good = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-002")),
                _jsonl_line(_make_text_event("good output\n")),
            ],
            stderr=b"",
        )

        def validator(text: str) -> str:
            if "bad" in text:
                raise ValidationError("output was bad")
            return text.strip()

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.side_effect = [mock_proc_bad, mock_proc_good]

            result = invoke_claude_validated(
                "test",
                validator=validator,
                retry_prompt_template=jinja2.Template(
                    "Fix this error: {{ error }}"
                ),
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

        assert result == "good output"
        assert mock_popen.call_count == 2

    def test_raises_after_max_retries(self) -> None:
        mock_procs = [
            _make_mock_popen(
                stdout_lines=[
                    _jsonl_line(_make_init_event("sess-001")),
                    _jsonl_line(_make_text_event("always bad\n")),
                ],
                stderr=b"",
            )
            for _ in range(3)
        ]

        def validator(text: str) -> str:
            raise ValidationError("still bad")

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.side_effect = mock_procs

            with pytest.raises(ValidationError, match="still bad"):
                invoke_claude_validated(
                    "test",
                    validator=validator,
                    retry_prompt_template=jinja2.Template("Retry: {{ error }}"),
                    max_retries=3,
                    call_type=CallType.BUILD,
                    config=ZingConfig(),
                )

        # 1 initial call + 2 retry calls = 3 total
        assert mock_popen.call_count == 3

    def test_calls_on_retry_callback(self) -> None:
        mock_proc_bad = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-001")),
                _jsonl_line(_make_text_event("bad\n")),
            ],
            stderr=b"",
        )
        mock_proc_good = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-002")),
                _jsonl_line(_make_text_event("good\n")),
            ],
            stderr=b"",
        )

        call_count = 0

        def validator(text: str) -> str:
            nonlocal call_count
            call_count += 1
            if call_count <= 1:
                raise ValidationError("bad output")
            return text.strip()

        on_retry = MagicMock()

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.side_effect = [mock_proc_bad, mock_proc_good]

            result = invoke_claude_validated(
                "test",
                validator=validator,
                retry_prompt_template=jinja2.Template("Fix: {{ error }}"),
                on_retry=on_retry,
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

        assert result == "good"
        on_retry.assert_called_once_with(1, "bad output")

    def test_retry_uses_resume_session(self) -> None:
        """The retry call should pass --resume with the session ID from the first call."""
        mock_proc_bad = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-original")),
                _jsonl_line(_make_text_event("bad\n")),
            ],
            stderr=b"",
        )
        mock_proc_good = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-retry")),
                _jsonl_line(_make_text_event("good\n")),
            ],
            stderr=b"",
        )

        call_count = 0

        def validator(text: str) -> str:
            nonlocal call_count
            call_count += 1
            if call_count <= 1:
                raise ValidationError("bad")
            return text.strip()

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.side_effect = [mock_proc_bad, mock_proc_good]

            invoke_claude_validated(
                "test",
                validator=validator,
                retry_prompt_template=jinja2.Template("Fix: {{ error }}"),
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

            call_cmds = [call[0][0] for call in mock_popen.call_args_list]

        # Second call should have --resume flag with the session from first call
        second_call_cmd = call_cmds[1]
        assert "--resume" in second_call_cmd
        idx = second_call_cmd.index("--resume")
        assert second_call_cmd[idx + 1] == "sess-original"

    def test_retry_prompt_rendered_with_error(self) -> None:
        """The retry prompt should be rendered with the error message."""
        mock_proc_bad = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-001")),
                _jsonl_line(_make_text_event("bad\n")),
            ],
            stderr=b"",
        )
        mock_proc_good = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-002")),
                _jsonl_line(_make_text_event("good\n")),
            ],
            stderr=b"",
        )

        call_count = 0

        def validator(text: str) -> str:
            nonlocal call_count
            call_count += 1
            if call_count <= 1:
                raise ValidationError("missing <plan> element")
            return text.strip()

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.side_effect = [mock_proc_bad, mock_proc_good]

            invoke_claude_validated(
                "test",
                validator=validator,
                retry_prompt_template=jinja2.Template(
                    "Your output had an error: {{ error }}. Please fix it."
                ),
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

            call_cmds = [call[0][0] for call in mock_popen.call_args_list]

        second_call_cmd = call_cmds[1]
        separator_idx = second_call_cmd.index("--")
        retry_prompt = second_call_cmd[separator_idx + 1]
        assert "missing <plan> element" in retry_prompt
        assert "Please fix it." in retry_prompt

    def test_max_retries_one(self) -> None:
        """With max_retries=1, should fail immediately on first validation error."""
        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-001")),
                _jsonl_line(_make_text_event("bad\n")),
            ],
            stderr=b"",
        )

        def validator(text: str) -> str:
            raise ValidationError("always fails")

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            with pytest.raises(ValidationError, match="always fails"):
                invoke_claude_validated(
                    "test",
                    validator=validator,
                    retry_prompt_template=jinja2.Template("Fix: {{ error }}"),
                    max_retries=1,
                    call_type=CallType.BUILD,
                    config=ZingConfig(),
                )

        # Only one call -- no retries with max_retries=1
        assert mock_popen.call_count == 1

    def test_no_on_retry_callback(self) -> None:
        """When on_retry is None, retries still work without callback."""
        mock_proc_bad = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-001")),
                _jsonl_line(_make_text_event("bad\n")),
            ],
            stderr=b"",
        )
        mock_proc_good = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-002")),
                _jsonl_line(_make_text_event("good\n")),
            ],
            stderr=b"",
        )

        call_count = 0

        def validator(text: str) -> str:
            nonlocal call_count
            call_count += 1
            if call_count <= 1:
                raise ValidationError("bad")
            return text.strip()

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.side_effect = [mock_proc_bad, mock_proc_good]

            result = invoke_claude_validated(
                "test",
                validator=validator,
                retry_prompt_template=jinja2.Template("Fix: {{ error }}"),
                on_retry=None,
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

        assert result == "good"

    def test_zing_dir_passed_through(self, tmp_path: Path) -> None:
        """The zing_dir parameter is forwarded to invoke_claude_full."""
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()

        mock_proc = _make_mock_popen(
            stdout_lines=[
                _jsonl_line(_make_init_event("sess-001")),
                _jsonl_line(_make_text_event("output\n")),
            ],
            stderr=b"",
        )

        with patch(
            "zing_ai.orchestrator.claude.subprocess.Popen"
        ) as mock_popen:
            mock_popen.return_value = mock_proc

            # When zing_dir is provided, prompt should have temp file instruction
            invoke_claude_validated(
                "test",
                validator=lambda x: x.strip(),
                retry_prompt_template=jinja2.Template("Fix: {{ error }}"),
                zing_dir=zing_dir,
                call_type=CallType.BUILD,
                config=ZingConfig(),
            )

            cmd = mock_popen.call_args[0][0]

        separator_idx = cmd.index("--")
        prompt = cmd[separator_idx + 1]
        assert "Write your complete response to the file at" in prompt
