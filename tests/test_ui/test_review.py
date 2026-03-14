"""Playwright tests for the Zing review page."""

from __future__ import annotations

import re

import pytest
from playwright.sync_api import Page, expect

from zing_ai.server.models import ResponseAction, UserResponse

from tests.test_ui.conftest import _ServerInfo

pytestmark = pytest.mark.ui

_STEP = "review"


def _create_ready_session(
    server: _ServerInfo,
    session_id: str = "rev-test",
    title: str = "Review Test",
) -> str:
    """Create a session in READY state with sample findings. Returns step_id."""
    manager = server.manager
    session = manager.create_session(
        session_id=session_id, title=title, steps=[_STEP]
    )
    step_id = session.steps[0].step_id
    manager.start_step(session_id, step_id)

    # Add one of each finding type
    manager.add_finding(session_id, step_id, {
        "type": "triage",
        "id": "triage-1",
        "title": "SQL Injection Risk",
        "body": "Potential SQL injection in query builder.",
        "category": "security",
        "severity": "high",
        "confidence": "high",
    })
    manager.add_finding(session_id, step_id, {
        "type": "text",
        "id": "text-1",
        "title": "Describe your testing strategy",
        "body": "How will you test this change?",
    })
    manager.add_finding(session_id, step_id, {
        "type": "triage",
        "id": "triage-2",
        "title": "Pick an approach",
        "options": [
            {"label": "Option A", "description": "First approach"},
            {"label": "Option B", "description": "Second approach"},
        ],
    })

    manager.mark_step_ready(session_id, step_id)
    return step_id


def _create_session_with_all_finding_types(
    server: _ServerInfo,
    session_id: str = "all-types",
    title: str = "All Finding Types",
) -> str:
    """Create a READY session with all finding types (triage, text, triage-with-options, evaluation). Returns step_id."""
    manager = server.manager
    session = manager.create_session(
        session_id=session_id, title=title, steps=[_STEP]
    )
    step_id = session.steps[0].step_id
    manager.start_step(session_id, step_id)

    manager.add_finding(session_id, step_id, {
        "type": "triage",
        "id": "triage-1",
        "title": "SQL Injection Risk",
        "body": "Potential SQL injection in query builder.",
        "category": "security",
        "severity": "high",
        "confidence": "high",
        "options": [
            {"label": "Parameterize queries", "description": "Use parameterized queries"},
            {"label": "Use ORM", "description": "Switch to ORM layer"},
        ],
    })
    manager.add_finding(session_id, step_id, {
        "type": "text",
        "id": "text-1",
        "title": "Describe your testing strategy",
        "body": "How will you test this change?",
    })
    manager.add_finding(session_id, step_id, {
        "type": "triage",
        "id": "triage-2",
        "title": "Pick an approach",
        "options": [
            {"label": "Option A", "description": "First approach"},
            {"label": "Option B", "description": "Second approach"},
        ],
    })
    manager.add_finding(session_id, step_id, {
        "type": "evaluation",
        "id": "eval-1",
        "title": "Plan Quality Assessment",
        "body": "Overall evaluation of the implementation plan.",
        "criteria": [
            {"name": "Completeness", "rating": "strong", "justification": "All cases covered"},
            {"name": "Clarity", "rating": "adequate", "justification": "Mostly clear"},
        ],
        "litmus_tests": [
            {"name": "Can a junior dev follow it?", "result": "Yes"},
        ],
        "warnings": [
            {"name": "Missing error handling", "found": True, "details": "No retry logic"},
        ],
    })

    manager.mark_step_ready(session_id, step_id)
    return step_id


def _assert_no_console_errors(errors: list[str]) -> None:
    """Assert that no JS console errors were collected."""
    assert errors == [], f"Unexpected JS console errors: {errors}"


def _save_and_reload(
    server: _ServerInfo,
    page: Page,
    session_id: str,
    step_id: str,
    responses: dict[str, UserResponse],
) -> Page:
    """Save responses server-side, then navigate to the review page. Returns the page."""
    manager = server.manager
    for finding_id, response in responses.items():
        manager.save_response(session_id, step_id, finding_id, response)
    page.goto(f"{server.base_url}/{session_id}")
    return page


def _wait_for_datastar(page: Page) -> None:
    """Wait for Datastar JS to load and initialize."""
    page.wait_for_load_state("networkidle", timeout=3000)


def _create_completed_session(
    server: _ServerInfo,
    session_id: str = "completed-test",
    title: str = "Completed Test",
) -> str:
    """Create a session in COMPLETED state with sample responses. Returns step_id."""
    step_id = _create_ready_session(server, session_id=session_id, title=title)
    manager = server.manager
    _, step = manager.get_step_by_id(step_id)
    responses = []
    for finding in step.findings:
        if finding.type == "triage" and finding.options:
            responses.append(UserResponse(action=ResponseAction.ACCEPT, selected="Option A"))
        elif finding.type == "triage":
            responses.append(UserResponse(action=ResponseAction.ACCEPT))
        elif finding.type == "text":
            responses.append(UserResponse(answer="Test answer"))
        else:
            msg = f"Unhandled finding type in test helper: {finding.type}"
            raise ValueError(msg)
    manager.submit_responses(session_id, step_id, responses)
    return step_id


