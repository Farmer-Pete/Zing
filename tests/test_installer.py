"""Tests for the Claude Code and OpenCode installers."""

from __future__ import annotations

import importlib.resources
import os
import re
import stat
from pathlib import Path
from unittest.mock import patch

import pytest

from zing_ai.installer import InstallError, install_claude, install_claude_hooks, install_opencode

# All markdown files that should be installed, relative to the target dir.
EXPECTED_FILES = [
    "zing.md",
    "zing/new.md",
    "zing/build.md",
    "zing/plan.md",
    "zing/plan-audit.md",
    "zing/build-audit.md",
    "zing/pr-audit.md",
    "zing/plan-linear.md",
    "zing/custom-audit.md",
    "zing/pr-respond.md",
    "zing/_shared/review-core.md",
    "zing/pr-audit-visual.md",
    "zing/learn.md",
    "zing/sim-viz.md",
]

# Directories that should be created.
EXPECTED_DIRS = [
    "zing",
    "zing/_shared",
]


# ---------------------------------------------------------------------------
# Claude Code installer
# ---------------------------------------------------------------------------


def test_claude_creates_directory_structure(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_claude(target_dir=target)

    for d in EXPECTED_DIRS:
        assert (target / d).is_dir(), f"Expected directory missing: {d}"


def test_claude_all_expected_files_exist(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_claude(target_dir=target)

    for f in EXPECTED_FILES:
        assert (target / f).is_file(), f"Expected file missing: {f}"


def test_claude_no_extra_files(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_claude(target_dir=target)

    installed: set[str] = set()
    for root, _dirs, files in os.walk(target):
        for name in files:
            rel = os.path.relpath(os.path.join(root, name), target)
            installed.add(rel)

    expected_set = {f.replace("/", os.sep) for f in EXPECTED_FILES}
    expected_set.add("zing-manifest.json")
    assert installed == expected_set


def test_claude_file_contents_match_source(tmp_path: Path) -> None:
    from zing_ai.config import default_config
    from zing_ai.templating import render_template

    target = tmp_path / "commands"
    cfg = default_config()
    install_claude(target_dir=target, config=cfg)

    commands_root = importlib.resources.files("zing_ai.commands")

    src_content = render_template(
        commands_root.joinpath("zing.md").read_text(encoding="utf-8"), cfg
    )
    dst_content = (target / "zing.md").read_text(encoding="utf-8")
    assert src_content == dst_content, "zing.md content mismatch"

    src_content = render_template(
        commands_root.joinpath("zing").joinpath("build.md").read_text(encoding="utf-8"), cfg
    )
    dst_content = (target / "zing" / "build.md").read_text(encoding="utf-8")
    assert src_content == dst_content, "zing/build.md content mismatch"

    src_content = render_template(
        commands_root.joinpath("_shared").joinpath("review-core.md").read_text(encoding="utf-8"),
        cfg,
    )
    dst_content = (target / "zing" / "_shared" / "review-core.md").read_text(encoding="utf-8")
    assert src_content == dst_content, "zing/_shared/review-core.md content mismatch"


def test_claude_idempotent_install(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_claude(target_dir=target)
    install_claude(target_dir=target)

    for f in EXPECTED_FILES:
        assert (target / f).is_file(), f"Expected file missing after re-install: {f}"


# ---------------------------------------------------------------------------
# Claude Code installer errors
# ---------------------------------------------------------------------------


def test_claude_unwritable_directory_raises_install_error(tmp_path: Path) -> None:
    unwritable = tmp_path / "locked"
    unwritable.mkdir()
    os.chmod(unwritable, stat.S_IRUSR | stat.S_IXUSR)

    target = unwritable / "commands"
    try:
        with pytest.raises(InstallError):
            install_claude(target_dir=target)
    finally:
        os.chmod(unwritable, stat.S_IRWXU)


def test_claude_unwritable_directory_leaves_no_partial_files(tmp_path: Path) -> None:
    unwritable = tmp_path / "locked"
    unwritable.mkdir()
    os.chmod(unwritable, stat.S_IRUSR | stat.S_IXUSR)

    target = unwritable / "commands"
    try:
        with pytest.raises(InstallError):
            install_claude(target_dir=target)
        assert not target.exists(), "Partial directory left behind"
    finally:
        os.chmod(unwritable, stat.S_IRWXU)


def test_claude_partial_install_cleanup(tmp_path: Path) -> None:
    target = tmp_path / "commands"

    real_write_text = Path.write_text
    write_count = 0

    def patched_write_text(self: Path, data: str, encoding: str = "utf-8") -> int:
        nonlocal write_count
        if str(self).startswith(str(target)) and self.suffix == ".md":
            write_count += 1
            if write_count > 2:
                raise OSError("Simulated disk full")
        return real_write_text(self, data, encoding=encoding)

    with patch.object(Path, "write_text", patched_write_text), pytest.raises(InstallError):
        install_claude(target_dir=target)

    md_files = list(target.rglob("*.md")) if target.exists() else []
    assert md_files == [], f"Partial files left behind: {md_files}"


# ---------------------------------------------------------------------------
# OpenCode installer
# ---------------------------------------------------------------------------

OPENCODE_EXPECTED_FILES = [
    "zing.md",
    "zing-new.md",
    "zing-build.md",
    "zing-plan.md",
    "zing-plan-audit.md",
    "zing-build-audit.md",
    "zing-pr-audit.md",
    "zing-plan-linear.md",
    "zing-custom-audit.md",
    "zing-pr-respond.md",
    "_shared/review-core.md",
    "zing-pr-audit-visual.md",
    "zing-learn.md",
    "zing-sim-viz.md",
]

OPENCODE_EXPECTED_DIRS = [
    "_shared",
]


def test_opencode_creates_directory_structure(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    for d in OPENCODE_EXPECTED_DIRS:
        assert (target / d).is_dir(), f"Expected directory missing: {d}"


def test_opencode_all_expected_files_exist(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    for f in OPENCODE_EXPECTED_FILES:
        assert (target / f).is_file(), f"Expected file missing: {f}"


def test_opencode_no_extra_files(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    installed: set[str] = set()
    for root, _dirs, files in os.walk(target):
        for name in files:
            rel = os.path.relpath(os.path.join(root, name), target)
            installed.add(rel)

    expected_set = {f.replace("/", os.sep) for f in OPENCODE_EXPECTED_FILES}
    expected_set.add("zing-manifest.json")
    assert installed == expected_set


def test_opencode_flat_naming_scheme(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    assert not (target / "zing").exists(), "zing/ subdirectory should not exist in OpenCode layout"
    assert (target / "zing-build.md").is_file()
    assert (target / "zing-plan.md").is_file()


def test_opencode_content_is_converted(tmp_path: Path) -> None:
    from zing_ai.config import default_config
    from zing_ai.converter import convert_for_opencode
    from zing_ai.templating import render_template

    target = tmp_path / "commands"
    cfg = default_config()
    install_opencode(target_dir=target, config=cfg)

    commands_root = importlib.resources.files("zing_ai.commands")

    src_content = render_template(
        commands_root.joinpath("zing.md").read_text(encoding="utf-8"), cfg
    )
    expected = convert_for_opencode(src_content)
    actual = (target / "zing.md").read_text(encoding="utf-8")
    assert actual == expected, "zing.md content not converted"

    src_content = render_template(
        commands_root.joinpath("zing").joinpath("build.md").read_text(encoding="utf-8"), cfg
    )
    expected = convert_for_opencode(src_content)
    actual = (target / "zing-build.md").read_text(encoding="utf-8")
    assert actual == expected, "zing-build.md content not converted"

    src_content = render_template(
        commands_root.joinpath("_shared").joinpath("review-core.md").read_text(encoding="utf-8"),
        cfg,
    )
    expected = convert_for_opencode(src_content)
    actual = (target / "_shared" / "review-core.md").read_text(encoding="utf-8")
    assert actual == expected, "_shared/review-core.md content not converted"


def test_opencode_tool_names_are_lowercase(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    claude_tool_names = ["TaskCreate", "TaskUpdate", "Bash", "Read", "Grep"]
    for f in OPENCODE_EXPECTED_FILES:
        content = (target / f).read_text(encoding="utf-8")
        for tool in claude_tool_names:
            if re.search(rf"\b{tool}\b", content):
                pytest.fail(f"Claude tool name '{tool}' found unconverted in {f}")


def test_opencode_idempotent_install(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_opencode(target_dir=target)
    install_opencode(target_dir=target)

    for f in OPENCODE_EXPECTED_FILES:
        assert (target / f).is_file(), f"Expected file missing after re-install: {f}"


# ---------------------------------------------------------------------------
# OpenCode installer errors
# ---------------------------------------------------------------------------


def test_opencode_unwritable_directory_raises_install_error(tmp_path: Path) -> None:
    unwritable = tmp_path / "locked"
    unwritable.mkdir()
    os.chmod(unwritable, stat.S_IRUSR | stat.S_IXUSR)

    target = unwritable / "commands"
    try:
        with pytest.raises(InstallError):
            install_opencode(target_dir=target)
    finally:
        os.chmod(unwritable, stat.S_IRWXU)


def test_opencode_unwritable_directory_leaves_no_partial_files(tmp_path: Path) -> None:
    unwritable = tmp_path / "locked"
    unwritable.mkdir()
    os.chmod(unwritable, stat.S_IRUSR | stat.S_IXUSR)

    target = unwritable / "commands"
    try:
        with pytest.raises(InstallError):
            install_opencode(target_dir=target)
        assert not target.exists(), "Partial directory left behind"
    finally:
        os.chmod(unwritable, stat.S_IRWXU)


def test_opencode_partial_install_cleanup(tmp_path: Path) -> None:
    target = tmp_path / "commands"

    real_write_text = Path.write_text
    write_count = 0

    def patched_write_text(self: Path, data: str, encoding: str = "utf-8") -> int:
        nonlocal write_count
        if str(self).startswith(str(target)) and self.suffix == ".md":
            write_count += 1
            if write_count > 2:
                raise OSError("Simulated disk full")
        return real_write_text(self, data, encoding=encoding)

    with patch.object(Path, "write_text", patched_write_text), pytest.raises(InstallError):
        install_opencode(target_dir=target)

    md_files = list(target.rglob("*.md")) if target.exists() else []
    assert md_files == [], f"Partial files left behind: {md_files}"


# ---------------------------------------------------------------------------
# MCP registration integration
# ---------------------------------------------------------------------------


def test_claude_install_calls_register_mcp_server(tmp_path: Path) -> None:
    """install_claude() calls register_mcp_server('claude') after writing manifest."""
    target = tmp_path / "commands"
    with patch("zing_ai.installer.register_mcp_server") as mock_reg:
        install_claude(target_dir=target)

    mock_reg.assert_called_once_with("claude")


def test_opencode_install_calls_register_mcp_server(tmp_path: Path) -> None:
    """install_opencode() calls register_mcp_server('opencode') after writing manifest."""
    target = tmp_path / "commands"
    with patch("zing_ai.installer.register_mcp_server") as mock_reg:
        install_opencode(target_dir=target)

    mock_reg.assert_called_once_with("opencode")


# ---------------------------------------------------------------------------
# Jinja rendering tests
# ---------------------------------------------------------------------------


def test_render_substitutes_jinja_token(tmp_path: Path) -> None:
    """_copy_resource_file renders Jinja tokens when config is provided."""
    from zing_ai.config import default_config
    from zing_ai.installer import _copy_resource_file

    # Build a fake source file with a Jinja token.
    src_file = tmp_path / "test.md"
    src_file.write_text("value={{ thresholds.large_file_lines }}", encoding="utf-8")

    dst_file = tmp_path / "out.md"
    cfg = default_config()

    _copy_resource_file(src_file, dst_file, [], config=cfg)

    result = dst_file.read_text(encoding="utf-8")
    assert result == "value=1000", f"Unexpected output: {result!r}"


def test_render_runs_before_opencode_convert(tmp_path: Path) -> None:
    """_copy_resource_file_converted renders Jinja first, then converts tool names."""
    from zing_ai.config import default_config
    from zing_ai.converter import convert_for_opencode
    from zing_ai.installer import _copy_resource_file_converted

    # Content has both a Jinja token AND a Claude Code tool name that the
    # converter rewrites (Bash -> bash).
    src_file = tmp_path / "test.md"
    src_file.write_text(
        "lines={{ thresholds.large_file_lines }} use Bash here",
        encoding="utf-8",
    )

    dst_file = tmp_path / "out.md"
    cfg = default_config()

    _copy_resource_file_converted(src_file, dst_file, convert_for_opencode, [], config=cfg)

    result = dst_file.read_text(encoding="utf-8")
    # Rendered token survives.
    assert "1000" in result, f"Jinja token not rendered in: {result!r}"
    # Converter ran: Bash -> bash.
    assert "bash" in result, f"Converter did not run (no 'bash') in: {result!r}"
    assert "Bash" not in result, f"Converter did not replace 'Bash' in: {result!r}"


# ---------------------------------------------------------------------------
# _source_mtime_max tests
# ---------------------------------------------------------------------------


def test_source_mtime_max_real_path(tmp_path: Path) -> None:
    """Returns the max mtime across files in a real filesystem tree."""
    from zing_ai.installer import _source_mtime_max

    src = tmp_path / "src"
    src.mkdir()
    file_a = src / "a.md"
    file_b = src / "b.md"
    file_a.write_text("a", encoding="utf-8")
    file_b.write_text("b", encoding="utf-8")

    os.utime(file_a, (1000.0, 1000.0))
    os.utime(file_b, (2000.0, 2000.0))

    result = _source_mtime_max(src.iterdir())
    assert result == 2000.0


def test_source_mtime_max_returns_none_on_oserror(tmp_path: Path) -> None:
    """Returns None when stat() raises OSError (e.g. wheel/zip install)."""
    from unittest.mock import patch

    from zing_ai.installer import _source_mtime_max

    with patch.object(Path, "stat", side_effect=OSError("no stat")):
        result = _source_mtime_max([tmp_path / "anything"])
    assert result is None


# ---------------------------------------------------------------------------
# is_install_stale tests
# ---------------------------------------------------------------------------


def test_stale_when_manifest_missing(tmp_path: Path) -> None:
    """Returns True when no manifest exists in the target directory."""
    from zing_ai.config import default_config
    from zing_ai.installer import is_install_stale

    assert is_install_stale(tmp_path, default_config()) is True


def test_stale_when_config_hash_differs(tmp_path: Path) -> None:
    """Returns True when manifest config_hash doesn't match current config."""
    from zing_ai import __version__
    from zing_ai.config import default_config
    from zing_ai.installer import is_install_stale
    from zing_ai.manifest import write_manifest

    write_manifest(
        tmp_path,
        "claude-code",
        [],
        config_hash="other_hash",
        source_mtime_max=None,
        package_version=__version__,
    )
    assert is_install_stale(tmp_path, default_config()) is True


def test_stale_when_package_version_differs(tmp_path: Path) -> None:
    """Returns True when manifest package_version doesn't match installed version."""
    from zing_ai.config import config_hash, default_config
    from zing_ai.installer import is_install_stale
    from zing_ai.manifest import write_manifest

    cfg = default_config()
    write_manifest(
        tmp_path,
        "claude-code",
        [],
        config_hash=config_hash(cfg),
        source_mtime_max=None,
        package_version="not_current",
    )
    assert is_install_stale(tmp_path, cfg) is True


def test_fresh_when_all_match(tmp_path: Path, monkeypatch) -> None:
    """Returns False when config hash, version, and mtime all match (wheel path)."""
    from zing_ai import __version__, installer
    from zing_ai.config import config_hash, default_config
    from zing_ai.installer import is_install_stale
    from zing_ai.manifest import write_manifest

    cfg = default_config()
    # Force the wheel-install path: current_mtime is None.
    monkeypatch.setattr(installer, "_source_mtime_max", lambda *_a, **_kw: None)
    write_manifest(
        tmp_path,
        "claude-code",
        [],
        config_hash=config_hash(cfg),
        source_mtime_max=None,
        package_version=__version__,
    )
    assert is_install_stale(tmp_path, cfg) is False


def test_stale_when_source_mtime_advances(tmp_path: Path) -> None:
    """Returns True when manifest source_mtime_max is older than current source files."""
    from zing_ai import __version__
    from zing_ai.config import config_hash, default_config
    from zing_ai.installer import is_install_stale
    from zing_ai.manifest import write_manifest

    cfg = default_config()
    write_manifest(
        tmp_path,
        "claude-code",
        [],
        config_hash=config_hash(cfg),
        source_mtime_max=1.0,  # epoch second — far in the past
        package_version=__version__,
    )
    assert is_install_stale(tmp_path, cfg) is True


def test_fresh_when_mtime_matches(tmp_path: Path, monkeypatch) -> None:
    """Equality boundary: stored == current → False (would catch a `>=` typo)."""
    from zing_ai import __version__, installer
    from zing_ai.config import config_hash, default_config
    from zing_ai.installer import is_install_stale
    from zing_ai.manifest import write_manifest

    cfg = default_config()
    monkeypatch.setattr(installer, "_source_mtime_max", lambda *_a, **_kw: 12345.0)
    write_manifest(
        tmp_path,
        "claude-code",
        [],
        config_hash=config_hash(cfg),
        source_mtime_max=12345.0,
        package_version=__version__,
    )
    assert is_install_stale(tmp_path, cfg) is False


def test_stale_when_stored_mtime_missing_but_current_exists(tmp_path: Path, monkeypatch) -> None:
    """An older manifest without source_mtime_max forces a reinstall."""
    from zing_ai import __version__, installer
    from zing_ai.config import config_hash, default_config
    from zing_ai.installer import is_install_stale
    from zing_ai.manifest import write_manifest

    cfg = default_config()
    monkeypatch.setattr(installer, "_source_mtime_max", lambda *_a, **_kw: 12345.0)
    write_manifest(
        tmp_path,
        "claude-code",
        [],
        config_hash=config_hash(cfg),
        source_mtime_max=None,
        package_version=__version__,
    )
    assert is_install_stale(tmp_path, cfg) is True


# ---------------------------------------------------------------------------
# Hook installation tests
# ---------------------------------------------------------------------------


def test_hook_script_is_installed(tmp_path: Path) -> None:
    """install_claude_hooks() copies notify-zing.sh to the hooks directory."""
    hooks_dir = tmp_path / "hooks"
    settings_path = tmp_path / "settings.json"
    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)

    assert (hooks_dir / "notify-zing.sh").is_file(), "notify-zing.sh not installed"


def test_hook_script_is_executable(tmp_path: Path) -> None:
    """notify-zing.sh must be executable after install."""
    hooks_dir = tmp_path / "hooks"
    settings_path = tmp_path / "settings.json"
    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)

    hook = hooks_dir / "notify-zing.sh"
    mode = hook.stat().st_mode
    assert mode & 0o100, "notify-zing.sh is not owner-executable"


def test_hook_script_content_matches_source(tmp_path: Path) -> None:
    """The installed hook script content must match the bundled source."""
    import importlib.resources

    hooks_dir = tmp_path / "hooks"
    settings_path = tmp_path / "settings.json"
    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)

    src = (
        importlib.resources.files("zing_ai")
        .joinpath("hooks")
        .joinpath("notify-zing.sh")
        .read_bytes()
    )
    dst = (hooks_dir / "notify-zing.sh").read_bytes()
    assert src == dst, "Installed hook content differs from source"


def test_settings_pretooluse_entry_added(tmp_path: Path) -> None:
    """install_claude_hooks() adds PreToolUse AskUserQuestion hook to settings.json."""
    import json

    hooks_dir = tmp_path / "hooks"
    settings_path = tmp_path / "settings.json"
    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)

    settings = json.loads(settings_path.read_text(encoding="utf-8"))
    pretooluse = settings.get("hooks", {}).get("PreToolUse", [])
    matchers = [e.get("matcher") for e in pretooluse if isinstance(e, dict)]
    assert "AskUserQuestion" in matchers, "PreToolUse AskUserQuestion entry not found"


def test_settings_existing_hooks_preserved(tmp_path: Path) -> None:
    """Existing hooks in settings.json are not overwritten."""
    import json

    hooks_dir = tmp_path / "hooks"
    settings_path = tmp_path / "settings.json"

    # Write pre-existing settings with a different hook.
    existing = {
        "hooks": {
            "PreToolUse": [
                {
                    "matcher": "Bash",
                    "hooks": [{"type": "command", "command": "echo hello"}],
                }
            ]
        }
    }
    settings_path.write_text(json.dumps(existing), encoding="utf-8")

    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)

    settings = json.loads(settings_path.read_text(encoding="utf-8"))
    pretooluse = settings.get("hooks", {}).get("PreToolUse", [])
    matchers = [e.get("matcher") for e in pretooluse if isinstance(e, dict)]
    assert "Bash" in matchers, "Existing Bash hook was removed"
    assert "AskUserQuestion" in matchers, "AskUserQuestion hook was not added"


def test_settings_idempotent_no_duplicate(tmp_path: Path) -> None:
    """Running install_claude_hooks() twice does not duplicate the PreToolUse entry."""
    import json

    hooks_dir = tmp_path / "hooks"
    settings_path = tmp_path / "settings.json"

    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)
    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)

    settings = json.loads(settings_path.read_text(encoding="utf-8"))
    pretooluse = settings.get("hooks", {}).get("PreToolUse", [])
    ask_entries = [
        e for e in pretooluse if isinstance(e, dict) and e.get("matcher") == "AskUserQuestion"
    ]
    assert len(ask_entries) == 1, f"Expected 1 AskUserQuestion entry, found {len(ask_entries)}"


def test_settings_created_from_scratch(tmp_path: Path) -> None:
    """install_claude_hooks() creates settings.json if it doesn't exist."""
    import json

    hooks_dir = tmp_path / "hooks"
    settings_path = tmp_path / "nonexistent" / "settings.json"

    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)

    assert settings_path.exists(), "settings.json was not created"
    settings = json.loads(settings_path.read_text(encoding="utf-8"))
    assert "hooks" in settings


def test_install_claude_calls_install_hooks(tmp_path: Path) -> None:
    """install_claude() calls install_claude_hooks() as part of the install flow."""
    target = tmp_path / "commands"
    with (
        patch("zing_ai.installer.install_claude_hooks") as mock_hooks,
        patch("zing_ai.installer.register_mcp_server"),
    ):
        install_claude(target_dir=target)

    mock_hooks.assert_called_once()


def test_idle_hook_script_is_installed(tmp_path: Path) -> None:
    """install_claude_hooks() copies notify-zing-idle.sh to the hooks directory."""
    hooks_dir = tmp_path / "hooks"
    settings_path = tmp_path / "settings.json"
    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)

    assert (hooks_dir / "notify-zing-idle.sh").is_file(), "notify-zing-idle.sh not installed"


def test_idle_hook_script_is_executable(tmp_path: Path) -> None:
    """notify-zing-idle.sh must be executable after install."""
    hooks_dir = tmp_path / "hooks"
    settings_path = tmp_path / "settings.json"
    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)

    hook = hooks_dir / "notify-zing-idle.sh"
    mode = hook.stat().st_mode
    assert mode & 0o100, "notify-zing-idle.sh is not owner-executable"


