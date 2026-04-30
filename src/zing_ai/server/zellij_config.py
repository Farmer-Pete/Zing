"""Write Zellij config and layout files to the persistent data directory."""

from __future__ import annotations

import os
from pathlib import Path

_ZELLIJ_DATA_DIR = Path.home() / ".local" / "share" / "zing-ai" / "zellij"

_CONFIG_KDL = """\
keybinds clear-defaults=true {}
theme "default"
default_layout "bare"
pane_frames false
scroll_buffer_size 50000
web_sharing "on"
simplified_ui true
show_startup_tips false
show_release_notes false
"""

_BARE_LAYOUT_KDL = """\
layout {
    pane
}
"""


def get_zellij_data_dir() -> Path:
    """Return the persistent Zellij data directory, creating it if needed."""
    _ZELLIJ_DATA_DIR.mkdir(parents=True, exist_ok=True)
    return _ZELLIJ_DATA_DIR


def ensure_zellij_config() -> tuple[Path, Path]:
    """Write config.kdl and bare.kdl, overwriting if content has drifted.

    These files are owned by zing-ai (not user-customisable in place), so the
    bundled defaults are always written. This lets us add or change options
    (e.g. ``show_startup_tips false``) and have them take effect on the next
    launch without users having to delete their config manually.

    Returns:
        (config_path, config_dir).
    """
    data_dir = get_zellij_data_dir()
    config_path = data_dir / "config.kdl"
    if not config_path.exists() or config_path.read_text() != _CONFIG_KDL:
        config_path.write_text(_CONFIG_KDL)
    bare_layout = data_dir / "bare.kdl"
    if not bare_layout.exists() or bare_layout.read_text() != _BARE_LAYOUT_KDL:
        bare_layout.write_text(_BARE_LAYOUT_KDL)
    return config_path, data_dir


def _kdl_quote(s: str) -> str:
    """Wrap *s* in double quotes, escaping characters that KDL strings reserve."""
    escaped = s.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n").replace("\t", "\\t")
    return f'"{escaped}"'


def write_command_layout(command: str, args: list[str]) -> Path:
    """Write a temporary layout file for launching a command in a Zellij pane.

    Layout files are written to /tmp (not the persistent data dir) so the OS
    cleans them up on reboot. They are only needed during the `zellij attach`
    call that creates the session.
    """
    import tempfile

    args_kdl = " ".join(_kdl_quote(a) for a in args)
    layout = f"""\
layout {{
    pane command={_kdl_quote(command)} {{
        args {args_kdl}
    }}
}}
"""
    fd, path = tempfile.mkstemp(suffix=".kdl", prefix="zing-layout-")
    os.write(fd, layout.encode())
    os.close(fd)
    return Path(path)
