"""Playwright test for the /install page autosave + badge flip."""

from __future__ import annotations

import shutil
import tempfile
from pathlib import Path

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo

pytestmark = pytest.mark.ui


@pytest.fixture
def tmp_install_env(monkeypatch):
    tmp_install = tempfile.mkdtemp()
    tmp_config_dir = tempfile.mkdtemp()
    tmp_config = Path(tmp_config_dir) / "config.toml"

    # Patch config_path so /config and /install routes use a temp file
    import zing_ai.config as cfg_mod

    monkeypatch.setattr(cfg_mod, "config_path", lambda: tmp_config)
    from zing_ai.config import default_config, save_config

    save_config(default_config())

    # Patch _install_target_for so the /install page checks our temp dirs
    import zing_ai.server.routes_install as install_mod

    def fake_target(runtime: str) -> Path:
        return Path(tmp_install) / runtime

    monkeypatch.setattr(install_mod, "_install_target_for", fake_target)

    # Stub install_claude / install_opencode to write a fresh manifest
    # without touching real installer behavior
    from zing_ai import __version__
    from zing_ai.config import config_hash
    from zing_ai.manifest import write_manifest

    def fake_install(target_dir=None, config=None):
        assert target_dir is not None
        target_dir.mkdir(parents=True, exist_ok=True)
        write_manifest(
            target_dir,
            "claude-code",
            [],
            config_hash=config_hash(config or default_config()),
            source_mtime_max=None,
            package_version=__version__,
        )

    monkeypatch.setattr(install_mod, "install_claude", fake_install)
    monkeypatch.setattr(install_mod, "install_opencode", fake_install)

    yield tmp_install
    shutil.rmtree(tmp_install, ignore_errors=True)
    shutil.rmtree(tmp_config_dir, ignore_errors=True)


def test_install_page_flow(server: _ServerInfo, page: Page, tmp_install_env) -> None:
    """Initial visit shows pending; clicking install flips badge to Up to date."""
    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)

    # 1. Initial visit — both runtimes should be "Updates pending" (no manifest yet)
    page.goto(f"{server.base_url}/install")
    page.wait_for_load_state("networkidle", timeout=5000)
    expect(page.locator("#install-status-claude")).to_contain_text("Updates pending", timeout=3000)
    expect(page.locator("#install-status-opencode")).to_contain_text(
        "Updates pending", timeout=3000
    )

    # 2. Click claude install button; wait for the POST to /install/run to complete
    with page.expect_response(
        lambda r: "/install/run" in r.url and r.status == 200,
        timeout=8000,
    ):
        page.locator("#install-status-claude button.install-btn").click()

    # 3. After install, claude badge should flip to "Up to date"
    # The SSE response patches the DOM asynchronously via Datastar
    expect(page.locator("#install-status-claude")).to_contain_text("Up to date", timeout=5000)
    # opencode still pending
    expect(page.locator("#install-status-opencode")).to_contain_text(
        "Updates pending", timeout=3000
    )

    assert errors == [], f"JS console errors: {errors}"
