"""Datastar signal-key sanitisation shared by models and templates.

This module is the single source of truth for the rule that turns a free-form
identifier (Linear ticket key, session id, repo name, etc.) into a fragment
safe to compose into a Datastar signal-property name. Datastar parses
``$busyButtons.launch_BAK-1234`` as subtraction; replacing non-alphanumerics
with ``_`` keeps the surrounding expression syntactically valid.
"""

from __future__ import annotations

import re

# Replace any character outside [A-Za-z0-9_] with `_` so the resulting string is
# safe to use as a JavaScript identifier suffix.
_SIGNAL_KEY_RE = re.compile(r"[^A-Za-z0-9_]")


def to_signal_key(value: str) -> str:
    """Sanitise *value* for inclusion in a Datastar signal-property name."""
    return _SIGNAL_KEY_RE.sub("_", value)