def test_review_page_shows_findings(server: _ServerInfo, page: Page) -> None:
    """Ready review page pre-renders all findings."""
    _create_ready_session(server)

    page.goto(f"{server.base_url}/rev-test")

    expect(page.locator("#finding-triage-1")).to_be_visible(timeout=3000)
    expect(page.locator("#finding-text-1")).to_be_visible(timeout=3000)
    expect(page.locator("#finding-triage-2")).to_be_visible(timeout=3000)
    expect(page.locator("text=SQL Injection Risk")).to_be_visible(timeout=3000)


def test_triage_button_toggles_selected_class(server: _ServerInfo, page: Page) -> None:
    """Clicking a triage action button toggles the 'selected' class via Datastar."""
    _create_ready_session(server)

    page.goto(f"{server.base_url}/rev-test")
    expect(page.locator("#finding-triage-1")).to_be_visible(timeout=3000)
    _wait_for_datastar(page)

    # Click the "Accept" button — Datastar should add 'selected' class
    accept_btn = page.locator("#finding-triage-1 .action-btn", has_text="Accept")
    accept_btn.click()
    expect(accept_btn).to_have_class(re.compile("selected"), timeout=3000)

    # Click "Drop" — Accept should lose 'selected', Drop should gain it
    drop_btn = page.locator("#finding-triage-1 .action-btn", has_text="Drop")
    drop_btn.click()
    expect(drop_btn).to_have_class(re.compile("selected"), timeout=3000)
    expect(accept_btn).not_to_have_class(re.compile("selected"), timeout=3000)


def test_text_finding_textarea_binding(server: _ServerInfo, page: Page) -> None:
    """Text finding textarea is bound to Datastar signals via data-bind."""
    _create_ready_session(server)

    page.goto(f"{server.base_url}/rev-test")
    expect(page.locator("#finding-text-1")).to_be_visible(timeout=3000)
    _wait_for_datastar(page)

    textarea = page.locator("#finding-text-1 textarea")
    textarea.fill("My testing strategy is comprehensive.")
    expect(textarea).to_have_value("My testing strategy is comprehensive.", timeout=3000)


def test_triage_approach_selection(server: _ServerInfo, page: Page) -> None:
    """Selecting an approach radio in a triage finding updates Datastar signals."""
    _create_ready_session(server)

    page.goto(f"{server.base_url}/rev-test")
    expect(page.locator("#finding-triage-2")).to_be_visible(timeout=3000)
    _wait_for_datastar(page)

    radio = page.locator("#finding-triage-2 input[name='approach-triage-2'][value='Option A']")
    radio.click()
    expect(radio).to_be_checked(timeout=3000)


def test_submit_review_completes_workflow(server: _ServerInfo, page: Page) -> None:
    """Clicking Submit Review button completes the workflow step."""
    _create_ready_session(server)

    page.goto(f"{server.base_url}/rev-test")
    _wait_for_datastar(page)

    submit_btn = page.locator(".submit-btn")
    expect(submit_btn).to_be_visible(timeout=3000)
    expect(submit_btn).to_have_text("Submit Review", timeout=3000)

    with page.expect_response("**/submit", timeout=3000):
        submit_btn.click()

    # After submit, banner should show success
    expect(page.locator("#review-status")).to_contain_text("Review submitted", timeout=3000)

    # Button should be disabled
    expect(page.locator(".submit-btn")).to_be_disabled(timeout=3000)


def test_submit_captures_triage_response(server: _ServerInfo, page: Page) -> None:
    """Submit sends triage action selection to the server via Datastar signals."""
    step_id = _create_ready_session(server)

    page.goto(f"{server.base_url}/rev-test")
    _wait_for_datastar(page)

    # Select "Accept" on the triage finding
    page.locator("#finding-triage-1 .action-btn", has_text="Accept").click()

    # Select "Option A" on the triage-2 approach finding
    page.locator("#finding-triage-2 input[name='approach-triage-2'][value='Option A']").click()

    # Submit
    with page.expect_response("**/submit", timeout=3000):
        page.locator(".submit-btn").click()

    expect(page.locator("#review-status")).to_contain_text("Review submitted", timeout=3000)

    # Verify server-side state
    _, step = server.manager.get_step_by_id(step_id)
    assert step.responses is not None
    assert len(step.responses) == 3

    # Triage finding: action should be "accept"
    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses[triage_idx].action is not None
    assert step.responses[triage_idx].action.value == "accept"

    # Triage-2 finding: selected approach should be "Option A"
    triage2_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-2")
    assert step.responses[triage2_idx].selected == "Option A"


