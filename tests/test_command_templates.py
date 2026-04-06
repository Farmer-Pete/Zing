"""Per-file content tests for templatized command markdowns."""

from __future__ import annotations

import importlib.resources
import unittest

from zing_ai.config import default_config
from zing_ai.templating import render_template


def _render(name: str, config=None):
    """Render a bundled command markdown by relative name (e.g. 'zing/build.md')."""
    cfg = config if config is not None else default_config()
    parts = name.split("/")
    res = importlib.resources.files("zing_ai.commands")
    for p in parts:
        res = res.joinpath(p)
    text = res.read_text(encoding="utf-8")
    return render_template(text, cfg)


class TestBuildMd(unittest.TestCase):
    def test_build_md_substitutions(self) -> None:
        out = _render("zing/build.md")
        self.assertIn("60", out)
        self.assertIn("zing/", out)
        self.assertIn("Co-Authored-By: Zing <zing@farmerpete.net>", out)
        self.assertIn('model: "sonnet"', out)


class TestPlanMd(unittest.TestCase):
    def test_plan_md_substitutions(self) -> None:
        out = _render("zing/plan.md")
        self.assertIn("150 words", out)
        self.assertIn("around 4", out)
        self.assertIn('model: "sonnet"', out)
        self.assertIn("≤3 steps", out)


class TestPlanAuditMd(unittest.TestCase):
    def test_plan_audit_md_substitutions(self) -> None:
        out = _render("zing/plan-audit.md")
        self.assertIn("4", out)
        self.assertIn('model: "sonnet"', out)
        self.assertIn("< 20 words", out)
        self.assertIn("> 40 words", out)


class TestBuildAuditMd(unittest.TestCase):
    def test_build_audit_md_substitutions(self) -> None:
        out = _render("zing/build-audit.md")
        self.assertIn("<5 files", out)
        self.assertIn("<100 total lines", out)
        self.assertIn("%Y-%m-%d-%H%M", out)
