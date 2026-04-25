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
    """Write config.kdl and bare.kdl if they don't exist. Return (config_path, config_dir)."""
    data_dir = get_zellij_data_dir()
    config_path = data_dir / "config.kdl"
    if not config_path.exists():
        config_path.write_text(_CONFIG_KDL)
    bare_layout = data_dir / "bare.kdl"
    if not bare_layout.exists():
        bare_layout.write_text(_BARE_LAYOUT_KDL)
    return config_path, data_dir


def write_command_layout(command: str, args: list[str]) -> Path:
    """Write a temporary layout file for launching a command in a Zellij pane.

    Layout files are written to /tmp (not the persistent data dir) so the OS
    cleans them up on reboot. They are only needed during the `zellij attach`
    call that creates the session.
    """
    import tempfile

    args_kdl = "\n".join(f'        "{a}"' for a in args)
    layout = f"""\
layout {{
    pane command="{command}" {{
        args {args_kdl}
    }}
}}
"""
    fd, path = tempfile.mkstemp(suffix=".kdl", prefix="zing-layout-")
    os.write(fd, layout.encode())
    os.close(fd)
    return Path(path)
