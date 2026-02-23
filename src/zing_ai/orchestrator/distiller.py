"""Aid CLI distiller wrapper with SHA256 hash-based caching.

Provides async helpers for invoking the ``aid`` CLI to distill files,
with a file-content-hash-based cache to avoid redundant distillations.
"""

from __future__ import annotations

import asyncio
import hashlib
import logging
from pathlib import Path

logger = logging.getLogger(__name__)

CACHE_DIR = ".zing/.cache/"


def _hash_file(path: Path) -> str:
    """Compute the SHA256 hex digest of a file's content.

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


async def distill_file(file_path: Path, *, project_root: Path) -> str:
    """Distill a single file using the ``aid`` CLI, with caching.

    Computes the SHA256 hash of the file content and checks for a cached
    result in ``<project_root>/.zing/.cache/<hash>.txt``.  If cached,
    returns the cached content.  Otherwise, invokes ``aid distill_file``
    as an async subprocess, caches the result, and returns it.

    Parameters
    ----------
    file_path:
        Path to the file to distill.
    project_root:
        The project root directory (used to locate the cache).

    Returns
    -------
    str
        The distilled content of the file.
    """
    file_hash = _hash_file(file_path)
    cache_dir = project_root / CACHE_DIR
    cache_file = cache_dir / f"{file_hash}.txt"

    # Cache hit
    if cache_file.is_file():
        logger.debug("Cache hit for %s (hash=%s)", file_path, file_hash)
        return cache_file.read_text(encoding="utf-8")

    # Cache miss — invoke aid CLI
    logger.debug("Cache miss for %s (hash=%s), invoking aid", file_path, file_hash)

    proc = await asyncio.create_subprocess_exec(
        "aid",
        "distill_file",
        str(file_path),
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )

    stdout_data, stderr_data = await proc.communicate()

    if stderr_data:
        stderr_text = stderr_data.decode("utf-8", errors="replace")
        logger.debug("aid stderr: %s", stderr_text)

    result = stdout_data.decode("utf-8", errors="replace")

    # Cache the result
    cache_dir.mkdir(parents=True, exist_ok=True)
    cache_file.write_text(result, encoding="utf-8")
    logger.debug("Cached result for %s at %s", file_path, cache_file)

    return result


async def distill_files(
    file_paths: list[Path],
    *,
    project_root: Path,
) -> dict[Path, str]:
    """Distill multiple files concurrently using :func:`distill_file`.

    Parameters
    ----------
    file_paths:
        List of file paths to distill.
    project_root:
        The project root directory (used to locate the cache).

    Returns
    -------
    dict[Path, str]
        Mapping of each file path to its distilled content.
    """
    results = await asyncio.gather(
        *(distill_file(fp, project_root=project_root) for fp in file_paths)
    )
    return dict(zip(file_paths, results, strict=True))
