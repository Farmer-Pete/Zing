"""Template lint tests (Decision #25 from realign-datastar-usage plan).

Enforces project conventions about Datastar attribute usage so regressions
do not silently slip back in.
"""

from __future__ import annotations

import pathlib
import re

TEMPLATE_ROOT = pathlib.Path(__file__).parent.parent / "src/zing_ai/server/templates"
# Match a full attribute name starting with "data-on-" — letters, digits,
# dashes, and the v1 modifier separators (``__`` and ``.``). Anchoring on
# "data-on-" then consuming the whole token avoids false positives on
# prose mentions in attribute *values* (e.g. comments or strings) that
# happen to contain the substring "data-on-".
DASHED_RE = re.compile(r"\bdata-on-[a-z][a-zA-Z0-9_.-]*")
ALLOWED_PATTERNS = {
    "data-on-load",
    "data-on-signal-patch",
    "data-on-signal-patch-filter",
    # Canonical v1 directives that share the dashed prefix because they
    # are attribute names, not event listeners (verified via Datastar
    # reference docs at /websites/data-star_dev).
    "data-on-interval",
}
_JINJA_COMMENT_RE = re.compile(r"\{#.*?#\}", re.DOTALL)


def test_no_dashed_data_on_attributes() -> None:
    """Event listeners must use the colon form (``data-on:click``).

    Datastar v1 accepts both ``data-on:click`` (canonical) and the legacy
    dashed form ``data-on-click``. Mixing forms is a stale-pattern smell;
    this test pins the project to the colon form. A small allowlist covers
    real v1 attribute-style directives (``data-on-load``,
    ``data-on-signal-patch``, ``data-on-signal-patch-filter``,
    ``data-on-interval``) plus their modifier suffixes
    (e.g. ``data-on-interval__duration.30s``) — these are not event
    listeners (verified via the Datastar reference).
    """
    offenders: list[str] = []
    for path in TEMPLATE_ROOT.rglob("*.html"):
        # Strip Jinja comments so prose references to attribute names
        # don't trip the scanner.
        text = _JINJA_COMMENT_RE.sub("", path.read_text())
        for match in DASHED_RE.finditer(text):
            attr_name = match.group(0)
            # Strip any modifier suffix (``__duration.30s``) for the
            # allowlist comparison; the base name is what matters.
            base = attr_name.split("__", 1)[0]
            if base in ALLOWED_PATTERNS:
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
    "fragments/launch_button.html",
    "fragments/management_tray.html",
    "command_center.html",
    "fragments/flow_body_attach.html",
    "fragments/flow_body_empty.html",
    "fragments/flow_body_findings.html",
    "fragments/flow_body_question.html",
    "fragments/flow_palette.html",
    "fragments/flow_progress_strip.html",
    "fragments/flow_toolbar.html",
    "fragments/_flow_board_toggle.html",
    "fragments/launch_popup.html",
    "flow.html",
]


# Datastar v1's fetch-action options destructure (verified against
# ``starfederation/datastar@1.0.0/bundles/datastar.js``). Anything not in
# this set is silently ignored at runtime — typos like ``body:`` (instead
# of ``payload:``) ship as no-ops with no warning. The lint below catches
# unknown keys at commit time so the silent-failure mode never recurs.
_ALLOWED_FETCH_OPTIONS = frozenset(
    {
        "headers",
        "contentType",
        "filterSignals",
        "openWhenHidden",
        "payload",
        "requestCancellation",
        "retry",
        "retryInterval",
        "retryScaler",
        "retryMaxWait",
        "retryMaxCount",
        "abort",
        "selector",
    }
)
_FETCH_VERB_RE = re.compile(r"@(get|post|put|patch|delete)\s*\(")
_ATTR_VALUE_RE = re.compile(r'data-[\w:.-]+="([^"]*)"', re.DOTALL)


def _walk_balanced(text: str, start: int, opener: str, closer: str) -> int:
    """Return index just past the closer matching ``text[start] == opener``.

    Tracks string state so quoted braces/parens don't fool the count.
    Returns ``-1`` on imbalance (caller treats as "skip this match").
    """
    if start >= len(text) or text[start] != opener:
        return -1
    depth = 1
    in_string: str | None = None
    i = start + 1
    n = len(text)
    while i < n:
        c = text[i]
        if in_string is not None:
            if c == "\\":
                i += 2
                continue
            if c == in_string:
                in_string = None
            i += 1
            continue
        if c in ("'", '"', "`"):
            in_string = c
        elif c == opener:
            depth += 1
        elif c == closer:
            depth -= 1
            if depth == 0:
                return i + 1
        i += 1
    return -1


