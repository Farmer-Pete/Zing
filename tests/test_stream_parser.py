"""Tests for the JSONL stream event parser."""

from __future__ import annotations

import json

from zing_ai.orchestrator.stream_parser import (
    collect_assistant_text,
    extract_result_text,
    extract_session_id,
    format_event,
    parse_event,
)


# ---------------------------------------------------------------------------
# parse_event tests
# ---------------------------------------------------------------------------


class TestParseEvent:
    """Tests for parse_event()."""

    def test_valid_json(self) -> None:
        line = '{"type": "system", "subtype": "init"}\n'
        result = parse_event(line)
        assert result == {"type": "system", "subtype": "init"}

    def test_empty_line(self) -> None:
        assert parse_event("") is None
        assert parse_event("\n") is None
        assert parse_event("  \n") is None

    def test_invalid_json(self) -> None:
        assert parse_event("not json at all\n") is None

    def test_partial_json(self) -> None:
        assert parse_event('{"type": "system"') is None

    def test_strips_whitespace(self) -> None:
        line = '  {"type": "assistant"}  \n'
        result = parse_event(line)
        assert result == {"type": "assistant"}


# ---------------------------------------------------------------------------
# format_event tests
# ---------------------------------------------------------------------------


class TestFormatEvent:
    """Tests for format_event()."""

    def test_system_init_skipped(self) -> None:
        event = {"type": "system", "subtype": "init", "session_id": "abc"}
        assert format_event(event) is None

    def test_system_task_started(self) -> None:
        event = {"type": "system", "subtype": "task_started", "description": "Building code"}
        assert format_event(event) == "Task: Building code\n"

    def test_assistant_text(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [{"type": "text", "text": "Hello world"}],
            },
        }
        assert format_event(event) == "Hello world"

    def test_assistant_thinking_skipped(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [{"type": "thinking", "thinking": "let me think..."}],
            },
        }
        assert format_event(event) is None

    def test_assistant_tool_use(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [{"type": "tool_use", "name": "Read", "input": {}}],
            },
        }
        assert format_event(event) == "Tool: Read\n"

    def test_assistant_mixed_content(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "text", "text": "Analyzing... "},
                    {"type": "tool_use", "name": "Grep", "input": {}},
                ],
            },
        }
        result = format_event(event)
        assert result == "Analyzing... Tool: Grep\n"

    def test_tool_use_with_command(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [{"type": "tool_use", "name": "Bash", "input": {"command": "ls -la"}}],
            },
        }
        assert format_event(event) == "Tool: Bash (ls -la)\n"

    def test_tool_use_with_file_path(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "tool_use", "name": "Read", "input": {"file_path": "/path/to/file.py"}},
                ],
            },
        }
        assert format_event(event) == "Tool: Read (/path/to/file.py)\n"

    def test_tool_use_truncates_long_value(self) -> None:
        long_cmd = "x" * 100
        event = {
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "tool_use", "name": "Bash", "input": {"command": long_cmd}},
                ],
            },
        }
        result = format_event(event)
        assert result is not None
        assert result.endswith("...)\n")
        assert "x" * 80 in result

    def test_tool_use_newlines_collapsed(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "tool_use", "name": "Bash", "input": {"command": "echo\nhello"}},
                ],
            },
        }
        assert format_event(event) == "Tool: Bash (echo hello)\n"

    def test_tool_use_unknown_keys_no_detail(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [{"type": "tool_use", "name": "Custom", "input": {"foo": "bar"}}],
            },
        }
        assert format_event(event) == "Tool: Custom\n"

    def test_tool_use_priority_command_over_description(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [
                    {
                        "type": "tool_use",
                        "name": "Bash",
                        "input": {"command": "ls", "description": "List files"},
                    },
                ],
            },
        }
        assert format_event(event) == "Tool: Bash (ls)\n"

    def test_assistant_empty_content(self) -> None:
        event = {"type": "assistant", "message": {"content": []}}
        assert format_event(event) is None

    def test_user_event_skipped(self) -> None:
        event = {"type": "user", "message": {"content": [{"type": "tool_result"}]}}
        assert format_event(event) is None

    def test_result_success(self) -> None:
        event = {
            "type": "result",
            "subtype": "success",
            "duration_seconds": 12.5,
            "total_cost_usd": 0.0321,
        }
        result = format_event(event)
        assert result == "Completed in 12.5s | Cost: $0.0321\n"

    def test_result_success_duration_only(self) -> None:
        event = {
            "type": "result",
            "subtype": "success",
            "duration_seconds": 5.0,
        }
        result = format_event(event)
        assert result == "Completed in 5.0s\n"

    def test_result_success_cost_only(self) -> None:
        event = {
            "type": "result",
            "subtype": "success",
            "total_cost_usd": 0.01,
        }
        result = format_event(event)
        assert result == "Cost: $0.0100\n"

    def test_result_success_no_metadata(self) -> None:
        event = {"type": "result", "subtype": "success"}
        assert format_event(event) is None

    def test_rate_limit_event_skipped(self) -> None:
        event = {"type": "rate_limit_event"}
        assert format_event(event) is None

    def test_unknown_event_type_skipped(self) -> None:
        event = {"type": "something_new", "data": "stuff"}
        assert format_event(event) is None

    def test_assistant_no_message(self) -> None:
        event = {"type": "assistant"}
        assert format_event(event) is None

    def test_system_unknown_subtype(self) -> None:
        event = {"type": "system", "subtype": "unknown_thing"}
        assert format_event(event) is None

    def test_result_non_success(self) -> None:
        event = {"type": "result", "subtype": "error", "error": "something went wrong"}
        assert format_event(event) is None


