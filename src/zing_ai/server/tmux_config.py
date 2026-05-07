"""Managed tmux config for zing-ai sessions.

Writes a small ``tmux.conf`` to the persistent data directory and points
``tmux -f <path>`` at it. The user's own ``~/.tmux.conf`` is left untouched.

Mouse mode is the only setting we need today: it lets the wheel scroll the
Claude Code TUI inside an iframe, which it can't do otherwise. Scrollback
history is intentionally not configured — Claude Code's fullscreen rendering
manages its own scrollback in the alternate screen buffer.
"""

from __future__ import annotations

from pathlib import Path

_DATA_DIR = Path.home() / ".local" / "share" / "zing-ai" / "tmux"

_TMUX_CONF = """\
set -g mouse on
"""


def get_tmux_data_dir() -> Path:
    """Return the persistent data directory, creating it if needed."""
    _DATA_DIR.mkdir(parents=True, exist_ok=True)
    return _DATA_DIR


def ensure_tmux_config() -> Path:
    """Write the managed tmux.conf, overwriting if content has drifted.

    Returns the path to the written conf file. The file is owned by zing-ai
    (not user-customisable in place); bundled defaults are always re-applied
    so future option additions take effect on the next launch.
    """
    data_dir = get_tmux_data_dir()
    conf_path = data_dir / "tmux.conf"
    if not conf_path.exists() or conf_path.read_text() != _TMUX_CONF:
        conf_path.write_text(_TMUX_CONF)
    return conf_path
