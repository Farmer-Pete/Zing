"""CLI entry point for zing-ai."""

from __future__ import annotations

import logging
import sys
from collections.abc import Callable
from pathlib import Path

import click

logger = logging.getLogger("zing_ai")

RUNTIMES = ("claude", "opencode")


def _runtime_options[F: Callable[..., object]](f: F) -> F:
    """Shared --claude/--opencode/--all options for subcommands."""
    f = click.option(
        "--all",
        "all_runtimes",
        is_flag=True,
        default=False,
        help="Target all supported runtimes.",
    )(f)
    f = click.option("--opencode", is_flag=True, default=False, help="Target OpenCode.")(f)
    f = click.option("--claude", is_flag=True, default=False, help="Target Claude Code.")(f)
    return f


def _setup_logging(*, verbose: bool) -> None:
    """Configure the ``zing_ai`` logger to write to stderr."""
    level = logging.DEBUG if verbose else logging.WARNING
    handler = logging.StreamHandler(sys.stderr)
    handler.setFormatter(logging.Formatter("%(levelname)s: %(name)s: %(message)s"))
    root = logging.getLogger("zing_ai")
    root.setLevel(level)
    root.addHandler(handler)


@click.group(invoke_without_command=True)
@click.version_option(package_name="zing-ai", prog_name="zing-ai")
@click.option(
    "--verbose", "-v", is_flag=True, default=False, help="Enable debug logging to stderr."
)
@click.pass_context
def cli(ctx: click.Context, *, verbose: bool) -> None:
    """Zing AI development pipeline installer."""
    _setup_logging(verbose=verbose)
    logger.debug("CLI invoked (verbose=%s)", verbose)
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


@cli.command()
@_runtime_options
def install(claude: bool, opencode: bool, all_runtimes: bool) -> None:
    """Install Zing commands for the selected runtime(s)."""
    runtimes = _resolve_runtimes(claude, opencode, all_runtimes)
    logger.info("Resolved runtimes: %s", runtimes)
    for rt in runtimes:
        if rt == "claude":
            from zing_ai.installer import install_claude

            click.echo("Installing for Claude Code...")
            install_claude()
            click.echo("Claude Code commands installed successfully.")
        elif rt == "opencode":
            from zing_ai.installer import install_opencode

            click.echo("Installing for OpenCode...")
            install_opencode()
            click.echo("OpenCode commands installed successfully.")


@cli.command("reapply-patches")
@_runtime_options
def reapply_patches_cmd(claude: bool, opencode: bool, all_runtimes: bool) -> None:
    """List backed-up patches for the selected runtime(s)."""
    from zing_ai.backup import reapply_patches

    runtimes = _resolve_runtimes(claude, opencode, all_runtimes)
    logger.info("Resolved runtimes for reapply-patches: %s", runtimes)
    for rt in runtimes:
        click.echo(f"Patches for {rt}:")
        if rt == "claude":
            target_dir = Path.home() / ".claude" / "commands"
        elif rt == "opencode":
            target_dir = Path.home() / ".config" / "opencode" / "commands"
        else:
            continue
        logger.debug("Scanning patches in %s", target_dir)
        reapply_patches(target_dir)


# ---------------------------------------------------------------------------
# Orchestrator subcommands
# ---------------------------------------------------------------------------


@cli.command()
@click.argument("zing_file", required=False)
@click.option(
    "--skip-permissions",
    is_flag=True,
    default=False,
    help="Pass --dangerously-skip-permissions to all Claude calls.",
)
def new(zing_file: str | None, *, skip_permissions: bool) -> None:
    """Collect requirements for a new zing file."""
    from zing_ai.orchestrator.commands.new import run_new
    from zing_ai.orchestrator.config import load_config
    from zing_ai.orchestrator.project import find_project_root

    project_root = find_project_root()
    config = load_config(project_root)
    run_new(
        zing_file=zing_file,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )


@cli.command()
@click.argument("zing_file", required=False)
@click.option(
    "--skip-permissions",
    is_flag=True,
    default=False,
    help="Pass --dangerously-skip-permissions to all Claude calls.",
)
def plan(zing_file: str | None, *, skip_permissions: bool) -> None:
    """Generate a development plan from a zing file."""
    from zing_ai.orchestrator.commands.plan import run_plan
    from zing_ai.orchestrator.config import load_config
    from zing_ai.orchestrator.project import find_project_root

    project_root = find_project_root()
    config = load_config(project_root)
    run_plan(
        zing_file=zing_file,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )


