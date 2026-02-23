"""Tests for the aid distiller wrapper."""

from __future__ import annotations

import asyncio
import hashlib
from pathlib import Path
from unittest.mock import AsyncMock, patch

from zing_ai.orchestrator.distiller import (
    CACHE_DIR,
    _hash_file,
    distill_file,
    distill_files,
)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _write_file(path: Path, content: str) -> Path:
    """Write a file and return its path."""
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")
    return path


def _sha256(content: str) -> str:
    """Compute the SHA256 hex digest of a string (encoded as UTF-8)."""
    return hashlib.sha256(content.encode("utf-8")).hexdigest()


def _make_mock_process(
    stdout: bytes = b"",
    stderr: bytes = b"",
    returncode: int = 0,
) -> AsyncMock:
    """Create a mock asyncio subprocess."""
    proc = AsyncMock()
    proc.returncode = returncode
    proc.communicate = AsyncMock(return_value=(stdout, stderr))
    return proc


# ---------------------------------------------------------------------------
# Tests for _hash_file
# ---------------------------------------------------------------------------


class TestHashFile:
    """Tests for _hash_file computing SHA256 hex digest."""

    def test_simple_content(self, tmp_path: Path) -> None:
        f = _write_file(tmp_path / "hello.txt", "hello world")
        result = _hash_file(f)
        expected = _sha256("hello world")
        assert result == expected

    def test_empty_file(self, tmp_path: Path) -> None:
        f = _write_file(tmp_path / "empty.txt", "")
        result = _hash_file(f)
        expected = hashlib.sha256(b"").hexdigest()
        assert result == expected

    def test_binary_content(self, tmp_path: Path) -> None:
        f = tmp_path / "binary.bin"
        data = bytes(range(256))
        f.write_bytes(data)
        result = _hash_file(f)
        expected = hashlib.sha256(data).hexdigest()
        assert result == expected

    def test_different_content_different_hash(self, tmp_path: Path) -> None:
        f1 = _write_file(tmp_path / "a.txt", "aaa")
        f2 = _write_file(tmp_path / "b.txt", "bbb")
        assert _hash_file(f1) != _hash_file(f2)

    def test_same_content_same_hash(self, tmp_path: Path) -> None:
        f1 = _write_file(tmp_path / "a.txt", "same")
        f2 = _write_file(tmp_path / "b.txt", "same")
        assert _hash_file(f1) == _hash_file(f2)

    def test_returns_lowercase_hex(self, tmp_path: Path) -> None:
        f = _write_file(tmp_path / "test.txt", "test")
        result = _hash_file(f)
        assert result == result.lower()
        assert all(c in "0123456789abcdef" for c in result)

    def test_digest_length(self, tmp_path: Path) -> None:
        f = _write_file(tmp_path / "test.txt", "test")
        result = _hash_file(f)
        # SHA256 hex digest is 64 characters
        assert len(result) == 64


# ---------------------------------------------------------------------------
# Tests for distill_file
# ---------------------------------------------------------------------------


class TestDistillFileCacheHit:
    """Tests for distill_file when the cache already has the result."""

    def test_returns_cached_content(self, tmp_path: Path) -> None:
        """Cache hit should return cached content without invoking aid."""
        source = _write_file(tmp_path / "src" / "main.py", "print('hello')")
        file_hash = _hash_file(source)

        # Pre-populate the cache
        cache_dir = tmp_path / CACHE_DIR
        cache_dir.mkdir(parents=True)
        (cache_dir / f"{file_hash}.txt").write_text(
            "cached distillation", encoding="utf-8"
        )

        with patch("zing_ai.orchestrator.distiller.asyncio") as mock_asyncio:
            result = asyncio.run(distill_file(source, project_root=tmp_path))

        assert result == "cached distillation"
        # Subprocess should NOT have been called
        mock_asyncio.create_subprocess_exec.assert_not_called()

    def test_does_not_invoke_subprocess(self, tmp_path: Path) -> None:
        """Verify no subprocess is spawned on cache hit."""
        source = _write_file(tmp_path / "code.py", "x = 1")
        file_hash = _hash_file(source)

        cache_dir = tmp_path / CACHE_DIR
        cache_dir.mkdir(parents=True)
        (cache_dir / f"{file_hash}.txt").write_text("cached", encoding="utf-8")

        with patch(
            "zing_ai.orchestrator.distiller.asyncio.create_subprocess_exec"
        ) as mock_exec:
            result = asyncio.run(distill_file(source, project_root=tmp_path))

        assert result == "cached"
        mock_exec.assert_not_called()