def test_sse_streams_findings_into_dom(server: _ServerInfo, page: Page) -> None:
    """SSE streams new findings into the DOM as they arrive."""
    manager = server.manager
    session = manager.create_session(
        session_id="sse-stream", title="SSE Stream Test", steps=[_STEP]
    )
    step_id = session.steps[0].step_id
    manager.start_step("sse-stream", step_id)

    # Navigate while step is in STARTED state (SSE will be active)
    page.goto(f"{server.base_url}/sse-stream")

    # Now add a finding server-side — it should appear via SSE
    manager.add_finding("sse-stream", step_id, {
        "type": "triage",
        "id": "sse-finding-1",
        "title": "SSE Streamed Finding",
        "category": "correctness",
        "severity": "medium",
        "confidence": "high",
    })

    expect(page.locator("#finding-sse-finding-1")).to_be_visible(timeout=3000)
    expect(page.locator("text=SSE Streamed Finding")).to_be_visible(timeout=3000)


def test_tab_navigation(server: _ServerInfo, page: Page) -> None:
    """Tab navigation works between workflow steps."""
    manager = server.manager
    manager.create_session(
        session_id="tab-test", title="Tab Test", steps=["audit", "review"]
    )

    page.goto(f"{server.base_url}/tab-test")

    # Both step tabs should be visible
    expect(page.locator(".step-link", has_text="audit")).to_be_visible(timeout=3000)
    expect(page.locator(".step-link", has_text="review")).to_be_visible(timeout=3000)

    # Click the audit tab
    page.locator(".step-link", has_text="audit").click()

    # URL should update with the step parameter
    page.wait_for_url("**/tab-test?step=*", timeout=3000)


# ---------------------------------------------------------------------------
# Batch 1: Critical — auto-save & saved state
# ---------------------------------------------------------------------------


def test_saved_responses_restore_on_page_load(server: _ServerInfo, page: Page) -> None:
    """Saved responses are restored into the DOM when the page loads with pre-existing state."""
    step_id = _create_ready_session(server, session_id="restore-test")
    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    _save_and_reload(
        server, page, "restore-test", step_id,
        {
            "triage-1": UserResponse(action=ResponseAction.ACCEPT),
            "text-1": UserResponse(answer="Integration tests with fixtures"),
            "triage-2": UserResponse(action=ResponseAction.ACCEPT, selected="Option A"),
        },
    )
    _wait_for_datastar(page)

    # Triage: Accept button should have 'selected' class
    accept_btn = page.locator("#finding-triage-1 .action-btn", has_text="Accept")
    expect(accept_btn).to_have_class(re.compile("selected"), timeout=3000)

    # Text: textarea should have the saved value
    textarea = page.locator("#finding-text-1 textarea")
    expect(textarea).to_have_value("Integration tests with fixtures", timeout=3000)

    # Triage-2 (approach): Accept button should have 'selected' class
    accept_btn_2 = page.locator("#finding-triage-2 .action-btn", has_text="Accept")
    expect(accept_btn_2).to_have_class(re.compile("selected"), timeout=3000)

    # Triage-2 (approach): Option A radio should be checked
    radio = page.locator("#finding-triage-2 input[name='approach-triage-2'][value='Option A']")
    expect(radio).to_be_checked(timeout=3000)

    _assert_no_console_errors(console_errors)


def test_triage_button_fires_save_response_post(server: _ServerInfo, page: Page) -> None:
    """Clicking a triage button fires a POST to /save-response and updates server state."""
    step_id = _create_ready_session(server, session_id="triage-post")

    page.goto(f"{server.base_url}/triage-post")
    _wait_for_datastar(page)

    # Click Accept — intercept the save-response POST
    accept_btn = page.locator("#finding-triage-1 .action-btn", has_text="Accept")
    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        accept_btn.click()
    assert resp_info.value.status == 200

    # Verify server state
    _, step = server.manager.get_step_by_id(step_id)
    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses is not None
    assert step.responses[triage_idx].action == ResponseAction.ACCEPT

    # Click Drop — should overwrite
    drop_btn = page.locator("#finding-triage-1 .action-btn", has_text="Drop")
    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        drop_btn.click()
    assert resp_info.value.status == 200

    _, step = server.manager.get_step_by_id(step_id)
    assert step.responses[triage_idx].action == ResponseAction.DROP


