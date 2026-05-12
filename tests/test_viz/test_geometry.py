"""Tests for server/geometry.py (SVG path generation)."""

from __future__ import annotations

import math

import pytest

from zing_ai.server import geometry as geom


@pytest.fixture
def rect_node() -> dict:
    return {"shape": "rect", "x": 100, "y": 50, "w": 200, "h": 44}


@pytest.fixture
def diamond_node() -> dict:
    return {"shape": "diamond", "x": 0, "y": 0, "w": 200, "h": 100}


@pytest.fixture
def hexagon_node() -> dict:
    return {"shape": "hexagon", "x": 0, "y": 0, "w": 240, "h": 56}


@pytest.fixture
def parallelogram_node() -> dict:
    return {"shape": "parallelogram", "x": 0, "y": 0, "w": 180, "h": 44}


@pytest.fixture
def diverged_node() -> dict:
    return {
        "shape": "diverged",
        "x": 0,
        "y": 0,
        "w": 240,
        "h": 100,
        "concern": "outcome on validation",
        "today_label": "skip row",
        "proposed_label": "return tuple",
    }


def test_node_outline_rect_returns_path_with_rounded_corners(rect_node: dict) -> None:
    d = geom.node_outline(rect_node)
    assert d.startswith("M ")
    assert d.endswith(" Z")
    # Rounded corners use SVG arcs
    assert " A 3 3 " in d


def test_node_outline_diamond_traces_four_points(diamond_node: dict) -> None:
    d = geom.node_outline(diamond_node)
    # Four L commands → four corners, closed with Z
    assert d.count(" L ") == 3
    assert d.endswith(" Z")
    # Center-x apex at top: 100,0
    assert d.startswith("M 100.0 0")


def test_node_outline_hexagon_has_six_corners(hexagon_node: dict) -> None:
    d = geom.node_outline(hexagon_node)
    # Hexagon: M + 5 L + Z = six vertices
    assert d.count(" L ") == 5
    assert d.endswith(" Z")


def test_node_outline_parallelogram_skews_right(parallelogram_node: dict) -> None:
    d = geom.node_outline(parallelogram_node)
    assert d.count(" L ") == 3
    assert d.endswith(" Z")
    # First point at (skew=14, 0)
    assert d.startswith("M 14 0")


def test_node_outline_diverged_raises_value_error(diverged_node: dict) -> None:
    with pytest.raises(ValueError, match="diverged_paths"):
        geom.node_outline(diverged_node)


def test_diverged_paths_returns_structured_layout(diverged_node: dict) -> None:
    layout = geom.diverged_paths(diverged_node)
    assert "outer" in layout
    assert layout["outer"].startswith("M ")
    # today_half is the upper inner rect; proposed_half is the lower one
    assert layout["today_half"]["y"] < layout["proposed_half"]["y"]
    # Dividers span the full width
    assert layout["divider_top"]["x2"] - layout["divider_top"]["x1"] == diverged_node["w"]
    # Concern anchor is centred at top
    assert layout["concern_anchor"]["x"] == diverged_node["x"] + diverged_node["w"] / 2


def test_make_bezier_uses_vertical_bend_when_dy_dominant() -> None:
    d = geom.make_bezier(0, 0, 0, 100)
    # Vertical layout: control points share x with their endpoint
    assert " C 0 " in d
    assert d.endswith(" 0 100")


def test_make_bezier_uses_horizontal_bend_when_dx_dominant() -> None:
    d = geom.make_bezier(0, 0, 100, 0)
    # Horizontal: control y == endpoint y
    assert " C " in d
    assert d.endswith(" 100 0")


def test_inner_edge_path_default_departs_bottom_arrives_top_for_vertical() -> None:
    from_node = {"x": 0, "y": 0, "w": 100, "h": 44}
    to_node = {"x": 0, "y": 120, "w": 100, "h": 44}
    edge = {"from": "a", "to": "b"}
    d = geom.inner_edge_path(edge, from_node, to_node)
    # Departs from (50, 44) - bottom of from_node
    assert d.startswith("M 50.0 44")
    # Arrives at top of to_node (50, 120)
    assert d.endswith(" 50.0 120")


