"""Playwright tests for the /config autosave UX."""

from __future__ import annotations

import shutil
import tempfile
from pathlib import Path

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo

pytestmark = pytest.mark.ui


@pytest.fixture
def tmp_config(monkeypatch):
    """Redirect config_path to a temp file so tests never touch the user's config."""
    tmpdir = tempfile.mkdtemp()
    tmp_path = Path(tmpdir) / "config.toml"
    import zing_ai.config as cfg_mod

    monkeypatch.setattr(cfg_mod, "config_path", lambda: tmp_path)
    # Seed with defaults so the file exists before the server tries to read it
    from zing_ai.config import default_config, save_config

    save_config(default_config())
    yield tmp_path
    shutil.rmtree(tmpdir, ignore_errors=True)


def test_autosave_on_change(server: _ServerInfo, page: Page, tmp_config: Path) -> None:
    """Changing a number input triggers a POST that persists the new value."""
    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)

    page.goto(f"{server.base_url}/config")
    page.wait_for_load_state("networkidle", timeout=5000)

    locator = page.locator("#input-thresholds_large_file_lines")
    locator.fill("1500")

    with page.expect_response(
        lambda r: "/config/save/thresholds" in r.url and r.status == 200,
        timeout=5000,
    ):
        locator.dispatch_event("change")

    from zing_ai.config import load_config

    assert load_config().thresholds.large_file_lines == 1500
    assert errors == [], f"JS console errors: {errors}"


def test_saved_value_persists_on_reload(server: _ServerInfo, page: Page, tmp_config: Path) -> None:
    """A value saved via autosave is still present after a full page reload."""
    page.goto(f"{server.base_url}/config")
    page.wait_for_load_state("networkidle", timeout=5000)

    locator = page.locator("#input-thresholds_large_file_lines")
    locator.fill("1500")

    with page.expect_response(
        lambda r: "/config/save/thresholds" in r.url and r.status == 200,
        timeout=5000,
    ):
        locator.dispatch_event("change")

    # Reload the page — the value should come back from saved config
    page.reload()
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    reloaded = page.locator("#input-thresholds_large_file_lines")
    expect(reloaded).to_have_value("1500", timeout=3000)


def test_validation_error_leaves_page_usable(
    server: _ServerInfo, page: Page, tmp_config: Path
) -> None:
    """Submitting a negative value either persists or fails gracefully; page stays usable."""
    page.goto(f"{server.base_url}/config")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    locator = page.locator("#input-thresholds_large_file_lines")

    with page.expect_response(
        lambda r: "/config/save/thresholds" in r.url,
        timeout=5000,
    ) as resp_info:
        locator.fill("-1")
        locator.dispatch_event("change")

    # Server must reject the negative value with 422
    assert resp_info.value.status == 422

    # The page must still be rendered and show the heading — no crash
    expect(page.locator("h1")).to_have_text("Configuration", timeout=3000)


def test_select_autosave(server: _ServerInfo, page: Page, tmp_config: Path) -> None:
    """Changing a select input immediately POSTs and persists the new value."""
    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)

    page.goto(f"{server.base_url}/config")
    page.wait_for_load_state("networkidle", timeout=5000)

    locator = page.locator("#input-git_workflow_mode")

    with page.expect_response(
        lambda r: "/config/save/git" in r.url and r.status == 200,
        timeout=5000,
    ):
        locator.select_option("worktree")

    from zing_ai.config import load_config

    assert load_config().git.workflow_mode == "worktree"
    assert errors == [], f"JS console errors: {errors}"


def test_no_save_button_exists(server: _ServerInfo, page: Page, tmp_config: Path) -> None:
    """The config page has no submit button — saving is fully automatic."""
    page.goto(f"{server.base_url}/config")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    # No <button type="submit"> anywhere on the page
    assert page.locator("button[type=submit]").count() == 0

    # No button labelled exactly "Save"
    assert page.get_by_role("button", name="Save").count() == 0
