"""Orchestrator ``build`` command — execute plan steps."""

from __future__ import annotations

from pathlib import Path

from zing_ai.orchestrator.config import ZingConfig


async def run_build(
    *,
    zing_file: str | None,
    no_browser: bool,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
) -> None:
    """Run the ``build`` orchestrator command.

    Parameters
    ----------
    zing_file:
        Optional zing file name to build from.
    no_browser:
        If ``True``, do not open the browser automatically.
    skip_permissions:
        If ``True``, pass ``--dangerously-skip-permissions`` to all Claude
        calls.
    config:
        Parsed ``.zing.toml`` configuration.
    project_root:
        Path to the project root directory.
    """
    raise NotImplementedError("Not yet implemented")
