"""Project root detection and .zing directory management."""

from __future__ import annotations

import logging
from pathlib import Path

import click

logger = logging.getLogger(__name__)


def find_project_root() -> Path:
    """Walk up from cwd looking for a directory containing ``.git/``.

    Returns the first ancestor (or cwd itself) that contains a ``.git``
    directory.

    Raises:
        click.UsageError: If no ``.git/`` directory is found in any ancestor.
    """
    current = Path.cwd().resolve()
    logger.debug("Searching for project root from %s", current)
    while True:
        if (current / ".git").is_dir():
            logger.debug("Found project root: %s", current)
            return current
        parent = current.parent
        if parent == current:
            # Reached filesystem root without finding .git
            raise click.UsageError(
                "Not inside a git repository (no .git/ found in any parent directory)."
            )
        current = parent


def ensure_zing_dir(root: Path) -> Path:
    """Create ``.zing/`` under *root* if it doesn't exist and return its path."""
    zing_dir = root / ".zing"
    logger.debug("Ensuring .zing directory exists: %s", zing_dir)
    zing_dir.mkdir(exist_ok=True)
    return zing_dir


def list_zing_files(root: Path) -> list[Path]:
    """Return all ``.xml`` files inside ``<root>/.zing/``."""
    zing_dir = root / ".zing"
    logger.debug("Listing zing files in %s", zing_dir)
    if not zing_dir.is_dir():
        logger.debug("No .zing directory found")
        return []
    result = sorted(zing_dir.glob("*.xml"))
    logger.debug("Found %d zing file(s)", len(result))
    return result


def resolve_zing_file(arg: str | None, root: Path) -> Path:
    """Resolve a zing XML file path inside ``<root>/.zing/``.

    If *arg* is provided, resolve it relative to ``.zing/`` and verify
    the file exists.  If *arg* is ``None``, list available zing files and
    prompt the user to pick one (or abort if none are available).

    Raises:
        click.UsageError: If the resolved file does not exist or there are
            no zing files to choose from.
    """
    zing_dir = root / ".zing"
    logger.debug("Resolving zing file: arg=%s, root=%s", arg, root)

    if arg is not None:
        resolved = zing_dir / arg
        if not resolved.is_file():
            raise click.UsageError(f"Zing file not found: {resolved}")
        logger.debug("Resolved explicit zing file: %s", resolved)
        return resolved

    # No arg provided -- interactive selection
    files = list_zing_files(root)
    if not files:
        raise click.UsageError(
            f"No .xml files found in {zing_dir}. Create a zing file first."
        )

    if len(files) == 1:
        logger.debug("Auto-selected single zing file: %s", files[0])
        return files[0]

    click.echo("Available zing files:")
    for i, f in enumerate(files, 1):
        click.echo(f"  {i}. {f.name}")

    choice = click.prompt(
        "Select a file",
        type=click.IntRange(1, len(files)),
    )
    selected = files[choice - 1]
    logger.debug("User selected zing file: %s", selected)
    return selected
