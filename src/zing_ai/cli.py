"""CLI entry point for zing-ai."""

from __future__ import annotations

import json
import logging
import re
import shlex
import shutil
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


@cli.command()
@click.argument("target")  # ticket ID, PR URL, or markdown file path
@click.option(
    "--resume",
    is_flag=False,
    flag_value="auto",
    default="auto",
    help="Resume a session. Pass a session UUID to resume a specific session, "
    "or omit the value for auto-detect. Use --no-resume to skip.",
)
@click.option("--no-resume", "resume", flag_value="", help="Skip session resume.")
@click.option("--port", default=9876, type=int, help="Port the Zing server is listening on.")
@click.option(
    "--skill", default=None, type=str, help="Skill to use for the session (e.g. pr-respond)."
)
@click.option("--detach", is_flag=True, default=False, help="Run in a detached session.")
@click.option(
    "--setup-only",
    is_flag=True,
    default=False,
    help="Set up the worktree/branch and register the session, but don't start Claude.",
)
def launch(
    target: str, resume: str, port: int, skill: str | None, detach: bool, setup_only: bool
) -> None:
    """Launch a Claude Code session for a ticket, PR, or plan file."""
    from zing_ai.config import ConfigError, load_config
    from zing_ai.launch import (
        TICKET_ID_PATTERN,
        LaunchError,
        build_claude_args,
        build_session_name,
        checkout_pr_branch,
        create_session_on_server,
        create_worktree,
        derive_branch_name,
        detect_action,
        detect_action_by_title,
        exec_or_detach,
        extract_ticket_id,
        fetch_pr_data,
        fetch_session,
        move_ticket_in_progress,
        parse_pr_url,
        require_session_backend,
        resolve_repo_root,
        rollback_worktree,
        run_init_script,
        sanitize_branch_name,
        validate_markdown_target,
    )

    server_url = f"http://127.0.0.1:{port}"

    try:
        # Load config
        try:
            cfg = load_config()
        except ConfigError as e:
            click.echo(f"error: {e}", err=True)
            sys.exit(1)

        git_cfg = cfg.git
        workflow_mode = git_cfg.workflow_mode

        # Check server is running (any HTTP response means it's up)
        check_req = urllib.request.Request(server_url, method="GET")
        try:
            urllib.request.urlopen(check_req)
        except urllib.error.HTTPError:
            pass  # Server is running, just returned a non-2xx status
        except urllib.error.URLError as e:
            raise LaunchError(
                f"Zing server is not running at {server_url}. Start it with 'zing-ai mcp'."
            ) from e

        if detach:
            require_session_backend()

        # -- shared helpers (capture enclosing scope) ----------------------

        def _attempt_resume(
            existing_session_id: str,
            existing_worktree: str | None,
            name: str,
            target_arg: str | None,
            default_skill: str,
        ) -> bool:
            """Try to resume an existing session. Returns True if resumed."""
            args = build_claude_args(
                skill="resume",
                session_id=existing_session_id,
                name=name,
                claude_flags=cfg.command_center.claude_flags,
            )
            resume_cwd = Path(existing_worktree) if existing_worktree else Path.cwd()
            session_name = build_session_name(name) if detach else None
            explicit = resume not in ("auto", "")
            if explicit:
                click.echo(f"cwd: {resume_cwd}", err=True)
                click.echo(f"cmd: {shlex.join(args)}", err=True)
            result = subprocess.run(args, cwd=resume_cwd, capture_output=explicit, text=True)
            if result.returncode == 0:
                return True
            if explicit:
                detail = result.stderr.strip() if result.stderr else ""
                click.echo(
                    f"Claude resume failed (exit {result.returncode})"
                    + (f": {detail}" if detail else ""),
                    err=True,
                )
                click.echo(
                    f"Checking Zing server at {server_url} for session {existing_session_id}...",
                    err=True,
                )
                session_data = fetch_session(server_url, existing_session_id)
                if session_data is not None:
                    wt = session_data.get("worktree_path") or str(Path.cwd())
                    session_skill = session_data.get("skill") or default_skill
                    session_title = session_data.get("title") or name
                    click.echo("No Claude conversation for this session yet.")
                    click.echo(f"Working directory: {wt}")
                    if click.confirm("Launch Claude with this session ID?", default=True):
                        relaunch_args = build_claude_args(
                            skill=session_skill,
                            session_id=existing_session_id,
                            name=session_title,
                            target=target_arg,
                            claude_flags=cfg.command_center.claude_flags,
                        )
                        click.echo(f"cwd: {wt}", err=True)
                        click.echo(f"cmd: {shlex.join(relaunch_args)}", err=True)
                        sys.stderr.flush()
                        exec_or_detach(relaunch_args, Path(wt), session_name)
                        return True
                    raise LaunchError("Aborted by user.")
                click.echo("Session not found on Zing server either.", err=True)
                raise LaunchError(f"Session {existing_session_id} not found")
            click.echo(
                f"Resume failed (exit {result.returncode}); starting a new session instead.",
                err=True,
            )
            return False

        def _detect_resume(
            detect_fn: Callable[[str, str], tuple[str, str | None, str | None]],
            detect_arg: str,
        ) -> tuple[str, str | None, str | None]:
            """Determine whether to resume or start new based on flags."""
            if setup_only:
                return "new", None, None
            if resume == "auto":
                return detect_fn(detect_arg, server_url)
            if resume:
                session_data = fetch_session(server_url, resume)
                existing_wt = session_data.get("worktree_path") if session_data else None
                return "resume", resume, existing_wt
            return "new", None, None

        def _create_new_branch_worktree(
            repo_root: Path,
            branch_name: str,
            wf_mode: str,
        ) -> tuple[Path, Path | None]:
            """Create a worktree or branch for new-branch flows (ticket/markdown).

            Returns (work_dir, worktree_path). worktree_path is set only when
            workflow_mode == "worktree" (needed for rollback).
            """
            worktree_path: Path | None = None

            if wf_mode == "worktree":
                worktree_path = create_worktree(
                    repo_root=repo_root,
                    branch_name=branch_name,
                    worktree_root_template=git_cfg.worktree_root,
                    branch_prefix=git_cfg.branch_prefix,
                )
                return worktree_path, worktree_path
            if wf_mode == "branch":
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
                return Path.cwd(), None
            # workflow_mode == "none"
            return Path.cwd(), None

        def _init_create_and_launch(
            *,
            repo_root: Path,
            work_dir: Path,
            worktree_path: Path | None,
            branch_name: str,
            name: str,
            target_arg: str | None,
            resolved_skill: str,
            title: str,
            ticket_id: str | None,
            session_name: str | None,
            pre_create_hook: Callable[[], None] | None = None,
            pr_number: int | None = None,
            pr_repo: str | None = None,
        ) -> None:
            """Run init script, create session, and launch (or print setup-only)."""
            session_id = str(uuid.uuid4())
            succeeded = False
            try:
                run_init_script(
                    repo_root=repo_root,
                    script_name=git_cfg.zing_init_script,
                    worktree_path=work_dir,
                    branch=branch_name,
                )
                if pre_create_hook is not None:
                    pre_create_hook()
                create_session_on_server(
                    server_url=server_url,
                    session_id=session_id,
                    title=title,
                    ticket_id=ticket_id,
                    worktree_path=str(work_dir) if work_dir else None,
                    skill=resolved_skill,
                    pr_number=pr_number,
                    pr_repo=pr_repo,
                    terminal_session=session_name,
                )
                succeeded = True
            finally:
                if not succeeded and worktree_path is not None:
                    rollback_worktree(worktree_path)

            if setup_only:
                click.echo(f"Environment ready: {work_dir}")
                click.echo(f"Session ID: {session_id}")
                target_display = target_arg or name
                click.echo(
                    f"To start: cd {work_dir} && claude /zing:{resolved_skill} {target_display}"
                )
                return

            args = build_claude_args(
                skill=resolved_skill,
                session_id=session_id,
                name=name,
                target=target_arg,
                claude_flags=cfg.command_center.claude_flags,
            )
            exec_or_detach(args, work_dir, session_name)

        def _prompt_workflow_mode() -> str:
            """Prompt for workflow mode if configured as 'ask'."""
            if workflow_mode == "ask":
                return click.prompt(
                    "Workflow mode",
                    type=click.Choice(["worktree", "branch", "none"]),
                    default="worktree",
                )
            return workflow_mode

        # -- target dispatch -----------------------------------------------

        is_ticket = bool(re.match(rf"^{TICKET_ID_PATTERN}$", target))
        is_markdown = not is_ticket and target.endswith(".md")

        if is_ticket:
            ticket_id = target
            action, existing_session_id, existing_worktree = _detect_resume(
                detect_action, ticket_id
            )

            if (
                action == "resume"
                and existing_session_id is not None
                and _attempt_resume(
                    existing_session_id, existing_worktree, ticket_id, ticket_id, "new"
                )
            ):
                return

            # Read Linear API key (only needed for tickets)
            lr_config_path = Path.home() / ".config" / "lr" / "config.json"
            try:
                lr_config = json.loads(lr_config_path.read_text())
                api_key = lr_config["workspaces"][lr_config["activeWorkspace"]]["apiKey"]
            except (FileNotFoundError, KeyError, json.JSONDecodeError) as e:
                raise LaunchError(
                    f"Could not read Linear API key from {lr_config_path}: {e}"
                ) from e

            repo_root = resolve_repo_root(Path.cwd())
            branch_name = derive_branch_name(ticket_id, api_key)
            wf_mode = _prompt_workflow_mode()
            work_dir, worktree_path = _create_new_branch_worktree(repo_root, branch_name, wf_mode)
            session_name = build_session_name(ticket_id) if detach else None
            ticket_skill = skill or "new"

            _init_create_and_launch(
                repo_root=repo_root,
                work_dir=work_dir,
                worktree_path=worktree_path,
                branch_name=branch_name,
                name=ticket_id,
                target_arg=ticket_id,
                resolved_skill=ticket_skill,
                title=ticket_id,
                ticket_id=ticket_id,
                session_name=session_name,
                pre_create_hook=lambda: move_ticket_in_progress(ticket_id, api_key),
            )

        elif is_markdown:
            md_path = validate_markdown_target(target)
            md_name = md_path.stem
            branch_name = sanitize_branch_name(md_name)

            action, existing_session_id, existing_worktree = _detect_resume(
                detect_action_by_title, md_name
            )

            if (
                action == "resume"
                and existing_session_id is not None
                and _attempt_resume(
                    existing_session_id, existing_worktree, md_name, str(md_path), "build"
                )
            ):
                return

            repo_root = resolve_repo_root(Path.cwd())
            wf_mode = _prompt_workflow_mode()
            work_dir, worktree_path = _create_new_branch_worktree(repo_root, branch_name, wf_mode)

            # .zing/ is gitignored so it won't exist in a new worktree.
            # Copy the target file so it's available at the same relative path.
            if worktree_path is not None:
                try:
                    rel = md_path.relative_to(repo_root)
                except ValueError:
                    pass
                else:
                    dest = worktree_path / rel
                    dest.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copy2(md_path, dest)
                    md_path = dest

            session_name = build_session_name(md_name) if detach else None
            md_skill = skill or "new"

            _init_create_and_launch(
                repo_root=repo_root,
                work_dir=work_dir,
                worktree_path=worktree_path,
                branch_name=branch_name,
                name=md_name,
                target_arg=str(md_path),
                resolved_skill=md_skill,
                title=md_name,
                ticket_id=None,
                session_name=session_name,
            )

        else:
            # PR URL flow
            try:
                owner, repo, pr_number = parse_pr_url(target)
            except LaunchError:
                raise LaunchError(
                    f"Unrecognized target: {target!r}. Expected one of:\n"
                    "  - A Linear ticket ID (e.g. ENG-123)\n"
                    "  - A GitHub PR URL (e.g. https://github.com/owner/repo/pull/123)\n"
                    "  - A path to a markdown file (e.g. .zing/my-plan.md)"
                ) from None
            pr_skill = skill or "pr-audit"
            pr_data = fetch_pr_data(owner, repo, pr_number)
            branch_name = pr_data["headRefName"]
            pr_title = pr_data.get("title", "")
            pr_body = pr_data.get("body", "") or ""
            ticket_id = extract_ticket_id(branch_name, pr_title, pr_body)
            pr_name = f"PR #{pr_number} Review"

            repo_root = resolve_repo_root(Path.cwd())
            wf_mode = _prompt_workflow_mode()

            worktree_path: Path | None = None
            if wf_mode == "worktree":
                worktree_path = checkout_pr_branch(
                    repo_root=repo_root,
                    branch_name=branch_name,
                    worktree_root_template=git_cfg.worktree_root,
                )
                work_dir = worktree_path
            elif wf_mode == "branch":
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
                work_dir = Path.cwd()

            session_name = build_session_name(target, pr_number=pr_number) if detach else None

            _init_create_and_launch(
                repo_root=repo_root,
                work_dir=work_dir,
                worktree_path=worktree_path,
                branch_name=branch_name,
                name=pr_name,
                target_arg=target,
                resolved_skill=pr_skill,
                title=pr_name,
                ticket_id=ticket_id,
                session_name=session_name,
                pr_number=pr_number,
                pr_repo=f"{owner}/{repo}",
            )

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
