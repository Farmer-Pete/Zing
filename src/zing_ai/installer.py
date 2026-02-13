"""Installer logic for copying bundled command files to target directories."""

from __future__ import annotations

import importlib.resources
import sys
from collections.abc import Callable
from importlib.resources.abc import Traversable
from pathlib import Path


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

    commands_root = importlib.resources.files("zing_ai.commands")

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
            dst_subdir = target_dir / "zing" / subdir_name if subdir_name == "_shared" else target_dir / subdir_name
            _copy_resource_tree(src_subdir, dst_subdir, created_files, created_dirs)

    except SystemExit:
        raise
    except Exception as exc:
        # Roll back any files/dirs we created during this run.
        _rollback(created_files, created_dirs)
        print(f"error: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc


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

    commands_root = importlib.resources.files("zing_ai.commands")

    created_files: list[Path] = []
    created_dirs: list[Path] = []

    try:
        # -- 1. Ensure the top-level target directory exists ------------------
        _ensure_dir(target_dir, created_dirs)

        # -- 2. Copy and convert the top-level zing.md -----------------------
        src_top = commands_root.joinpath(_TOP_LEVEL_FILE)
        dst_top = target_dir / _TOP_LEVEL_FILE
        _copy_resource_file_converted(
            src_top, dst_top, convert_for_opencode, created_files,
        )

        # -- 3. Copy and convert zing/ sub-commands (flattened) --------------
        src_zing = commands_root.joinpath("zing")
        for item in src_zing.iterdir():
            if item.is_file() and item.name.endswith(".md"):
                dst_name = f"zing-{item.name}"
                _copy_resource_file_converted(
                    item, target_dir / dst_name, convert_for_opencode,
                    created_files,
                )

        # -- 4. Copy and convert _shared/ (preserves structure) --------------
        src_shared = commands_root.joinpath("_shared")
        dst_shared = target_dir / "_shared"
        _copy_resource_tree_converted(
            src_shared, dst_shared, convert_for_opencode,
            created_files, created_dirs,
        )

    except SystemExit:
        raise
    except Exception as exc:
        _rollback(created_files, created_dirs)
        print(f"error: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc


def _ensure_dir(path: Path, created_dirs: list[Path]) -> None:
    """Create *path* and any missing parents, tracking newly created dirs."""
    # Walk from the deepest dir upward to find which parts we'll create.
    dirs_to_check = []
    current = path
    while not current.exists():
        dirs_to_check.append(current)
        current = current.parent

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
                item, dst_dir / item.name, converter, created_files,
            )
        elif item.is_dir():
            _copy_resource_tree_converted(
                item, dst_dir / item.name, converter,
                created_files, created_dirs,
            )


def _rollback(created_files: list[Path], created_dirs: list[Path]) -> None:
    """Remove files and directories created during a failed install."""
    for f in reversed(created_files):
        try:
            f.unlink(missing_ok=True)
        except OSError:
            pass

    # Remove directories in reverse order (deepest first).
    for d in reversed(created_dirs):
        try:
            # Only remove if empty — we don't want to delete user files.
            if d.exists() and not any(d.iterdir()):
                d.rmdir()
        except OSError:
            pass
