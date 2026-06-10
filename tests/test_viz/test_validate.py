"""Tests for viz/validate.py."""

from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest

from zing_ai.viz.validate import ValidationIssue, validate

FIXTURE = Path(__file__).parent / "fixtures" / "BAK-1321" / "BAK-1321-direct-flatten.viz.json"
MIXED_FIXTURE = (
    Path(__file__).parent / "fixtures" / "mixed-axis-activity-feed" / "activity-feed.viz.json"
)


@pytest.fixture
def graph() -> dict:
    return json.loads(FIXTURE.read_text())


@pytest.fixture
def mixed_graph() -> dict:
    """A two-step plan exercising both logic primitives and struct primitives."""
    return json.loads(MIXED_FIXTURE.read_text())


def test_valid_fixture_returns_no_issues(graph: dict) -> None:
    assert validate(graph) == []


def test_unknown_shape_yields_schema_error_with_pointer(graph: dict) -> None:
    g = copy.deepcopy(graph)
    g["steps"][0]["nodes"][0]["shape"] = "unknown_shape"
    issues = validate(g)
    assert issues
    assert any(
        i.pointer == "/steps/0/nodes/0/shape" and "is not one of" in i.message for i in issues
    )


def test_cross_flow_from_node_typo_suggests_correct_id(graph: dict) -> None:
    g = copy.deepcopy(graph)
    # Find a cross_flow whose from_node is g-yield and typo it to g_yield
    target_idx = next(i for i, cf in enumerate(g["cross_flows"]) if cf["from_node"] == "g-yield")
    g["cross_flows"][target_idx]["from_node"] = "g_yield"
    issues = validate(g)
    issue = next(i for i in issues if i.pointer == f"/cross_flows/{target_idx}/from_node")
    assert "g_yield" in issue.message
    assert issue.suggestion == "g-yield"


def test_cross_flow_pointing_at_nonexistent_step_yields_error(graph: dict) -> None:
    g = copy.deepcopy(graph)
    g["cross_flows"][0]["to_step"] = 999
    issues = validate(g)
    assert any(i.pointer == "/cross_flows/0/to_step" and "999" in i.message for i in issues)


def test_duplicate_node_id_within_step_yields_error(graph: dict) -> None:
    g = copy.deepcopy(graph)
    # Duplicate the first node in step 0
    dup = copy.deepcopy(g["steps"][0]["nodes"][0])
    g["steps"][0]["nodes"].append(dup)
    issues = validate(g)
    assert any(
        i.pointer.startswith("/steps/0/nodes/") and "duplicate node id" in i.message for i in issues
    )


def test_diverged_node_missing_today_label_yields_schema_error(graph: dict) -> None:
    g = copy.deepcopy(graph)
    # Find a diverged node in step 0 and strip today_label
    diverged_idx = next(i for i, n in enumerate(g["steps"][0]["nodes"]) if n["shape"] == "diverged")
    del g["steps"][0]["nodes"][diverged_idx]["today_label"]
    issues = validate(g)
    assert any("today_label" in i.message for i in issues)


def test_non_diverged_shape_with_side_diverged_yields_schema_error(graph: dict) -> None:
    g = copy.deepcopy(graph)
    # Set a rect node to side=diverged
    rect_idx = next(i for i, n in enumerate(g["steps"][0]["nodes"]) if n["shape"] == "rect")
    g["steps"][0]["nodes"][rect_idx]["side"] = "diverged"
    issues = validate(g)
    assert issues


def test_duplicate_step_number_yields_error(graph: dict) -> None:
    g = copy.deepcopy(graph)
    # Set step 2's number to 1 (collide with step 1)
    g["steps"][1]["step"] = g["steps"][0]["step"]
    issues = validate(g)
    assert any("duplicate step number" in i.message for i in issues)


def test_duplicate_step_id_yields_error(graph: dict) -> None:
    g = copy.deepcopy(graph)
    g["steps"][1]["id"] = g["steps"][0]["id"]
    issues = validate(g)
    assert any("duplicate step id" in i.message for i in issues)


def test_in_step_edge_pointing_at_nonexistent_node_yields_error(graph: dict) -> None:
    g = copy.deepcopy(graph)
    g["steps"][0]["edges"][0]["to"] = "no-such-node"
    issues = validate(g)
    assert any(i.pointer == "/steps/0/edges/0/to" and "no-such-node" in i.message for i in issues)


def test_unknown_kind_in_cross_flow_yields_error(graph: dict) -> None:
    g = copy.deepcopy(graph)
    g["cross_flows"][0]["kind"] = "bogus"
    issues = validate(g)
    issue = next(i for i in issues if i.pointer == "/cross_flows/0/kind")
    assert "bogus" in issue.message
    assert sorted(g["kinds"].keys()) == issue.available


def test_validation_issue_format_includes_path_pointer_and_suggestion() -> None:
    issue = ValidationIssue(
        pointer="/cross_flows/2/from_node",
        message='step 6 has no node with id "g_yield"',
        available=["g-yield", "g-merge"],
        suggestion="g-yield",
    )
    text = issue.format(".zing/BAK-1321.viz.json")
    assert ".zing/BAK-1321.viz.json:/cross_flows/2/from_node:" in text
    assert "available: g-yield, g-merge" in text
    assert 'did you mean "g-yield"?' in text


