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


# Match a literal id="..." attribute. We deliberately don't try to handle
# templated ids (those contain Jinja tags); the regex below ignores any
# id whose value contains "{{" or "{%" because at lint time we can't tell
# whether the rendered ids will collide. This still catches the bug class
# the lint exists for: hard-coded "id=..." literals duplicated within a
# single fragment.
_ID_LITERAL_RE = re.compile(r'\bid="([^"{}]+)"')

# A handful of fragments are *partials* that get rendered multiple times in
# a single page (e.g. one per row in a list comprehension). Their literal
# ids necessarily repeat at the *page* level — but they're still unique
# within a single render of the fragment, which is what this lint enforces.
# These shouldn't be allowed to have literal-id collisions either.
_FRAGMENTS_TO_LINT = [
    "fragments/kanban_card.html",
    "fragments/kanban_board.html",
    "fragments/kanban_column.html",
    "fragments/kanban_column_done.html",
    "fragments/kanban_column_review.html",
    "fragments/review_drawer.html",
    "fragments/drawer_attach.html",
    "fragments/drawer_step_history.html",
    "fragments/launch_button.html",
    "fragments/attention_bar.html",
    "fragments/management_tray.html",
    "command_center.html",
]


def test_no_duplicate_literal_html_ids_within_fragments() -> None:
    """No template fragment may emit two literal ``id="..."`` with the same value.

    CSS ``#id`` selectors and ``getElementById`` both match the *first*
    element only. Multiple identical literal ids inside one fragment is a
    silent bug — server SSE patches and Datastar selectors alike will only
    affect one of the supposed-to-be-multiple elements. Templated ids
    (those containing Jinja tags ``{{`` / ``{%``) are skipped because lint
    can't reason about runtime collisions; per-row macros disambiguate
    them with a slot suffix (see ``launch_button.html``).
    """
    offenders: list[str] = []
    # Strip Jinja {# ... #} comments before scanning so docs/examples inside
    # comments don't count.
    jinja_comment_re = re.compile(r"\{#.*?#\}", re.DOTALL)
    for rel in _FRAGMENTS_TO_LINT:
        path = TEMPLATE_ROOT / rel
        if not path.exists():
            continue
        raw = path.read_text()
        text = jinja_comment_re.sub("", raw)
        seen: dict[str, list[int]] = {}
        for match in _ID_LITERAL_RE.finditer(text):
            value = match.group(1)
            line = text[: match.start()].count("\n") + 1
            seen.setdefault(value, []).append(line)
        for value, lines in seen.items():
            if len(lines) > 1:
                offenders.append(f"{rel}: duplicate literal id={value!r} on lines {lines}")
    assert not offenders, "Duplicate literal HTML ids in fragment(s):\n" + "\n".join(offenders)
