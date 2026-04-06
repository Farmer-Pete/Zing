"""Jinja rendering pipeline for command markdown templates.

Renders command markdowns at install time so the LLM only ever sees
fully-resolved markdown — config values and conditionals are baked in
before files are written to the runtime directory.

Note: this module uses `from_string` only — no PackageLoader is
configured. The Jinja env here is intentionally separate from the web
server's env in `server/templates.py` (different autoescape and
undefined behavior).
"""

from __future__ import annotations

import jinja2

from zing_ai.config import Config

_env = jinja2.Environment(
    undefined=jinja2.StrictUndefined,
    autoescape=False,
    keep_trailing_newline=True,
)


def render_template(text: str, config: Config) -> str:
    """Render a single template string against a Config.

    Raises jinja2.UndefinedError if the template references a missing
    config key — fail loud at install time rather than producing a
    silent empty string in installed command markdown.
    """
    return _env.from_string(text).render(**config.model_dump())