def _split_top_level_args(call_body: str) -> int:
    """Index in *call_body* just past the first top-level comma, or -1."""
    depth = 0
    in_string: str | None = None
    i = 0
    n = len(call_body)
    while i < n:
        c = call_body[i]
        if in_string is not None:
            if c == "\\":
                i += 2
                continue
            if c == in_string:
                in_string = None
            i += 1
            continue
        if c in ("'", '"', "`"):
            in_string = c
        elif c in "({[":
            depth += 1
        elif c in ")}]":
            depth -= 1
        elif c == "," and depth == 0:
            return i + 1
        i += 1
    return -1


def _extract_top_level_keys(options_body: str) -> list[str]:
    """Return identifier-like keys appearing at depth 0 of an options body.

    Skips quoted keys (``'foo': 1``) and ternary middles (``? a : b``) by
    inspecting the character immediately preceding the colon.
    """
    keys: list[str] = []
    depth = 0
    in_string: str | None = None
    i = 0
    n = len(options_body)
    while i < n:
        c = options_body[i]
        if in_string is not None:
            if c == "\\":
                i += 2
                continue
            if c == in_string:
                in_string = None
            i += 1
            continue
        if c in ("'", '"', "`"):
            in_string = c
        elif c in "({[":
            depth += 1
        elif c in ")}]":
            depth -= 1
        elif depth == 0 and c == ":":
            j = i - 1
            while j >= 0 and options_body[j].isspace():
                j -= 1
            end = j + 1
            while j >= 0 and (options_body[j].isalnum() or options_body[j] in "_$"):
                j -= 1
            key = options_body[j + 1 : end]
            preceding = options_body[j] if j >= 0 else ""
            # Skip empty keys, quoted-string keys, and ternary middles.
            if key and preceding not in ("'", '"', "?"):
                keys.append(key)
        i += 1
    return keys


def test_only_recognized_datastar_fetch_options() -> None:
    """``@get/@post/@put/@patch/@delete(...)`` may only use known v1 option keys.

    Datastar v1 silently ignores unrecognized fetch-action options — a typo
    like ``body:`` instead of ``payload:`` ships as a no-op with no
    runtime warning. This lint walks every Datastar fetch call in the
    templates and pins the option-key surface to the documented v1 list.
    """
    offenders: list[str] = []
    for path in TEMPLATE_ROOT.rglob("*.html"):
        text = _JINJA_COMMENT_RE.sub("", path.read_text())
        for attr_match in _ATTR_VALUE_RE.finditer(text):
            expr = attr_match.group(1)
            for verb_match in _FETCH_VERB_RE.finditer(expr):
                paren_open = expr.find("(", verb_match.start())
                if paren_open < 0:
                    continue
                call_end = _walk_balanced(expr, paren_open, "(", ")")
                if call_end < 0:
                    continue
                call_body = expr[paren_open + 1 : call_end - 1]
                arg2_start = _split_top_level_args(call_body)
                if arg2_start < 0:
                    continue
                j = arg2_start
                while j < len(call_body) and call_body[j].isspace():
                    j += 1
                if j >= len(call_body) or call_body[j] != "{":
                    continue
                opt_end = _walk_balanced(call_body, j, "{", "}")
                if opt_end < 0:
                    continue
                options_body = call_body[j + 1 : opt_end - 1]
                for key in _extract_top_level_keys(options_body):
                    if key in _ALLOWED_FETCH_OPTIONS:
                        continue
                    line_num = text[: attr_match.start(1) + verb_match.start()].count("\n") + 1
                    rel = path.relative_to(TEMPLATE_ROOT.parent.parent)
                    offenders.append(
                        f"{rel}:{line_num} → @{verb_match.group(1)}() option {key!r} "
                        f"is not a recognized Datastar v1 fetch-action key"
                    )
    assert not offenders, (
        "Unrecognized Datastar fetch-action options (these are silently "
        f"dropped at runtime). Allowed: {sorted(_ALLOWED_FETCH_OPTIONS)}\n" + "\n".join(offenders)
    )


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
