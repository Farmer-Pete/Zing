"""Full-tree smoke test: every bundled command markdown must render cleanly."""

from __future__ import annotations

import importlib.resources
import unittest

from zing_ai.config import default_config
from zing_ai.templating import render_template


def _iter_command_files():
    """Yield (relative_name, text) for every .md under zing_ai.commands."""
    root = importlib.resources.files("zing_ai.commands")

    def _walk(node, prefix: str):
        for child in node.iterdir():
            name = child.name
            if child.is_dir():
                yield from _walk(child, f"{prefix}{name}/")
            elif name.endswith(".md"):
                yield (f"{prefix}{name}", child.read_text(encoding="utf-8"))

    yield from _walk(root, "")


class TestTemplatesRender(unittest.TestCase):
    def test_every_command_renders_with_default_config(self) -> None:
        cfg = default_config()
        files = list(_iter_command_files())
        self.assertGreater(len(files), 0, "no command markdowns found")
        for name, text in files:
            with self.subTest(name=name):
                try:
                    render_template(text, cfg)
                except Exception as e:
                    self.fail(f"render failed for {name}: {e}")
