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
        cfg = default_config()
        cfg.thresholds.step_merge_min_words = 20
        cfg.thresholds.step_merge_max_words = 40
        out = _render("zing/plan-audit.md", config=cfg)
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


class TestCustomAuditMd(unittest.TestCase):
    def test_custom_audit_md_substitutions(self) -> None:
        out = _render("zing/custom-audit.md")
        self.assertIn("~2000", out)
        self.assertIn("~5000", out)
        self.assertIn("about 25", out)
        self.assertIn(">50 files", out)


class TestPrAuditMd(unittest.TestCase):
    def test_pr_audit_md_substitutions(self) -> None:
        out = _render("zing/pr-audit.md")
        self.assertIn("%Y-%m-%d-%H%M", out)


class TestPrAuditVisualMd(unittest.TestCase):
    def test_pr_audit_visual_md_substitutions(self) -> None:
        out = _render("zing/pr-audit-visual.md")
        # Visual one uses browser timeout
        self.assertIn("~10 seconds", out)


class TestPrRespondMd(unittest.TestCase):
    def test_pr_respond_md_substitutions(self) -> None:
        out = _render("zing/pr-respond.md")
        self.assertIn("Co-Authored-By: Zing <zing@farmerpete.net>", out)


class TestReviewCoreMd(unittest.TestCase):
    def test_review_core_md_substitutions(self) -> None:
        out = _render("_shared/review-core.md")
        self.assertIn("over 1000 lines", out)
        self.assertIn('model: "sonnet"', out)

    def test_review_core_omits_agents_1_3_model_when_empty(self) -> None:
        out = _render("_shared/review-core.md")
        # default review_agents_1_3 = "" so model: "" must NOT appear
        self.assertNotIn('model: ""', out)


class TestBuildMdWorkflowModes(unittest.TestCase):
    def _render_with_mode(self, mode: str) -> str:
        from zing_ai.config import default_config

        cfg = default_config()
        cfg.git.workflow_mode = mode  # type: ignore[assignment]
        return _render("zing/build.md", config=cfg)

    def test_build_md_branch_mode(self) -> None:
        out = self._render_with_mode("branch")
        self.assertIn("git checkout -b", out)
        self.assertNotIn("git worktree add", out)

    def test_build_md_worktree_mode(self) -> None:
        out = self._render_with_mode("worktree")
        self.assertIn("git worktree add", out)
        self.assertIn("zing-init.sh", out)
        self.assertIn("ZING_WORKTREE_PATH", out)
        self.assertIn("worktree_path:", out)

    def test_build_md_none_mode(self) -> None:
        out = self._render_with_mode("none")
        self.assertIn("No isolation", out)
        self.assertNotIn("git checkout -b", out)
        self.assertNotIn("git worktree add", out)

    def test_build_md_ask_mode(self) -> None:
        out = self._render_with_mode("ask")
        self.assertIn("AskUserQuestion", out)

    def test_review_core_includes_agents_1_3_model_when_set(self) -> None:
        from zing_ai.config import default_config

        cfg = default_config()
        cfg.models.review_agents_1_3 = "opus"
        out = _render("_shared/review-core.md", config=cfg)
        self.assertIn('model: "opus"', out)


class TestWorktreePathDocs(unittest.TestCase):
    def test_audit_files_mention_worktree_path(self) -> None:
        for name in ("zing/build-audit.md", "zing/pr-audit.md", "zing/pr-respond.md"):
            out = _render(name)
            with self.subTest(file=name):
                self.assertIn("worktree_path", out)