def test_auto_save_text_on_blur(server: _ServerInfo, page: Page) -> None:
    """Text finding auto-saves on blur by posting to /save-response."""
    step_id = _create_ready_session(server, session_id="text-blur")

    page.goto(f"{server.base_url}/text-blur")
    _wait_for_datastar(page)

    textarea = page.locator("#finding-text-1 textarea")
    textarea.fill("My test answer")

    # Blur by clicking elsewhere, intercepting the POST
    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        page.locator("#finding-triage-1").click()  # click away to trigger blur
    assert resp_info.value.status == 200

    # Verify server state
    _, step = server.manager.get_step_by_id(step_id)
    text_idx = next(i for i, f in enumerate(step.findings) if f.id == "text-1")
    assert step.responses is not None
    assert step.responses[text_idx].answer == "My test answer"


def test_auto_save_triage_approach_on_change(server: _ServerInfo, page: Page) -> None:
    """Triage approach auto-saves on radio change by posting to /save-response."""
    step_id = _create_ready_session(server, session_id="approach-change")

    page.goto(f"{server.base_url}/approach-change")
    _wait_for_datastar(page)

    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        page.locator("#finding-triage-2 input[name='approach-triage-2'][value='Option B']").click()
    assert resp_info.value.status == 200

    _, step = server.manager.get_step_by_id(step_id)
    triage2_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-2")
    assert step.responses is not None
    assert step.responses[triage2_idx].selected == "Option B"


# ---------------------------------------------------------------------------
# Batch 2: High priority — interaction completeness
# ---------------------------------------------------------------------------


def test_datastar_initializes_without_console_errors(server: _ServerInfo, page: Page) -> None:
    """Datastar initializes without JS console errors, including after interactions."""
    _create_ready_session(server, session_id="ds-init")
    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    page.goto(f"{server.base_url}/ds-init")
    _wait_for_datastar(page)

    # Interact to exercise Datastar bindings
    page.locator("#finding-triage-1 .action-btn", has_text="Accept").click()
    page.wait_for_timeout(500)

    _assert_no_console_errors(console_errors)


def test_triage_with_suggested_approaches(server: _ServerInfo, page: Page) -> None:
    """Triage finding with options shows suggested approaches and saves selection."""
    step_id = _create_session_with_all_finding_types(server, session_id="approaches")

    page.goto(f"{server.base_url}/approaches")
    _wait_for_datastar(page)

    # Suggested approaches section should be visible
    expect(page.locator("#finding-triage-1 .triage-options")).to_be_visible(timeout=3000)

    # Click Accept first
    with page.expect_response("**/save-response", timeout=3000):
        page.locator("#finding-triage-1 .action-btn", has_text="Accept").click()

    # Select an approach radio
    approach_radio = page.locator(
        "#finding-triage-1 input[value='Parameterize queries']"
    )
    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        approach_radio.click()
    assert resp_info.value.status == 200

    # Verify server state has both action and selected
    _, step = server.manager.get_step_by_id(step_id)
    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses is not None
    assert step.responses[triage_idx].action == ResponseAction.ACCEPT
    assert step.responses[triage_idx].selected == "Parameterize queries"


def test_submit_captures_all_finding_types(server: _ServerInfo, page: Page) -> None:
    """Submit captures responses for triage, text, and triage-with-approach findings."""
    step_id = _create_ready_session(server, session_id="submit-all")

    page.goto(f"{server.base_url}/submit-all")
    _wait_for_datastar(page)

    # Fill all three finding types
    page.locator("#finding-triage-1 .action-btn", has_text="Accept").click()
    page.locator("#finding-text-1 textarea").fill("Comprehensive test plan")
    page.locator("#finding-triage-2 .action-btn", has_text="Accept").click()
    page.locator("#finding-triage-2 input[name='approach-triage-2'][value='Option B']").click()

    with page.expect_response("**/submit", timeout=3000):
        page.locator(".submit-btn").click()

    expect(page.locator("#review-status")).to_contain_text("Review submitted", timeout=3000)

    # Verify ALL three server-side responses
    _, step = server.manager.get_step_by_id(step_id)
    assert step.responses is not None
    assert len(step.responses) == 3

    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses[triage_idx].action == ResponseAction.ACCEPT

    text_idx = next(i for i, f in enumerate(step.findings) if f.id == "text-1")
    assert step.responses[text_idx].answer == "Comprehensive test plan"

    triage2_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-2")
    assert step.responses[triage2_idx].action == ResponseAction.ACCEPT
    assert step.responses[triage2_idx].selected == "Option B"


