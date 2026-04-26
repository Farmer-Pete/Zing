"""Template lint tests (Decision #25 from realign-datastar-usage plan).

Enforces project conventions about Datastar attribute usage so regressions
do not silently slip back in.
"""

from __future__ import annotations

import pathlib
import re

TEMPLATE_ROOT = pathlib.Path(__file__).parent.parent / "src/zing_ai/server/templates"
DASHED_RE = re.compile(r"data-on-[a-z]")
ALLOWED_PATTERNS = {
    "data-on-load",
    "data-on-signal-patch",
    "data-on-signal-patch-filter",
}


def test_no_dashed_data_on_attributes() -> None:
    """Event listeners must use the colon form (``data-on:click``).

    Datastar v1 accepts both ``data-on:click`` (canonical) and the legacy
    dashed form ``data-on-click``. Mixing forms is a stale-pattern smell;
    this test pins the project to the colon form. Three exceptions are
    allowed: ``data-on-load``, ``data-on-signal-patch``,
    ``data-on-signal-patch-filter`` — these are attribute names in v1,
    not event listeners (verified via the Datastar reference).
    """
    offenders: list[str] = []
    for path in TEMPLATE_ROOT.rglob("*.html"):
        text = path.read_text()
        for match in DASHED_RE.finditer(text):
            tail = text[match.start() : match.start() + 50]
            attr_name = tail.split('"')[0].split("'")[0].split("=")[0].strip()
            if attr_name in ALLOWED_PATTERNS:
                continue
            line_num = text[: match.start()].count("\n") + 1
            offenders.append(
                f"{path.relative_to(TEMPLATE_ROOT.parent.parent)}:{line_num} → {attr_name}"
            )
    assert not offenders, "Dashed data-on-* attributes found:\n" + "\n".join(offenders)
