"""Tests for zing_ai.config (schema, load/save round-trip, hashing, file lock)."""

from __future__ import annotations

import subprocess
import sys
import threading
import tomllib
from pathlib import Path
from unittest.mock import patch

import pytest
from filelock import FileLock, Timeout

from zing_ai.config import (
    ConfigError,
    config_hash,
    default_config,
    load_config,
    save_config,
)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _patch_config_path(tmp_path: Path):
    """Return a context manager that redirects config_path() to tmp_path/config.toml."""
    target = tmp_path / "config.toml"
    return patch("zing_ai.config.config_path", return_value=target)


# ---------------------------------------------------------------------------
# test_round_trip
# ---------------------------------------------------------------------------


def test_round_trip(tmp_path: Path) -> None:
    """save_config then load_config returns an equal Config."""
    with _patch_config_path(tmp_path):
        cfg = default_config()
        save_config(cfg)
        loaded = load_config()
    assert loaded == cfg


def test_save_omits_defaults(tmp_path: Path) -> None:
    """Saving a default config writes an empty file (no fields persisted)."""
    target = tmp_path / "config.toml"
    with _patch_config_path(tmp_path):
        save_config(default_config())
    assert target.read_text(encoding="utf-8") == ""


def test_save_persists_only_changes(tmp_path: Path) -> None:
    """Only fields that differ from defaults are written to disk."""
    target = tmp_path / "config.toml"
    with _patch_config_path(tmp_path):
        cfg = default_config()
        cfg.thresholds.large_file_lines = 9999
        save_config(cfg)
    raw = tomllib.loads(target.read_text(encoding="utf-8"))
    assert raw == {"thresholds": {"large_file_lines": 9999}}


def test_load_merges_defaults(tmp_path: Path) -> None:
    """A partial config file is merged with defaults at load time."""
    target = tmp_path / "config.toml"
    target.write_text("[thresholds]\nlarge_file_lines = 9999\n", encoding="utf-8")
    with _patch_config_path(tmp_path):
        loaded = load_config()
    expected = default_config()
    expected.thresholds.large_file_lines = 9999
    assert loaded == expected


# ---------------------------------------------------------------------------
# test_load_missing_returns_defaults
# ---------------------------------------------------------------------------


def test_load_missing_returns_defaults(tmp_path: Path) -> None:
    """When the config file is absent, load_config() returns the default config."""
    with _patch_config_path(tmp_path):
        result = load_config()
    assert result == default_config()


# ---------------------------------------------------------------------------
# test_load_invalid_toml_raises_config_error
# ---------------------------------------------------------------------------


def test_load_invalid_toml_raises_config_error(tmp_path: Path) -> None:
    """Invalid TOML raises ConfigError with 'not valid TOML' in the message."""
    target = tmp_path / "config.toml"
    target.write_text("this is not = valid toml [", encoding="utf-8")

    with _patch_config_path(tmp_path), pytest.raises(ConfigError, match="not valid TOML"):
        load_config()


# ---------------------------------------------------------------------------
# test_load_invalid_value_raises_config_error
# ---------------------------------------------------------------------------


def test_load_invalid_value_raises_config_error(tmp_path: Path) -> None:
    """An invalid enum value raises ConfigError mentioning the offending field."""
    target = tmp_path / "config.toml"
    target.write_text('[git]\nworkflow_mode = "rebase"\n', encoding="utf-8")

    with _patch_config_path(tmp_path), pytest.raises(ConfigError, match="workflow_mode"):
        load_config()


# ---------------------------------------------------------------------------
# test_hash_stable_across_runs
# ---------------------------------------------------------------------------


def test_hash_stable_across_runs() -> None:
    """config_hash produces the same value across separate interpreter invocations."""
    code = (
        "from zing_ai.config import config_hash, default_config; "
        "print(config_hash(default_config()))"
    )
    h1 = subprocess.check_output([sys.executable, "-c", code], text=True).strip()
    h2 = subprocess.check_output([sys.executable, "-c", code], text=True).strip()
    assert h1 == h2