def test_triage_approach_other_flow(server: _ServerInfo, page: Page) -> None:
    """Selecting 'Other' approach in a triage finding shows textarea and saves custom answer."""
    step_id = _create_ready_session(server, session_id="approach-other")

    page.goto(f"{server.base_url}/approach-other")
    _wait_for_datastar(page)

    # Click the "Other" approach radio
    other_radio = page.locator("#finding-triage-2 input[name='approach-triage-2'][value='__other__']")
    with page.expect_response("**/save-response", timeout=3000):
        other_radio.click()

    # Conditional textarea should become visible
    other_textarea = page.locator(
        "#finding-triage-2 div[data-show] textarea"
    )
    expect(other_textarea).to_be_visible(timeout=3000)

    # Fill and blur to trigger save
    other_textarea.fill("Custom answer")
    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        page.locator("#finding-triage-1").click()  # blur
    assert resp_info.value.status == 200

    _, step = server.manager.get_step_by_id(step_id)
    triage2_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-2")
    assert step.responses is not None
    assert step.responses[triage2_idx].selected == "__other__"
    assert step.responses[triage2_idx].other_text == "Custom answer"


# ---------------------------------------------------------------------------
# Batch 3: Medium priority — coverage breadth
# ---------------------------------------------------------------------------


def test_evaluation_finding_renders_tables(server: _ServerInfo, page: Page) -> None:
    """Evaluation finding renders criteria tables and informational meta text."""
    _create_session_with_all_finding_types(server, session_id="eval-render")

    page.goto(f"{server.base_url}/eval-render")
    _wait_for_datastar(page)

    eval_finding = page.locator("#finding-eval-1")
    expect(eval_finding).to_be_visible(timeout=3000)
    expect(eval_finding).to_have_class(re.compile("evaluation-finding"), timeout=3000)

    # Criteria table should be visible
    expect(eval_finding.locator(".eval-table").first).to_be_visible(timeout=3000)

    # Informational meta text
    expect(eval_finding.locator(".finding-meta")).to_contain_text("Informational", timeout=3000)

    # No action buttons or textarea within eval finding
    expect(eval_finding.locator(".action-btn")).to_have_count(0, timeout=3000)
    expect(eval_finding.locator("textarea")).to_have_count(0, timeout=3000)


def test_sse_streams_agent_status_and_logs(server: _ServerInfo, page: Page) -> None:
    """SSE streams agent status and log entries into the DOM."""
    manager = server.manager
    session = manager.create_session(
        session_id="sse-agent", title="SSE Agent Test", steps=[_STEP]
    )
    step_id = session.steps[0].step_id
    manager.start_step("sse-agent", step_id)
    manager.start_agent("sse-agent", step_id, "test-agent", "Running analysis")

    # Add an initial log so the log-viewer container exists in the DOM
    manager.add_log("sse-agent", step_id, "test-agent", "Starting analysis")

    page.goto(f"{server.base_url}/sse-agent")

    # Agent should appear in the agents panel
    expect(page.locator(".agents-panel")).to_be_visible(timeout=3000)
    expect(page.locator(".agents-panel strong", has_text="test-agent")).to_be_visible(timeout=3000)

    # Log viewer should be present with the initial log
    expect(page.locator("#log-viewer")).to_be_visible(timeout=3000)
    expect(page.locator("#log-viewer .log-output")).to_contain_text(
        "Starting analysis", timeout=3000
    )

    # Add another log entry via SSE — should appear in the log output
    manager.add_log("sse-agent", step_id, "test-agent", "Found 3 issues")
    expect(page.locator("#log-viewer .log-output")).to_contain_text(
        "Found 3 issues", timeout=5000
    )

    # Stop the agent — should get completed class
    manager.stop_agent("sse-agent", step_id, "test-agent")
    expect(page.locator(".agent-completed")).to_be_visible(timeout=3000)


def test_plan_tab_renders_markdown(server: _ServerInfo, page: Page, tmp_path: str) -> None:
    """Plan tab renders markdown content from the zing file."""
    from pathlib import Path

    plan_dir = Path(tmp_path)
    plan_file = plan_dir / "test-plan.md"
    plan_file.write_text("# Test Plan\n\nThis is a test plan paragraph.\n")

    manager = server.manager
    manager.create_session(
        session_id="plan-tab",
        title="Plan Tab Test",
        zing_file=str(plan_file),
        steps=[_STEP],
    )

    page.goto(f"{server.base_url}/plan-tab?tab=plan")

    expect(page.get_by_role("heading", name="Test Plan")).to_be_visible(timeout=3000)
    expect(page.locator("text=This is a test plan paragraph")).to_be_visible(timeout=3000)


# ---------------------------------------------------------------------------
# Batch 4: Lower priority
# ---------------------------------------------------------------------------


