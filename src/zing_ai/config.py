"""User configuration for zing-ai, loaded from ~/.config/zing-ai/config.toml."""

from __future__ import annotations

import hashlib
import json
import tomllib
from pathlib import Path
from typing import Literal

import tomli_w
from filelock import FileLock
from pydantic import BaseModel, Field, ValidationError


class ConfigError(Exception):
    """Raised when config.toml cannot be loaded or validated."""


class ThresholdsConfig(BaseModel):
    large_file_lines: int = 1000
    branch_name_max_length: int = 60
    simple_spec_max_words: int = 150
    plan_small_step_count: int = 3
    step_merge_min_words: int = 20
    step_merge_max_words: int = 40
    small_diff_max_files: int = 5
    small_diff_max_lines: int = 100
    audit_scope_small_lines: int = 2000
    audit_scope_medium_lines: int = 5000
    audit_always_read_lines: int = 200
    scope_max_files: int = 50
    scope_narrow_target: int = 25
    scope_slug_max_length: int = 30
    comment_truncation_chars: int = 100
    browser_wait_timeout_seconds: int = 10


class ModelsConfig(BaseModel):
    plan_exploration: str = "sonnet"
    plan_audit: str = "sonnet"
    build_step: str = "sonnet"
    review_agents_1_3: str = ""
    review_agents_4_6: str = "sonnet"


class GitConfig(BaseModel):
    branch_prefix: str = "zing/"
    coauthor_trailer: str = "Co-Authored-By: Zing <zing@farmerpete.net>"
    workflow_mode: Literal["branch", "worktree", "none", "ask"] = "branch"
    worktree_root: str = "../{repo}-{branch}"


class AgentsConfig(BaseModel):
    plan_exploration_count: int = 4
    plan_audit_count: int = 4
    review_small_diff_count: int = 2
    review_large_diff_count: int = 6


class ReportConfig(BaseModel):
    datetime_format: str = "%Y-%m-%d-%H%M"


class Config(BaseModel):
    thresholds: ThresholdsConfig = Field(default_factory=ThresholdsConfig)
    models: ModelsConfig = Field(default_factory=ModelsConfig)
    git: GitConfig = Field(default_factory=GitConfig)
    agents: AgentsConfig = Field(default_factory=AgentsConfig)
    report: ReportConfig = Field(default_factory=ReportConfig)


def config_path() -> Path:
    """Return the canonical path to the zing-ai config file."""
    return Path.home() / ".config" / "zing-ai" / "config.toml"


def default_config() -> Config:
    """Return a Config instance with all default values."""
    return Config()


def load_config() -> Config:
    """Load config from disk, returning defaults if the file does not exist.

    Raises:
        ConfigError: If the file exists but contains invalid TOML or fails validation.
    """
    path = config_path()
    if not path.exists():
        return default_config()

    text = path.read_text(encoding="utf-8")
    try:
        data = tomllib.loads(text)
    except tomllib.TOMLDecodeError as e:
        raise ConfigError(f"config.toml is not valid TOML: {e}") from e

    try:
        return Config.model_validate(data)
    except ValidationError as e:
        err = e.errors()[0]
        loc = ".".join(str(p) for p in err["loc"])
        raise ConfigError(
            f"config.toml field {loc} is invalid: expected {err['type']}, got {err['input']!r}"
        ) from e


def save_config(cfg: Config) -> None:
    """Write cfg to disk as TOML, using a file lock to prevent concurrent writes.

    Raises:
        filelock.Timeout: If the lock cannot be acquired within 5 seconds.
    """
    path = config_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    lock = FileLock(str(path) + ".lock", timeout=5)
    with lock:
        path.write_text(tomli_w.dumps(cfg.model_dump()), encoding="utf-8")


def config_hash(cfg: Config) -> str:
    """Return a stable SHA-256 hex digest of the config's serialised values."""
    payload = json.dumps(cfg.model_dump(), sort_keys=True, default=str).encode()
    return hashlib.sha256(payload).hexdigest()