class TestDistillFileCacheMiss:
    """Tests for distill_file when there is no cached result."""

    def test_invokes_aid_and_returns_output(self, tmp_path: Path) -> None:
        """Cache miss should invoke aid CLI and return its stdout."""
        source = _write_file(tmp_path / "src" / "main.py", "print('hello')")
        mock_proc = _make_mock_process(stdout=b"distilled output")

        with patch(
            "zing_ai.orchestrator.distiller.asyncio.create_subprocess_exec",
            return_value=mock_proc,
        ) as mock_exec:
            result = asyncio.run(distill_file(source, project_root=tmp_path))

        assert result == "distilled output"
        mock_exec.assert_called_once_with(
            "aid",
            "distill_file",
            str(source),
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )

    def test_caches_result(self, tmp_path: Path) -> None:
        """Cache miss should write the result to the cache directory."""
        source = _write_file(tmp_path / "code.py", "x = 1")
        file_hash = _hash_file(source)
        mock_proc = _make_mock_process(stdout=b"distilled x")

        with patch(
            "zing_ai.orchestrator.distiller.asyncio.create_subprocess_exec",
            return_value=mock_proc,
        ):
            asyncio.run(distill_file(source, project_root=tmp_path))

        cache_file = tmp_path / CACHE_DIR / f"{file_hash}.txt"
        assert cache_file.is_file()
        assert cache_file.read_text(encoding="utf-8") == "distilled x"

    def test_creates_cache_dir_if_missing(self, tmp_path: Path) -> None:
        """Cache directory should be created automatically."""
        source = _write_file(tmp_path / "code.py", "y = 2")
        mock_proc = _make_mock_process(stdout=b"result")

        cache_dir = tmp_path / CACHE_DIR
        assert not cache_dir.exists()

        with patch(
            "zing_ai.orchestrator.distiller.asyncio.create_subprocess_exec",
            return_value=mock_proc,
        ):
            asyncio.run(distill_file(source, project_root=tmp_path))

        assert cache_dir.is_dir()

    def test_modified_file_gets_new_hash(self, tmp_path: Path) -> None:
        """Changing file content should result in a new cache entry."""
        source = tmp_path / "code.py"
        _write_file(source, "version 1")
        hash_v1 = _hash_file(source)

        mock_proc_v1 = _make_mock_process(stdout=b"distilled v1")
        with patch(
            "zing_ai.orchestrator.distiller.asyncio.create_subprocess_exec",
            return_value=mock_proc_v1,
        ):
            result_v1 = asyncio.run(distill_file(source, project_root=tmp_path))

        assert result_v1 == "distilled v1"

        # Modify the file
        _write_file(source, "version 2")
        hash_v2 = _hash_file(source)
        assert hash_v1 != hash_v2

        mock_proc_v2 = _make_mock_process(stdout=b"distilled v2")
        with patch(
            "zing_ai.orchestrator.distiller.asyncio.create_subprocess_exec",
            return_value=mock_proc_v2,
        ):
            result_v2 = asyncio.run(distill_file(source, project_root=tmp_path))

        assert result_v2 == "distilled v2"

        # Both cache entries should exist
        cache_dir = tmp_path / CACHE_DIR
        assert (cache_dir / f"{hash_v1}.txt").is_file()
        assert (cache_dir / f"{hash_v2}.txt").is_file()