def test_step_state_badges_in_review_page(server: _ServerInfo, page: Page) -> None:
    """Step tabs show correct state badges that update on reload."""
    manager = server.manager
    session = manager.create_session(
        session_id="badges", title="Badge Test", steps=["audit", "review"]
    )
    audit_step = session.steps[0]
    review_step = session.steps[1]

    manager.start_step("badges", audit_step.step_id)

    page.goto(f"{server.base_url}/badges")

    # First tab (audit) should show "started" badge
    audit_tab = page.locator(f"#step-tab-{audit_step.step_id}")
    expect(audit_tab.locator(".status-badge")).to_have_text("started", timeout=3000)

    # Second tab (review) should show "pending" badge
    review_tab = page.locator(f"#step-tab-{review_step.step_id}")
    expect(review_tab.locator(".status-badge")).to_have_text("pending", timeout=3000)

    # Mark audit step as ready via manager, then reload
    manager.mark_step_ready("badges", audit_step.step_id)
    page.reload()

    audit_tab = page.locator(f"#step-tab-{audit_step.step_id}")
    expect(audit_tab.locator(".status-badge")).to_have_text("ready", timeout=3000)


# ---------------------------------------------------------------------------
# Batch 5: Missing coverage — actions, completed state, triage "Other", multi-step
# ---------------------------------------------------------------------------


def test_downgrade_button_saves_and_toggles(server: _ServerInfo, page: Page) -> None:
    """Clicking Downgrade fires save-response POST and toggles the selected class."""
    step_id = _create_ready_session(server, session_id="downgrade-test")

    page.goto(f"{server.base_url}/downgrade-test")
    _wait_for_datastar(page)

    downgrade_btn = page.locator("#finding-triage-1 .action-btn[data-action='downgrade']")
    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        downgrade_btn.click()
    assert resp_info.value.status == 200

    # Downgrade should be selected, Accept and Drop should not
    expect(downgrade_btn).to_have_class(re.compile("selected"), timeout=3000)
    accept_btn = page.locator("#finding-triage-1 .action-btn", has_text="Accept")
    expect(accept_btn).not_to_have_class(re.compile("selected"), timeout=3000)
    drop_btn = page.locator("#finding-triage-1 .action-btn", has_text="Drop")
    expect(drop_btn).not_to_have_class(re.compile("selected"), timeout=3000)

    # Verify server state
    _, step = server.manager.get_step_by_id(step_id)
    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses is not None
    assert step.responses[triage_idx].action == ResponseAction.DOWNGRADE


def test_discuss_button_saves_and_toggles(server: _ServerInfo, page: Page) -> None:
    """Clicking Discuss fires save-response POST and toggles the selected class."""
    step_id = _create_ready_session(server, session_id="discuss-test")

    page.goto(f"{server.base_url}/discuss-test")
    _wait_for_datastar(page)

    discuss_btn = page.locator("#finding-triage-1 .action-btn[data-action='discuss']")
    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        discuss_btn.click()
    assert resp_info.value.status == 200

    # Discuss should be selected, Accept and Drop should not
    expect(discuss_btn).to_have_class(re.compile("selected"), timeout=3000)
    accept_btn = page.locator("#finding-triage-1 .action-btn", has_text="Accept")
    expect(accept_btn).not_to_have_class(re.compile("selected"), timeout=3000)
    drop_btn = page.locator("#finding-triage-1 .action-btn", has_text="Drop")
    expect(drop_btn).not_to_have_class(re.compile("selected"), timeout=3000)

    # Verify server state
    _, step = server.manager.get_step_by_id(step_id)
    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses is not None
    assert step.responses[triage_idx].action == ResponseAction.DISCUSS


def test_completed_step_renders_disabled_submit(server: _ServerInfo, page: Page) -> None:
    """Completed step shows disabled submit button and success banner without console errors."""
    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    _create_completed_session(server)

    page.goto(f"{server.base_url}/completed-test")
    _wait_for_datastar(page)

    # Submit button should be disabled with "Review submitted" text
    submit_btn = page.locator(".submit-btn")
    expect(submit_btn).to_be_disabled(timeout=3000)
    expect(submit_btn).to_have_text("Review submitted", timeout=3000)

    # Banner should show success message
    expect(page.locator("#review-status")).to_contain_text("Review submitted", timeout=3000)

    _assert_no_console_errors(console_errors)


def test_completed_step_save_response_accepted(server: _ServerInfo, page: Page) -> None:
    """save-response POST on completed steps returns 200 (no step-state guard)."""
    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    step_id = _create_completed_session(server, session_id="completed-save")

    page.goto(f"{server.base_url}/completed-save")
    _wait_for_datastar(page)

    # Click Accept on triage finding — should succeed
    accept_btn = page.locator("#finding-triage-1 .action-btn", has_text="Accept")
    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        accept_btn.click()
    assert resp_info.value.status == 200

    # Verify server state was updated
    _, step = server.manager.get_step_by_id(step_id)
    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses is not None
    assert step.responses[triage_idx].action == ResponseAction.ACCEPT

    _assert_no_console_errors(console_errors)


