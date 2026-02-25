"""Configuration loader for ``.zing.toml`` files."""

from __future__ import annotations

import logging
import tomllib
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path

logger = logging.getLogger(__name__)


class CallType(StrEnum):
    """Enum for the different types of Claude CLI calls."""

    INVESTIGATE = "investigate"
    PLAN = "plan"
    BUILD = "build"
    AUDIT = "audit"


DEFAULT_MCP_TOOLS: list[str] = [
    "mcp__serena__*",
    "mcp__aid__*",
    "mcp__CodeGraphContext__*",
    "mcp__storybook-mcp__*",
]

DEFAULT_EXTRA_TOOLS: dict[CallType, list[str]] = {
    CallType.INVESTIGATE: [],
    CallType.PLAN: [],
    CallType.BUILD: ["Bash", "Read", "Edit", "Write", "Glob", "Grep"],
    CallType.AUDIT: ["Read", "Glob", "Grep"],
}

DEFAULT_MODELS: dict[CallType, str] = {
    CallType.INVESTIGATE: "opus",
    CallType.PLAN: "opus",
    CallType.BUILD: "sonnet",
    CallType.AUDIT: "opus",
}

_KNOWN_TOP_LEVEL_KEYS = {"settings", "permissions"}
_KNOWN_SETTINGS_KEYS = {"subprocess_timeout"}
_KNOWN_PERMISSIONS_KEYS = {
    "mcp_tools",
    CallType.INVESTIGATE,
    CallType.PLAN,
    CallType.BUILD,
    CallType.AUDIT,
}
_KNOWN_CALL_TYPE_KEYS = {"extra_tools", "model"}


@dataclass
class ZingConfig:
    """Parsed configuration from ``.zing.toml``."""

    mcp_tools: list[str] = field(default_factory=lambda: list(DEFAULT_MCP_TOOLS))
    extra_tools: dict[CallType, list[str]] = field(
        default_factory=lambda: {ct: list(tools) for ct, tools in DEFAULT_EXTRA_TOOLS.items()}
    )
    models: dict[CallType, str] = field(
        default_factory=lambda: dict(DEFAULT_MODELS)
    )
    subprocess_timeout: int = 300


def load_config(project_root: Path) -> ZingConfig:
    """Read ``.zing.toml`` from *project_root* and return a :class:`ZingConfig`.

    If the file does not exist, returns a ``ZingConfig`` with all defaults.
    Warns on unrecognised keys.
    """
    config_path = project_root / ".zing.toml"
    logger.debug("Loading config from %s", config_path)
    if not config_path.is_file():
        logger.debug("No .zing.toml found, using defaults")
        return ZingConfig()

    with config_path.open("rb") as f:
        raw = tomllib.load(f)
    logger.debug("Loaded .zing.toml with top-level keys: %s", list(raw.keys()))

    # Warn on unrecognised top-level keys
    for key in raw:
        if key not in _KNOWN_TOP_LEVEL_KEYS:
            logger.warning("Unrecognized top-level key in .zing.toml: %s", key)

    config = ZingConfig()

    # --- [settings] ---
    settings = raw.get("settings", {})
    for key in settings:
        if key not in _KNOWN_SETTINGS_KEYS:
            logger.warning("Unrecognized key in [settings]: %s", key)
    if "subprocess_timeout" in settings:
        config.subprocess_timeout = int(settings["subprocess_timeout"])

    # --- [permissions] ---
    permissions = raw.get("permissions", {})
    for key in permissions:
        if key not in _KNOWN_PERMISSIONS_KEYS:
            logger.warning("Unrecognized key in [permissions]: %s", key)

    if "mcp_tools" in permissions:
        config.mcp_tools = list(permissions["mcp_tools"])

    # Per call-type sections
    for ct in CallType:
        section = permissions.get(ct, {})
        for key in section:
            if key not in _KNOWN_CALL_TYPE_KEYS:
                logger.warning("Unrecognized key in [permissions.%s]: %s", ct, key)
        if "extra_tools" in section:
            config.extra_tools[ct] = list(section["extra_tools"])
        if "model" in section:
            config.models[ct] = str(section["model"])

    logger.debug(
        "Config loaded: mcp_tools=%d, subprocess_timeout=%d",
        len(config.mcp_tools), config.subprocess_timeout,
    )
    for ct in CallType:
        logger.debug(
            "Config [%s]: model=%s, extra_tools=%s",
            ct, config.models[ct], config.extra_tools[ct],
        )
    return config


def get_allowed_tools(config: ZingConfig, call_type: CallType) -> list[str]:
    """Combine ``config.mcp_tools`` and ``config.extra_tools[call_type]`` into a flat list."""
    result = config.mcp_tools + config.extra_tools[call_type]
    logger.debug("Allowed tools for %s: %d tool(s)", call_type, len(result))
    return result


def get_model(config: ZingConfig, call_type: CallType) -> str:
    """Return the model configured for *call_type*."""
    model = config.models[call_type]
    logger.debug("Model for %s: %s", call_type, model)
    return model
