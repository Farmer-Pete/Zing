"""User configuration for zing-ai, loaded from ~/.config/zing-ai/config.toml."""

from __future__ import annotations

import contextlib
import hashlib
import json
import os
import tempfile
import tomllib
from pathlib import Path
from typing import Annotated, Literal

import tomli_w
from filelock import FileLock
from pydantic import BaseModel, Field, ValidationError


class ConfigError(Exception):
    """Raised when config.toml cannot be loaded or validated."""


PositiveInt = Annotated[int, Field(ge=1)]


class ThresholdsConfig(BaseModel):
    # File reading
    large_file_lines: PositiveInt = 1000
    audit_always_read_lines: PositiveInt = 200
    # Naming limits
    branch_name_max_length: PositiveInt = 60
    scope_slug_max_length: PositiveInt = 30
    # Planning
    simple_spec_max_words: PositiveInt = 150
    plan_small_step_count: PositiveInt = 3
    step_merge_min_words: PositiveInt = 100
    step_merge_max_words: PositiveInt = 300
    # Diff & audit sizing
    small_diff_max_files: PositiveInt = 5
    small_diff_max_lines: PositiveInt = 100
    audit_scope_small_lines: PositiveInt = 2000
    audit_scope_medium_lines: PositiveInt = 5000
    # Scope
    scope_max_files: PositiveInt = 50
    scope_narrow_target: PositiveInt = 25
    # Misc
    comment_truncation_chars: PositiveInt = 100
    browser_wait_timeout_seconds: PositiveInt = 10


class ModelsConfig(BaseModel):
    plan_exploration: str = "sonnet"
    plan_audit: str = "sonnet"
    build_step: str = "sonnet"
    review_agents_1_3: str = ""
    review_agents_4_6: str = "sonnet"


class GitConfig(BaseModel):
    workflow_mode: Literal["branch", "worktree", "none", "ask"] = "branch"
    branch_prefix: str = "zing/"
    worktree_root: str = "../{repo}-{branch}"
    zing_init_script: str = ".zing-init.sh"
    code_dir: str = ""


class AgentsConfig(BaseModel):
    plan_exploration_count: int = 4
    plan_audit_count: int = 4
    review_small_diff_count: int = 2
    review_large_diff_count: int = 6


class ReportConfig(BaseModel):
    datetime_format: str = "%Y-%m-%d-%H%M"


PollSeconds = Annotated[int, Field(ge=10)]


class CommandCenterConfig(BaseModel):
    linear_api_key: str = ""
    github_token: str = ""
    github_excluded_repos: list[str] = Field(default_factory=list)
    poll_seconds: PollSeconds = 60
    claude_flags: str = ""
    iterm2_integration: bool = False


class Config(BaseModel):
    thresholds: ThresholdsConfig = Field(default_factory=ThresholdsConfig)
    models: ModelsConfig = Field(default_factory=ModelsConfig)
    git: GitConfig = Field(default_factory=GitConfig)
    agents: AgentsConfig = Field(default_factory=AgentsConfig)
    report: ReportConfig = Field(default_factory=ReportConfig)
    command_center: CommandCenterConfig = Field(default_factory=CommandCenterConfig)


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
        errors = e.errors()
        if not errors:
            raise ConfigError("config.toml failed validation") from e
        err = errors[0]
        loc = ".".join(str(p) for p in err.get("loc", ()))
        err_type = err.get("type", "value")
        err_input = err.get("input", "<unknown>")
        raise ConfigError(
            f"config.toml field {loc} is invalid: expected {err_type}, got {err_input!r}"
        ) from e


def save_config(cfg: Config) -> None:
    """Write cfg to disk as TOML, using a file lock to prevent concurrent writes.

    Raises:
        filelock.Timeout: If the lock cannot be acquired within 5 seconds.
    """
    path = config_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    lock = FileLock(str(path) + ".lock", timeout=5)
    data = cfg.model_dump(exclude_defaults=True)
    # Drop empty sub-tables so we don't write bare `[section]` headers.
    data = {k: v for k, v in data.items() if v != {}}
    with lock:
        # Atomic write: tempfile in same dir, then os.replace().
        fd, tmp_path = tempfile.mkstemp(prefix=path.name + ".", suffix=".tmp", dir=str(path.parent))
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                f.write(tomli_w.dumps(data))
                f.flush()
                os.fsync(f.fileno())
            os.replace(tmp_path, path)
        except Exception:
            with contextlib.suppress(FileNotFoundError):
                os.unlink(tmp_path)
            raise


_HASH_EXCLUDE = {"command_center": {"linear_api_key", "github_token", "github_excluded_repos"}}


def config_hash(cfg: Config) -> str:
    """Return a stable SHA-256 hex digest of the config's serialised values.

    Fields listed in ``_HASH_EXCLUDE`` are stripped before hashing so that
    credential changes do not trigger a "reinstall needed" banner.
    """
    data = cfg.model_dump()
    for section, keys in _HASH_EXCLUDE.items():
        if section in data:
            for key in keys:
                data[section].pop(key, None)
    payload = json.dumps(data, sort_keys=True, default=str).encode()
    return hashlib.sha256(payload).hexdigest()
