"""Jinja2 prompt template environment for Zing AI.

Provides a pre-configured Jinja2 environment that loads ``.j2`` templates
from the ``zing_ai/prompts`` package directory and a convenience function
for rendering them.
"""

from __future__ import annotations

import jinja2

_env = jinja2.Environment(
    loader=jinja2.PackageLoader("zing_ai", "prompts"),
    keep_trailing_newline=True,
    trim_blocks=True,
    lstrip_blocks=True,
)


def render_prompt(template_name: str, **context: object) -> str:
    """Load a Jinja2 template by name and render it with the given context.

    Parameters
    ----------
    template_name:
        File name of the template relative to the ``zing_ai/prompts``
        package directory (e.g. ``"new_investigate.md.j2"``).
    **context:
        Keyword arguments forwarded to :meth:`jinja2.Template.render`.

    Returns
    -------
    str
        The rendered template string.

    Raises
    ------
    jinja2.TemplateNotFound
        If *template_name* does not correspond to an existing template file.
    """
    template = _env.get_template(template_name)
    return template.render(**context)
