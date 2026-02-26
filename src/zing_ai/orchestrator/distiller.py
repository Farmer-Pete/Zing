"""Aid CLI distiller wrapper with SHA256 hash-based caching.

Provides sync helpers for invoking the ``aid`` CLI to distill files,
with a file-content-hash-based cache to avoid redundant distillations.
"""

from __future__ import annotations

import hashlib
import logging
import subprocess
from concurrent.futures import ThreadPoolExecutor
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


def distill_file(
    file_path: Path, *, project_root: Path, aid_path: str = "aid", timeout: float = 60,
) -> str | None:
    """Distill a single file using the ``aid`` CLI, with caching.

    Computes the SHA256 hash of the file content and checks for a cached
    result in ``<project_root>/.zing/.cache/<hash>.txt``.  If cached,
    returns the cached content.  Otherwise, invokes ``aid distill_file``
    as a subprocess, caches the result, and returns it.

    Parameters
    ----------
    file_path:
        Path to the file to distill.
    project_root:
        The project root directory (used to locate the cache).
    aid_path:
        Path or command name for the ``aid`` binary (default ``"aid"``).
    timeout:
        Maximum seconds to wait for the ``aid`` subprocess (default 60).

    Returns
    -------
    str | None
        The distilled content of the file, or ``None`` on failure.
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

    try:
        result = subprocess.run(
            [aid_path, str(file_path), "--stdout"],
            capture_output=True,
            timeout=timeout,
        )
    except subprocess.TimeoutExpired:
        logger.warning("aid distill_file timed out for %s after %.0fs", file_path, timeout)
        return None

    if result.returncode != 0:
        stderr_text = result.stderr.decode("utf-8", errors="replace")
        logger.warning(
            "aid distill_file failed for %s (returncode=%d): %s",
            file_path,
            result.returncode,
            stderr_text.strip(),
        )
        return None

    if result.stderr:
        stderr_text = result.stderr.decode("utf-8", errors="replace")
        logger.debug("aid stderr: %s", stderr_text)

    output = result.stdout.decode("utf-8", errors="replace")

    # Cache the result
    cache_dir.mkdir(parents=True, exist_ok=True)
    cache_file.write_text(output, encoding="utf-8")
    logger.debug("Cached result for %s at %s", file_path, cache_file)

    return output


def distill_files(
    file_paths: list[Path],
    *,
    project_root: Path,
    aid_path: str = "aid",
) -> dict[Path, str]:
    """Distill multiple files concurrently using :func:`distill_file`.

    Parameters
    ----------
    file_paths:
        List of file paths to distill.
    project_root:
        The project root directory (used to locate the cache).
    aid_path:
        Path or command name for the ``aid`` binary (default ``"aid"``).

    Returns
    -------
    dict[Path, str]
        Mapping of each file path to its distilled content.
    """
    def _distill_one(fp: Path) -> tuple[Path, str | None]:
        return fp, distill_file(fp, project_root=project_root, aid_path=aid_path)

    with ThreadPoolExecutor() as executor:
        results = list(executor.map(_distill_one, file_paths))

    return {fp: r for fp, r in results if r is not None}