# ---------------------------------------------------------------------------
# Tests for distill_files
# ---------------------------------------------------------------------------


class TestDistillFiles:
    """Tests for distill_files processing multiple files concurrently."""

    def test_returns_dict_of_results(self, tmp_path: Path) -> None:
        """distill_files should return a dict mapping paths to distilled content."""
        f1 = _write_file(tmp_path / "a.py", "a = 1")
        f2 = _write_file(tmp_path / "b.py", "b = 2")

        mock_proc_a = _make_mock_process(stdout=b"distilled a")
        mock_proc_b = _make_mock_process(stdout=b"distilled b")

        call_count = 0

        async def mock_exec(*args, **kwargs):  # noqa: ARG001
            nonlocal call_count
            call_count += 1
            # Return different mock based on which file is being distilled
            if str(f1) in args:
                return mock_proc_a
            return mock_proc_b

        with patch(
            "zing_ai.orchestrator.distiller.asyncio.create_subprocess_exec",
            side_effect=mock_exec,
        ):
            result = asyncio.run(distill_files([f1, f2], project_root=tmp_path))

        assert isinstance(result, dict)
        assert result[f1] == "distilled a"
        assert result[f2] == "distilled b"

    def test_empty_list(self, tmp_path: Path) -> None:
        """distill_files with no files should return an empty dict."""
        result = asyncio.run(distill_files([], project_root=tmp_path))
        assert result == {}

    def test_single_file(self, tmp_path: Path) -> None:
        """distill_files with one file should work correctly."""
        f = _write_file(tmp_path / "only.py", "x = 42")
        mock_proc = _make_mock_process(stdout=b"distilled only")

        with patch(
            "zing_ai.orchestrator.distiller.asyncio.create_subprocess_exec",
            return_value=mock_proc,
        ):
            result = asyncio.run(distill_files([f], project_root=tmp_path))

        assert len(result) == 1
        assert result[f] == "distilled only"

    def test_uses_cache_for_some_files(self, tmp_path: Path) -> None:
        """Some files cached, some not -- only uncached ones invoke aid."""
        cached_file = _write_file(tmp_path / "cached.py", "cached content")
        uncached_file = _write_file(tmp_path / "uncached.py", "uncached content")

        # Pre-populate cache for the first file
        cached_hash = _hash_file(cached_file)
        cache_dir = tmp_path / CACHE_DIR
        cache_dir.mkdir(parents=True)
        (cache_dir / f"{cached_hash}.txt").write_text(
            "from cache", encoding="utf-8"
        )

        mock_proc = _make_mock_process(stdout=b"from aid")

        with patch(
            "zing_ai.orchestrator.distiller.asyncio.create_subprocess_exec",
            return_value=mock_proc,
        ) as mock_exec:
            result = asyncio.run(
                distill_files([cached_file, uncached_file], project_root=tmp_path)
            )

        assert result[cached_file] == "from cache"
        assert result[uncached_file] == "from aid"
        # Only one subprocess call (for the uncached file)
        mock_exec.assert_called_once()

    def test_concurrent_execution(self, tmp_path: Path) -> None:
        """Verify distill_files uses asyncio.gather for concurrency."""
        f1 = _write_file(tmp_path / "a.py", "a")
        f2 = _write_file(tmp_path / "b.py", "b")
        f3 = _write_file(tmp_path / "c.py", "c")

        mock_proc = _make_mock_process(stdout=b"result")

        with patch(
            "zing_ai.orchestrator.distiller.asyncio.create_subprocess_exec",
            return_value=mock_proc,
        ) as mock_exec:
            result = asyncio.run(
                distill_files([f1, f2, f3], project_root=tmp_path)
            )

        assert len(result) == 3
        # All three files should have been processed
        assert mock_exec.call_count == 3
