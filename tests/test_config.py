"""Tests for the .zing.toml configuration loader."""

from __future__ import annotations

from pathlib import Path

import pytest

from zing_ai.orchestrator.config import (
    DEFAULT_EXTRA_TOOLS,
    DEFAULT_MCP_TOOLS,
    DEFAULT_MODELS,
    CallType,
    ZingConfig,
    get_allowed_tools,
    get_model,
    load_config,
)


class TestCallType:
    """Tests for the CallType enum."""

    def test_all_values_exist(self) -> None:
        assert CallType.INVESTIGATE == "investigate"
        assert CallType.PLAN == "plan"
        assert CallType.BUILD == "build"
        assert CallType.AUDIT == "audit"

    def test_is_str(self) -> None:
        for ct in CallType:
            assert isinstance(ct, str)

    def test_four_members(self) -> None:
        assert len(CallType) == 4


class TestZingConfigDefaults:
    """Tests for ZingConfig default values."""

    def test_default_mcp_tools(self) -> None:
        config = ZingConfig()
        assert config.mcp_tools == DEFAULT_MCP_TOOLS

    def test_default_extra_tools(self) -> None:
        config = ZingConfig()
        assert config.extra_tools == DEFAULT_EXTRA_TOOLS

    def test_default_models(self) -> None:
        config = ZingConfig()
        assert config.models == DEFAULT_MODELS

    def test_default_subprocess_timeout(self) -> None:
        config = ZingConfig()
        assert config.subprocess_timeout == 300

    def test_defaults_are_independent_copies(self) -> None:
        """Mutating one config must not affect another."""
        a = ZingConfig()
        b = ZingConfig()
        a.mcp_tools.append("extra")
        assert "extra" not in b.mcp_tools

        a.extra_tools[CallType.BUILD].append("extra")
        assert "extra" not in b.extra_tools[CallType.BUILD]


class TestLoadConfigNoFile:
    """Tests that load_config returns defaults when no .zing.toml exists."""

    def test_returns_defaults(self, tmp_path: Path) -> None:
        config = load_config(tmp_path)
        assert config.mcp_tools == DEFAULT_MCP_TOOLS
        assert config.extra_tools == DEFAULT_EXTRA_TOOLS
        assert config.models == DEFAULT_MODELS
        assert config.subprocess_timeout == 300


class TestLoadConfigCustom:
    """Tests that load_config correctly parses a full .zing.toml file."""

    FULL_TOML = """\
[settings]
subprocess_timeout = 600

[permissions]
mcp_tools = ["mcp__custom__*"]

[permissions.investigate]
extra_tools = ["Read"]
model = "haiku"

[permissions.plan]
extra_tools = ["Glob"]
model = "sonnet"

[permissions.build]
extra_tools = ["Bash", "Write"]
model = "haiku"

[permissions.audit]
extra_tools = []
model = "sonnet"
"""

    def test_settings_parsed(self, tmp_path: Path) -> None:
        (tmp_path / ".zing.toml").write_text(self.FULL_TOML)
        config = load_config(tmp_path)
        assert config.subprocess_timeout == 600

    def test_mcp_tools_overridden(self, tmp_path: Path) -> None:
        (tmp_path / ".zing.toml").write_text(self.FULL_TOML)
        config = load_config(tmp_path)
        assert config.mcp_tools == ["mcp__custom__*"]

    def test_extra_tools_overridden(self, tmp_path: Path) -> None:
        (tmp_path / ".zing.toml").write_text(self.FULL_TOML)
        config = load_config(tmp_path)
        assert config.extra_tools[CallType.INVESTIGATE] == ["Read"]
        assert config.extra_tools[CallType.PLAN] == ["Glob"]
        assert config.extra_tools[CallType.BUILD] == ["Bash", "Write"]
        assert config.extra_tools[CallType.AUDIT] == []

    def test_models_overridden(self, tmp_path: Path) -> None:
        (tmp_path / ".zing.toml").write_text(self.FULL_TOML)
        config = load_config(tmp_path)
        assert config.models[CallType.INVESTIGATE] == "haiku"
        assert config.models[CallType.PLAN] == "sonnet"
        assert config.models[CallType.BUILD] == "haiku"
        assert config.models[CallType.AUDIT] == "sonnet"

    def test_partial_override_keeps_defaults(self, tmp_path: Path) -> None:
        """Override only one call type; others keep defaults."""
        toml = """\
[permissions.build]
extra_tools = ["Bash"]
model = "haiku"
"""
        (tmp_path / ".zing.toml").write_text(toml)
        config = load_config(tmp_path)

        # Overridden
        assert config.extra_tools[CallType.BUILD] == ["Bash"]
        assert config.models[CallType.BUILD] == "haiku"

        # Defaults preserved
        assert config.extra_tools[CallType.INVESTIGATE] == DEFAULT_EXTRA_TOOLS[CallType.INVESTIGATE]
        assert config.models[CallType.INVESTIGATE] == DEFAULT_MODELS[CallType.INVESTIGATE]
        assert config.mcp_tools == DEFAULT_MCP_TOOLS
        assert config.subprocess_timeout == 300


