"""Installer logic for copying bundled command files to target directories."""

from __future__ import annotations

import importlib.resources
import json
import logging
import shutil
import subprocess
import sys
from collections.abc import Callable
from importlib.resources.abc import Traversable
from pathlib import Path

logger = logging.getLogger(__name__)

# Files that live directly in the commands/ package root.
# zing.md is special: it installs one level up from the zing/ subdirectory.
_TOP_LEVEL_FILE = "zing.md"

# Subdirectories within the commands package to copy recursively.
_SUBDIRS = ("zing", "_shared")


def install_claude(target_dir: Path | None = None) -> None:
    """Install Zing command files for Claude Code.

    Copies the bundled markdown command files to the Claude Code commands
    directory.  The default target is ``~/.claude/commands``.

    Parameters
    ----------
    target_dir:
        Override for the commands directory.  Useful for testing.

    Raises
    ------
    SystemExit
        On any I/O error (permissions, disk full, etc.).  Partial files are
        cleaned up before exiting.
    """
    if target_dir is None:
        target_dir = Path.home() / ".claude" / "commands"

    logger.info("Installing Claude Code commands to %s", target_dir)

    # Back up any user-modified files before overwriting.
    from zing_ai.backup import backup_modified_files

    backed_up = backup_modified_files(target_dir)
    for relpath, backup_path in backed_up:
        print(f"  Backed up modified file: {relpath} -> {backup_path}")

    commands_root = importlib.resources.files("zing_ai.commands")
    logger.debug("Commands package root: %s", commands_root)

    # Track files and directories created during this run so we can roll back
    # on failure.
    created_files: list[Path] = []
    created_dirs: list[Path] = []

    try:
        # -- 1. Ensure the top-level target directory exists ------------------
        _ensure_dir(target_dir, created_dirs)

        # -- 2. Copy the top-level zing.md ------------------------------------
        src_top = commands_root.joinpath(_TOP_LEVEL_FILE)
        dst_top = target_dir / _TOP_LEVEL_FILE
        _copy_resource_file(src_top, dst_top, created_files)

        # -- 3. Copy subdirectories (zing/, _shared/) ------------------------
        for subdir_name in _SUBDIRS:
            src_subdir = commands_root.joinpath(subdir_name)
            dst_subdir = (
                target_dir / "zing" / subdir_name
                if subdir_name == "_shared"
                else target_dir / subdir_name
            )
            _copy_resource_tree(src_subdir, dst_subdir, created_files, created_dirs)

    except SystemExit:
        raise
    except Exception as exc:
        # Roll back any files/dirs we created during this run.
        logger.warning("Install failed, rolling back: %s", exc)
        _rollback(created_files, created_dirs)
        print(f"error: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc

    logger.debug("Installed %d files", len(created_files))

    # Write manifest for update detection (non-fatal on failure).
    from zing_ai.manifest import write_manifest

    relpaths = [str(f.relative_to(target_dir)) for f in created_files]
    write_manifest(target_dir, "claude-code", relpaths)

    register_mcp_server("claude")


def install_opencode(target_dir: Path | None = None) -> None:
    """Install Zing command files for OpenCode.

    Reads the bundled markdown command files, converts them for OpenCode
    using :func:`~zing_ai.converter.convert_for_opencode`, and writes
    them to the OpenCode commands directory.  The default target is
    ``~/.config/opencode/commands``.

    The output uses a flat naming scheme:

    * ``zing.md`` stays as ``zing.md``
    * Sub-commands become ``zing-{name}.md`` (e.g. ``zing-build.md``)
    * The ``_shared/`` directory retains its structure

    Parameters
    ----------
    target_dir:
        Override for the commands directory.  Useful for testing.

    Raises
    ------
    SystemExit
        On any I/O error (permissions, disk full, etc.).  Partial files are
        cleaned up before exiting.
    """
    from zing_ai.converter import convert_for_opencode

    if target_dir is None:
        target_dir = Path.home() / ".config" / "opencode" / "commands"

    logger.info("Installing OpenCode commands to %s", target_dir)

    # Back up any user-modified files before overwriting.
    from zing_ai.backup import backup_modified_files

    backed_up = backup_modified_files(target_dir)
    for relpath, backup_path in backed_up:
        print(f"  Backed up modified file: {relpath} -> {backup_path}")

    commands_root = importlib.resources.files("zing_ai.commands")
    logger.debug("Commands package root: %s", commands_root)

    created_files: list[Path] = []
    created_dirs: list[Path] = []

    try:
        # -- 1. Ensure the top-level target directory exists ------------------
        _ensure_dir(target_dir, created_dirs)

        # -- 2. Copy and convert the top-level zing.md -----------------------
        src_top = commands_root.joinpath(_TOP_LEVEL_FILE)
        dst_top = target_dir / _TOP_LEVEL_FILE
        _copy_resource_file_converted(
            src_top,
            dst_top,
            convert_for_opencode,
            created_files,
        )

        # -- 3. Copy and convert zing/ sub-commands (flattened) --------------
        src_zing = commands_root.joinpath("zing")
        for item in src_zing.iterdir():
            if item.is_file() and item.name.endswith(".md"):
                dst_name = f"zing-{item.name}"
                logger.debug("Converting sub-command: %s -> %s", item.name, dst_name)
                _copy_resource_file_converted(
                    item,
                    target_dir / dst_name,
                    convert_for_opencode,
                    created_files,
                )

        # -- 4. Copy and convert _shared/ (preserves structure) --------------
        src_shared = commands_root.joinpath("_shared")
        dst_shared = target_dir / "_shared"
        _copy_resource_tree_converted(
            src_shared,
            dst_shared,
            convert_for_opencode,
            created_files,
            created_dirs,
        )

    except SystemExit:
        raise
    except Exception as exc:
        logger.warning("Install failed, rolling back: %s", exc)
        _rollback(created_files, created_dirs)
        print(f"error: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc

    logger.debug("Installed %d files", len(created_files))

    # Write manifest for update detection (non-fatal on failure).
    from zing_ai.manifest import write_manifest

    relpaths = [str(f.relative_to(target_dir)) for f in created_files]
    write_manifest(target_dir, "opencode", relpaths)

    register_mcp_server("opencode")


def _ensure_dir(path: Path, created_dirs: list[Path]) -> None:
    """Create *path* and any missing parents, tracking newly created dirs."""
    # Walk from the deepest dir upward to find which parts we'll create.
    dirs_to_check = []
    current = path
    while not current.exists():
        dirs_to_check.append(current)
        current = current.parent

    if dirs_to_check:
        logger.debug("Creating directory tree: %s", path)

    # Create the directory tree.
    path.mkdir(parents=True, exist_ok=True)

    # Record newly created directories (deepest first for rollback ordering).
    for d in dirs_to_check:
        created_dirs.append(d)


def _copy_resource_file(
    src: Traversable,
    dst: Path,
    created_files: list[Path],
) -> None:
    """Copy a single resource file to *dst*, tracking it for rollback."""
    logger.debug("Copying %s -> %s", src, dst)
    data = src.read_text(encoding="utf-8")
    dst.write_text(data, encoding="utf-8")
    created_files.append(dst)


def _copy_resource_tree(
    src_dir: Traversable,
    dst_dir: Path,
    created_files: list[Path],
    created_dirs: list[Path],
) -> None:
    """Recursively copy a resource directory tree to *dst_dir*."""
    _ensure_dir(dst_dir, created_dirs)

    for item in src_dir.iterdir():
        if item.is_file():
            # Skip __init__.py and other Python files — only copy .md files.
            if not item.name.endswith(".md"):
                continue
            _copy_resource_file(item, dst_dir / item.name, created_files)
        elif item.is_dir():
            _copy_resource_tree(item, dst_dir / item.name, created_files, created_dirs)


def _copy_resource_file_converted(
    src: Traversable,
    dst: Path,
    converter: Callable[[str], str],
    created_files: list[Path],
) -> None:
    """Read *src*, run through *converter*, write to *dst*."""
    logger.debug("Converting and copying %s -> %s", src, dst)
    data = src.read_text(encoding="utf-8")
    data = converter(data)
    dst.write_text(data, encoding="utf-8")
    created_files.append(dst)


def _copy_resource_tree_converted(
    src_dir: Traversable,
    dst_dir: Path,
    converter: Callable[[str], str],
    created_files: list[Path],
    created_dirs: list[Path],
) -> None:
    """Recursively copy and convert a resource directory tree to *dst_dir*."""
    _ensure_dir(dst_dir, created_dirs)

    for item in src_dir.iterdir():
        if item.is_file():
            if not item.name.endswith(".md"):
                continue
            _copy_resource_file_converted(
                item,
                dst_dir / item.name,
                converter,
                created_files,
            )
        elif item.is_dir():
            _copy_resource_tree_converted(
                item,
                dst_dir / item.name,
                converter,
                created_files,
                created_dirs,
            )


def _rollback(created_files: list[Path], created_dirs: list[Path]) -> None:
    """Remove files and directories created during a failed install."""
    import contextlib

    logger.debug("Rolling back %d files and %d dirs", len(created_files), len(created_dirs))

    for f in reversed(created_files):
        with contextlib.suppress(OSError):
            logger.debug("Removing file: %s", f)
            f.unlink(missing_ok=True)

    # Remove directories in reverse order (deepest first).
    for d in reversed(created_dirs):
        with contextlib.suppress(OSError):
            # Only remove if empty — we don't want to delete user files.
            if d.exists() and not any(d.iterdir()):
                logger.debug("Removing empty directory: %s", d)
                d.rmdir()


def register_mcp_server(runtime: str) -> None:
    """Register the Zing MCP server for the given runtime.

    Parameters
    ----------
    runtime:
        Either ``"claude"`` or ``"opencode"``.

    For Claude Code, runs ``claude mcp add -s user zing-ai -- zing-ai mcp``
    via subprocess.  If the ``claude`` CLI is not on PATH, a warning is logged
    and the function returns without error.

    For OpenCode, merges the MCP server entry into
    ``~/.config/opencode/opencode.json``, preserving any existing config.

    The operation is idempotent — running it again is a no-op.
    """
    if runtime == "claude":
        _register_mcp_claude()
    elif runtime == "opencode":
        _register_mcp_opencode()
    else:
        logger.warning("Unknown runtime %r, skipping MCP registration", runtime)


def _register_mcp_claude() -> None:
    """Register the Zing MCP server for Claude Code via the CLI.

    Uses mcp-remote (npx) as a stdio-to-HTTP bridge to avoid Claude Code's
    OAuth discovery issues with direct HTTP transport.
    """
    if shutil.which("claude") is None:
        logger.warning(
            "claude CLI not found on PATH; skipping MCP server registration. "
            "Run 'claude mcp add -s user zing-ai -- "
            "npx mcp-remote@0.1.18 http://127.0.0.1:9876/mcp' manually."
        )
        return

    logger.info("Registering Zing MCP server for Claude Code")
    try:
        subprocess.run(
            [
                "claude", "mcp", "add",
                "-s", "user",
                "zing-ai",
                "--",
                "npx", "mcp-remote", "http://127.0.0.1:9876/mcp",
            ],
            check=True,
            capture_output=True,
            text=True,
        )
    except subprocess.CalledProcessError as exc:
        logger.warning("Failed to register MCP server via claude CLI: %s", exc.stderr)


def _register_mcp_opencode() -> None:
    """Register the Zing MCP server in the OpenCode config file."""
    config_path = Path.home() / ".config" / "opencode" / "opencode.json"

    logger.info("Registering Zing MCP server in %s", config_path)

    config: dict = {}
    if config_path.exists():
        try:
            config = json.loads(config_path.read_text(encoding="utf-8"))
        except (json.JSONDecodeError, OSError) as exc:
            logger.warning("Could not read existing OpenCode config: %s", exc)
            config = {}

    mcp_section = config.setdefault("mcp", {})
    mcp_section["zing-ai"] = {
        "type": "http",
        "url": "http://127.0.0.1:9876/mcp",
    }

    config_path.parent.mkdir(parents=True, exist_ok=True)
    config_path.write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")