@cli.command("plan-audit")
@click.argument("zing_file", required=False)
@click.option(
    "--skip-permissions",
    is_flag=True,
    default=False,
    help="Pass --dangerously-skip-permissions to all Claude calls.",
)
def plan_audit(zing_file: str | None, *, skip_permissions: bool) -> None:
    """Audit an existing development plan."""
    from zing_ai.orchestrator.commands.plan_audit import run_plan_audit
    from zing_ai.orchestrator.config import load_config
    from zing_ai.orchestrator.project import find_project_root

    project_root = find_project_root()
    config = load_config(project_root)
    run_plan_audit(
        zing_file=zing_file,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )


@cli.command("plan-review")
@click.argument("zing_file", required=False)
@click.option(
    "--skip-permissions",
    is_flag=True,
    default=False,
    help="Pass --dangerously-skip-permissions to all Claude calls.",
)
def plan_review(zing_file: str | None, *, skip_permissions: bool) -> None:
    """Review and approve a development plan."""
    from zing_ai.orchestrator.commands.plan_review import run_plan_review
    from zing_ai.orchestrator.config import load_config
    from zing_ai.orchestrator.project import find_project_root

    project_root = find_project_root()
    config = load_config(project_root)
    run_plan_review(
        zing_file=zing_file,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )


@cli.command()
@click.argument("zing_file", required=False)
@click.option(
    "--skip-permissions",
    is_flag=True,
    default=False,
    help="Pass --dangerously-skip-permissions to all Claude calls.",
)
def build(zing_file: str | None, *, skip_permissions: bool) -> None:
    """Execute plan steps to build the project."""
    from zing_ai.orchestrator.commands.build import run_build
    from zing_ai.orchestrator.config import load_config
    from zing_ai.orchestrator.project import find_project_root

    project_root = find_project_root()
    config = load_config(project_root)
    run_build(
        zing_file=zing_file,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )


@cli.command("build-audit")
@click.argument("zing_file", required=False)
@click.option(
    "--skip-permissions",
    is_flag=True,
    default=False,
    help="Pass --dangerously-skip-permissions to all Claude calls.",
)
def build_audit(zing_file: str | None, *, skip_permissions: bool) -> None:
    """Audit build output for issues."""
    from zing_ai.orchestrator.commands.build_audit import run_build_audit
    from zing_ai.orchestrator.config import load_config
    from zing_ai.orchestrator.project import find_project_root

    project_root = find_project_root()
    config = load_config(project_root)
    run_build_audit(
        zing_file=zing_file,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )


def _resolve_runtimes(claude: bool, opencode: bool, all_runtimes: bool) -> list[str]:
    """Return the list of selected runtimes, prompting interactively if needed."""
    logger.debug(
        "Resolving runtimes (claude=%s, opencode=%s, all=%s)", claude, opencode, all_runtimes
    )
    if all_runtimes and (claude or opencode):
        raise click.UsageError("--all cannot be combined with --claude or --opencode")

    if all_runtimes:
        return list(RUNTIMES)

    selected: list[str] = []
    if claude:
        selected.append("claude")
    if opencode:
        selected.append("opencode")

    if selected:
        return selected

    logger.debug("No runtime flags given, prompting interactively")
    return _prompt_runtime_selection()


def _prompt_runtime_selection() -> list[str]:
    """Interactively ask the user which runtimes to target."""
    click.echo("Which runtimes would you like to target?\n")
    click.echo("  1) Claude Code")
    click.echo("  2) OpenCode")
    click.echo("  3) All")
    click.echo()

    while True:
        try:
            choice = click.prompt("Enter choice [1/2/3]", default="", show_default=False).strip()
        except (EOFError, click.Abort):
            raise SystemExit(130) from None

        if choice == "1":
            return ["claude"]
        if choice == "2":
            return ["opencode"]
        if choice == "3":
            return list(RUNTIMES)

        click.echo(f"Invalid choice: {choice!r}. Please enter 1, 2, or 3.")


def main() -> None:
    """CLI entry point."""
    cli()