def test_inner_edge_path_respects_from_side_left() -> None:
    from_node = {"x": 100, "y": 0, "w": 100, "h": 44}
    to_node = {"x": 0, "y": 0, "w": 80, "h": 44}
    edge = {"from": "a", "to": "b", "from_side": "left"}
    d = geom.inner_edge_path(edge, from_node, to_node)
    # Left port of from_node: (100, 22)
    assert d.startswith("M 100 22.0")


def test_pick_port_pair_prefers_horizontal_when_close() -> None:
    # from_node at left, to_node at right — natural right→left pair
    from_ports: dict[str, geom.Point] = {
        "left": {"x": 0.0, "y": 50.0},
        "right": {"x": 100.0, "y": 50.0},
        "top": {"x": 50.0, "y": 0.0},
        "bottom": {"x": 50.0, "y": 100.0},
    }
    to_ports: dict[str, geom.Point] = {
        "left": {"x": 200.0, "y": 50.0},
        "right": {"x": 300.0, "y": 50.0},
        "top": {"x": 250.0, "y": 0.0},
        "bottom": {"x": 250.0, "y": 100.0},
    }
    best = geom.pick_port_pair(from_ports, to_ports)
    assert best["from_side"] == "right"
    assert best["to_side"] == "left"


def test_cross_flow_path_returns_bezier_and_endpoints() -> None:
    from_ports: dict[str, geom.Point] = {
        "left": {"x": 0.0, "y": 50.0},
        "right": {"x": 100.0, "y": 50.0},
        "top": {"x": 50.0, "y": 0.0},
        "bottom": {"x": 50.0, "y": 100.0},
    }
    to_ports: dict[str, geom.Point] = {
        "left": {"x": 200.0, "y": 50.0},
        "right": {"x": 300.0, "y": 50.0},
        "top": {"x": 250.0, "y": 0.0},
        "bottom": {"x": 250.0, "y": 100.0},
    }
    result = geom.cross_flow_path(from_ports, to_ports)
    assert result["a"] == from_ports["right"]
    assert result["b"] == to_ports["left"]
    assert result["d"].startswith("M 100.0 50.0")
    assert result["d"].endswith("200.0 50.0")


def test_cross_flow_path_offset_increases_with_endpoint_distance() -> None:
    near_from: dict[str, geom.Point] = {
        "left": {"x": 0.0, "y": 0.0},
        "right": {"x": 0.0, "y": 0.0},
        "top": {"x": 0.0, "y": 0.0},
        "bottom": {"x": 0.0, "y": 0.0},
    }
    near_to: dict[str, geom.Point] = {
        "left": {"x": 10.0, "y": 0.0},
        "right": {"x": 10.0, "y": 0.0},
        "top": {"x": 10.0, "y": 0.0},
        "bottom": {"x": 10.0, "y": 0.0},
    }
    far_to: dict[str, geom.Point] = {
        "left": {"x": 1000.0, "y": 0.0},
        "right": {"x": 1000.0, "y": 0.0},
        "top": {"x": 1000.0, "y": 0.0},
        "bottom": {"x": 1000.0, "y": 0.0},
    }
    near_path = geom.cross_flow_path(near_from, near_to)["d"]
    far_path = geom.cross_flow_path(near_from, far_to)["d"]
    # Far path's control-point x-offset is larger than near's
    near_first_ctrl_x = float(near_path.split("C ")[1].split(" ")[0])
    far_first_ctrl_x = float(far_path.split("C ")[1].split(" ")[0])
    assert abs(far_first_ctrl_x) > abs(near_first_ctrl_x)


def test_port_normal_unit_vectors_match_prototype() -> None:
    assert geom.PORT_NORMAL["left"] == {"x": -1.0, "y": 0.0}
    assert geom.PORT_NORMAL["right"] == {"x": 1.0, "y": 0.0}
    assert geom.PORT_NORMAL["top"] == {"x": 0.0, "y": -1.0}
    assert geom.PORT_NORMAL["bottom"] == {"x": 0.0, "y": 1.0}
    # Verify they're unit vectors
    for v in geom.PORT_NORMAL.values():
        assert math.isclose(math.hypot(v["x"], v["y"]), 1.0)
