"""Tests for the zing-ai CLI argument parsing and dispatch."""

from __future__ import annotations

import io
import sys
import unittest
from unittest.mock import patch

from zing_ai.cli import build_parser, main, _resolve_runtimes


class TestBuildParser(unittest.TestCase):
    """Verify the argparse structure produced by ``build_parser``."""

    def setUp(self) -> None:
        self.parser = build_parser()

    # -- top-level -----------------------------------------------------------

    def test_no_args_prints_help_and_exits_zero(self) -> None:
        with self.assertRaises(SystemExit) as ctx:
            main([])
        self.assertEqual(ctx.exception.code, 0)

    def test_version_flag(self) -> None:
        with self.assertRaises(SystemExit) as ctx:
            self.parser.parse_args(["--version"])
        self.assertEqual(ctx.exception.code, 0)

    # -- install subcommand --------------------------------------------------

    def test_install_claude(self) -> None:
        args = self.parser.parse_args(["install", "--claude"])
        self.assertTrue(args.claude)
        self.assertFalse(args.opencode)
        self.assertFalse(args.all)
        self.assertEqual(args.command, "install")

    def test_install_opencode(self) -> None:
        args = self.parser.parse_args(["install", "--opencode"])
        self.assertFalse(args.claude)
        self.assertTrue(args.opencode)
        self.assertFalse(args.all)

    def test_install_all(self) -> None:
        args = self.parser.parse_args(["install", "--all"])
        self.assertTrue(args.all)

    def test_install_claude_and_opencode(self) -> None:
        args = self.parser.parse_args(["install", "--claude", "--opencode"])
        self.assertTrue(args.claude)
        self.assertTrue(args.opencode)

    # -- reapply-patches subcommand ------------------------------------------

    def test_reapply_patches_claude(self) -> None:
        args = self.parser.parse_args(["reapply-patches", "--claude"])
        self.assertTrue(args.claude)
        self.assertEqual(args.command, "reapply-patches")

    def test_reapply_patches_all(self) -> None:
        args = self.parser.parse_args(["reapply-patches", "--all"])
        self.assertTrue(args.all)


class TestResolveRuntimes(unittest.TestCase):
    """Verify runtime resolution logic."""

    def _make_ns(self, *, claude: bool = False, opencode: bool = False, all: bool = False) -> object:
        """Build a minimal namespace."""
        import argparse

        return argparse.Namespace(claude=claude, opencode=opencode, all=all)

    def test_all_flag_returns_both(self) -> None:
        result = _resolve_runtimes(self._make_ns(all=True))
        self.assertEqual(result, ["claude", "opencode"])

    def test_claude_only(self) -> None:
        result = _resolve_runtimes(self._make_ns(claude=True))
        self.assertEqual(result, ["claude"])

    def test_opencode_only(self) -> None:
        result = _resolve_runtimes(self._make_ns(opencode=True))
        self.assertEqual(result, ["opencode"])

    def test_both_explicit(self) -> None:
        result = _resolve_runtimes(self._make_ns(claude=True, opencode=True))
        self.assertEqual(result, ["claude", "opencode"])

    def test_all_with_claude_is_error(self) -> None:
        with self.assertRaises(SystemExit) as ctx:
            _resolve_runtimes(self._make_ns(all=True, claude=True))
        self.assertEqual(ctx.exception.code, 1)

    def test_all_with_opencode_is_error(self) -> None:
        with self.assertRaises(SystemExit) as ctx:
            _resolve_runtimes(self._make_ns(all=True, opencode=True))
        self.assertEqual(ctx.exception.code, 1)


class TestInteractivePrompt(unittest.TestCase):
    """Verify interactive runtime selection."""

    def _make_ns(self) -> object:
        import argparse

        return argparse.Namespace(claude=False, opencode=False, all=False)

    @patch("builtins.input", return_value="1")
    def test_choice_1_selects_claude(self, _mock_input: object) -> None:
        result = _resolve_runtimes(self._make_ns())
        self.assertEqual(result, ["claude"])

    @patch("builtins.input", return_value="2")
    def test_choice_2_selects_opencode(self, _mock_input: object) -> None:
        result = _resolve_runtimes(self._make_ns())
        self.assertEqual(result, ["opencode"])

    @patch("builtins.input", return_value="3")
    def test_choice_3_selects_all(self, _mock_input: object) -> None:
        result = _resolve_runtimes(self._make_ns())
        self.assertEqual(result, ["claude", "opencode"])

    @patch("builtins.input", side_effect=EOFError)
    def test_eof_exits_130(self, _mock_input: object) -> None:
        with self.assertRaises(SystemExit) as ctx:
            _resolve_runtimes(self._make_ns())
        self.assertEqual(ctx.exception.code, 130)

    @patch("builtins.input", side_effect=["x", "1"])
    def test_invalid_then_valid(self, _mock_input: object) -> None:
        result = _resolve_runtimes(self._make_ns())
        self.assertEqual(result, ["claude"])


class TestMainDispatch(unittest.TestCase):
    """Verify that ``main`` dispatches to the correct handler."""

    @patch("zing_ai.cli._handle_install")
    def test_install_dispatches(self, mock_handler: object) -> None:
        main(["install", "--claude"])
        mock_handler.assert_called_once()

    @patch("zing_ai.cli._handle_reapply_patches")
    def test_reapply_patches_dispatches(self, mock_handler: object) -> None:
        main(["reapply-patches", "--opencode"])
        mock_handler.assert_called_once()


if __name__ == "__main__":
    unittest.main()
