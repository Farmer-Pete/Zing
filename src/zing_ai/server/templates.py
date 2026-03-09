"""Template rendering helpers for the Zing batch review server."""

from __future__ import annotations

from typing import Any

from jinja2 import Environment, PackageLoader

_env = Environment(
    loader=PackageLoader("zing_ai", "server/templates"),
    autoescape=True,
)


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
