"""Playwright tests for the /config autosave UX."""

from __future__ import annotations

import shutil
import tempfile
from pathlib import Path

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo

pytestmark = pytest.mark.ui

# base.html loads external CDN scripts (Datastar, Mermaid, Google Fonts).
# Playwright's default goto wait_until="load" blocks until ALL resources
# finish, which can time out on slow networks or under CI load.  The config
# page only needs the DOM — use "domcontentloaded" everywhere.
_GOTO_WAIT = "domcontentloaded"


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

    page.goto(f"{server.base_url}/config", wait_until=_GOTO_WAIT)
    page.wait_for_timeout(300)

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
    page.goto(f"{server.base_url}/config", wait_until=_GOTO_WAIT)
    page.wait_for_timeout(300)

    locator = page.locator("#input-thresholds_large_file_lines")
    locator.fill("1500")

    with page.expect_response(
        lambda r: "/config/save/thresholds" in r.url and r.status == 200,
        timeout=5000,
    ):
        locator.dispatch_event("change")

    # Reload the page — the value should come back from saved config
    page.reload(wait_until=_GOTO_WAIT)
    page.wait_for_timeout(300)

    reloaded = page.locator("#input-thresholds_large_file_lines")
    expect(reloaded).to_have_value("1500", timeout=3000)


def test_validation_error_leaves_page_usable(
    server: _ServerInfo, page: Page, tmp_config: Path
) -> None:
    """Submitting a negative value either persists or fails gracefully; page stays usable."""
    page.goto(f"{server.base_url}/config", wait_until=_GOTO_WAIT)
    page.wait_for_timeout(300)

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

    page.goto(f"{server.base_url}/config", wait_until=_GOTO_WAIT)
    page.wait_for_timeout(300)

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
    page.goto(f"{server.base_url}/config", wait_until=_GOTO_WAIT)
    page.wait_for_timeout(300)

    # No <button type="submit"> anywhere on the page
    assert page.locator("button[type=submit]").count() == 0

    # No button labelled exactly "Save"
    assert page.get_by_role("button", name="Save").count() == 0


# ---------------------------------------------------------------------------
# GitHub repo checkbox tests (Datastar signals)
# ---------------------------------------------------------------------------


@pytest.fixture
def server_with_repos(server: _ServerInfo, tmp_config: Path) -> _ServerInfo:
    """Seed the external cache with two repos under the same owner."""
    server.external_cache.github_repos = ["acme/alpha", "acme/beta"]
    return server


def _wait_for_datastar(page: Page) -> None:
    """Wait for Datastar to initialize on the page by waiting for the CDN module to load."""
    # The Datastar module script fires after "load" (all resources). Use
    # wait_for_function to poll until the Datastar store is initialized —
    # signalled by the presence of a [data-signals] element that has been
    # processed (its signals appear in __ds_store on the window, or we fall
    # back to a generous timeout when offline).
    page.wait_for_load_state("load", timeout=15000)
    # Give Datastar a moment to bind after load
    page.wait_for_timeout(300)


def test_group_checkbox_cascades_to_children(server_with_repos: _ServerInfo, page: Page) -> None:
    """Clicking the group checkbox checks all child checkboxes via Datastar signals."""
    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)

    page.goto(f"{server_with_repos.base_url}/config", wait_until="load")
    _wait_for_datastar(page)

    group_cb = page.locator("#grp-acme")
    # Signal names use '__' in place of '/' to keep valid JS identifier paths
    child_alpha = page.locator("input[data-bind='repos.acme__alpha']")
    child_beta = page.locator("input[data-bind='repos.acme__beta']")

    # Capture the POST to /config/github-repos/toggle-group
    with page.expect_request(
        lambda r: "/config/github-repos/toggle-group" in r.url,
        timeout=8000,
    ) as req_info:
        # Uncheck the group (it starts checked since both repos are enabled)
        group_cb.uncheck()

    # Both children should now be unchecked (Datastar signal cascade)
    page.wait_for_timeout(200)
    assert not child_alpha.is_checked(), "acme/alpha should be unchecked after group uncheck"
    assert not child_beta.is_checked(), "acme/beta should be unchecked after group uncheck"

    # Verify POST payload
    post_body = req_info.value.post_data_json
    assert post_body is not None
    assert post_body.get("owner") == "acme"
    assert post_body.get("enabled") is False

    assert errors == [], f"JS console errors: {errors}"


def test_group_checkbox_checks_all_children(server_with_repos: _ServerInfo, page: Page) -> None:
    """After unchecking the group, re-checking it checks all children."""
    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)

    # Seed both repos as excluded so group starts unchecked
    from zing_ai.config import load_config, save_config

    cfg = load_config()
    cfg.command_center.github_excluded_repos = ["acme/alpha", "acme/beta"]
    save_config(cfg)

    page.goto(f"{server_with_repos.base_url}/config", wait_until="load")
    _wait_for_datastar(page)

    group_cb = page.locator("#grp-acme")
    child_alpha = page.locator("input[data-bind='repos.acme__alpha']")
    child_beta = page.locator("input[data-bind='repos.acme__beta']")

    # Both children should start unchecked
    assert not child_alpha.is_checked(), "acme/alpha should start unchecked"
    assert not child_beta.is_checked(), "acme/beta should start unchecked"

    # Check the group
    with page.expect_request(
        lambda r: "/config/github-repos/toggle-group" in r.url,
        timeout=8000,
    ) as req_info:
        group_cb.check()

    # Both children should now be checked
    page.wait_for_timeout(200)
    assert child_alpha.is_checked(), "acme/alpha should be checked after group check"
    assert child_beta.is_checked(), "acme/beta should be checked after group check"

    post_body = req_info.value.post_data_json
    assert post_body is not None
    assert post_body.get("owner") == "acme"
    assert post_body.get("enabled") is True

    assert errors == [], f"JS console errors: {errors}"


def test_independent_child_toggle_does_not_cascade(
    server_with_repos: _ServerInfo, page: Page
) -> None:
    """Toggling a single child checkbox only fires the per-repo POST, not the group POST."""
    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)

    page.goto(f"{server_with_repos.base_url}/config", wait_until="load")
    _wait_for_datastar(page)

    child_alpha = page.locator("input[data-bind='repos.acme__alpha']")
    child_beta = page.locator("input[data-bind='repos.acme__beta']")

    # Track all network requests during the child click
    requests_seen: list[str] = []
    page.on("request", lambda r: requests_seen.append(r.url))

    with page.expect_request(
        lambda r: "/config/github-repos/toggle" in r.url and "toggle-group" not in r.url,
        timeout=8000,
    ) as req_info:
        child_alpha.uncheck()

    page.wait_for_timeout(500)

    # The other child (beta) must stay checked
    assert child_beta.is_checked(), "acme/beta should still be checked"

    # Verify correct per-repo POST payload
    post_body = req_info.value.post_data_json
    assert post_body is not None
    assert post_body.get("repo") == "acme/alpha"
    assert post_body.get("enabled") is False

    # Group POST must NOT have fired
    group_posts = [u for u in requests_seen if "toggle-group" in u]
    assert group_posts == [], f"Group POST fired unexpectedly: {group_posts}"

    assert errors == [], f"JS console errors: {errors}"
