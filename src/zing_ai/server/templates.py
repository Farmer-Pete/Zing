"""Template rendering helpers for the Zing batch review server."""

from __future__ import annotations

import html
import json
import logging
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import markupsafe
import mistune
from jinja2 import Environment, PackageLoader
from pygments import highlight as pygments_highlight
from pygments.formatters import HtmlFormatter
from pygments.lexers import get_lexer_by_name, guess_lexer
from pygments.util import ClassNotFound

from zing_ai.server.card_view import build_card_view, column_from_cls

logger = logging.getLogger(__name__)

_formatter = HtmlFormatter(nowrap=True, style="monokai")


class _PygmentsRenderer(mistune.HTMLRenderer):
    """Mistune HTML renderer that syntax-highlights fenced code blocks with Pygments.

    Raw HTML tags in markdown are escaped to prevent XSS.
    """

    def html(self, text: str) -> str:
        """Escape raw HTML blocks to prevent stored XSS."""
        return html.escape(text)

    def inline_html(self, text: str) -> str:
        """Escape inline raw HTML to prevent stored XSS."""
        return html.escape(text)

    def block_code(self, code: str, info: str | None = None) -> str:
        """Render a fenced code block with Pygments syntax highlighting.

        Mermaid blocks are rendered as ``<pre class="mermaid">`` so the
        Mermaid JS client-side library can pick them up.
        """
        lang = info.split()[0] if info else ""
        if lang == "mermaid":
            return f'<pre class="mermaid">{html.escape(code)}</pre>\n'
        try:
            lexer = get_lexer_by_name(lang, stripall=True) if lang else guess_lexer(code)
        except ClassNotFound:
            lexer = get_lexer_by_name("text", stripall=True)
        highlighted = pygments_highlight(code, lexer, _formatter)
        return f'<div class="highlight"><pre><code>{highlighted}</code></pre></div>\n'


_renderer = _PygmentsRenderer()
_markdown = mistune.create_markdown(
    renderer=_renderer,
    plugins=["table", "strikethrough", "task_lists", "footnotes"],
)


def _strip_frontmatter(text: str) -> str:
    """Strip YAML frontmatter (``---\\n...\\n---``) from the head of a doc.

    Zing plan markdowns start with a YAML block delimited by ``---`` lines
    that the markdown renderer would otherwise display as literal text.
    Mirrors python-frontmatter's split semantics without the dependency.
    """
    if not text.startswith("---\n"):
        return text
    end = text.find("\n---", 4)
    if end == -1:
        return text
    after = end + len("\n---")
    # Consume the line terminator after the closing fence so the rendered
    # markdown doesn't start with a leading blank line.
    if after < len(text) and text[after] == "\n":
        after += 1
    return text[after:]


def _render_markdown(text: str | None) -> markupsafe.Markup:
    """Jinja2 filter: convert markdown to HTML with syntax-highlighted code blocks.

    Returns a Markup object so Jinja2 doesn't double-escape the HTML.
    Falls back to HTML-escaped plain text in <pre> tags if rendering fails.
    """
    if not text:
        return markupsafe.Markup("")
    try:
        result = _markdown(_strip_frontmatter(text))
        return markupsafe.Markup(result)
    except Exception:
        logger.exception("Markdown rendering failed, falling back to plain text")
        return markupsafe.Markup(f"<pre>{html.escape(text)}</pre>")


def render_markdown(text: str) -> markupsafe.Markup:
    """Render markdown text to HTML with syntax-highlighted code blocks.

    Public wrapper around the internal markdown renderer for use outside
    of Jinja2 templates (e.g. rendering zing file content in route handlers).
    """
    return _render_markdown(text)


def _humanize_time(value: Any) -> str:
    """Jinja2 filter: render a datetime as a relative time string.

    Accepts a datetime, an ISO-8601 string, or None. Returns "just now",
    "N minutes ago", "N hours ago", "N days ago", or an absolute date for
    older values.
    """
    if value is None or value == "":
        return ""
    if isinstance(value, str):
        try:
            value = datetime.fromisoformat(value)
        except ValueError:
            return value
    if not isinstance(value, datetime):
        return str(value)
    if value.tzinfo is None:
        value = value.replace(tzinfo=UTC)
    delta = datetime.now(UTC) - value
    seconds = int(delta.total_seconds())
    if seconds < 60:
        return "just now"
    minutes = seconds // 60
    if minutes < 60:
        return f"{minutes} minute{'s' if minutes != 1 else ''} ago"
    hours = minutes // 60
    if hours < 24:
        return f"{hours} hour{'s' if hours != 1 else ''} ago"
    days = hours // 24
    if days < 30:
        return f"{days} day{'s' if days != 1 else ''} ago"
    return f"on {value.strftime('%Y-%m-%d')}"


def _js_str(value: Any) -> markupsafe.Markup:
    """Jinja2 filter: return ``value`` rendered as a safe JS string literal.

    Use inside a Datastar / inline JS expression embedded in an HTML attribute,
    e.g. ``data-on:click="@post(..., {payload: {card_key: {{ card.key | js_str }}}})"``.

    Why this exists: ``| e`` (autoescape) converts ``'`` to ``&#39;`` which the
    browser decodes back to ``'`` inside a quoted attribute, breaking any
    surrounding single-quoted JS string. ``json.dumps`` produces a properly
    JS-escaped double-quoted string literal; the ``"`` characters that delimit
    that literal would themselves break a surrounding ``data-foo="..."``
    attribute, so they are HTML-escaped to ``&quot;`` here. The browser
    decodes ``&quot;`` back to ``"`` *inside attribute parsing only*, so the
    JS literal stays intact.

    The returned string includes the surrounding (escaped) double quotes —
    callers should NOT add their own quotes:

        data-foo="alert({{ name | js_str }})"  →  data-foo="alert(&quot;world&quot;)"
    """
    if value is None:
        return markupsafe.Markup("&quot;&quot;")
    text = value if isinstance(value, str) else str(value)
    # json.dumps handles JS-side escaping of \, control chars, and unicode.
    # html.escape(quote=True) then HTML-escapes the surrounding " (and any
    # < > & that json.dumps left alone) so the literal can sit inside a
    # double-quoted HTML attribute without breaking it.
    return markupsafe.Markup(html.escape(json.dumps(text), quote=True))


_env = Environment(
    loader=PackageLoader("zing_ai", "server/templates"),
    autoescape=True,
)
_env.filters["markdown"] = _render_markdown
_env.filters["humanize_time"] = _humanize_time
_env.filters["js_str"] = _js_str


def _compute_asset_version() -> str:
    """Return a cache-busting token derived from CSS and JS file mtimes."""
    static_dir = Path(__file__).parent / "static"
    mtimes: list[int] = []
    for sub, pattern in (("css", "*.css"), ("js", "*.js")):
        try:
            mtimes.extend(int(p.stat().st_mtime) for p in (static_dir / sub).glob(pattern))
        except FileNotFoundError:
            continue
    return str(max(mtimes)) if mtimes else "0"


_env.globals["asset_version"] = _compute_asset_version
_env.globals["build_card_view"] = build_card_view
_env.globals["column_from_cls"] = column_from_cls


def render(template_name: str, **context: Any) -> str:
    """Render a Jinja2 template from the server/templates directory.

    Args:
        template_name: Name of the template file (e.g. "review.html").
        **context: Template context variables.

    Returns:
        The rendered HTML string.
    """
    template = _env.get_template(template_name)
    return template.render(**context)