def test_triage_other_approach_saves_custom_text(server: _ServerInfo, page: Page) -> None:
    """Triage 'Other' approach shows textarea and saves custom text to server."""
    step_id = _create_session_with_all_finding_types(server, session_id="triage-other")

    page.goto(f"{server.base_url}/triage-other")
    _wait_for_datastar(page)

    # Click Accept first
    with page.expect_response("**/save-response", timeout=3000):
        page.locator("#finding-triage-1 .action-btn", has_text="Accept").click()

    # Click "Other" radio in triage options
    other_radio = page.locator("#finding-triage-1 input[value='__other__']")
    with page.expect_response("**/save-response", timeout=3000):
        other_radio.click()

    # Conditional textarea should become visible
    other_textarea = page.locator(
        "#finding-triage-1 div[data-show] textarea"
    )
    expect(other_textarea).to_be_visible(timeout=3000)

    # Fill and blur to trigger save
    other_textarea.fill("Custom approach")
    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        page.locator("#finding-text-1").click()  # blur
    assert resp_info.value.status == 200

    # Verify server state
    _, step = server.manager.get_step_by_id(step_id)
    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses is not None
    assert step.responses[triage_idx].action == ResponseAction.ACCEPT
    assert step.responses[triage_idx].selected == "__other__"
    assert step.responses[triage_idx].other_text == "Custom approach"


def test_completed_step_responses_restore_on_load(server: _ServerInfo, page: Page) -> None:
    """Completed step restores saved responses when the page loads."""
    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    _create_completed_session(server, session_id="restore-completed")

    page.goto(f"{server.base_url}/restore-completed")
    _wait_for_datastar(page)

    # Triage: Accept button should have 'selected' class
    accept_btn = page.locator("#finding-triage-1 .action-btn", has_text="Accept")
    expect(accept_btn).to_have_class(re.compile("selected"), timeout=3000)

    # Text: textarea should have the saved value
    textarea = page.locator("#finding-text-1 textarea")
    expect(textarea).to_have_value("Test answer", timeout=3000)

    # Triage-2 (approach): Accept button should have 'selected' class
    accept_btn_2 = page.locator("#finding-triage-2 .action-btn", has_text="Accept")
    expect(accept_btn_2).to_have_class(re.compile("selected"), timeout=3000)

    # Triage-2 (approach): Option A radio should be checked
    radio = page.locator("#finding-triage-2 input[name='approach-triage-2'][value='Option A']")
    expect(radio).to_be_checked(timeout=3000)

    # Submit button should be disabled
    expect(page.locator(".submit-btn")).to_be_disabled(timeout=3000)

    _assert_no_console_errors(console_errors)


def test_multi_step_mixed_state_navigation(server: _ServerInfo, page: Page) -> None:
    """Multi-step session with mixed states shows correct badges and submit states."""
    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    manager = server.manager
    session = manager.create_session(
        session_id="mixed-steps", title="Mixed Steps", steps=["audit", "review"]
    )
    audit_step = session.steps[0]
    review_step = session.steps[1]

    # Drive audit through STARTED → READY → COMPLETED
    manager.start_step("mixed-steps", audit_step.step_id)
    manager.add_finding("mixed-steps", audit_step.step_id, {
        "type": "triage",
        "id": "triage-1",
        "title": "Audit Finding",
        "body": "Audit finding body.",
        "category": "security",
        "severity": "high",
        "confidence": "high",
    })
    manager.mark_step_ready("mixed-steps", audit_step.step_id)
    manager.submit_responses(
        "mixed-steps", audit_step.step_id,
        [UserResponse(action=ResponseAction.ACCEPT)],
    )

    # Drive review through STARTED → READY
    manager.start_step("mixed-steps", review_step.step_id)
    manager.add_finding("mixed-steps", review_step.step_id, {
        "type": "triage",
        "id": "triage-1",
        "title": "Review Finding",
        "body": "Review finding body.",
        "category": "correctness",
        "severity": "medium",
        "confidence": "medium",
    })
    manager.mark_step_ready("mixed-steps", review_step.step_id)

    page.goto(f"{server.base_url}/mixed-steps")
    _wait_for_datastar(page)

    # Audit tab should show "completed" badge
    audit_tab = page.locator(f"#step-tab-{audit_step.step_id}")
    expect(audit_tab.locator(".status-badge")).to_have_text("completed", timeout=3000)

    # Review tab should show "ready" badge
    review_tab = page.locator(f"#step-tab-{review_step.step_id}")
    expect(review_tab.locator(".status-badge")).to_have_text("ready", timeout=3000)

    # Click audit tab — submit button should be disabled
    audit_tab.click()
    _wait_for_datastar(page)
    expect(page.locator(".submit-btn")).to_be_disabled(timeout=3000)

    # Click review tab — submit button should be enabled
    review_tab.click()
    _wait_for_datastar(page)
    submit_btn = page.locator(".submit-btn")
    expect(submit_btn).to_be_enabled(timeout=3000)

    _assert_no_console_errors(console_errors)