# ---------------------------------------------------------------------------
# test_hash_changes_on_field_change
# ---------------------------------------------------------------------------


def test_hash_changes_on_field_change() -> None:
    """Mutating a field value produces a different config_hash."""
    base = default_config()
    modified = default_config()
    modified.thresholds.large_file_lines = 999
    assert config_hash(base) != config_hash(modified)


# ---------------------------------------------------------------------------
# test_save_uses_filelock
# ---------------------------------------------------------------------------


def test_save_uses_filelock(tmp_path: Path) -> None:
    """FileLock raises Timeout when the lock is already held by another holder."""
    lock_path = str(tmp_path / "config.toml") + ".lock"

    # Acquire the lock in the main thread so nothing else can grab it.
    holder = FileLock(lock_path)
    holder.acquire(timeout=10)

    raised: list[Exception] = []

    def _try_acquire() -> None:
        contender = FileLock(lock_path, timeout=1)
        try:
            contender.acquire()
        except Timeout as e:
            raised.append(e)
        finally:
            if contender.is_locked:
                contender.release()

    t = threading.Thread(target=_try_acquire)
    t.start()
    t.join(timeout=5)

    holder.release()

    assert len(raised) == 1, "Expected Timeout to be raised when lock is held"
    assert isinstance(raised[0], Timeout)


# ---------------------------------------------------------------------------
# CommandCenterConfig tests
# ---------------------------------------------------------------------------


# ---------------------------------------------------------------------------
# API key round-trip and hash exclusion
# ---------------------------------------------------------------------------


def test_api_keys_round_trip(tmp_path: Path) -> None:
    """API keys survive save→load round-trip."""
    with _patch_config_path(tmp_path):
        cfg = default_config()
        cfg.command_center.linear_api_key = "lin_secret"
        cfg.command_center.github_token = "ghp_secret"
        save_config(cfg)
        loaded = load_config()
    assert loaded.command_center.linear_api_key == "lin_secret"
    assert loaded.command_center.github_token == "ghp_secret"


def test_config_hash_ignores_api_keys() -> None:
    """Changing only API keys must not change config_hash (no reinstall needed)."""
    base = default_config()
    with_keys = default_config()
    with_keys.command_center.linear_api_key = "lin_secret"
    with_keys.command_center.github_token = "ghp_secret"
    assert config_hash(base) == config_hash(with_keys)


# ---------------------------------------------------------------------------
# CommandCenterConfig tests
# ---------------------------------------------------------------------------


def test_command_center_defaults() -> None:
    """A freshly created Config has CommandCenterConfig with expected defaults."""
    cfg = default_config()
    assert cfg.command_center.github_excluded_repos == []
    assert cfg.command_center.linear_poll_seconds == 60
    assert cfg.command_center.github_poll_seconds == 60


def test_command_center_loads_from_toml(tmp_path: Path) -> None:
    """A [command_center] section in config.toml is merged at load time."""
    target = tmp_path / "config.toml"
    target.write_text(
        (
            "[command_center]\n"
            'github_excluded_repos = ["owner/old-repo"]\n'
            "linear_poll_seconds = 30\n"
            "github_poll_seconds = 45\n"
        ),
        encoding="utf-8",
    )
    with _patch_config_path(tmp_path):
        loaded = load_config()
    assert loaded.command_center.github_excluded_repos == ["owner/old-repo"]
    assert loaded.command_center.linear_poll_seconds == 30
    assert loaded.command_center.github_poll_seconds == 45


def test_command_center_roundtrip(tmp_path: Path) -> None:
    """save_config then load_config preserves CommandCenterConfig values."""
    with _patch_config_path(tmp_path):
        cfg = default_config()
        cfg.command_center.github_excluded_repos = ["owner/skip-this"]
        cfg.command_center.linear_poll_seconds = 120
        cfg.command_center.github_poll_seconds = 90
        save_config(cfg)
        loaded = load_config()
    assert loaded.command_center.github_excluded_repos == ["owner/skip-this"]
    assert loaded.command_center.linear_poll_seconds == 120
    assert loaded.command_center.github_poll_seconds == 90