class TestLoadConfigWarnings:
    """Tests that load_config warns on unrecognised keys."""

    def test_warns_unrecognized_top_level(
        self, tmp_path: Path, caplog: pytest.LogCaptureFixture
    ) -> None:
        (tmp_path / ".zing.toml").write_text('[bogus]\nfoo = 1\n')
        with caplog.at_level("WARNING"):
            load_config(tmp_path)
        assert "Unrecognized top-level key" in caplog.text
        assert "bogus" in caplog.text

    def test_warns_unrecognized_settings_key(
        self, tmp_path: Path, caplog: pytest.LogCaptureFixture
    ) -> None:
        (tmp_path / ".zing.toml").write_text('[settings]\nunknown_key = 42\n')
        with caplog.at_level("WARNING"):
            load_config(tmp_path)
        assert "Unrecognized key in [settings]" in caplog.text
        assert "unknown_key" in caplog.text

    def test_warns_unrecognized_permissions_key(
        self, tmp_path: Path, caplog: pytest.LogCaptureFixture
    ) -> None:
        (tmp_path / ".zing.toml").write_text('[permissions]\nbogus_key = "x"\n')
        with caplog.at_level("WARNING"):
            load_config(tmp_path)
        assert "Unrecognized key in [permissions]" in caplog.text
        assert "bogus_key" in caplog.text

    def test_warns_unrecognized_call_type_key(
        self, tmp_path: Path, caplog: pytest.LogCaptureFixture
    ) -> None:
        (tmp_path / ".zing.toml").write_text('[permissions.build]\nfoo = "bar"\n')
        with caplog.at_level("WARNING"):
            load_config(tmp_path)
        assert "Unrecognized key in [permissions.build]" in caplog.text
        assert "foo" in caplog.text


class TestGetAllowedTools:
    """Tests for get_allowed_tools merging mcp_tools + extra_tools per call type."""

    def test_investigate_default(self) -> None:
        config = ZingConfig()
        tools = get_allowed_tools(config, CallType.INVESTIGATE)
        # MCP tools only, no extra tools
        assert tools == DEFAULT_MCP_TOOLS

    def test_build_default(self) -> None:
        config = ZingConfig()
        tools = get_allowed_tools(config, CallType.BUILD)
        expected = DEFAULT_MCP_TOOLS + DEFAULT_EXTRA_TOOLS[CallType.BUILD]
        assert tools == expected

    def test_audit_default(self) -> None:
        config = ZingConfig()
        tools = get_allowed_tools(config, CallType.AUDIT)
        expected = DEFAULT_MCP_TOOLS + DEFAULT_EXTRA_TOOLS[CallType.AUDIT]
        assert tools == expected

    def test_plan_default(self) -> None:
        config = ZingConfig()
        tools = get_allowed_tools(config, CallType.PLAN)
        assert tools == DEFAULT_MCP_TOOLS

    def test_custom_mcp_tools_merge(self) -> None:
        config = ZingConfig(mcp_tools=["mcp__custom__*"])
        tools = get_allowed_tools(config, CallType.BUILD)
        assert tools == ["mcp__custom__*", "Bash", "Read", "Edit", "Write", "Glob", "Grep"]

    def test_custom_extra_tools_merge(self) -> None:
        config = ZingConfig()
        config.extra_tools[CallType.INVESTIGATE] = ["Read"]
        tools = get_allowed_tools(config, CallType.INVESTIGATE)
        assert tools == DEFAULT_MCP_TOOLS + ["Read"]

    def test_all_call_types(self) -> None:
        """Every CallType should produce a valid tool list."""
        config = ZingConfig()
        for ct in CallType:
            tools = get_allowed_tools(config, ct)
            assert isinstance(tools, list)
            assert all(isinstance(t, str) for t in tools)


class TestGetModel:
    """Tests for get_model returning the correct model for each call type."""

    def test_default_models(self) -> None:
        config = ZingConfig()
        assert get_model(config, CallType.INVESTIGATE) == "opus"
        assert get_model(config, CallType.PLAN) == "opus"
        assert get_model(config, CallType.BUILD) == "sonnet"
        assert get_model(config, CallType.AUDIT) == "opus"

    def test_custom_model(self) -> None:
        config = ZingConfig()
        config.models[CallType.BUILD] = "haiku"
        assert get_model(config, CallType.BUILD) == "haiku"

    def test_all_call_types(self) -> None:
        """Every CallType should return a string model name."""
        config = ZingConfig()
        for ct in CallType:
            model = get_model(config, ct)
            assert isinstance(model, str)
            assert len(model) > 0


class TestInvalidCallType:
    """Tests for invalid call type handling."""

    def test_invalid_string_not_in_enum(self) -> None:
        with pytest.raises(ValueError, match="is not a valid CallType"):
            CallType("nonexistent")

    def test_get_allowed_tools_with_invalid_key(self) -> None:
        config = ZingConfig()
        with pytest.raises(KeyError):
            get_allowed_tools(config, "nonexistent")  # type: ignore[arg-type]

    def test_get_model_with_invalid_key(self) -> None:
        config = ZingConfig()
        with pytest.raises(KeyError):
            get_model(config, "nonexistent")  # type: ignore[arg-type]