# ---------------------------------------------------------------------------
# extract_session_id tests
# ---------------------------------------------------------------------------


class TestExtractSessionId:
    """Tests for extract_session_id()."""

    def test_from_init_event(self) -> None:
        event = {"type": "system", "subtype": "init", "session_id": "abc-def-123"}
        assert extract_session_id(event) == "abc-def-123"

    def test_non_init_event(self) -> None:
        event = {"type": "assistant", "session_id": "abc"}
        assert extract_session_id(event) is None

    def test_init_without_session_id(self) -> None:
        event = {"type": "system", "subtype": "init"}
        assert extract_session_id(event) is None

    def test_init_empty_session_id(self) -> None:
        event = {"type": "system", "subtype": "init", "session_id": ""}
        assert extract_session_id(event) is None

    def test_system_non_init(self) -> None:
        event = {"type": "system", "subtype": "task_started", "session_id": "abc"}
        assert extract_session_id(event) is None


# ---------------------------------------------------------------------------
# extract_result_text tests
# ---------------------------------------------------------------------------


class TestExtractResultText:
    """Tests for extract_result_text()."""

    def test_from_result_success(self) -> None:
        event = {"type": "result", "subtype": "success", "result": "The answer is 42"}
        assert extract_result_text(event) == "The answer is 42"

    def test_non_result_event(self) -> None:
        event = {"type": "assistant", "result": "something"}
        assert extract_result_text(event) is None

    def test_result_without_result_field(self) -> None:
        event = {"type": "result", "subtype": "success"}
        assert extract_result_text(event) is None

    def test_result_error_subtype(self) -> None:
        event = {"type": "result", "subtype": "error", "result": "oops"}
        assert extract_result_text(event) is None

    def test_result_empty_string(self) -> None:
        event = {"type": "result", "subtype": "success", "result": ""}
        assert extract_result_text(event) is None


# ---------------------------------------------------------------------------
# collect_assistant_text tests
# ---------------------------------------------------------------------------


class TestCollectAssistantText:
    """Tests for collect_assistant_text()."""

    def test_text_block(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [{"type": "text", "text": "Hello world"}],
            },
        }
        assert collect_assistant_text(event) == "Hello world"

    def test_tool_use_excluded(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [{"type": "tool_use", "name": "Read", "input": {}}],
            },
        }
        assert collect_assistant_text(event) is None

    def test_mixed_content_only_text(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [
                    {"type": "text", "text": "Part 1"},
                    {"type": "tool_use", "name": "Grep", "input": {}},
                    {"type": "text", "text": "Part 2"},
                ],
            },
        }
        assert collect_assistant_text(event) == "Part 1Part 2"

    def test_non_assistant_event(self) -> None:
        event = {"type": "user", "message": {"content": [{"type": "text", "text": "hi"}]}}
        assert collect_assistant_text(event) is None

    def test_thinking_excluded(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [{"type": "thinking", "thinking": "hmm..."}],
            },
        }
        assert collect_assistant_text(event) is None

    def test_empty_text_excluded(self) -> None:
        event = {
            "type": "assistant",
            "message": {
                "content": [{"type": "text", "text": ""}],
            },
        }
        assert collect_assistant_text(event) is None


# ---------------------------------------------------------------------------
# Integration: parse + format round-trip
# ---------------------------------------------------------------------------


class TestParseAndFormat:
    """Test parse_event + format_event together with raw JSONL lines."""

    def test_full_round_trip(self) -> None:
        init_line = json.dumps({
            "type": "system",
            "subtype": "init",
            "session_id": "sess-123",
        }) + "\n"
        text_line = json.dumps({
            "type": "assistant",
            "message": {"content": [{"type": "text", "text": "Hello\n"}]},
        }) + "\n"
        result_line = json.dumps({
            "type": "result",
            "subtype": "success",
            "duration_seconds": 3.2,
            "total_cost_usd": 0.005,
        }) + "\n"

        init_event = parse_event(init_line)
        assert init_event is not None
        assert format_event(init_event) is None
        assert extract_session_id(init_event) == "sess-123"

        text_event = parse_event(text_line)
        assert text_event is not None
        assert format_event(text_event) == "Hello\n"

        result_event = parse_event(result_line)
        assert result_event is not None
        result_formatted = format_event(result_event)
        assert result_formatted is not None
        assert "3.2s" in result_formatted
        assert "$0.0050" in result_formatted
