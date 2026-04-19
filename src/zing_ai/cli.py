"""CLI entry point for zing-ai."""

from __future__ import annotations

import json
import logging
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request
import uuid
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


_TICKET_RE = re.compile(r"^[A-Z]+-\d+$")


@cli.command()
@click.argument("target")  # ticket ID or PR URL
@click.option("--resume/--no-resume", default=True, help="Auto-resume existing session if found")
@click.option("--server-url", default="http://127.0.0.1:9876/mcp", help="Zing MCP server URL")
def launch(target: str, resume: bool, server_url: str) -> None:
    """Launch a Claude Code session for a ticket or PR."""
    from zing_ai.config import ConfigError, load_config
    from zing_ai.launch import (
        LaunchError,
        build_claude_args,
        checkout_pr_branch,
        create_mcp_session,
        create_worktree,
        derive_branch_name,
        detect_action,
        extract_ticket_id,
        fetch_pr_data,
        move_ticket_in_progress,
        parse_pr_url,
        resolve_repo_root,
        rollback_worktree,
        run_init_script,
    )

    try:
        # Load config
        try:
            cfg = load_config()
        except ConfigError as e:
            click.echo(f"error: {e}", err=True)
            sys.exit(1)

        git_cfg = cfg.git
        workflow_mode = git_cfg.workflow_mode

        # Read Linear API key from ~/.config/lr/config.json
        lr_config_path = Path.home() / ".config" / "lr" / "config.json"
        try:
            lr_config = json.loads(lr_config_path.read_text())
            api_key = lr_config["workspaces"][lr_config["activeWorkspace"]]["apiKey"]
        except (FileNotFoundError, KeyError, json.JSONDecodeError) as e:
            raise LaunchError(f"Could not read Linear API key from {lr_config_path}: {e}") from e

        # Check MCP server is running
        check_req = urllib.request.Request(server_url, method="GET")
        try:
            urllib.request.urlopen(check_req)
        except urllib.error.URLError as e:
            raise LaunchError(
                f"Zing MCP server is not running at {server_url}. Start it with 'zing-ai mcp'."
            ) from e

        # Detect target type: ticket ID or PR URL
        is_ticket = bool(_TICKET_RE.match(target))

        if is_ticket:
            ticket_id = target

            # Check for existing session
            if resume:
                action, existing_session_id = detect_action(ticket_id, server_url)
            else:
                action, existing_session_id = "new", None

            if action == "resume" and existing_session_id is not None:
                args = build_claude_args(
                    skill="resume",
                    ticket_id=ticket_id,
                    session_id=existing_session_id,
                    name=ticket_id,
                )
                os.execvp("claude", args)
                return  # unreachable, but satisfies type checkers

            # New ticket flow
            repo_root = resolve_repo_root(Path.cwd())
            branch_name = derive_branch_name(ticket_id, api_key)

            # Handle workflow_mode
            if workflow_mode == "ask":
                workflow_mode = click.prompt(
                    "Workflow mode",
                    type=click.Choice(["worktree", "branch", "none"]),
                    default="worktree",
                )

            session_id = str(uuid.uuid4())
            worktree_path: Path | None = None

            if workflow_mode == "worktree":
                worktree_path = create_worktree(
                    repo_root=repo_root,
                    branch_name=branch_name,
                    worktree_root_template=git_cfg.worktree_root,
                    branch_prefix=git_cfg.branch_prefix,
                )
                work_dir = worktree_path
            elif workflow_mode == "branch":
                full_branch = f"{git_cfg.branch_prefix}{branch_name}"
                try:
                    subprocess.run(
                        ["git", "checkout", "-b", full_branch],
                        check=True,
                        capture_output=True,
                        text=True,
                        cwd=Path.cwd(),
                    )
                except subprocess.CalledProcessError as exc:
                    raise LaunchError(
                        f"git checkout -b {full_branch} failed: {exc.stderr.strip()}"
                    ) from exc
                work_dir = Path.cwd()
            else:
                # workflow_mode == "none"
                work_dir = Path.cwd()

            # Subsequent steps — roll back worktree on failure
            try:
                run_init_script(
                    repo_root=repo_root,
                    script_name=git_cfg.zing_init_script,
                    worktree_path=work_dir,
                    branch=branch_name,
                )
            except LaunchError:
                if worktree_path is not None:
                    rollback_worktree(worktree_path)
                raise

            try:
                move_ticket_in_progress(ticket_id, api_key)
            except LaunchError:
                if worktree_path is not None:
                    rollback_worktree(worktree_path)
                raise

            try:
                create_mcp_session(
                    server_url=server_url,
                    session_id=session_id,
                    title=ticket_id,
                    ticket_id=ticket_id,
                    worktree_path=str(work_dir) if work_dir else None,
                    skill="new",
                )
            except LaunchError:
                if worktree_path is not None:
                    rollback_worktree(worktree_path)
                raise

            args = build_claude_args(
                skill="new",
                ticket_id=ticket_id,
                session_id=session_id,
                name=ticket_id,
            )
            os.execvp("claude", args)

        else:
            # PR URL flow
            owner, repo, pr_number = parse_pr_url(target)
            pr_data = fetch_pr_data(owner, repo, pr_number)
            branch_name = pr_data["headRefName"]
            pr_title = pr_data.get("title", "")
            pr_body = pr_data.get("body", "") or ""
            ticket_id = extract_ticket_id(branch_name, pr_title, pr_body) or ""
            pr_name = f"PR #{pr_number} Review"

            repo_root = resolve_repo_root(Path.cwd())

            # Handle workflow_mode
            if workflow_mode == "ask":
                workflow_mode = click.prompt(
                    "Workflow mode",
                    type=click.Choice(["worktree", "branch", "none"]),
                    default="worktree",
                )

            session_id = str(uuid.uuid4())
            worktree_path = None

            if workflow_mode == "worktree":
                worktree_path = checkout_pr_branch(
                    repo_root=repo_root,
                    branch_name=branch_name,
                    worktree_root_template=git_cfg.worktree_root,
                )
                work_dir = worktree_path
            elif workflow_mode == "branch":
                try:
                    subprocess.run(
                        ["git", "checkout", branch_name],
                        check=True,
                        capture_output=True,
                        text=True,
                        cwd=Path.cwd(),
                    )
                except subprocess.CalledProcessError as exc:
                    raise LaunchError(
                        f"git checkout {branch_name} failed: {exc.stderr.strip()}"
                    ) from exc
                work_dir = Path.cwd()
            else:
                # workflow_mode == "none"
                work_dir = Path.cwd()

            try:
                run_init_script(
                    repo_root=repo_root,
                    script_name=git_cfg.zing_init_script,
                    worktree_path=work_dir,
                    branch=branch_name,
                )
            except LaunchError:
                if worktree_path is not None:
                    rollback_worktree(worktree_path)
                raise

            try:
                create_mcp_session(
                    server_url=server_url,
                    session_id=session_id,
                    title=pr_name,
                    ticket_id=ticket_id,
                    worktree_path=str(work_dir) if work_dir else None,
                    skill="pr-audit",
                )
            except LaunchError:
                if worktree_path is not None:
                    rollback_worktree(worktree_path)
                raise

            args = build_claude_args(
                skill="pr-audit",
                ticket_id=ticket_id,
                session_id=session_id,
                name=pr_name,
            )
            os.execvp("claude", args)

    except LaunchError as e:
        click.echo(str(e), err=True)
        sys.exit(1)


def _register_sim() -> None:
    """Register the sim command group eagerly (imports sim module at CLI load time)."""
    from zing_ai.sim import sim

    cli.add_command(sim)


_register_sim()


def main() -> None:
    """CLI entry point."""
    cli()