def test_settings_notification_entry_added(tmp_path: Path) -> None:
    """install_claude_hooks() adds a Notification hook running notify-zing-idle.sh."""
    import json

    hooks_dir = tmp_path / "hooks"
    settings_path = tmp_path / "settings.json"
    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)

    settings = json.loads(settings_path.read_text(encoding="utf-8"))
    notification_list = settings.get("hooks", {}).get("Notification", [])
    commands = [
        h.get("command")
        for entry in notification_list
        if isinstance(entry, dict)
        for h in (entry.get("hooks") or [])
        if isinstance(h, dict)
    ]
    assert "~/.claude/hooks/notify-zing-idle.sh" in commands, (
        f"Notification hook for idle script not found; got {commands}"
    )


def test_settings_notification_idempotent(tmp_path: Path) -> None:
    """Running install_claude_hooks() twice does not duplicate the Notification entry."""
    import json

    hooks_dir = tmp_path / "hooks"
    settings_path = tmp_path / "settings.json"

    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)
    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)

    settings = json.loads(settings_path.read_text(encoding="utf-8"))
    notification_list = settings.get("hooks", {}).get("Notification", [])
    matching = [
        entry
        for entry in notification_list
        if isinstance(entry, dict)
        and any(
            isinstance(h, dict) and h.get("command") == "~/.claude/hooks/notify-zing-idle.sh"
            for h in (entry.get("hooks") or [])
        )
    ]
    assert len(matching) == 1, f"Expected 1 idle Notification entry, found {len(matching)}"


def test_settings_existing_notification_preserved(tmp_path: Path) -> None:
    """Pre-existing Notification entries are not overwritten."""
    import json

    hooks_dir = tmp_path / "hooks"
    settings_path = tmp_path / "settings.json"

    existing = {
        "hooks": {
            "Notification": [
                {"hooks": [{"type": "command", "command": "echo other"}]},
            ]
        }
    }
    settings_path.write_text(json.dumps(existing), encoding="utf-8")

    install_claude_hooks(hooks_dir=hooks_dir, settings_path=settings_path)

    settings = json.loads(settings_path.read_text(encoding="utf-8"))
    notification_list = settings.get("hooks", {}).get("Notification", [])
    commands = [
        h.get("command")
        for entry in notification_list
        if isinstance(entry, dict)
        for h in (entry.get("hooks") or [])
        if isinstance(h, dict)
    ]
    assert "echo other" in commands, "Pre-existing Notification hook was removed"
    assert "~/.claude/hooks/notify-zing-idle.sh" in commands, "Idle hook was not added"
