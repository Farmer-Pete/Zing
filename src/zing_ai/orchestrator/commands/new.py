"""Orchestrator ``new`` command — collect requirements for a new zing file."""

from __future__ import annotations

import logging
import re
from pathlib import Path

from zing_ai.orchestrator import claude, project
from zing_ai.orchestrator.config import CallType, ZingConfig
from zing_ai.orchestrator.models import ZingDocument
from zing_ai.orchestrator.xml_parser import write_zing_file
from zing_ai.prompts import render_prompt

logger = logging.getLogger(__name__)


def _extract_project_name(markdown: str) -> str:
    """Extract a project name from Claude's markdown output.

    Strategy:
    1. Look for the first markdown heading (``# ...``).
    2. If no heading is found, use the first non-empty line.
    3. Convert the extracted name to kebab-case suitable for a filename.

    Returns
    -------
    str
        A kebab-case project name (e.g. ``"recipe-app"``).

    Raises
    ------
    ValueError
        If *markdown* is empty or contains no usable text.
    """
    name: str | None = None

    # Try to find the first markdown heading
    for line in markdown.splitlines():
        stripped = line.strip()
        match = re.match(r"^#+\s+(.+)$", stripped)
        if match:
            name = match.group(1).strip()
            break

    # Fallback: first non-empty line
    if name is None:
        for line in markdown.splitlines():
            stripped = line.strip()
            if stripped:
                name = stripped
                break

    if not name:
        raise ValueError("Cannot extract project name from empty markdown output")

    return _to_kebab_case(name)


def _to_kebab_case(text: str) -> str:
    """Convert a human-readable name to kebab-case.

    Examples
    --------
    >>> _to_kebab_case("Recipe App")
    'recipe-app'
    >>> _to_kebab_case("My Cool Project!!!")
    'my-cool-project'
    >>> _to_kebab_case("  hello   world  ")
    'hello-world'
    """
    # Lowercase
    text = text.lower()
    # Replace non-alphanumeric characters (except hyphens) with spaces
    text = re.sub(r"[^a-z0-9-]", " ", text)
    # Collapse whitespace and strip
    text = re.sub(r"\s+", " ", text).strip()
    # Replace spaces with hyphens
    text = text.replace(" ", "-")
    # Collapse multiple hyphens
    text = re.sub(r"-+", "-", text)
    # Strip leading/trailing hyphens
    text = text.strip("-")
    return text


async def run_new(
    *,
    zing_file: str | None,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
) -> None:
    """Run the ``new`` orchestrator command.

    Launches an interactive Claude session to collect project requirements
    from the user.  The resulting markdown is saved as a ``.zing/{name}.xml``
    file, then the flow continues into ``run_plan()``.

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

    # 2. Render the prompt template
    prompt = render_prompt("new.md.j2")
    logger.debug("Rendered new.md.j2 prompt (%d chars)", len(prompt))

    # 3. Invoke Claude interactively to collect requirements
    #    This is an interactive session — Claude talks to the user via the terminal.
    #    We use invoke_claude_full() which collects all output.
    markdown, _session_id = await claude.invoke_claude_full(
        prompt,
        call_type=CallType.INVESTIGATE,
        config=config,
        skip_permissions=skip_permissions,
    )
    logger.debug("Claude returned %d chars of output", len(markdown))

    # 4. Extract project name from the markdown output
    name = _extract_project_name(markdown)
    logger.info("Extracted project name: %s", name)

    # 5. Create the zing file
    zing_path = zing_dir / f"{name}.xml"
    doc = ZingDocument(
        stage="new",
        content=markdown,
        plan=None,
        interactions=None,
        audit=False,
        approved=False,
    )
    write_zing_file(zing_path, doc)
    logger.info("Wrote zing file: %s", zing_path)

    # 6. Flow into planning
    from zing_ai.orchestrator.commands.plan import run_plan

    await run_plan(
        zing_file=zing_path.name,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )
