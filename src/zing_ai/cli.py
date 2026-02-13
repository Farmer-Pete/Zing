"""CLI entry point for zing-ai."""

from __future__ import annotations

import argparse
import sys


RUNTIMES = ("claude", "opencode")


def _add_runtime_flags(parser: argparse.ArgumentParser) -> None:
    """Add --claude, --opencode, and --all flags to *parser*."""
    group = parser.add_argument_group("runtime selection")
    group.add_argument(
        "--claude",
        action="store_true",
        default=False,
        help="target Claude Code",
    )
    group.add_argument(
        "--opencode",
        action="store_true",
        default=False,
        help="target OpenCode",
    )
    group.add_argument(
        "--all",
        action="store_true",
        default=False,
        help="target all supported runtimes",
    )


def _resolve_runtimes(args: argparse.Namespace) -> list[str]:
    """Return the list of selected runtimes, prompting interactively if needed.

    Raises ``SystemExit`` when ``--all`` is combined with an explicit runtime
    flag (that is an invalid combination).
    """
    if args.all and (args.claude or args.opencode):
        print("error: --all cannot be combined with --claude or --opencode", file=sys.stderr)
        raise SystemExit(1)

    if args.all:
        return list(RUNTIMES)

    selected: list[str] = []
    if args.claude:
        selected.append("claude")
    if args.opencode:
        selected.append("opencode")

    if selected:
        return selected

    # Interactive selection
    return _prompt_runtime_selection()


def _prompt_runtime_selection() -> list[str]:
    """Interactively ask the user which runtimes to target."""
    print("Which runtimes would you like to target?\n")
    print("  1) Claude Code")
    print("  2) OpenCode")
    print("  3) All")
    print()

    while True:
        try:
            choice = input("Enter choice [1/2/3]: ").strip()
        except (EOFError, KeyboardInterrupt):
            print(file=sys.stderr)
            raise SystemExit(130)

        if choice == "1":
            return ["claude"]
        if choice == "2":
            return ["opencode"]
        if choice == "3":
            return list(RUNTIMES)

        print(f"Invalid choice: {choice!r}. Please enter 1, 2, or 3.")


# ---- subcommand handlers ----------------------------------------------------


def _handle_install(args: argparse.Namespace) -> None:
    """Handle the ``install`` subcommand."""
    runtimes = _resolve_runtimes(args)
    for rt in runtimes:
        if rt == "claude":
            from zing_ai.installer import install_claude

            print("Installing for Claude Code...")
            install_claude()
            print("Claude Code commands installed successfully.")
        elif rt == "opencode":
            from zing_ai.installer import install_opencode

            print("Installing for OpenCode...")
            install_opencode()
            print("OpenCode commands installed successfully.")


def _handle_reapply_patches(args: argparse.Namespace) -> None:
    """Handle the ``reapply-patches`` subcommand."""
    runtimes = _resolve_runtimes(args)
    for rt in runtimes:
        print(f"Reapplying patches for {rt}...")
    # Actual patch logic will be added in a later step.


# ---- main -------------------------------------------------------------------


def build_parser() -> argparse.ArgumentParser:
    """Build and return the top-level argument parser."""
    parser = argparse.ArgumentParser(
        prog="zing-ai",
        description="Zing AI development pipeline installer",
    )
    parser.add_argument(
        "--version",
        action="version",
        version=f"%(prog)s {_get_version()}",
    )

    subparsers = parser.add_subparsers(dest="command")

    # install
    install_parser = subparsers.add_parser(
        "install",
        help="install Zing commands for the selected runtime(s)",
    )
    _add_runtime_flags(install_parser)
    install_parser.set_defaults(handler=_handle_install)

    # reapply-patches
    reapply_parser = subparsers.add_parser(
        "reapply-patches",
        help="re-apply patches for the selected runtime(s)",
    )
    _add_runtime_flags(reapply_parser)
    reapply_parser.set_defaults(handler=_handle_reapply_patches)

    return parser


def _get_version() -> str:
    from zing_ai import __version__

    return __version__


def main(argv: list[str] | None = None) -> None:
    """CLI entry point."""
    parser = build_parser()
    args = parser.parse_args(argv)

    if args.command is None:
        parser.print_help()
        raise SystemExit(0)

    args.handler(args)