# ---------------------------------------------------------------------------
# Batch 6: Reload restore gaps — triage approach, other text
# ---------------------------------------------------------------------------


def test_triage_approach_restores_on_reload(server: _ServerInfo, page: Page) -> None:
    """Triage approach radio is re-selected when the page reloads with saved state."""
    step_id = _create_session_with_all_finding_types(server, session_id="approach-reload")
    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    _save_and_reload(
        server, page, "approach-reload", step_id,
        {
            "triage-1": UserResponse(
                action=ResponseAction.ACCEPT,
                selected="Parameterize queries",
            ),
        },
    )
    _wait_for_datastar(page)

    # Action button should be selected
    accept_btn = page.locator("#finding-triage-1 .action-btn", has_text="Accept")
    expect(accept_btn).to_have_class(re.compile("selected"), timeout=3000)

    # Approach radio should be checked
    approach_radio = page.locator(
        "#finding-triage-1 input[value='Parameterize queries']"
    )
    expect(approach_radio).to_be_checked(timeout=3000)

    _assert_no_console_errors(console_errors)


def test_triage_other_approach_restores_on_reload(server: _ServerInfo, page: Page) -> None:
    """Triage 'Other' approach radio and custom text are restored on reload."""
    step_id = _create_session_with_all_finding_types(server, session_id="triage-other-reload")
    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    _save_and_reload(
        server, page, "triage-other-reload", step_id,
        {
            "triage-1": UserResponse(
                action=ResponseAction.ACCEPT,
                selected="__other__",
                other_text="Custom approach text",
            ),
        },
    )
    _wait_for_datastar(page)

    # Action button should be selected
    accept_btn = page.locator("#finding-triage-1 .action-btn", has_text="Accept")
    expect(accept_btn).to_have_class(re.compile("selected"), timeout=3000)

    # "Other" radio should be checked
    other_radio = page.locator("#finding-triage-1 input[value='__other__']")
    expect(other_radio).to_be_checked(timeout=3000)

    # Custom textarea should be visible and pre-filled
    other_textarea = page.locator("#finding-triage-1 div[data-show] textarea")
    expect(other_textarea).to_be_visible(timeout=3000)
    expect(other_textarea).to_have_value("Custom approach text", timeout=3000)

    _assert_no_console_errors(console_errors)


def test_triage_approach_other_restores_on_reload(server: _ServerInfo, page: Page) -> None:
    """Triage 'Other' approach radio and custom text are restored on reload."""
    step_id = _create_ready_session(server, session_id="approach-other-reload")
    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    _save_and_reload(
        server, page, "approach-other-reload", step_id,
        {
            "triage-2": UserResponse(
                action=ResponseAction.ACCEPT,
                selected="__other__",
                other_text="My custom choice",
            ),
        },
    )
    _wait_for_datastar(page)

    # Accept button should have 'selected' class
    accept_btn = page.locator("#finding-triage-2 .action-btn", has_text="Accept")
    expect(accept_btn).to_have_class(re.compile("selected"), timeout=3000)

    # "Other" radio should be checked
    other_radio = page.locator("#finding-triage-2 input[name='approach-triage-2'][value='__other__']")
    expect(other_radio).to_be_checked(timeout=3000)

    # Custom textarea should be visible and pre-filled
    other_textarea = page.locator("#finding-triage-2 div[data-show] textarea")
    expect(other_textarea).to_be_visible(timeout=3000)
    expect(other_textarea).to_have_value("My custom choice", timeout=3000)

    _assert_no_console_errors(console_errors)


def test_submit_captures_triage_action_and_approach(server: _ServerInfo, page: Page) -> None:
    """Submit captures both triage action AND selected approach in a single response."""
    step_id = _create_session_with_all_finding_types(server, session_id="submit-approach")

    page.goto(f"{server.base_url}/submit-approach")
    _wait_for_datastar(page)

    # Select Accept action
    page.locator("#finding-triage-1 .action-btn", has_text="Accept").click()

    # Select an approach
    page.locator("#finding-triage-1 input[value='Parameterize queries']").click()

    # Fill text finding (required for submit)
    page.locator("#finding-text-1 textarea").fill("Test plan")

    # Select triage-2 approach (required for submit)
    page.locator("#finding-triage-2 .action-btn", has_text="Accept").click()
    page.locator("#finding-triage-2 input[name='approach-triage-2'][value='Option A']").click()

    with page.expect_response("**/submit", timeout=3000):
        page.locator(".submit-btn").click()

    expect(page.locator("#review-status")).to_contain_text("Review submitted", timeout=3000)

    # Verify server state has BOTH action and approach
    _, step = server.manager.get_step_by_id(step_id)
    assert step.responses is not None

    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses[triage_idx].action == ResponseAction.ACCEPT
    assert step.responses[triage_idx].selected == "Parameterize queries"