def test_validation_issue_format_with_no_optional_fields() -> None:
    issue = ValidationIssue(pointer="/title", message="title is required")
    text = issue.format(".zing/foo.viz.json")
    assert text == ".zing/foo.viz.json:/title: title is required"


# ===== struct primitive (data axis) =====


def test_mixed_axis_fixture_validates_clean(mixed_graph: dict) -> None:
    """A viz with both logic primitives (rect, diamond) and struct primitives
    (struct/union/collections) in the same graph must validate clean."""
    assert validate(mixed_graph) == []


def test_struct_node_without_fields_is_rejected(mixed_graph: dict) -> None:
    g = copy.deepcopy(mixed_graph)
    struct_idx = next(i for i, n in enumerate(g["steps"][0]["nodes"]) if n["shape"] == "struct")
    del g["steps"][0]["nodes"][struct_idx]["fields"]
    issues = validate(g)
    assert any("fields" in i.message for i in issues)


def test_struct_node_with_empty_fields_is_rejected(mixed_graph: dict) -> None:
    g = copy.deepcopy(mixed_graph)
    struct_idx = next(i for i, n in enumerate(g["steps"][0]["nodes"]) if n["shape"] == "struct")
    g["steps"][0]["nodes"][struct_idx]["fields"] = []
    issues = validate(g)
    assert issues, "empty fields[] must fail (minItems: 1)"


def test_struct_node_with_side_diverged_is_rejected(mixed_graph: dict) -> None:
    """The wrapper side may NOT be diverged on a struct — per-field sides do
    the change-marking. side=diverged is reserved for shape=diverged."""
    g = copy.deepcopy(mixed_graph)
    struct_idx = next(i for i, n in enumerate(g["steps"][0]["nodes"]) if n["shape"] == "struct")
    g["steps"][0]["nodes"][struct_idx]["side"] = "diverged"
    issues = validate(g)
    assert issues


def test_non_struct_shape_with_fields_is_rejected(mixed_graph: dict) -> None:
    """A rect / diamond / etc. node must not carry fields[]."""
    g = copy.deepcopy(mixed_graph)
    rect_idx = next(i for i, n in enumerate(g["steps"][0]["nodes"]) if n["shape"] == "rect")
    g["steps"][0]["nodes"][rect_idx]["fields"] = [{"name": "stowaway", "side": "unchanged"}]
    issues = validate(g)
    assert issues


def test_non_struct_shape_with_kind_is_rejected(mixed_graph: dict) -> None:
    """`kind` is only meaningful on struct nodes — reject elsewhere."""
    g = copy.deepcopy(mixed_graph)
    rect_idx = next(i for i, n in enumerate(g["steps"][0]["nodes"]) if n["shape"] == "rect")
    g["steps"][0]["nodes"][rect_idx]["kind"] = "struct"
    issues = validate(g)
    assert issues


def test_struct_kind_union_and_collections_validate(mixed_graph: dict) -> None:
    """All three struct kinds in the fixture should be present and valid."""
    kinds_seen = {
        n["kind"] for step in mixed_graph["steps"] for n in step["nodes"] if n["shape"] == "struct"
    }
    assert kinds_seen == {"struct", "union", "collections"}
    assert validate(mixed_graph) == []


def test_struct_with_unknown_kind_is_rejected(mixed_graph: dict) -> None:
    g = copy.deepcopy(mixed_graph)
    struct_idx = next(i for i, n in enumerate(g["steps"][0]["nodes"]) if n["shape"] == "struct")
    g["steps"][0]["nodes"][struct_idx]["kind"] = "tuple"
    issues = validate(g)
    assert issues


def test_diverged_field_missing_today_is_rejected(mixed_graph: dict) -> None:
    g = copy.deepcopy(mixed_graph)
    # event-payload in step 1 has a diverged field (actor_id)
    payload = next(n for n in g["steps"][0]["nodes"] if n["id"] == "event-payload")
    diverged_field = next(f for f in payload["fields"] if f["side"] == "diverged")
    del diverged_field["today"]
    issues = validate(g)
    assert any("today" in i.message for i in issues)


def test_diverged_field_missing_proposed_is_rejected(mixed_graph: dict) -> None:
    g = copy.deepcopy(mixed_graph)
    payload = next(n for n in g["steps"][0]["nodes"] if n["id"] == "event-payload")
    diverged_field = next(f for f in payload["fields"] if f["side"] == "diverged")
    del diverged_field["proposed"]
    issues = validate(g)
    assert any("proposed" in i.message for i in issues)


def test_non_diverged_field_with_today_is_rejected(mixed_graph: dict) -> None:
    """today/proposed are only valid when side=diverged."""
    g = copy.deepcopy(mixed_graph)
    payload = next(n for n in g["steps"][0]["nodes"] if n["id"] == "event-payload")
    unchanged_field = next(f for f in payload["fields"] if f["side"] == "unchanged")
    unchanged_field["today"] = "stowaway"
    issues = validate(g)
    assert issues


def test_existing_logic_diverged_rule_still_fires(graph: dict) -> None:
    """Regression: the struct addition must not have broken the original
    shape=diverged ⇒ side=diverged rule. Setting a rect to side=diverged in
    the original fixture should still fail, exactly as it did before."""
    g = copy.deepcopy(graph)
    rect_idx = next(i for i, n in enumerate(g["steps"][0]["nodes"]) if n["shape"] == "rect")
    g["steps"][0]["nodes"][rect_idx]["side"] = "diverged"
    issues = validate(g)
    assert issues
