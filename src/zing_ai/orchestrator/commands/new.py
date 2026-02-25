"""Orchestrator ``new`` command — collect requirements for a new zing file."""

from __future__ import annotations

import logging
import subprocess
import sys
from pathlib import Path

from zing_ai.orchestrator import project
from zing_ai.orchestrator.config import ZingConfig
from zing_ai.prompts import render_prompt

logger = logging.getLogger(__name__)


def run_new(
    *,
    zing_file: str | None,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
) -> None:
    """Run the ``new`` orchestrator command.

    Launches an interactive Claude session to collect project requirements
    from the user.  Claude is invoked directly via ``subprocess.run`` with
    inherited stdio so the user interacts with Claude in their terminal.

    After Claude exits, the newest ``.xml`` file in ``.zing/`` (by mtime)
    is identified as the file Claude created, and the flow auto-chains
    into ``run_plan()``.

    Parameters
    ----------
    zing_file:
        Optional zing file name (unused for ``new``, reserved for interface
        consistency).
    skip_permissions:
        If ``True``, pass ``--dangerously-skip-permissions`` to all Claude
        calls.
    config:
        Parsed ``.zing.toml`` configuration.
    project_root:
        Path to the project root directory.
    """
    # 1. Ensure .zing/ directory exists
    zing_dir = project.ensure_zing_dir(project_root)
    logger.debug("Ensured .zing directory at %s", zing_dir)

    # 2. Render the system prompt template
    system_prompt = render_prompt("new.md.j2")
    logger.debug("Rendered new.md.j2 system prompt (%d chars)", len(system_prompt))

    # 3. Invoke Claude interactively with inherited stdio
    #    The user talks to Claude directly in the terminal.
    subprocess.run(
        ["claude", "--system-prompt", system_prompt, "Greet the user"],
        stdin=sys.stdin,
        stdout=sys.stdout,
        stderr=sys.stderr,
    )
    logger.debug("Claude process exited")

    # 4. Scan .zing/ for the newest .xml file (by mtime)
    xml_files = sorted(zing_dir.glob("*.xml"), key=lambda p: p.stat().st_mtime)
    if not xml_files:
        logger.error("No .xml file found in %s after Claude exited", zing_dir)
        print("No valid zing file was created. Please run `zing-ai new` again.")
        return

    newest_xml = xml_files[-1]
    logger.info("Found newest zing file: %s", newest_xml)

    # 5. Auto-chain into planning
    from zing_ai.orchestrator.commands.plan import run_plan

    run_plan(
        zing_file=newest_xml.name,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )
