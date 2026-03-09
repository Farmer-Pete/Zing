"""Template rendering helpers for the Zing batch review server."""

from __future__ import annotations

import html
import logging
from typing import Any

import markupsafe
import mistune
from jinja2 import Environment, PackageLoader
from pygments import highlight as pygments_highlight
from pygments.formatters import HtmlFormatter
from pygments.lexers import get_lexer_by_name, guess_lexer
from pygments.util import ClassNotFound

logger = logging.getLogger(__name__)

_formatter = HtmlFormatter(nowrap=True, style="monokai")


class _PygmentsRenderer(mistune.HTMLRenderer):
    """Mistune HTML renderer that syntax-highlights fenced code blocks with Pygments."""

    def block_code(self, code: str, info: str | None = None) -> str:
        """Render a fenced code block with Pygments syntax highlighting."""
        lang = info.split()[0] if info else ""
        try:
            lexer = get_lexer_by_name(lang, stripall=True) if lang else guess_lexer(code)
        except ClassNotFound:
            lexer = get_lexer_by_name("text", stripall=True)
        highlighted = pygments_highlight(code, lexer, _formatter)
        return f'<div class="highlight"><pre><code>{highlighted}</code></pre></div>\n'


_renderer = _PygmentsRenderer()
_markdown = mistune.create_markdown(renderer=_renderer)


def _render_markdown(text: str | None) -> markupsafe.Markup:
    """Jinja2 filter: convert markdown to HTML with syntax-highlighted code blocks.

    Returns a Markup object so Jinja2 doesn't double-escape the HTML.
    Falls back to HTML-escaped plain text in <pre> tags if rendering fails.
    """
    if not text:
        return markupsafe.Markup("")
    try:
        result = _markdown(text)
        return markupsafe.Markup(result)
    except Exception:
        logger.exception("Markdown rendering failed, falling back to plain text")
        return markupsafe.Markup(f"<pre>{html.escape(text)}</pre>")


_env = Environment(
    loader=PackageLoader("zing_ai", "server/templates"),
    autoescape=True,
)
_env.filters["markdown"] = _render_markdown


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
