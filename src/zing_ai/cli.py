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
    from zing_ai.config import ConfigError, load_config
    from zing_ai.installer import InstallError, install_claude, install_opencode

    runtimes = _resolve_runtimes(claude, opencode, all_runtimes)
    logger.info("Resolved runtimes: %s", runtimes)
    try:
        cfg = load_config()
    except ConfigError as e:
        click.echo(f"error: {e}", err=True)
        sys.exit(1)
    for rt in runtimes:
        try:
            if rt == "claude":
                click.echo("Installing for Claude Code...")
                install_claude(config=cfg)
                click.echo("Claude Code commands installed successfully.")
            elif rt == "opencode":
                click.echo("Installing for OpenCode...")
                install_opencode(config=cfg)
                click.echo("OpenCode commands installed successfully.")
        except InstallError as e:
            click.echo(f"error: {e}", err=True)
            sys.exit(1)


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


@cli.command("mcp")
@click.option("--port", default=9876, type=int, help="Port to listen on.")
def mcp_cmd(port: int) -> None:
    """Start the Zing MCP + HTTP server."""
    import uvicorn

    from zing_ai.server.app import create_app

    app = create_app(port=port)
    click.echo(f"Starting Zing server on http://127.0.0.1:{port}")
    click.echo(f"Dashboard: http://127.0.0.1:{port}/dashboard")
    uvicorn.run(app, host="127.0.0.1", port=port, timeout_graceful_shutdown=3)


def _register_sim() -> None:
    """Register the sim command group eagerly (imports sim module at CLI load time)."""
    from zing_ai.sim import sim

    cli.add_command(sim)


_register_sim()


def main() -> None:
    """CLI entry point."""
    cli()
