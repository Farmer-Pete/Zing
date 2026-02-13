"""Patch backup and restore for modified command files."""

from __future__ import annotations

import logging
import shutil
import sys
from datetime import UTC, datetime
from pathlib import Path

from zing_ai.manifest import detect_modified_files

logger = logging.getLogger(__name__)

_PATCHES_DIR = "zing-patches"


def backup_modified_files(target_dir: Path) -> list[tuple[str, Path]]:
    """Back up user-modified files before re-installing.

    Uses :func:`~zing_ai.manifest.detect_modified_files` to find files
    that have changed since the last install.  Each modified file is
    copied into ``target_dir / "zing-patches/"`` with a timestamped
    suffix so that multiple backups can coexist.

    Parameters
    ----------
    target_dir:
        The directory where commands are installed (and where the
        manifest lives).

    Returns
    -------
    list[tuple[str, Path]]
        A list of ``(original_relpath, backup_path)`` for every file
        that was backed up.  Returns an empty list when no manifest
        exists (fresh install) or when no files have been modified.
    """
    logger.debug("Checking for modified files in %s", target_dir)
    try:
        modified = detect_modified_files(target_dir)
    except Exception:
        # Manifest missing or corrupt -- treat as fresh install.
        logger.debug("No manifest found or manifest corrupt, treating as fresh install")
        return []

    if not modified:
        logger.debug("No modified files detected")
        return []

    logger.info("Detected %d modified file(s), backing up", len(modified))

    patches_dir = target_dir / _PATCHES_DIR
    try:
        patches_dir.mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        logger.warning("Could not create patches directory: %s", exc)
        print(
            f"warning: could not create patches directory: {exc}",
            file=sys.stderr,
        )
        return []

    timestamp = datetime.now(UTC).strftime("%Y-%m-%dT%H%M%S")
    backed_up: list[tuple[str, Path]] = []

    for relpath in modified:
        src = target_dir / relpath
        if not src.is_file():
            # File was deleted -- nothing to back up.
            logger.debug("Skipping deleted file: %s", relpath)
            continue
        backup_name = f"{relpath}.{timestamp}"
        backup_path = patches_dir / backup_name
        # Ensure parent dirs exist for nested relpaths (e.g. zing/_shared/foo.md).
        backup_path.parent.mkdir(parents=True, exist_ok=True)
        logger.debug("Backing up %s -> %s", relpath, backup_path)
        shutil.copy2(src, backup_path)
        backed_up.append((relpath, backup_path))

    return backed_up


def list_patches(target_dir: Path) -> list[tuple[str, Path]]:
    """List all backed-up patch files.

    Scans ``target_dir / "zing-patches/"`` and derives the original
    relative path from each backup filename by stripping the timestamp
    suffix (the last ``.YYYY-MM-DDTHHMMSS`` segment).

    Parameters
    ----------
    target_dir:
        The directory containing the ``zing-patches/`` subdirectory.

    Returns
    -------
    list[tuple[str, Path]]
        A list of ``(original_relpath, backup_path)`` tuples sorted by
        backup filename.  Returns an empty list if the patches directory
        does not exist or is empty.
    """
    patches_dir = target_dir / _PATCHES_DIR
    if not patches_dir.is_dir():
        logger.debug("No patches directory at %s", patches_dir)
        return []

    results: list[tuple[str, Path]] = []
    for backup_path in sorted(patches_dir.rglob("*")):
        if not backup_path.is_file():
            continue
        # The backup filename is "<original_relpath>.<timestamp>".
        # Strip the timestamp suffix (last dot-separated segment that
        # matches our timestamp format).
        name = str(backup_path.relative_to(patches_dir))
        # Find the last dot that separates the original name from timestamp.
        last_dot = name.rfind(".")
        if last_dot == -1:
            continue
        original_relpath = name[:last_dot]
        results.append((original_relpath, backup_path))

    logger.debug("Found %d patch backup(s)", len(results))
    return results


def reapply_patches(target_dir: Path) -> None:
    """Print backed-up patches so the user can manually restore them.

    Called by ``zing-ai reapply-patches``.  Lists each backed-up file
    with its original installed path and backup file path.

    Parameters
    ----------
    target_dir:
        The directory containing the ``zing-patches/`` subdirectory.
    """
    patches = list_patches(target_dir)
    if not patches:
        print("No backed-up patches found.")
        return

    print("Backed-up patches:")
    print()
    for original_relpath, backup_path in patches:
        original_path = target_dir / original_relpath
        print(f"  Original: {original_path}")
        print(f"  Backup:   {backup_path}")
        print()
