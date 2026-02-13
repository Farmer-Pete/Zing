"""Manifest system for tracking installed files and detecting modifications."""

from __future__ import annotations

import hashlib
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

from zing_ai import __version__

_MANIFEST_FILENAME = "zing-manifest.json"


def hash_file(path: Path) -> str:
    """Return the SHA256 hex digest of a file.

    Parameters
    ----------
    path:
        Path to the file to hash.

    Returns
    -------
    str
        Lowercase hex SHA256 digest.
    """
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(8192), b""):
            h.update(chunk)
    return h.hexdigest()


def hash_installed_files(
    target_dir: Path,
    file_relpaths: list[str],
) -> dict[str, dict[str, str]]:
    """Hash all installed files and return the ``files`` dict for the manifest.

    Parameters
    ----------
    target_dir:
        The directory where files were installed.
    file_relpaths:
        Relative paths (from *target_dir*) of the installed files.

    Returns
    -------
    dict
        Mapping of ``relpath -> {"sha256": "<hex>"}`` for each file.
    """
    files: dict[str, dict[str, str]] = {}
    for relpath in file_relpaths:
        full_path = target_dir / relpath
        if full_path.is_file():
            files[relpath] = {"sha256": hash_file(full_path)}
    return files


def write_manifest(
    target_dir: Path,
    runtime: str,
    file_relpaths: list[str],
) -> None:
    """Write a ``zing-manifest.json`` file alongside installed commands.

    Generates SHA256 hashes for all installed files and writes a JSON
    manifest to ``target_dir / "zing-manifest.json"``.

    If writing fails for any reason (e.g. permissions), a warning is
    printed to stderr but **no exception is raised** -- the install
    should still succeed.

    Parameters
    ----------
    target_dir:
        The directory where files were installed.
    runtime:
        ``"claude-code"`` or ``"opencode"``.
    file_relpaths:
        Relative paths (from *target_dir*) of the installed files.
    """
    try:
        manifest = {
            "version": __version__,
            "installed_at": datetime.now(timezone.utc).isoformat(),
            "runtime": runtime,
            "files": hash_installed_files(target_dir, file_relpaths),
        }
        manifest_path = target_dir / _MANIFEST_FILENAME
        manifest_path.write_text(
            json.dumps(manifest, indent=2) + "\n",
            encoding="utf-8",
        )
    except Exception as exc:
        print(
            f"warning: could not write manifest: {exc}",
            file=sys.stderr,
        )


def read_manifest(target_dir: Path) -> dict | None:
    """Read and parse ``zing-manifest.json`` from the target directory.

    Parameters
    ----------
    target_dir:
        Directory containing the manifest file.

    Returns
    -------
    dict or None
        Parsed manifest dict, or ``None`` if the file doesn't exist or
        contains invalid JSON.
    """
    manifest_path = target_dir / _MANIFEST_FILENAME
    try:
        text = manifest_path.read_text(encoding="utf-8")
        return json.loads(text)
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return None


def detect_modified_files(target_dir: Path) -> list[str]:
    """Detect files that have been modified since installation.

    Reads the manifest, hashes the current files on disk, and compares
    against the recorded hashes.

    Parameters
    ----------
    target_dir:
        Directory containing both the manifest and installed files.

    Returns
    -------
    list[str]
        Relative paths of files whose current hash differs from the
        manifest.  Returns an empty list if no manifest exists.
    """
    manifest = read_manifest(target_dir)
    if manifest is None:
        return []

    modified: list[str] = []
    for relpath, entry in manifest.get("files", {}).items():
        full_path = target_dir / relpath
        if not full_path.is_file():
            # File was deleted -- treat as modified.
            modified.append(relpath)
            continue
        current_hash = hash_file(full_path)
        if current_hash != entry.get("sha256"):
            modified.append(relpath)

    return modified
