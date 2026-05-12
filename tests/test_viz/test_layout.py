"""Tests for viz/layout.py."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from zing_ai.viz import layout

FIXTURE = Path(__file__).parent / "fixtures" / "BAK-1321" / "BAK-1321-direct-flatten.viz.json"


@pytest.fixture
def fixture_graph() -> dict:
    return json.loads(FIXTURE.read_text())


def test_layout_step_returns_viewbox_and_node_coordinates(fixture_graph: dict) -> None:
    step = fixture_graph["steps"][0]
    result = layout.layout_step(step)
    assert "viewBox" in result
    assert isinstance(result["viewBox"], list)
    assert len(result["viewBox"]) == 4
    for node in result["nodes"]:
        for key in ("x", "y", "w", "h"):
            assert key in node
            assert isinstance(node[key], (int, float))


def test_layout_step_preserves_all_input_fields(fixture_graph: dict) -> None:
    step = fixture_graph["steps"][0]
    result = layout.layout_step(step)
    by_id_in = {n["id"]: n for n in step["nodes"]}
    by_id_out = {n["id"]: n for n in result["nodes"]}
    assert set(by_id_in) == set(by_id_out)
    for nid, node_out in by_id_out.items():
        node_in = by_id_in[nid]
        for key in ("label", "shape", "side"):
            assert node_out[key] == node_in[key]


def test_layout_step_bounding_boxes_do_not_overlap(fixture_graph: dict) -> None:
    step = fixture_graph["steps"][0]
    result = layout.layout_step(step)
    nodes = result["nodes"]
    for i, a in enumerate(nodes):
        for b in nodes[i + 1 :]:
            disjoint = (
                a["x"] + a["w"] <= b["x"]
                or b["x"] + b["w"] <= a["x"]
                or a["y"] + a["h"] <= b["y"]
                or b["y"] + b["h"] <= a["y"]
            )
            assert disjoint, f"Nodes {a['id']} and {b['id']} overlap"


def test_layout_graph_lays_out_every_step(fixture_graph: dict) -> None:
    result = layout.layout_graph(fixture_graph)
    assert len(result["steps"]) == len(fixture_graph["steps"])
    for step_out in result["steps"]:
        assert "viewBox" in step_out
        for node in step_out["nodes"]:
            for key in ("x", "y", "w", "h"):
                assert key in node


def test_layout_graph_preserves_top_level_fields(fixture_graph: dict) -> None:
    result = layout.layout_graph(fixture_graph)
    assert result["title"] == fixture_graph["title"]
    assert result["kinds"] == fixture_graph["kinds"]
    assert result["cross_flows"] == fixture_graph["cross_flows"]


def test_layout_small_graph_snapshot_with_tolerance() -> None:
    """Tiny graph: 3-node linear chain. Check shape + rough geometry, not exact numbers."""
    step = {
        "step": 1,
        "id": "tiny",
        "title": "tiny",
        "nodes": [
            {"id": "a", "shape": "rect", "side": "shared", "label": "first"},
            {"id": "b", "shape": "rect", "side": "shared", "label": "second"},
            {"id": "c", "shape": "rect", "side": "shared", "label": "third"},
        ],
        "edges": [
            {"from": "a", "to": "b"},
            {"from": "b", "to": "c"},
        ],
    }
    result = layout.layout_step(step)
    nodes = {n["id"]: n for n in result["nodes"]}
    # TB layout: a above b above c.
    assert nodes["a"]["y"] < nodes["b"]["y"] < nodes["c"]["y"]
    # All three have the same width (rect with same label length range)
    for nid in ("a", "b", "c"):
        assert 150 <= nodes[nid]["w"] <= 200
        assert 40 <= nodes[nid]["h"] <= 50
    # viewBox includes padding and covers all nodes
    vb = result["viewBox"]
    assert vb[0] == -16 and vb[1] == -16


def test_layout_step_handles_single_node() -> None:
    step = {
        "step": 1,
        "id": "single",
        "title": "single",
        "nodes": [{"id": "only", "shape": "rect", "side": "shared", "label": "only-one"}],
        "edges": [],
    }
    result = layout.layout_step(step)
    assert len(result["nodes"]) == 1
    node = result["nodes"][0]
    assert node["w"] > 0
    assert node["h"] > 0


def test_layout_step_diamond_decision_node(fixture_graph: dict) -> None:
    """Step 7 has a diamond branch node; verify it lays out with diamond dimensions."""
    step = next(s for s in fixture_graph["steps"] if s["id"] == "convert_to_struct")
    result = layout.layout_step(step)
    by_id = {n["id"]: n for n in result["nodes"]}
    # 'branch' is a diamond; allow small rounding from the inches round-trip
    assert by_id["branch"]["w"] >= 139
    assert abs(by_id["branch"]["h"] - 70) < 1
